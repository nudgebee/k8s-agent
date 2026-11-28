package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nudgebee/nudgebee-agent/pkg/config"
	"github.com/nudgebee/nudgebee-agent/pkg/observability/prometheus"
)

const (
	oneSeries = `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"2052"]}]}}`
	noSeries  = `{"status":"success","data":{"resultType":"vector","result":[]}}`
)

func TestClickhouseTimeSeriesURL(t *testing.T) {
	base := config.Config{ClickHouseEnabled: true, ClickHouseHost: "nudgebee-agent-clickhouse", ClickHousePort: 8123}
	for _, tc := range []struct {
		name string
		edit func(*config.Config)
		want string
	}{
		{"clickhouse", func(*config.Config) {}, "http://nudgebee-agent-clickhouse:8123/prometheus"},
		{"prometheus configured too", func(c *config.Config) { c.PrometheusURL = "http://prom:9090" }, "http://nudgebee-agent-clickhouse:8123/prometheus"},
		{"clickhouse disabled", func(c *config.Config) { c.ClickHouseEnabled = false }, ""},
		{"no host", func(c *config.Config) { c.ClickHouseHost = "" }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.edit(&cfg)
			if got := clickhouseTimeSeriesURL(&cfg); got != tc.want {
				t.Errorf("clickhouseTimeSeriesURL = %q; want %q", got, tc.want)
			}
		})
	}
}

// SERVICE_MAP_SOURCE=timeseries gives the service map a client of its own on
// ClickHouse, with the ClickHouse user and without the Prometheus headers,
// while the shared client stays on the configured Prometheus. Any other
// source, or no ClickHouse, keeps the shared client.
func TestServiceMapPromQL(t *testing.T) {
	ch := &fakeClickHouse{body: func() (int, string) { return 200, oneSeries }}
	chSrv := httptest.NewServer(ch)
	t.Cleanup(chSrv.Close)
	host, port, _ := strings.Cut(strings.TrimPrefix(chSrv.URL, "http://"), ":")
	chPort, _ := strconv.Atoi(port)

	var promCalls atomic.Int32
	promSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		promCalls.Add(1)
		_, _ = w.Write([]byte(oneSeries))
	}))
	t.Cleanup(promSrv.Close)

	base := config.Config{
		PrometheusURL:     promSrv.URL,
		ClickHouseEnabled: true, ClickHouseHost: host, ClickHousePort: chPort,
		ClickHouseUser: "default", ClickHousePassword: "pw",
	}
	for _, tc := range []struct {
		name           string
		edit           func(*config.Config)
		wantTimeSeries bool
	}{
		{"timeseries with a prometheus configured", func(c *config.Config) { c.ServiceMapSource = "timeseries" }, true},
		{"timeseries without clickhouse", func(c *config.Config) {
			c.ServiceMapSource = "timeseries"
			c.ClickHouseEnabled = false
		}, false},
		{"prometheus", func(c *config.Config) { c.ServiceMapSource = "prometheus" }, false},
		{"clickhouse summary tables", func(c *config.Config) { c.ServiceMapSource = "clickhouse" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.edit(&cfg)
			prom := prometheus.New(cfg.PrometheusURL, &http.Client{Timeout: 5 * time.Second})
			prom.ExtraHeaders = http.Header{"X-Scope-Orgid": {"tenant"}}
			before := ch.calls()
			promCalls.Store(0)

			c, hasPrometheus, ts := serviceMapPromQL(&cfg, prom, clickhousePromQLURL(&cfg))
			if ts != tc.wantTimeSeries {
				t.Fatalf("timeSeries = %v; want %v", ts, tc.wantTimeSeries)
			}
			if prom.URL() != promSrv.URL {
				t.Errorf("shared client moved to %q; want it on the Prometheus", prom.URL())
			}
			if _, err := c.Query(context.Background(), "up", "", ""); err != nil {
				t.Fatalf("query: %v", err)
			}
			if !tc.wantTimeSeries {
				if c != prom || !hasPrometheus() || promCalls.Load() != 1 || ch.calls() != before {
					t.Errorf("want the shared Prometheus client: same=%v hasPrometheus=%v prom calls=%d clickhouse calls=%d",
						c == prom, hasPrometheus(), promCalls.Load(), ch.calls()-before)
				}
				return
			}
			if c == prom || hasPrometheus() || promCalls.Load() != 0 || ch.calls() != before+1 {
				t.Fatalf("want a ClickHouse client of its own: same=%v hasPrometheus=%v prom calls=%d clickhouse calls=%d",
					c == prom, hasPrometheus(), promCalls.Load(), ch.calls()-before)
			}
			if len(c.ExtraHeaders) != 0 {
				t.Errorf("ClickHouse client carries the Prometheus headers %v", c.ExtraHeaders)
			}
			ch.mu.Lock()
			defer ch.mu.Unlock()
			if got := ch.auth[len(ch.auth)-1]; got != "default:pw" {
				t.Errorf("ClickHouse got auth %q; want the ClickHouse user", got)
			}
			if got := ch.paths[len(ch.paths)-1]; got != "/prometheus/api/v1/query" {
				t.Errorf("ClickHouse got path %q", got)
			}
			// The map's own client goes through clickhouseQueryLimits too, so
			// its queries are bounded like every other PromQL to ClickHouse.
			if got := ch.params[len(ch.params)-1]; got.Get("max_memory_usage") == "" || got.Get("max_execution_time") == "" || got.Has("timeout") {
				t.Errorf("ClickHouse got params %v; want max_memory_usage and max_execution_time, no timeout", got)
			}
		})
	}
}

