package telemetry

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nudgebee/nudgebee-agent/pkg/observability/prometheus"
)

func TestPrometheusRetention(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		code     int
		fallback string
		want     string
	}{
		{
			name: "modern flag name",
			body: `{"status":"success","data":{"storage.tsdb.retention.time":"15d"}}`,
			code: 200,
			want: "15d",
		},
		{
			name: "legacy retentionTime fallback",
			body: `{"status":"success","data":{"retentionTime":"10d"}}`,
			code: 200,
			want: "10d",
		},
		{
			name: "victoriametrics retentionPeriod",
			body: `{"status":"success","data":{"-retentionPeriod":"12"}}`,
			code: 200,
			want: "12",
		},
		{
			name:     "endpoint missing falls back to env",
			code:     404,
			fallback: "30d",
			want:     "30d",
		},
		{
			name: "endpoint missing, no fallback",
			code: 404,
			want: "",
		},
		{
			name:     "malformed body falls back to env",
			body:     `not json`,
			code:     200,
			fallback: "7d",
			want:     "7d",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/status/flags" {
					http.NotFound(w, r)
					return
				}
				if tc.code != 200 {
					w.WriteHeader(tc.code)
					return
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := prometheus.New(srv.URL, nil)
			if got := PrometheusRetention(context.Background(), c, tc.fallback, slog.Default()); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}

	// Nil client → fallback without panic.
	if got := PrometheusRetention(context.Background(), nil, "5d", slog.Default()); got != "5d" {
		t.Errorf("nil client should return fallback, got %q", got)
	}
}
