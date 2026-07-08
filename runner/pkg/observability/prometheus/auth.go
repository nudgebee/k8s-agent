package prometheus

// Managed-provider auth for Prometheus-compatible backends, mirroring the
// legacy prometrix PrometheusAuthorization logic:
//
//   - Coralogix : a static `token` header (CoralogixPrometheusConfig).
//   - AWS       : SigV4 request signing for Amazon Managed Prometheus (AMP).
//   - Azure     : a Bearer token minted via managed identity or client-secret,
//     cached until shortly before it expires.
//
// The plain header/basic cases are already covered by Client.ExtraHeaders
// (PROMETHEUS_HEADERS), so only these three dynamic schemes live here.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// Authorizer applies provider-specific auth to an outgoing request just
// before it is sent. Implementations must be safe for concurrent use.
type Authorizer interface {
	Apply(ctx context.Context, req *http.Request) error
}

// --- Coralogix -------------------------------------------------------------

type coralogixAuth struct{ token string }

// NewCoralogixAuth returns an Authorizer that sends the Coralogix
// `token` header (matches PrometheusAuthorization.get_authorization_headers).
func NewCoralogixAuth(token string) Authorizer { return &coralogixAuth{token: token} }

func (a *coralogixAuth) Apply(_ context.Context, req *http.Request) error {
	req.Header.Set("token", a.token)
	return nil
}

// --- AWS SigV4 -------------------------------------------------------------

// emptyPayloadHash is SHA-256 of the empty string — every Prometheus query
// the agent issues is a GET with no body.
var emptyPayloadHash = func() string {
	sum := sha256.Sum256(nil)
	return hex.EncodeToString(sum[:])
}()

type awsAuth struct {
	creds   aws.Credentials
	region  string
	service string
	signer  *v4.Signer
}

// NewAWSAuth returns an Authorizer that signs each request with AWS SigV4,
// as the legacy AWSPrometheusConnect does via botocore. service defaults to
// "aps" (Amazon Managed Prometheus) when empty.
func NewAWSAuth(accessKey, secretKey, region, service string) Authorizer {
	if service == "" {
		service = "aps"
	}
	return &awsAuth{
		creds:   aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secretKey},
		region:  region,
		service: service,
		signer:  v4.NewSigner(),
	}
}

func (a *awsAuth) Apply(ctx context.Context, req *http.Request) error {
	// GET requests carry the query in the URL and no body, so the empty
	// payload hash is correct for every call site.
	return a.signer.SignHTTP(ctx, a.creds, req, emptyPayloadHash, a.service, a.region, time.Now())
}

// --- Azure -----------------------------------------------------------------

// AzureAuthConfig carries the managed-identity / client-secret settings.
// Endpoints and resource have defaults applied by config.FromEnv.
type AzureAuthConfig struct {
	UseManagedID     string
	ClientID         string
	ClientSecret     string
	TenantID         string
	Resource         string
	MetadataEndpoint string
	TokenEndpoint    string
}

// authorized reports whether the config has enough to mint a token — mirrors
// PrometheusAuthorization.azure_authorization.
func (c AzureAuthConfig) authorized() bool {
	return c.ClientID != "" && c.TenantID != "" && (c.ClientSecret != "" || c.UseManagedID != "")
}

type azureAuth struct {
	cfg  AzureAuthConfig
	http *http.Client

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// NewAzureAuth returns an Authorizer that mints and caches an Azure AD bearer
// token. Returns nil if the config isn't complete enough to authorize, so the
// caller can fall through to no auth.
func NewAzureAuth(cfg AzureAuthConfig, httpClient *http.Client) Authorizer {
	if !cfg.authorized() {
		return nil
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &azureAuth{cfg: cfg, http: httpClient}
}

func (a *azureAuth) Apply(ctx context.Context, req *http.Request) error {
	tok, err := a.bearer(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return nil
}

func (a *azureAuth) bearer(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && time.Now().Add(60*time.Second).Before(a.expiry) {
		return a.token, nil
	}
	tok, exp, err := a.requestToken(ctx)
	if err != nil {
		return "", err
	}
	a.token, a.expiry = tok, exp
	return tok, nil
}

// azureTokenResponse matches both the IMDS and the client-credentials shapes.
// The expiry fields come back as JSON strings from IMDS but as JSON numbers
// from the client-credentials (v2) endpoint, so use json.Number, which
// unmarshals from either.
type azureTokenResponse struct {
	AccessToken string      `json:"access_token"`
	ExpiresIn   json.Number `json:"expires_in"`
	ExpiresOn   json.Number `json:"expires_on"` // unix seconds (absolute)
}

func (a *azureAuth) requestToken(ctx context.Context) (string, time.Time, error) {
	var req *http.Request
	var err error
	if a.cfg.UseManagedID != "" {
		// IMDS managed-identity flow: GET with query params + Metadata header.
		q := url.Values{}
		q.Set("api-version", "2018-02-01")
		q.Set("client_id", a.cfg.ClientID)
		q.Set("resource", a.cfg.Resource)
		u := a.cfg.MetadataEndpoint + "?" + q.Encode()
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return "", time.Time{}, err
		}
		req.Header.Set("Metadata", "true")
	} else {
		// Client-credentials flow: form POST to the token endpoint.
		form := url.Values{}
		form.Set("grant_type", "client_credentials")
		form.Set("client_id", a.cfg.ClientID)
		form.Set("client_secret", a.cfg.ClientSecret)
		form.Set("resource", a.cfg.Resource)
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.TokenEndpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return "", time.Time{}, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("azure token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", time.Time{}, err
	}
	if resp.StatusCode >= 400 {
		return "", time.Time{}, fmt.Errorf("azure token: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var tr azureTokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", time.Time{}, fmt.Errorf("azure token: decode: %w", err)
	}
	if tr.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("azure token: empty access_token")
	}
	return tr.AccessToken, azureExpiry(tr), nil
}

// azureExpiry derives an absolute expiry from the token response, preferring
// the absolute expires_on and falling back to now+expires_in. A short default
// keeps us from caching indefinitely if neither field parses.
func azureExpiry(tr azureTokenResponse) time.Time {
	if secs, err := tr.ExpiresOn.Int64(); err == nil && secs > 0 {
		return time.Unix(secs, 0)
	}
	if secs, err := tr.ExpiresIn.Int64(); err == nil && secs > 0 {
		return time.Now().Add(time.Duration(secs) * time.Second)
	}
	return time.Now().Add(5 * time.Minute)
}
