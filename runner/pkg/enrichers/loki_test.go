package enrichers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nudgebee/nudgebee-agent/pkg/observability/loki"
)

// TestLokiCompat_SendsBasicAuth covers LOKI_USERNAME/LOKI_PASSWORD on the
// compat actions: they build their own request instead of going through
// loki.Client.get, and used to drop the credentials.
func TestLokiCompat_SendsBasicAuth(t *testing.T) {
	var gotUser, gotPass, gotHeader string
	var gotAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, gotAuth = r.BasicAuth()
		gotHeader = r.Header.Get("X-Scope-OrgID")
		_, _ = w.Write([]byte(`{"status":"success","data":["namespace"]}`))
	}))
	defer srv.Close()

	c := loki.New(srv.URL, &http.Client{Timeout: 5 * time.Second})
	c.Username = "reader"
	c.Password = "s3cret"
	c.ExtraHeaders = http.Header{"X-Scope-OrgID": []string{"tenant-1"}}
	handlers := NewLokiCompat(c).Handlers()

	for _, action := range []string{"query_loki", "query_loki_labels"} {
		gotUser, gotPass, gotHeader, gotAuth = "", "", "", false
		res, err := handlers[action](context.Background(), map[string]any{"query": "query=%7Bjob%3D%22x%22%7D"})
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		if m, _ := res.(map[string]any); m["success"] != true {
			t.Fatalf("%s: reply %v, want success", action, res)
		}
		if !gotAuth || gotUser != "reader" || gotPass != "s3cret" {
			t.Errorf("%s: basic auth = (%q, %q, %v), want (reader, s3cret, true)", action, gotUser, gotPass, gotAuth)
		}
		if gotHeader != "tenant-1" {
			t.Errorf("%s: X-Scope-OrgID = %q, want tenant-1", action, gotHeader)
		}
	}
}

// TestLokiCompat_NoCredentialsNoAuthHeader keeps an unauthenticated Loki
// unauthenticated: a half-configured pair sends nothing, as loki.Client does.
func TestLokiCompat_NoCredentialsNoAuthHeader(t *testing.T) {
	var gotAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _, gotAuth = r.BasicAuth()
		_, _ = w.Write([]byte(`{"status":"success","data":[]}`))
	}))
	defer srv.Close()

	c := loki.New(srv.URL, &http.Client{Timeout: 5 * time.Second})
	c.Username = "reader" // no password
	if _, err := NewLokiCompat(c).Handlers()["query_loki_labels"](context.Background(), map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if gotAuth {
		t.Error("sent Basic-Auth with no password configured")
	}
}