func TestClickhousePromQLURL(t *testing.T) {
	base := config.Config{ClickHouseEnabled: true, ClickHouseHost: "nudgebee-agent-clickhouse", ClickHousePort: 8123}
	for _, tc := range []struct {
		name string
		edit func(*config.Config)
		want string
	}{
		{"fallback", func(*config.Config) {}, "http://nudgebee-agent-clickhouse:8123/prometheus"},
		{"prometheus configured or discovered", func(c *config.Config) { c.PrometheusURL = "http://prom:9090" }, ""},
		{"clickhouse disabled", func(c *config.Config) { c.ClickHouseEnabled = false }, ""},
		{"no host", func(c *config.Config) { c.ClickHouseHost = "" }, ""},
		{"host that normalises to empty", func(c *config.Config) { c.ClickHouseHost = "http://" }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.edit(&cfg)
			if got := clickhousePromQLURL(&cfg); got != tc.want {
				t.Errorf("clickhousePromQLURL = %q; want %q", got, tc.want)
			}
		})
	}
}

// fakeClickHouse serves the Prometheus query API under /prometheus and
// answers with body(), recording the queries and credentials it gets.
type fakeClickHouse struct {
	mu      sync.Mutex
	body    func() (int, string)
	queries []string
	auth    []string
	paths   []string
	params  []url.Values
}

func (f *fakeClickHouse) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u, p, _ := r.BasicAuth()
	f.mu.Lock()
	f.queries = append(f.queries, r.URL.Query().Get("query"))
	f.auth = append(f.auth, u+":"+p)
	f.paths = append(f.paths, r.URL.Path)
	f.params = append(f.params, r.URL.Query())
	body := f.body
	f.mu.Unlock()
	status, b := body()
	w.WriteHeader(status)
	_, _ = w.Write([]byte(b))
}

func (f *fakeClickHouse) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries)
}

func newFallbackClient(t *testing.T, ch *fakeClickHouse) (*prometheus.Client, string) {
	t.Helper()
	srv := httptest.NewServer(ch)
	t.Cleanup(srv.Close)
	chURL := srv.URL + clickhousePromQLPath
	c := prometheus.New("", &http.Client{Timeout: 5 * time.Second})
	clickhousePromQLRequests(c, chURL, "default", "pw")
	return c, chURL
}

// The probe asks for node-agent data, as the ClickHouse user, and passes only
// on at least one series: vector(1) would pass on an empty table.
func TestProbeClickHousePromQL(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		wantReason string // "" when the probe passes
	}{
		{"node-agent series", 200, oneSeries, ""},
		{"empty table", 200, noSeries, "no node-agent series"},
		{"handler missing", 404, "There is no handle /prometheus/api/v1/query", "HTTP 404"},
		{"auth rejected", 401, "Authentication failed", "HTTP 401"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch := &fakeClickHouse{body: func() (int, string) { return tc.status, tc.body }}
			c, chURL := newFallbackClient(t, ch)
			err := probeClickHousePromQL(context.Background(), c, chURL, "")
			switch {
			case tc.wantReason == "" && err != nil:
				t.Errorf("probe failed: %v; want it to pass", err)
			case tc.wantReason != "" && (err == nil || !strings.Contains(err.Error(), tc.wantReason)):
				t.Errorf("probe reason = %v; want it to mention %q", err, tc.wantReason)
			}
			ch.mu.Lock()
			defer ch.mu.Unlock()
			if ch.queries[0] != clickhousePromQLProbe("") || ch.auth[0] != "default:pw" || ch.paths[0] != "/prometheus/api/v1/query" {
				t.Errorf("probe sent query=%q auth=%q path=%q", ch.queries[0], ch.auth[0], ch.paths[0])
			}
		})
	}
}

