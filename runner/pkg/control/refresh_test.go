package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/nudgebee/nudgebee-agent/pkg/auth"
)

func TestRefresh_NoBackend_ReturnsNoOp(t *testing.T) {
	v := &auth.Validator{LightActions: map[string]struct{}{"ping": {}}}
	r := New("", "secret", "acc", "cluster", v)
	got, err := r.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got["refreshed"] != false {
		t.Errorf("expected refreshed=false; got %v", got)
	}
	// Allowlist must be unchanged.
	if _, ok := v.LightActionsSet()["ping"]; !ok {
		t.Error("ping should still be allowed")
	}
}

func TestRefresh_ValidatorRequired(t *testing.T) {
	r := New("http://x", "", "", "", nil)
	if _, err := r.Refresh(context.Background()); err == nil {
		t.Error("expected error when validator unset")
	}
}

func TestRefresh_HappyPath_SwapsAllowlist(t *testing.T) {
	var gotPath, gotAccount string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAccount = r.Header.Get("X-NB-Account-Id")
		_, _ = w.Write([]byte(`{"light_actions":["ping","query_loki","new_action"]}`))
	}))
	defer srv.Close()

	v := &auth.Validator{LightActions: map[string]struct{}{"ping": {}}}
	r := New(srv.URL, "secret", "acc-1", "cluster-x", v)
	r.StaticActions = []string{"health"} // always allowed

	got, err := r.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/agent/config" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAccount != "acc-1" {
		t.Errorf("X-NB-Account-Id = %q", gotAccount)
	}
	if got["refreshed"] != true {
		t.Errorf("refreshed = %v", got["refreshed"])
	}

	set := v.LightActionsSet()
	for _, want := range []string{"ping", "query_loki", "new_action", "health"} {
		if _, ok := set[want]; !ok {
			t.Errorf("set missing %q after refresh", want)
		}
	}
}

func TestRefresh_AuthHeaderIsBareBase64(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"light_actions":[]}`))
	}))
	defer srv.Close()
	r := New(srv.URL, "test-secret", "", "", newValidator())
	if _, err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Bare base64, no "Basic " prefix — matches the legacy sink format.
	if auth == "" || strings.HasPrefix(auth, "Basic ") {
		t.Errorf("Authorization = %q (expected bare base64, no Basic prefix)", auth)
	}
}

func TestRefresh_PropagatesHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"forbidden"}`))
	}))
	defer srv.Close()
	r := New(srv.URL, "", "", "", newValidator())
	if _, err := r.Refresh(context.Background()); err == nil {
		t.Error("expected error for HTTP 403")
	}
}

func TestRefresh_RejectsNonJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()
	r := New(srv.URL, "", "", "", newValidator())
	if _, err := r.Refresh(context.Background()); err == nil {
		t.Error("expected parse error")
	}
}

func TestRefresh_ConcurrentSafe(t *testing.T) {
	// Ensure concurrent Refresh + Validate calls don't tear the allowlist.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"light_actions":["a","b","c","d","e"]}`))
	}))
	defer srv.Close()

	v := &auth.Validator{LightActions: map[string]struct{}{}}
	r := New(srv.URL, "", "", "", v)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = r.Refresh(context.Background())
		}()
		go func() {
			defer wg.Done()
			_ = v.Validate(&auth.Request{ActionName: "a"})
		}()
	}
	wg.Wait()
	// No panic == pass; race detector will catch any data race.
	set := v.LightActionsSet()
	if _, ok := set["a"]; !ok {
		t.Errorf("expected 'a' in final set: %v", keysOf(set))
	}
}

func TestHandlers_RegistersRefreshPlaybook(t *testing.T) {
	r := New("http://x", "", "", "", newValidator())
	hs := Handlers(r)
	if _, ok := hs["refresh_playbook"]; !ok {
		t.Error("refresh_playbook not registered")
	}
}

func TestHandlers_DispatchEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"light_actions":["x"]}`))
	}))
	defer srv.Close()
	v := &auth.Validator{}
	r := New(srv.URL, "", "", "", v)
	hs := Handlers(r)
	got, err := hs["refresh_playbook"](context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := got.(map[string]any)
	if !ok || m["refreshed"] != true {
		t.Errorf("response = %v", got)
	}
	// Also verify the JSON-marshalability of the response.
	if _, err := json.Marshal(got); err != nil {
		t.Errorf("response not JSON-marshalable: %v", err)
	}
}

// newValidator returns a fresh empty validator. Avoids littering the tests
// with &auth.Validator{}.
func newValidator() *auth.Validator { return &auth.Validator{} }

func keysOf(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
