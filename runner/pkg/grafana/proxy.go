// Package grafana implements the Grafana HTTP-proxy the relay calls to
// render dashboards and run datasource queries on behalf of the UI.
//
// Wire shape (relay → agent):
//
//	{ method, url, body, content_length, header } — GrafanaRequest
//
// Wire shape (agent → relay):
//
//	{ status_code, body, content_length, header } — GrafanaResponse
//
// Body is base64-encoded both directions; the relay decodes before
// forwarding to the UI (see relay-server/pkg/utils/utils.go:294).
//
// The X-NB-Request-Type header decides the route on the agent side:
//
//	"Grafana"     (default) — proxy to GRAFANA_URL
//	"APIProxy"    — proxy to the URL in X-API-Base-URL header
//	"Prometheus"  — proxy to PROMETHEUS_URL (collector-server's
//	                 `/prometheus-v2/*` route forwards every Prometheus
//	                 HTTP call this way; see relay-server/pkg/utils/utils.go:77)
package grafana

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Request is the inbound shape (from relay/UI). Mirrors GrafanaRequest
// .
type Request struct {
	Method        string              `json:"method"`
	URL           string              `json:"url"`
	ContentLength int                 `json:"content_length,omitempty"`
	Body          string              `json:"body,omitempty"` // base64
	Header        map[string][]string `json:"header,omitempty"`
}

// Response is the outbound shape. Mirrors GrafanaResponse.
type Response struct {
	StatusCode    int                 `json:"status_code"`
	ContentLength int                 `json:"content_length,omitempty"`
	Body          string              `json:"body"` // base64
	Header        map[string][]string `json:"header,omitempty"`
}

// Proxy serves Grafana/API-proxy requests. One Proxy per agent process.
type Proxy struct {
	// GrafanaURL is the upstream Grafana base URL (e.g.
	// http://kube-prometheus-stack-grafana.monitoring.svc:80). Set from
	// GRAFANA_URL env.
	GrafanaURL string

	// PrometheusURL is the upstream Prometheus base URL. The collector
	// server's `/prometheus-v2/*` route forwards every HTTP call this
	// way; without it the `Prometheus` request-type returns 503.
	PrometheusURL string

	// PrometheusHeaders is the parsed PROMETHEUS_HEADERS env (raw
	// "Header: value; Header: value" string) — applied to every
	// Prometheus proxy request so X-Scope-OrgID / tenant headers reach
	// the upstream. Same shape ExtraHeaders uses for Grafana. When
	// PROMETHEUS_AUTH is set the caller overlays it here as Authorization.
	PrometheusHeaders http.Header

	// PrometheusQueryString (PROMETHEUS_URL_QUERY_STRING) is appended to the
	// query string of every proxied Prometheus request. Empty is a no-op.
	PrometheusQueryString string

	// Username/Password for Grafana basic auth (GRAFANA_USERNAME /
	// GRAFANA_PASSWORD env). Empty disables.
	Username string
	Password string

	// ExtraHeaders is the GRAFANA_EXTRA_HEADER env in semicolon-separated
	// "Header: value" form. Allows X-Scope-OrgID etc. Each header is sent
	// on every proxied request.
	ExtraHeaders []string

	HTTP *http.Client
}

// New returns a Proxy ready for HandleGrafana / HandleAPI / HandlePrometheus
// calls. promURL + promHeaders unlock the Prometheus request-type path;
// leave them empty (or nil) when Prometheus isn't configured on this
// agent — HandlePrometheus then returns 503.
func New(grafanaURL, user, pass string, extra []string, promURL string, promHeaders http.Header, c *http.Client) *Proxy {
	if c == nil {
		c = &http.Client{Timeout: 60 * time.Second}
	}
	return &Proxy{
		GrafanaURL:        strings.TrimRight(grafanaURL, "/"),
		PrometheusURL:     strings.TrimRight(promURL, "/"),
		PrometheusHeaders: promHeaders,
		Username:          user,
		Password:          pass,
		ExtraHeaders:      extra,
		HTTP:              c,
	}
}

// HandleGrafana proxies a request to GRAFANA_URL. Returns the upstream
// response with body base64-encoded. Empty GrafanaURL → 503.
func (p *Proxy) HandleGrafana(ctx context.Context, req *Request) *Response {
	if p.GrafanaURL == "" {
		return errResp(503, "Grafana not configured (GRAFANA_URL unset)")
	}
	return p.do(ctx, p.GrafanaURL+req.URL, req)
}

