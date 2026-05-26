package gcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient bypasses ADC by injecting a plain *http.Client. The test
// servers' URLs are wired in via the LoggingBaseURL / BigQueryBaseURL
// override fields. This is the same pattern the prom/loki/jaeger tests use.
func newTestClient() *Client {
	return NewWithHTTP(&http.Client{Timeout: 5 * time.Second})
}

func TestFetchNodePoolLogs_PostsExpectedFilter(t *testing.T) {
	var path, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = w.Write([]byte(`{"entries":[]}`))
	}))
	defer srv.Close()

	c := newTestClient()
	c.LoggingBaseURL = srv.URL
	if _, err := c.FetchNodePoolLogs(context.Background(), "my-proj", "us-central1-a", 50); err != nil {
		t.Fatal(err)
	}
	if path != "/v2/entries:list" {
		t.Errorf("path = %q", path)
	}
	if !strings.Contains(body, `"projects/my-proj"`) {
		t.Errorf("missing resourceNames: %s", body)
	}
	if !strings.Contains(body, `resource.labels.zone=\"us-central1-a\"`) {
		t.Errorf("missing zone filter: %s", body)
	}
	if !strings.Contains(body, `"pageSize":50`) {
		t.Errorf("missing pageSize: %s", body)
	}
}

func TestFetchNodePoolLogs_DefaultLimit(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := newTestClient()
	c.LoggingBaseURL = srv.URL
	if _, err := c.FetchNodePoolLogs(context.Background(), "p", "z", 0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"pageSize":100`) {
		t.Errorf("default limit not applied: %s", body)
	}
}

func TestFetchNodePoolLogs_RequiresProjectAndZone(t *testing.T) {
	c := newTestClient()
	if _, err := c.FetchNodePoolLogs(context.Background(), "", "z", 1); err == nil {
		t.Error("missing project should error")
	}
	if _, err := c.FetchNodePoolLogs(context.Background(), "p", "", 1); err == nil {
		t.Error("missing zone should error")
	}
}

func TestQueryBigQuery_PostsToProjectQueriesEndpoint(t *testing.T) {
	var path, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = w.Write([]byte(`{"jobComplete":true,"rows":[]}`))
	}))
	defer srv.Close()
	c := newTestClient()
	c.BigQueryBaseURL = srv.URL
	if _, err := c.QueryBigQuery(context.Background(), "my-proj", "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if path != "/bigquery/v2/projects/my-proj/queries" {
		t.Errorf("path = %q", path)
	}
	if !strings.Contains(body, `"query":"SELECT 1"`) || !strings.Contains(body, `"useLegacySql":false`) {
		t.Errorf("body = %s", body)
	}
}

func TestQueryBigQuery_RequiresProjectAndQuery(t *testing.T) {
	c := newTestClient()
	if _, err := c.QueryBigQuery(context.Background(), "", "SELECT 1"); err == nil {
		t.Error("missing project should error")
	}
	if _, err := c.QueryBigQuery(context.Background(), "p", ""); err == nil {
		t.Error("missing query should error")
	}
}

func TestPropagatesHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"message":"missing perm"}}`))
	}))
	defer srv.Close()
	c := newTestClient()
	c.LoggingBaseURL = srv.URL
	_, err := c.FetchNodePoolLogs(context.Background(), "p", "z", 1)
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("expected HTTP 403, got %v", err)
	}
}

func TestPostJSON_NoHTTPClient(t *testing.T) {
	c := &Client{}
	_, err := c.postJSON(context.Background(), "http://x", map[string]any{})
	if err == nil {
		t.Error("expected error when HTTP client unset")
	}
}

func TestHandlers_AllRegistered(t *testing.T) {
	hs := Handlers(newTestClient(), "default-proj")
	for _, want := range []string{"gke_logs", "gke_traces"} {
		if _, ok := hs[want]; !ok {
			t.Errorf("missing %s", want)
		}
	}
}

func TestHandlers_UseDefaultProjectID(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := newTestClient()
	c.LoggingBaseURL = srv.URL
	hs := Handlers(c, "fallback-proj")
	if _, err := hs["gke_logs"](context.Background(), map[string]any{
		"zone":  "us-east1-a",
		"limit": float64(7),
		// project_id intentionally omitted
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"projects/fallback-proj"`) {
		t.Errorf("default project_id not applied: %s", body)
	}
	if !strings.Contains(body, `"pageSize":7`) {
		t.Errorf("limit not applied: %s", body)
	}
}

func TestHandlers_ParamsOverrideDefault(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := newTestClient()
	c.BigQueryBaseURL = srv.URL
	hs := Handlers(c, "default-proj")
	got, err := hs["gke_traces"](context.Background(), map[string]any{
		"project_id": "explicit-proj",
		"query":      "SELECT 2",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := got.(json.RawMessage)
	if string(raw) == "" {
		t.Error("handler returned empty raw response")
	}
	if !strings.Contains(body, `SELECT 2`) {
		t.Errorf("query not forwarded: %s", body)
	}
}

func TestNew_FailsCleanlyWithoutCreds(t *testing.T) {
	// google.DefaultClient should still succeed on most dev machines (gcloud
	// installed), but we want the error path covered. Force ADC failure by
	// pointing GOOGLE_APPLICATION_CREDENTIALS at a non-existent file AND
	// disabling default token sources via NO_GCE_CHECK.
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/no/such/file.json")
	t.Setenv("NO_GCE_CHECK", "true")
	t.Setenv("CLOUDSDK_CONFIG", t.TempDir()) // empty gcloud config dir
	t.Setenv("HOME", t.TempDir())
	// On many CI machines, none of the above default token sources will resolve;
	// New() should error. On dev machines with gcloud creds, it'll succeed —
	// that's also fine.
	_, err := New(context.Background())
	if err == nil {
		t.Skip("dev machine has ADC creds; error path not exercised here")
	}
	if !strings.Contains(err.Error(), "ADC") && !strings.Contains(err.Error(), "credentials") {
		t.Errorf("error %q does not look like ADC failure", err.Error())
	}
}
