package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nudgebee/nudgebee-agent/pkg/config"
	"github.com/nudgebee/nudgebee-agent/pkg/observability/prometheus"
)

func TestClickhousePromQLURL(t *testing.T) {
	base := config.Config{
		ClickHouseEnabled:    true,
		ClickHouseHost:       "nudgebee-agent-clickhouse",
		ClickHousePort:       8123,
		ClickHousePromQLPath: "/prometheus/",
	}
	for _, tc := range []struct {
		name string
		edit func(*config.Config)
		want string
	}{
		{"fallback", func(*config.Config) {}, "http://nudgebee-agent-clickhouse:8123/prometheus"},
		{"prometheus configured or discovered", func(c *config.Config) { c.PrometheusURL = "http://prom:9090" }, ""},
		{"path unset (chart default)", func(c *config.Config) { c.ClickHousePromQLPath = "" }, ""},
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

// The runner starts on the ClickHouse fallback and hands over to a Prometheus
// that discovery finds later: the backend, the Grafana proxy's upstream and
// the credentials each backend receives all follow.
func TestClickHousePromQLHandover(t *testing.T) {
	var chAuth, promAuth string
	ch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, _ := r.BasicAuth()
		chAuth = u + ":" + p
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer ch.Close()
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, _ := r.BasicAuth()
		promAuth = u + ":" + p
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer prom.Close()

	c := prometheus.New("", &http.Client{Timeout: 5 * time.Second})
	chURL := ch.URL + "/prometheus"
	if got := promBackend(c, chURL); got != "" {
		t.Errorf("backend with no URL = %q; want empty", got)
	}

	proxyURL := prometheusProxyURL(c, chURL)
	useClickHousePromQL(c, chURL, "default", "pw")
	if got := promBackend(c, chURL); got != promBackendClickHouse {
		t.Errorf("backend = %q; want clickhouse", got)
	}
	if got := proxyURL(); got != "" {
		t.Errorf("proxy upstream on the fallback = %q; want empty (503)", got)
	}
	if _, err := c.Query(context.Background(), "vector(1)", "1", ""); err != nil {
		t.Fatal(err)
	}
	if chAuth != "default:pw" {
		t.Errorf("clickhouse auth = %q; want default:pw", chAuth)
	}

	// Discovery finds a Prometheus.
	c.SetURL(prom.URL)
	if got := promBackend(c, chURL); got != promBackendPrometheus {
		t.Errorf("backend after discovery = %q; want prometheus", got)
	}
	if got := proxyURL(); got != prom.URL {
		t.Errorf("proxy upstream after discovery = %q; want %q", got, prom.URL)
	}
	if _, err := c.Query(context.Background(), "vector(1)", "1", ""); err != nil {
		t.Fatal(err)
	}
	if promAuth != ":" {
		t.Errorf("discovered prometheus got credentials %q; want none", promAuth)
	}
}
