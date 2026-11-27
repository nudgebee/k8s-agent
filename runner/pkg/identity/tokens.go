package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// tokenTTL is the requested lifetime. 600 s is the TokenRequest minimum
	// and is below any --service-account-max-token-expiration.
	tokenTTL = 600 * time.Second
	// refreshFraction of the lifetime after which a token is replaced.
	refreshFraction = 0.8
	minBackoff      = time.Second
	maxBackoff      = 60 * time.Second
)

// TokenSource mints and refreshes a short-lived token for one ServiceAccount
// through the TokenRequest API, using the runner's own client. Token never
// blocks on the network after the first successful mint: a failed refresh
// keeps the current token until it expires.
type TokenSource struct {
	client            kubernetes.Interface
	namespace, sa     string
	logger            *slog.Logger
	now               func() time.Time
	sleep             func(context.Context, time.Duration) bool
	mu                sync.RWMutex
	token             string
	expires           time.Time
	refreshAt         time.Time
	lastErr           error
	mintedAtLeastOnce bool
}

// NewTokenSource returns a TokenSource for namespace/sa. Call Start before Token.
func NewTokenSource(client kubernetes.Interface, namespace, sa string, logger *slog.Logger) *TokenSource {
	if logger == nil {
		logger = slog.Default()
	}
	return &TokenSource{
		client: client, namespace: namespace, sa: sa, logger: logger,
		now: time.Now, sleep: sleepCtx,
	}
}

// mint requests one token. Audiences are left empty so the API server's own
// audience applies (it differs per managed platform).
func (t *TokenSource) mint(ctx context.Context) error {
	ttl := int64(tokenTTL / time.Second)
	tr, err := t.client.CoreV1().ServiceAccounts(t.namespace).CreateToken(ctx, t.sa,
		&authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &ttl}},
		metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("identity: mint token for %s/%s: %w", t.namespace, t.sa, err)
	}
	if tr.Status.Token == "" {
		return fmt.Errorf("identity: mint token for %s/%s: empty token", t.namespace, t.sa)
	}
	issued := t.now()
	exp := tr.Status.ExpirationTimestamp.Time
	if exp.IsZero() || !exp.After(issued) {
		exp = issued.Add(tokenTTL)
	}
	t.mu.Lock()
	t.token = tr.Status.Token
	t.expires = exp
	t.refreshAt = issued.Add(time.Duration(float64(exp.Sub(issued)) * refreshFraction))
	t.lastErr = nil
	t.mintedAtLeastOnce = true
	t.mu.Unlock()
	return nil
}

// Start mints the first token, retrying with backoff until it succeeds or ctx
// ends, then keeps refreshing in the background until ctx ends. It returns the
// error that ended the first attempt only if ctx ended before any success.
func (t *TokenSource) Start(ctx context.Context) error {
	backoff := minBackoff
	for {
		err := t.mint(ctx)
		if err == nil {
			break
		}
		t.setErr(err)
		t.logger.Warn("identity: token mint failed, retrying", "service_account", t.sa, "retry_in", backoff, "err", err)
		if !t.sleep(ctx, jitter(backoff)) {
			return err
		}
		backoff = min(backoff*2, maxBackoff)
	}
	go t.refreshLoop(ctx)
	return nil
}

func (t *TokenSource) refreshLoop(ctx context.Context) {
	backoff := minBackoff
	for {
		t.mu.RLock()
		wait := t.refreshAt.Sub(t.now())
		t.mu.RUnlock()
		if wait > 0 && !t.sleep(ctx, wait) {
			return
		}
		if err := t.mint(ctx); err != nil {
			t.setErr(err)
			t.logger.Warn("identity: token refresh failed, keeping the current token", "service_account", t.sa, "retry_in", backoff, "err", err)
			if !t.sleep(ctx, jitter(backoff)) {
				return
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = minBackoff
	}
}

func (t *TokenSource) setErr(err error) {
	t.mu.Lock()
	t.lastErr = err
	t.mu.Unlock()
}

// ErrNoToken is returned when no unexpired token is available.
var ErrNoToken = errors.New("identity: no valid token")

// Token returns the current token, or ErrNoToken (wrapping the last mint
// error) when none has been minted or the last one expired.
func (t *TokenSource) Token() (string, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.token == "" || !t.now().Before(t.expires) {
		if t.lastErr != nil {
			return "", fmt.Errorf("%w: %v", ErrNoToken, t.lastErr)
		}
		return "", ErrNoToken
	}
	return t.token, nil
}

// bearerTransport sets the current token on every request. The identity's
// rest.Config carries no other credential, so a request without a token fails
// closed rather than going out as the runner.
type bearerTransport struct {
	ts   *TokenSource
	next http.RoundTripper
}

func (b *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := b.ts.Token()
	if err != nil {
		return nil, err
	}
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+tok)
	return b.next.RoundTrip(r)
}

func jitter(d time.Duration) time.Duration {
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1)) //nolint:gosec // retry jitter, not a secret
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
