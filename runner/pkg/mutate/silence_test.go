package mutate

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetSilences_ProxiesRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/silences" {
			t.Errorf("path = %q", r.URL.Path)
		}
		filters := r.URL.Query()["filter"]
		if len(filters) != 2 || filters[0] != `alertname="X"` || filters[1] != "severity=warning" {
			t.Errorf("filters = %v", filters)
		}
		_, _ = w.Write([]byte(`[{"id":"s1"}]`))
	}))
	defer srv.Close()

	m := New(nil, srv.URL, map[string]string{"X-Tenant": "t1"})
	got, err := m.GetSilences(context.Background(), []string{`alertname="X"`, "severity=warning"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "s1") {
		t.Errorf("body = %s", got)
	}
}

func TestGetSilences_NoURL(t *testing.T) {
	m := New(nil, "", nil)
	if _, err := m.GetSilences(context.Background(), nil); err == nil {
		t.Error("expected error when URL not configured")
	}
}

func TestAddSilence_PostsBody(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		got = string(body)
		_, _ = w.Write([]byte(`{"silenceID":"abc"}`))
	}))
	defer srv.Close()

	m := New(nil, srv.URL, nil)
	resp, err := m.AddSilence(context.Background(), []byte(`{"matchers":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"matchers":[]}` {
		t.Errorf("body forwarded = %s", got)
	}
	if !strings.Contains(string(resp), "abc") {
		t.Errorf("resp = %s", resp)
	}
}

func TestAddSilence_RequiresBody(t *testing.T) {
	m := New(nil, "http://am", nil)
	if _, err := m.AddSilence(context.Background(), nil); err == nil {
		t.Error("expected error for missing body")
	}
}

func TestDeleteSilence_BuildsCorrectPath(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.EscapedPath()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := New(nil, srv.URL, nil)
	if _, err := m.DeleteSilence(context.Background(), "silence-id-123"); err != nil {
		t.Fatal(err)
	}
	if path != "/api/v2/silence/silence-id-123" {
		t.Errorf("path = %q", path)
	}
}

func TestDeleteSilence_RequiresID(t *testing.T) {
	m := New(nil, "http://am", nil)
	if _, err := m.DeleteSilence(context.Background(), ""); err == nil {
		t.Error("expected error for missing id")
	}
}

func TestSilences_PropagatesHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal err"))
	}))
	defer srv.Close()

	m := New(nil, srv.URL, nil)
	if _, err := m.GetSilences(context.Background(), nil); err == nil {
		t.Error("expected error for HTTP 500")
	}
}

func TestSilences_SendsHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Tenant"); got != "t1" {
			t.Errorf("X-Tenant = %q", got)
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	m := New(nil, srv.URL, map[string]string{"X-Tenant": "t1"})
	if _, err := m.GetSilences(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestParseSilenceBody_NestedAndFlat(t *testing.T) {
	flat := map[string]any{"comment": "test", "matchers": []any{}}
	got, err := ParseSilenceBody(flat)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"comment":"test"`) {
		t.Errorf("flat parse = %s", got)
	}

	nested := map[string]any{"body": map[string]any{"comment": "nested"}}
	got, err = ParseSilenceBody(nested)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"comment":"nested"`) {
		t.Errorf("nested parse = %s", got)
	}
}