// HandleAPI proxies a request to a caller-supplied base URL. The relay
// extracts the URL from the X-API-Base-URL header
// . Returns 400 if the base URL is empty.
func (p *Proxy) HandleAPI(ctx context.Context, baseURL string, req *Request) *Response {
	if baseURL == "" {
		return errResp(400, "Missing X-API-Base-URL header")
	}
	return p.do(ctx, strings.TrimRight(baseURL, "/")+req.URL, req)
}

// HandlePrometheus proxies a request to the configured PROMETHEUS_URL.
// The collector-server's `/prometheus-v2/*` route serializes every
// inbound HTTP call this way (relay-server/pkg/utils/utils.go:61-87),
// so this path covers `/api/v1/labels`, `/api/v1/query`, `/api/v1/series`,
// etc. — anything Grafana / api-server hits against Prometheus.
//
// Returns 503 when PROMETHEUS_URL is unset; same posture as
// HandleGrafana's "Grafana not configured" branch. PROMETHEUS_HEADERS
// (X-Scope-OrgID etc.) are merged onto the incoming request so tenanted
// Prometheus deployments work without the caller having to know them.
func (p *Proxy) HandlePrometheus(ctx context.Context, req *Request) *Response {
	if p.PrometheusURL == "" {
		return errResp(503, "Prometheus not configured (PROMETHEUS_URL unset)")
	}
	enriched := *req
	enriched.Header = mergeHeaders(req.Header, p.PrometheusHeaders)
	return p.do(ctx, appendQueryString(p.PrometheusURL+req.URL, p.PrometheusQueryString), &enriched)
}

// appendQueryString appends extra (a "k=v&k2=v2" fragment, optional leading
// "?") to rawurl's query, using "?" or "&" depending on whether rawurl already
// has a query. Empty extra is a no-op.
func appendQueryString(rawurl, extra string) string {
	extra = strings.TrimSpace(extra)
	extra = strings.TrimPrefix(extra, "?")
	if extra == "" {
		return rawurl
	}
	sep := "?"
	if strings.Contains(rawurl, "?") {
		sep = "&"
	}
	return rawurl + sep + extra
}

// mergeHeaders returns a new header map containing every entry from
// base, with every entry from extras appended (Add, not Set, so
// callers can carry their own X-Scope-OrgID while the agent's
// configured PROMETHEUS_HEADERS pile on too). Nil-safe in both args.
func mergeHeaders(base, extras http.Header) map[string][]string {
	out := make(map[string][]string, len(base))
	for k, vs := range base {
		out[k] = append([]string(nil), vs...)
	}
	for k, vs := range extras {
		out[k] = append(out[k], vs...)
	}
	return out
}

func (p *Proxy) do(ctx context.Context, fullURL string, req *Request) *Response {
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}

	var body io.Reader
	if req.Body != "" {
		decoded, err := base64.StdEncoding.DecodeString(req.Body)
		if err != nil {
			return errResp(400, "Invalid base64 body: "+err.Error())
		}
		body = strings.NewReader(string(decoded))
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return errResp(400, "Build request: "+err.Error())
	}

	// Auth + extra headers.
	if p.Username != "" && p.Password != "" {
		httpReq.SetBasicAuth(p.Username, p.Password)
	}
	for _, h := range p.ExtraHeaders {
		i := strings.IndexByte(h, ':')
		if i <= 0 {
			continue
		}
		httpReq.Header.Set(strings.TrimSpace(h[:i]), strings.TrimSpace(h[i+1:]))
	}
	// Forward incoming request headers — except cookies (intentionally dropped).
	for k, vs := range req.Header {
		if strings.EqualFold(k, "Cookie") {
			continue
		}
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}
	if req.ContentLength > 0 {
		httpReq.ContentLength = int64(req.ContentLength)
		httpReq.Header.Set("Content-Length", fmt.Sprintf("%d", req.ContentLength))
	}

	resp, err := p.HTTP.Do(httpReq)
	if err != nil {
		return errResp(502, "Upstream error: "+err.Error())
	}
	defer func() { _ = resp.Body.Close() }()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return errResp(502, "Read upstream body: "+err.Error())
	}

	return &Response{
		StatusCode:    resp.StatusCode,
		ContentLength: len(rawBody),
		Body:          base64.StdEncoding.EncodeToString(rawBody),
		Header:        resp.Header,
	}
}

func errResp(status int, msg string) *Response {
	return &Response{
		StatusCode:    status,
		Body:          base64.StdEncoding.EncodeToString([]byte(msg)),
		ContentLength: len(msg),
		Header:        map[string][]string{"Content-Type": {"text/plain"}},
	}
}
