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
		name string
		body string
		code int
		want string
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
			name: "endpoint missing (vmsingle)",
			code: 404,
			want: "",
		},
		{
			name: "malformed body",
			body: `not json`,
			code: 200,
			want: "",
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
			if got := PrometheusRetention(context.Background(), c, slog.Default()); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}

	// Nil client → empty without panic.
	if got := PrometheusRetention(context.Background(), nil, slog.Default()); got != "" {
		t.Errorf("nil client should return empty, got %q", got)
	}
}
