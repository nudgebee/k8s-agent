package enrichers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nudgebee/nudgebee-agent/pkg/observability/loki"
)

// TestLokiCompat_SendsBasicAuth verifies the compat handlers apply
// LOKI_USERNAME/LOKI_PASSWORD basic auth, matching the primitive
// loki.Client path. Regression guard for the 401-on-basic-auth-Loki bug.
func TestLokiCompat_SendsBasicAuth(t *testing.T) {
	var gotUser, gotPass string
	var gotOK bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, gotOK = r.BasicAuth()
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	defer srv.Close()

	lc := loki.New(srv.URL, &http.Client{Timeout: 5 * time.Second})
	lc.Username = "loki-user"
	lc.Password = "loki-pass"
	compat := NewLokiCompat(lc)

	h := compat.Handlers()["query_loki"]
	if _, err := h(context.Background(), map[string]any{"query": "query={foo=\"bar\"}"}); err != nil {
		t.Fatal(err)
	}
	if !gotOK {
		t.Fatal("no basic auth header reached upstream")
	}
	if gotUser != "loki-user" || gotPass != "loki-pass" {
		t.Errorf("basic auth = %q/%q; want loki-user/loki-pass", gotUser, gotPass)
	}
}

// TestLokiCompat_NoAuthWhenUnset confirms we don't send basic auth when
// username/password aren't configured (only one set counts as unset).
func TestLokiCompat_NoAuthWhenUnset(t *testing.T) {
	var sawAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _, sawAuth = r.BasicAuth()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	lc := loki.New(srv.URL, &http.Client{Timeout: 5 * time.Second})
	lc.Username = "only-user" // password empty → must not send basic auth
	compat := NewLokiCompat(lc)

	h := compat.Handlers()["query_loki"]
	if _, err := h(context.Background(), map[string]any{"query": "query=x"}); err != nil {
		t.Fatal(err)
	}
	if sawAuth {
		t.Error("basic auth sent even though password is empty")
	}
}
