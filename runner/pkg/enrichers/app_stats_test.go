package enrichers

import (
	"context"
	"testing"
)

// canned matrix body with one sample at start_time + 30s, value 0.5.
type cannedRangeProm struct{ body []byte }

func (c cannedRangeProm) Query(_ context.Context, _, _, _ string) ([]byte, error) {
	return c.body, nil
}
func (c cannedRangeProm) QueryRange(_ context.Context, _, _, _, _, _ string) ([]byte, error) {
	return c.body, nil
}
func (c cannedRangeProm) LabelValues(_ context.Context, _, _, _ string, _ []string) ([]byte, error) {
	return []byte(`{"status":"success","data":[]}`), nil
}

// TestRunAppStats_BucketsByOwnerAndReducesToScalars runs application_stats
// against a fake Prometheus that returns matrix samples for one workload,
// then verifies the response shape api-server expects (relay/model.go
// ApplicationStatsResponse): one entry per app_id, with each numeric metric
// reduced from a TimeSeries to a scalar.
//
// The sample's timestamp is start_time + step so it lands inside the grid.
func TestRunAppStats_BucketsByOwnerAndReducesToScalars(t *testing.T) {
	// start_time = 2024-01-01T00:00:00Z = 1704067200; step = 60. Place the
	// sample at +30s so it rounds into bucket 0.
	body := `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"pod":"web-77c7-x9q","namespace":"shop","container":"app"},"values":[[1704067230,"0.5"]]}]}}`
	a := &AppStatsEnricher{q: cannedRangeProm{body: []byte(body)}}
	out, err := a.Handler()(context.Background(), map[string]any{
		"r_start_time": "2024-01-01T00:00:00.000000Z",
		"r_end_time":   "2024-01-01T00:01:00.000000Z",
		"applications": []any{map[string]any{"name": "web", "namespace": "shop"}},
		"queries": map[string]any{
			"pod_cpu_usage_p99": ApplicationMonitoringQueries["pod_cpu_usage_p99"],
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := out.(map[string]any)
	if r["success"] != true {
		t.Fatalf("success = %v: %+v", r["success"], r)
	}
	apps := r["data"].([]map[string]any)
	if len(apps) != 1 {
		t.Fatalf("apps = %d; want 1", len(apps))
	}
	if apps[0]["application_id"] != "web/shop" {
		t.Errorf("application_id = %v", apps[0]["application_id"])
	}
	if v, ok := apps[0]["cpu_p99"].(float64); !ok || v != 0.5 {
		t.Errorf("cpu_p99 = %v (%T); want 0.5", apps[0]["cpu_p99"], apps[0]["cpu_p99"])
	}
}

func TestDictToPrometheusFilter(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"nil", nil, ""},
		{"empty", map[string]any{}, ""},
		{"list", map[string]any{"pod": []any{"a", "b"}}, `pod=~"a|b"`},
		{"single", map[string]any{"namespace": "shop"}, `namespace=~"shop"`},
		{"like", map[string]any{"pod": "web%"}, `pod=~"web.*"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := dictToPrometheusFilter(c.in)
			if got != c.want {
				t.Errorf("got %q; want %q", got, c.want)
			}
		})
	}
}

func TestOwnerFromPodName(t *testing.T) {
	cases := map[string]string{
		// Deployment-style: <name>-<hash>-<random>
		"frontend-77c7c5f9d-x9q8m": "frontend",
		// DaemonSet-style: <name>-<random>
		"node-exporter-x9q8m": "node-exporter",
		// StatefulSet-style: <name>-<index>
		"clickhouse-shard0-0": "clickhouse-shard0",
		// No match → returns input
		"some-arbitrary-name": "some-arbitrary-name",
		"":                    "",
	}
	for in, want := range cases {
		if got := ownerFromPodName(in); got != want {
			t.Errorf("ownerFromPodName(%q) = %q; want %q", in, got, want)
		}
	}
}