// The fallback turns on once the probe passes (the table filling in after
// startup), and a Prometheus discovered later takes over: the backend, the
// Grafana proxy's upstream and the credentials each backend gets all follow.
func TestWatchClickHousePromQLHandover(t *testing.T) {
	var ready atomic.Bool
	ch := &fakeClickHouse{body: func() (int, string) {
		if ready.Load() {
			return 200, oneSeries
		}
		return 200, noSeries
	}}
	c, chURL := newFallbackClient(t, ch)
	proxyURL := prometheusProxyURL(c, chURL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		watchClickHousePromQL(ctx, c, chURL, "", 10*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()

	for ch.calls() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := promBackend(c, chURL); got != "" {
		t.Fatalf("backend while the table is empty = %q; want none", got)
	}
	ready.Store(true)
	<-done
	if got := promBackend(c, chURL); got != promBackendClickHouse {
		t.Fatalf("backend = %q; want clickhouse", got)
	}
	if got := proxyURL(); got != "" {
		t.Errorf("proxy upstream on the fallback = %q; want empty (503)", got)
	}
	if got := nodeAgentCountQuery(promBackend(c, chURL), ""); !strings.Contains(got, "count by (instance)") {
		t.Errorf("node-agent count query on clickhouse = %q", got)
	}

	// Discovery finds a Prometheus.
	var promAuth string
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, _ := r.BasicAuth()
		promAuth = u + ":" + p
		_, _ = w.Write([]byte(noSeries))
	}))
	defer prom.Close()
	c.SetURL(prom.URL)
	if got := promBackend(c, chURL); got != promBackendPrometheus {
		t.Errorf("backend after discovery = %q; want prometheus", got)
	}
	if got := proxyURL(); got != prom.URL {
		t.Errorf("proxy upstream after discovery = %q; want %q", got, prom.URL)
	}
	if got := nodeAgentCountQuery(promBackend(c, chURL), ""); !strings.HasPrefix(got, "up{") {
		t.Errorf("node-agent count query on prometheus = %q", got)
	}
	if _, err := c.Query(context.Background(), "vector(1)", "1", ""); err != nil {
		t.Fatal(err)
	}
	if promAuth != ":" {
		t.Errorf("discovered prometheus got credentials %q; want none", promAuth)
	}
}

