package grafana

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestProxy_HandleGrafana verifies the request reaches the upstream URL
// with auth + extra headers attached, and the body comes back base64.
func TestProxy_HandleGrafana(t *testing.T) {
	var gotPath, gotAuth, gotOrgID string
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotOrgID = r.Header.Get("X-Scope-OrgID")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"datasources":[]}`))
	}))
	defer upstream.Close()

	p := New(upstream.URL, "admin", "secret", []string{"X-Scope-OrgID: tenant-1"}, "", nil, nil)

	rawBody := `{"q":"up"}`
	resp := p.HandleGrafana(context.Background(), &Request{
		Method:        "POST",
		URL:           "/api/datasources",
		Body:          base64.StdEncoding.EncodeToString([]byte(rawBody)),
		ContentLength: len(rawBody),
		Header:        map[string][]string{"Cookie": {"sess=1"}, "X-Custom": {"v"}},
	})

	if gotPath != "/api/datasources" {
		t.Errorf("upstream path = %q; want /api/datasources", gotPath)
	}
	if gotOrgID != "tenant-1" {
		t.Errorf("X-Scope-OrgID forwarded = %q; want tenant-1", gotOrgID)
	}
	if !strings.HasPrefix(gotAuth, "Basic ") {
		t.Errorf("Authorization = %q; want Basic <…>", gotAuth)
	}
	if string(gotBody) != rawBody {
		t.Errorf("upstream body = %q; want %q", gotBody, rawBody)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
	decoded, err := base64.StdEncoding.DecodeString(resp.Body)
	if err != nil {
		t.Fatalf("response body not base64: %v", err)
	}
	if string(decoded) != `{"datasources":[]}` {
		t.Errorf("decoded body = %q", decoded)
	}
}

// TestProxy_DropsCookie mirrors the backend — incoming cookie
// headers must NOT be forwarded to upstream.
func TestProxy_DropsCookie(t *testing.T) {
	var sawCookie bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			sawCookie = true
		}
		w.WriteHeader(204)
	}))
	defer upstream.Close()

	p := New(upstream.URL, "", "", nil, "", nil, nil)
	_ = p.HandleGrafana(context.Background(), &Request{
		Method: "GET", URL: "/x",
		Header: map[string][]string{"Cookie": {"sess=1"}},
	})
	if sawCookie {
		t.Error("upstream received Cookie header; should have been dropped")
	}
}

// TestProxy_HandleAPI uses a different base URL than GrafanaURL — proves
// the X-API-Base-URL header pattern is honoured.
func TestProxy_HandleAPI(t *testing.T) {
	var hit string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = r.URL.Path
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	// Empty GrafanaURL — confirms APIProxy doesn't depend on it.
	p := New("", "", "", nil, "", nil, nil)
	resp := p.HandleAPI(context.Background(), upstream.URL, &Request{
		Method: "GET", URL: "/v2/things",
	})
	if hit != "/v2/things" {
		t.Errorf("upstream hit = %q; want /v2/things", hit)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
}

// TestProxy_UnconfiguredGrafana — calling HandleGrafana with no GrafanaURL
// returns 503 with the configuration-hint body, not a panic.
func TestProxy_UnconfiguredGrafana(t *testing.T) {
	p := New("", "", "", nil, "", nil, nil)
	resp := p.HandleGrafana(context.Background(), &Request{Method: "GET", URL: "/x"})
	if resp.StatusCode != 503 {
		t.Errorf("status = %d; want 503", resp.StatusCode)
	}
}

// TestProxy_MissingAPIBase — APIProxy with empty base returns 400 not panic.
func TestProxy_MissingAPIBase(t *testing.T) {
	p := New("", "", "", nil, "", nil, nil)
	resp := p.HandleAPI(context.Background(), "", &Request{Method: "GET", URL: "/x"})
	if resp.StatusCode != 400 {
		t.Errorf("status = %d; want 400", resp.StatusCode)
	}
}

// TestProxy_HandlePrometheus_ForwardsToPrometheusURL covers the
// production hot-path: collector's `/prometheus-v2/api/v1/labels?...`
// arrives at the agent as a GrafanaRequest with type=Prometheus, body
// pre-base64-encoded by the relay. The proxy must hit the configured
// PROMETHEUS_URL with the verb + path + body preserved.
func TestProxy_HandlePrometheus_ForwardsToPrometheusURL(t *testing.T) {
	var (
		gotPath  string
		gotQuery string
		gotOrg   string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotOrg = r.Header.Get("X-Scope-OrgID")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":["job","instance"]}`))
	}))
	defer upstream.Close()

	// PROMETHEUS_HEADERS configured at agent startup — must reach upstream.
	promHeaders := http.Header{"X-Scope-OrgID": {"tenant-9"}}
	p := New("", "", "", nil, upstream.URL, promHeaders, nil)

	resp := p.HandlePrometheus(context.Background(), &Request{
		Method: "GET",
		URL:    `/api/v1/labels?&match[]=%7B__name__%3D%22container_http_requests_total%22%7D`,
	})

	if gotPath != "/api/v1/labels" {
		t.Errorf("upstream path = %q; want /api/v1/labels", gotPath)
	}
	if gotQuery == "" || !strings.Contains(gotQuery, "match%5B%5D") && !strings.Contains(gotQuery, "match[]") {
		t.Errorf("upstream query = %q; want match[] preserved", gotQuery)
	}
	if gotOrg != "tenant-9" {
		t.Errorf("X-Scope-OrgID forwarded = %q; want tenant-9 (from PROMETHEUS_HEADERS)", gotOrg)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
	decoded, err := base64.StdEncoding.DecodeString(resp.Body)
	if err != nil {
		t.Fatalf("response not base64: %v", err)
	}
	if !strings.Contains(string(decoded), `"status":"success"`) {
		t.Errorf("body = %q; want Prometheus success envelope", decoded)
	}
}

