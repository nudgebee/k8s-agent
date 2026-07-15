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
//   - gke_logs   : GKE cluster-autoscaler visibility logs for a node pool
//   - gke_traces : arbitrary BigQuery SQL (reshaped to {data,columns,column_types})
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

// FetchNodePoolLogs queries Cloud Logging for the GKE cluster-autoscaler
// visibility logs (node-pool scale-up/scale-down decisions), matching the
// legacy gcloud_client. These logs live under the k8s_cluster resource and
// are keyed by the cluster's location — zonal clusters use the zone, regional
// clusters use the region — so we try the zone first and fall back to the
// derived region.
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

	locations := []string{zone}
	if region := zoneToRegion(zone); region != "" && region != zone {
		locations = append(locations, region)
	}

	var last json.RawMessage
	for _, loc := range locations {
		raw, err := c.postJSON(ctx, c.loggingURL()+"/v2/entries:list", nodePoolLogsBody(projectID, loc, limit))
		if err != nil {
			return nil, err
		}
		last = raw
		if hasLogEntries(raw) {
			return raw, nil
		}
	}
	return last, nil
}

// nodePoolLogsBody builds the entries:list request scoped to one location.
func nodePoolLogsBody(projectID, location string, limit int) map[string]any {
	return map[string]any{
		"resourceNames": []string{"projects/" + projectID},
		"filter": fmt.Sprintf(
			`resource.type="k8s_cluster" AND resource.labels.project_id="%s" `+
				`AND resource.labels.location="%s" `+
				`AND logName="projects/%s/logs/container.googleapis.com%%2Fcluster-autoscaler-visibility" `+
				`AND severity>=DEFAULT`,
			projectID, location, projectID,
		),
		"pageSize": limit,
		"orderBy":  "timestamp desc",
	}
}

// zoneToRegion strips the trailing zone suffix, e.g. "us-central1-a" ->
// "us-central1". Returns the input unchanged when it has no suffix.
func zoneToRegion(zone string) string {
	if len(zone) > 2 && zone[len(zone)-2] == '-' {
		lastChar := zone[len(zone)-1]
		if lastChar >= 'a' && lastChar <= 'z' {
			return zone[:len(zone)-2]
		}
	}
	return zone
}

// hasLogEntries reports whether a Cloud Logging entries:list response carries
// at least one entry.
func hasLogEntries(raw json.RawMessage) bool {
	var resp struct {
		Entries []json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return false
	}
	return len(resp.Entries) > 0
}

// QueryBigQuery runs an arbitrary SQL query as a synchronous query job and
// reshapes the BigQuery REST response into the {data, columns, column_types}
// envelope the backend warehouse consumer expects (mirroring the legacy
// run_bigquery). location, when non-empty, scopes the job to the dataset's
// region (required for non-US/EU datasets). Does not perform server-side
// pagination.
func (c *Client) QueryBigQuery(ctx context.Context, projectID, query, location string) (json.RawMessage, error) {
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
	if location != "" {
		body["location"] = location
	}
	url := c.bigQueryURL() + "/bigquery/v2/projects/" + projectID + "/queries"
	raw, err := c.postJSON(ctx, url, body)
	if err != nil {
		return nil, err
	}
	return reshapeBigQuery(raw)
}

// reshapeBigQuery converts the BigQuery jobs.query REST response
// ({schema:{fields:[{name,type}]}, rows:[{f:[{v}]}]}) into the
// {data, columns, column_types} shape the backend warehouse consumer reads.
func reshapeBigQuery(raw json.RawMessage) (json.RawMessage, error) {
	var resp struct {
		Schema struct {
			Fields []struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"fields"`
		} `json:"schema"`
		Rows []struct {
			F []struct {
				V json.RawMessage `json:"v"`
			} `json:"f"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("gcp: parse bigquery response: %w", err)
	}

	columns := make([]string, len(resp.Schema.Fields))
	columnTypes := make([]string, len(resp.Schema.Fields))
	for i, f := range resp.Schema.Fields {
		columns[i] = f.Name
		columnTypes[i] = f.Type
	}

	data := make([][]any, 0, len(resp.Rows))
	for _, row := range resp.Rows {
		vals := make([]any, len(row.F))
		for i, cell := range row.F {
			var v any
			// BigQuery cell values are JSON scalars (usually strings); keep the
			// decoded value, or nil on an unexpected shape.
			_ = json.Unmarshal(cell.V, &v)
			vals[i] = v
		}
		data = append(data, vals)
	}

	return json.Marshal(map[string]any{
		"data":         data,
		"columns":      columns,
		"column_types": columnTypes,
	})
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
