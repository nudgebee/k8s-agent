package grafana

import "testing"

func TestIsReadRequest(t *testing.T) {
	for _, tc := range []struct {
		typ, method, url string
		read             bool
	}{
		{"Grafana", "GET", "/api/dashboards/uid/x", true},
		{"Grafana", "HEAD", "/api/health", true},
		{"Grafana", "POST", "/api/ds/query?ds_type=prometheus", true},
		{"Grafana", "POST", "/api/datasources/proxy/uid/abc/api/v1/query_range", true},
		{"Grafana", "POST", "/api/datasources/uid/abc/resources/api/v1/labels", true},
		{"Grafana", "POST", "/api/dashboards/db", false},
		{"Grafana", "POST", "/api/admin/users", false},
		{"Grafana", "DELETE", "/api/dashboards/uid/x", false},
		{"Grafana", "PUT", "/api/datasources/1", false},
		{"Grafana", "POST", "/api/datasources/proxy/uid/abc/api/v1/admin/tsdb/delete_series", false},
		{"Grafana", "POST", "/api/datasources/proxy/uid/abc/../../admin/users/api/v1/query", false},
		{"", "POST", "/api/ds/query", true},
		{"Prometheus", "POST", "/api/v1/query", true},
		{"Prometheus", "POST", "/api/v1/query_range?step=60", true},
		{"Prometheus", "POST", "/api/v1/label/job/values", true},
		{"Prometheus", "POST", "/prometheus/api/v1/series", true},
		{"Prometheus", "POST", "/api/v1/admin/tsdb/delete_series", false},
		{"Prometheus", "POST", "/api/v1/admin/tsdb/snapshot?x=/api/v1/query", false},
		{"Prometheus", "POST", "/api/v1/write", false},
		{"Prometheus", "POST", "/-/reload", false},
		{"Prometheus", "POST", "/api/v1/%71uery", false},
		{"Prometheus", "PUT", "/api/v1/query", false},
		{"Prometheus", "get", "/api/v1/query", false},
		{"APIProxy", "GET", "/v1/data", true},
		{"APIProxy", "POST", "/api/v1/query", false},
		{"Unknown", "POST", "/api/ds/query", false},
		{"Prometheus", "POST", "@evil.example/api/v1/query", false},
		{"Prometheus", "POST", "//evil.example/api/v1/query", false},
		{"Prometheus", "POST", "api/v1/query", false},
		{"Prometheus", "POST", "http://evil.example/api/v1/query", false},
	} {
		if got := IsReadRequest(tc.typ, tc.method, tc.url); got != tc.read {
			t.Errorf("%s %s %s: read = %v, want %v", tc.typ, tc.method, tc.url, got, tc.read)
		}
	}
}
