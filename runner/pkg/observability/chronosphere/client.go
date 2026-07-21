// Package chronosphere is a thin HTTP wrapper for Chronosphere's tracing
// search API.
//
// Action surface:
//   - chronosphere_query_traces : POST /api/v1/data/traces
//
// The action_params is forwarded as the JSON body unchanged. The backend
// composes the ListTraces request body (query_type, tag_filters, time range),
// matching the legacy agent which posts to the same /api/v1/data/traces path.
// (The earlier /api/unstable/data/traces/searches path 404s on Chronosphere.)
//
// Chronosphere's gateway sheds load with a transient gRPC UNAVAILABLE (code 14)
// mapped to HTTP 503 — "Something went wrong and the request could not
// complete ... trying again". QueryTraces retries that condition with backoff
// and bounds concurrent in-flight requests so a burst of trace queries doesn't
// pile onto an already-struggling backend. The api-server's legacy proxy path
// already retries the same message (isRetryableError); the agent path did not,
// so a single Chronosphere blip surfaced as a hard failure to the caller.
package chronosphere

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaxRetries    = 2
	defaultRetryBackoff  = 500 * time.Millisecond
	defaultMaxConcurrent = 4
	maxRetryBackoff      = 30 * time.Second
)

type Client struct {
	BaseURL string
	APIKey  string // sent as Authorization: Bearer <key>
	HTTP    *http.Client

	// MaxRetries is the number of additional attempts after the first when
	// Chronosphere returns a transient error (HTTP 503 / gRPC UNAVAILABLE).
	MaxRetries int
	// RetryBackoff is the base delay for exponential backoff between retries.
	RetryBackoff time.Duration
	// MaxConcurrent bounds concurrent in-flight requests; <= 0 disables the
	// limit. Set before the first QueryTraces call — the semaphore is built
	// once, lazily, on first use.
	MaxConcurrent int

	semOnce sync.Once
	sem     chan struct{}
}

func New(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{
		BaseURL:       strings.TrimRight(baseURL, "/"),
		HTTP:          httpClient,
		MaxRetries:    defaultMaxRetries,
		RetryBackoff:  defaultRetryBackoff,
		MaxConcurrent: defaultMaxConcurrent,
	}
}

// acquire takes a concurrency slot, returning a release func. When the limit is
// disabled (MaxConcurrent <= 0) it is a no-op. Slots are held only for the
// duration of an active HTTP request, not across backoff sleeps.
func (c *Client) acquire(ctx context.Context) (func(), error) {
	c.semOnce.Do(func() {
		if c.MaxConcurrent > 0 {
			c.sem = make(chan struct{}, c.MaxConcurrent)
		}
	})
	if c.sem == nil {
		return func() {}, nil
	}
	select {
	case c.sem <- struct{}{}:
		return func() { <-c.sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// QueryTraces forwards params as the search request body, retrying transient
// Chronosphere backend-unavailable errors with exponential backoff.
func (c *Client) QueryTraces(ctx context.Context, params any) (json.RawMessage, error) {
	if c.BaseURL == "" {
		return nil, errors.New("chronosphere: base URL not configured")
	}
	if params == nil {
		return nil, errors.New("chronosphere: params required")
	}
	body, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	var lastErr error
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			if err := sleepBackoff(ctx, c.RetryBackoff, attempt); err != nil {
				return nil, err
			}
		}
		raw, retryable, err := c.doQuery(ctx, body)
		if err == nil {
			return raw, nil
		}
		lastErr = err
		if !retryable || attempt >= c.MaxRetries {
			return nil, lastErr
		}
	}
}

// doQuery performs a single request. retryable is true when the failure is a
// transient Chronosphere condition worth another attempt.
func (c *Client) doQuery(ctx context.Context, body []byte) (raw json.RawMessage, retryable bool, err error) {
	// Bound concurrent requests so a burst of trace queries doesn't pile onto
	// an already-struggling Chronosphere backend (which sheds load with 503).
	// Held only for the active request, not across backoff sleeps.
	release, err := c.acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	defer release()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/api/v1/data/traces", bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// Transport errors are transient unless the context is done (retrying
		// a cancelled/expired request is pointless).
		return nil, ctx.Err() == nil, fmt.Errorf("chronosphere: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, ctx.Err() == nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, isRetryable(resp.StatusCode, respBody),
			fmt.Errorf("chronosphere HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return json.RawMessage(respBody), false, nil
}

// isRetryable reports whether a Chronosphere error response is a transient
// backend-unavailable condition. Mirrors api-server's isRetryableError plus the
// gRPC UNAVAILABLE (code 14) that Chronosphere's gateway maps to HTTP 503.
func isRetryable(status int, body []byte) bool {
	if status == http.StatusServiceUnavailable {
		return true
	}
	return bytes.Contains(body, []byte("Something went wrong and the request could not complete")) ||
		bytes.Contains(body, []byte("In many cases the issue can be resolved by trying again")) ||
		bytes.Contains(body, []byte("Service Unavailable")) ||
		bytes.Contains(body, []byte(`"code":14`))
}

// sleepBackoff waits base*2^(attempt-1), capped at maxRetryBackoff (and guarded
// against shift overflow), or returns early if ctx is done.
func sleepBackoff(ctx context.Context, base time.Duration, attempt int) error {
	shift := attempt - 1
	if shift > 30 {
		shift = 30
	}
	delay := base * time.Duration(1<<shift)
	if delay > maxRetryBackoff || delay <= 0 {
		delay = maxRetryBackoff
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