// A Prometheus discovered before the probe passes keeps the client: the
// watcher stops without switching it to ClickHouse.
func TestWatchClickHousePromQLPrometheusFirst(t *testing.T) {
	ch := &fakeClickHouse{body: func() (int, string) { return 200, oneSeries }}
	c, chURL := newFallbackClient(t, ch)
	c.SetURL("http://prometheus:9090")

	watchClickHousePromQL(context.Background(), c, chURL, "", time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := c.URL(); got != "http://prometheus:9090" {
		t.Errorf("URL = %q; want the discovered Prometheus", got)
	}
	if n := ch.calls(); n != 0 {
		t.Errorf("probes sent = %d; want 0", n)
	}
}

// A failing probe logs its reason once at Info, not on every retry.
func TestWatchClickHousePromQLLogsReasonOnce(t *testing.T) {
	var probes atomic.Int64
	ch := &fakeClickHouse{body: func() (int, string) {
		if probes.Add(1) > 3 {
			return 200, oneSeries
		}
		return 200, noSeries
	}}
	c, chURL := newFallbackClient(t, ch)
	var buf safeBuffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	watchClickHousePromQL(ctx, c, chURL, "", time.Millisecond, logger)

	out := buf.String()
	if n := strings.Count(out, "fallback not available yet"); n != 1 {
		t.Errorf("Info lines about the failing probe = %d; want 1:\n%s", n, out)
	}
	if !strings.Contains(out, `reason="no node-agent series"`) {
		t.Errorf("reason missing from the log:\n%s", out)
	}
	if !strings.Contains(out, "PromQL goes to ClickHouse") {
		t.Errorf("switch not logged:\n%s", out)
	}
}

// With a cluster label set, the probe and the node-agent count select this
// cluster's series only, so another cluster's data in a shared ClickHouse
// neither turns the fallback on nor counts as this cluster's node agents.
func TestClickHousePromQLClusterLabel(t *testing.T) {
	if got, want := clickhousePromQLProbe(""), "count(container_net_tcp_bytes_sent_total)"; got != want {
		t.Errorf("probe without cluster = %q; want %q", got, want)
	}
	if got, want := clickhousePromQLProbe("c1"), `count(container_net_tcp_bytes_sent_total{cluster="c1"})`; got != want {
		t.Errorf("probe = %q; want %q", got, want)
	}
	if got, want := nodeAgentCountQuery(promBackendClickHouse, "c1"), `count by (instance) (container_net_tcp_bytes_sent_total{cluster="c1"})`; got != want {
		t.Errorf("node-agent count = %q; want %q", got, want)
	}
	if got := nodeAgentCountQuery(promBackendPrometheus, "c1"); strings.Contains(got, "c1") {
		t.Errorf("node-agent count on prometheus = %q; want no cluster label", got)
	}

	ch := &fakeClickHouse{body: func() (int, string) { return 200, oneSeries }}
	c, chURL := newFallbackClient(t, ch)
	if err := probeClickHousePromQL(context.Background(), c, chURL, "c1"); err != nil {
		t.Fatal(err)
	}
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.queries[0] != `count(container_net_tcp_bytes_sent_total{cluster="c1"})` {
		t.Errorf("probe sent %q", ch.queries[0])
	}
}

// Each PromQL query to ClickHouse is bounded: the Prometheus `timeout`, which
// ClickHouse fails as an unknown setting, becomes max_execution_time, kept
// within the caller's deadline and the HTTP client's timeout, since ClickHouse
// runs a PromQL query to the end after the client has gone; memory is capped.
// A Prometheus gets the request as the caller made it.
func TestClickHousePromQLQueryLimits(t *testing.T) {
	for _, tc := range []struct {
		name     string
		timeout  string
		deadline time.Duration // 0: no deadline on the context
		wantExec string
	}{
		{"no timeout: the HTTP client's", "", 0, "5"},
		{"shorter timeout", "2s", 0, "2"},
		{"timeout in seconds", "1.5", 0, "2"},
		{"longer timeout: the HTTP client's", "10m", 0, "5"},
		{"unparsable timeout dropped", "soon", 0, "5"},
		{"tiny timeout in seconds", "1e-10", 0, "1"},
		{"huge timeout in seconds ignored", "1e300", 0, "5"},
		{"timeout over a day ignored", "25h", 0, "5"},
		{"NaN timeout ignored", "NaN", 0, "5"},
		{"shorter deadline", "4s", 3 * time.Second, "3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch := &fakeClickHouse{body: func() (int, string) { return 200, oneSeries }}
			c, chURL := newFallbackClient(t, ch)
			c.SetURL(chURL)
			ctx := context.Background()
			if tc.deadline > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.deadline)
				defer cancel()
			}
			if _, err := c.Query(ctx, "up", "", tc.timeout); err != nil {
				t.Fatal(err)
			}
			if _, err := c.QueryRange(ctx, "up", "1", "2", "1", tc.timeout); err != nil {
				t.Fatal(err)
			}
			ch.mu.Lock()
			defer ch.mu.Unlock()
			for i, p := range ch.params {
				if p.Has("timeout") {
					t.Errorf("request %d sent timeout=%q", i, p.Get("timeout"))
				}
				if got := p.Get("max_execution_time"); got != tc.wantExec {
					t.Errorf("request %d max_execution_time = %q; want %q", i, got, tc.wantExec)
				}
				if got := p.Get("max_memory_usage"); got != "1000000000" {
					t.Errorf("request %d max_memory_usage = %q", i, got)
				}
				if p.Get("query") != "up" || ch.auth[i] != "default:pw" {
					t.Errorf("request %d query=%q auth=%q", i, p.Get("query"), ch.auth[i])
				}
			}
		})
	}

	// A deadline that has already passed still sets the shortest limit. The
	// HTTP client would not send such a request, so Apply is called directly.
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "http://ch/prometheus/api/v1/query?query=up&timeout=30s", nil)
	limits := &clickhouseQueryLimits{auth: prometheus.NewBasicAuth("default", "pw"), maxWait: time.Minute}
	if err := limits.Apply(expired, req); err != nil {
		t.Fatal(err)
	}
	if got := req.URL.Query().Get("max_execution_time"); got != "1" {
		t.Errorf("expired deadline: max_execution_time = %q; want 1", got)
	}

	prom := &fakeClickHouse{body: func() (int, string) { return 200, oneSeries }}
	srv := httptest.NewServer(prom)
	t.Cleanup(srv.Close)
	c, _ := newFallbackClient(t, &fakeClickHouse{body: func() (int, string) { return 200, oneSeries }})
	c.SetURL(srv.URL)
	if _, err := c.Query(context.Background(), "up", "", "30s"); err != nil {
		t.Fatal(err)
	}
	prom.mu.Lock()
	defer prom.mu.Unlock()
	if p := prom.params[0]; p.Get("timeout") != "30s" || p.Has("max_execution_time") || p.Has("max_memory_usage") || prom.auth[0] != ":" {
		t.Errorf("prometheus got %v auth=%q; want the request unchanged", p, prom.auth[0])
	}
}

type safeBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