// TestProxy_HandlePrometheus_QueryStringAndAuth verifies
// PROMETHEUS_URL_QUERY_STRING is appended and a PROMETHEUS_AUTH-derived
// Authorization header (carried in PrometheusHeaders) reaches upstream.
func TestProxy_HandlePrometheus_QueryStringAndAuth(t *testing.T) {
	var gotQuery, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	defer upstream.Close()

	p := New("", "", "", nil, upstream.URL,
		http.Header{"Authorization": {"Bearer tok-1"}}, nil)
	p.PrometheusQueryString = "extra_label=foo"

	resp := p.HandlePrometheus(context.Background(), &Request{
		Method: "GET",
		URL:    "/api/v1/query?query=up",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	if !strings.Contains(gotQuery, "query=up") || !strings.Contains(gotQuery, "extra_label=foo") {
		t.Errorf("upstream query = %q; want both query=up and extra_label=foo", gotQuery)
	}
	if gotAuth != "Bearer tok-1" {
		t.Errorf("Authorization = %q; want Bearer tok-1", gotAuth)
	}
}

// TestProxy_HandlePrometheus_UnconfiguredReturns503 — without
// PROMETHEUS_URL the proxy must not panic. Same posture as
// TestProxy_UnconfiguredGrafana for the Grafana path.
func TestProxy_HandlePrometheus_UnconfiguredReturns503(t *testing.T) {
	p := New("", "", "", nil, "", nil, nil)
	resp := p.HandlePrometheus(context.Background(), &Request{Method: "GET", URL: "/api/v1/labels"})
	if resp.StatusCode != 503 {
		t.Errorf("status = %d; want 503", resp.StatusCode)
	}
}

// TestProxy_HandlePrometheus_CallerHeadersWin — when the inbound
// GrafanaRequest already carries a header the agent's
// PROMETHEUS_HEADERS would also set, both values reach upstream (Add,
// not Set), so Mimir-style header-stacking (multiple X-Scope-OrgID for
// federated tenancy) still works.
func TestProxy_HandlePrometheus_CallerHeadersWin(t *testing.T) {
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Values("X-Scope-OrgID")
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	p := New("", "", "", nil, upstream.URL, http.Header{"X-Scope-OrgID": {"agent-default"}}, nil)
	_ = p.HandlePrometheus(context.Background(), &Request{
		Method: "GET",
		URL:    "/api/v1/labels",
		Header: map[string][]string{"X-Scope-OrgID": {"caller-tenant"}},
	})
	if len(seen) < 2 {
		t.Fatalf("X-Scope-OrgID forwarded = %v; want both caller + agent values", seen)
	}
}

func TestMergeHeaders(t *testing.T) {
	cases := []struct {
		name   string
		base   http.Header
		extras http.Header
		want   map[string][]string
	}{
		{
			"nil base and extras",
			nil, nil,
			map[string][]string{},
		},
		{
			"only base",
			http.Header{"A": {"1"}},
			nil,
			map[string][]string{"A": {"1"}},
		},
		{
			"only extras",
			nil,
			http.Header{"A": {"x"}},
			map[string][]string{"A": {"x"}},
		},
		{
			"merge same key — both kept (Add semantics)",
			http.Header{"A": {"1"}},
			http.Header{"A": {"x"}},
			map[string][]string{"A": {"1", "x"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeHeaders(tc.base, tc.extras)
			if len(got) != len(tc.want) {
				t.Errorf("merged len = %d; want %d", len(got), len(tc.want))
			}
			for k, vs := range tc.want {
				gv := got[k]
				if len(gv) != len(vs) {
					t.Errorf("key %q: got %v; want %v", k, gv, vs)
					continue
				}
				for i := range vs {
					if gv[i] != vs[i] {
						t.Errorf("key %q[%d]: got %q; want %q", k, i, gv[i], vs[i])
					}
				}
			}
		})
	}
}
