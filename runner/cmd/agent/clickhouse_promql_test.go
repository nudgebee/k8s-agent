package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
}

func (f *fakeClickHouse) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u, p, _ := r.BasicAuth()
	f.mu.Lock()
	f.queries = append(f.queries, r.URL.Query().Get("query"))
	f.auth = append(f.auth, u+":"+p)
	f.paths = append(f.paths, r.URL.Path)
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
	clickhousePromQLAuth(c, chURL, "default", "pw")
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
			err := probeClickHousePromQL(context.Background(), c, chURL)
			switch {
			case tc.wantReason == "" && err != nil:
				t.Errorf("probe failed: %v; want it to pass", err)
			case tc.wantReason != "" && (err == nil || !strings.Contains(err.Error(), tc.wantReason)):
				t.Errorf("probe reason = %v; want it to mention %q", err, tc.wantReason)
			}
			ch.mu.Lock()
			defer ch.mu.Unlock()
			if ch.queries[0] != clickhousePromQLProbe || ch.auth[0] != "default:pw" || ch.paths[0] != "/prometheus/api/v1/query" {
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
		watchClickHousePromQL(ctx, c, chURL, 10*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	if got := nodeAgentCountQuery(promBackend(c, chURL)); !strings.Contains(got, "count by (instance)") {
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
	if got := nodeAgentCountQuery(promBackend(c, chURL)); !strings.HasPrefix(got, "up{") {
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

	watchClickHousePromQL(context.Background(), c, chURL, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	watchClickHousePromQL(ctx, c, chURL, time.Millisecond, logger)

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
