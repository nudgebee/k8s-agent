// Package gcp implements the gke_logs and gke_traces primitives by hitting
// Google Cloud's REST APIs directly. We deliberately avoid pulling
// cloud.google.com/go/{logging,bigquery} SDKs — they would add ~30 MB to
// the binary and a lot of dep tree. The REST API surface we need is small.
//
// Auth: Application Default Credentials via golang.org/x/oauth2/google.
// In-cluster this picks up Workload Identity automatically (the standard
// GKE pattern). Locally it picks up GOOGLE_APPLICATION_CREDENTIALS or
// gcloud user creds.
//
// Action surface:
//   - gke_logs   : Cloud Logging entries for a GKE node pool in a zone
//   - gke_traces : arbitrary BigQuery SQL (used for traces stored in BQ)
package gcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2/google"
)

// Client wraps an *http.Client that has ADC bearer-token injection wired in.
// Concurrent-safe.
type Client struct {
	HTTP *http.Client

	// Override URLs for tests. Empty = official Google endpoints.
	LoggingBaseURL  string
	BigQueryBaseURL string
}

const (
	defaultLoggingURL  = "https://logging.googleapis.com"
	defaultBigQueryURL = "https://bigquery.googleapis.com"
)

// New returns a Client whose HTTP transport carries ADC. ctx is used for
// initial token fetching and is honoured for context cancellation.
func New(ctx context.Context) (*Client, error) {
	scopes := []string{
		"https://www.googleapis.com/auth/logging.read",
		"https://www.googleapis.com/auth/bigquery.readonly",
	}
	httpClient, err := google.DefaultClient(ctx, scopes...)
	if err != nil {
		return nil, fmt.Errorf("gcp: load ADC: %w", err)
	}
	httpClient.Timeout = 60 * time.Second
	return &Client{HTTP: httpClient}, nil
}

// NewWithHTTP is a constructor for tests that bring their own client (e.g.
// httptest with a pre-injected fake token).
func NewWithHTTP(c *http.Client) *Client {
	return &Client{HTTP: c}
}

// FetchNodePoolLogs queries Cloud Logging for GCE-instance entries scoped
// to a zone.
//
// projectID : GCP project (required)
// zone      : compute zone, e.g. "us-central1-a" (required)
// limit     : max entries (defaults to 100)
func (c *Client) FetchNodePoolLogs(ctx context.Context, projectID, zone string, limit int) (json.RawMessage, error) {
	if projectID == "" {
		return nil, errors.New("gcp: project_id required")
	}
	if zone == "" {
		return nil, errors.New("gcp: zone required")
	}
	if limit <= 0 {
		limit = 100
	}

	body := map[string]any{
		"resourceNames": []string{"projects/" + projectID},
		"filter": fmt.Sprintf(
			`resource.type="gce_instance" AND resource.labels.zone="%s"`,
			zone,
		),
		"pageSize": limit,
		"orderBy":  "timestamp desc",
	}
	return c.postJSON(ctx, c.loggingURL()+"/v2/entries:list", body)
}

// QueryBigQuery runs an arbitrary SQL query as a synchronous query job.
// Does not perform server-side pagination (callers needing pagination iterate
// with maxResults + pageToken via a follow-up action).
func (c *Client) QueryBigQuery(ctx context.Context, projectID, query string) (json.RawMessage, error) {
	if projectID == "" {
		return nil, errors.New("gcp: project_id required")
	}
	if query == "" {
		return nil, errors.New("gcp: query required")
	}
	body := map[string]any{
		"query":        query,
		"useLegacySql": false,
		"timeoutMs":    30000,
		// No maxResults — let BigQuery default; backend can cap on its end.
	}
	url := c.bigQueryURL() + "/bigquery/v2/projects/" + projectID + "/queries"
	return c.postJSON(ctx, url, body)
}

func (c *Client) loggingURL() string {
	if c.LoggingBaseURL != "" {
		return strings.TrimRight(c.LoggingBaseURL, "/")
	}
	return defaultLoggingURL
}

func (c *Client) bigQueryURL() string {
	if c.BigQueryBaseURL != "" {
		return strings.TrimRight(c.BigQueryBaseURL, "/")
	}
	return defaultBigQueryURL
}

func (c *Client) postJSON(ctx context.Context, url string, body any) (json.RawMessage, error) {
	if c.HTTP == nil {
		return nil, errors.New("gcp: HTTP client not configured")
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gcp post %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("gcp %s: HTTP %d: %s", url, resp.StatusCode, string(respBody))
	}
	return json.RawMessage(respBody), nil
}
