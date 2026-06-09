package rightsize

import (
	"math"
	"testing"
)

func TestFormatAndRoundUnit(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0.05, "50m"},        // sub-core CPU → millicpu
		{0.01, "10m"},        // min CPU floor
		{0.5, "500m"},        // half a core
		{2, "2"},             // whole cores, bare
		{104857600, "100Mi"}, // 100 MiB exactly
		{100 * 1024 * 1024, "100Mi"},
		{1073741824, "1030Mi"}, // 1 GiB → Mi (1024 rounded UP to nearest 10)
		{0, "0m"},              // x<1 path → int(0*1000)=0 → "0m"
	}
	for _, c := range cases {
		if got := formatAndRoundUnit(c.in); got != c.want {
			t.Errorf("formatAndRoundUnit(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseCPUCores(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"250m", 0.25},
		{"1", 1},
		{"2.5", 2.5},
		{"", 0},
		{"garbage", 0},
	}
	for _, c := range cases {
		if got := parseCPUCores(c.in); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("parseCPUCores(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseMemBytes(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"100Mi", 100 * 1024 * 1024},
		{"1Gi", 1024 * 1024 * 1024},
		{"1000000", 1000000},
		{"", 0},
		{"nope", 0},
	}
	for _, c := range cases {
		if got := parseMemBytes(c.in); math.Abs(got-c.want) > 1e-3 {
			t.Errorf("parseMemBytes(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestGetRecommendedCPU(t *testing.T) {
	s := Settings{MinCPU: 0.01}
	// rounds up to nearest millicore
	if got := getRecommendedCPU(s, 0.1234); got != 0.124 {
		t.Errorf("getRecommendedCPU(0.1234) = %v, want 0.124", got)
	}
	// floor applies
	if got := getRecommendedCPU(s, 0.001); got != 0.01 {
		t.Errorf("getRecommendedCPU(0.001) = %v, want 0.01 (floor)", got)
	}
}

func TestGetRecommendedMemory(t *testing.T) {
	s := Settings{MinMemoryBytes: 50 * 1024 * 1024, OOMKillFactor: 1.5}

	// no OOM, usage above floor → usage wins
	usage := 200.0 * 1024 * 1024
	if got := getRecommendedMemory(s, usage, 0); got != usage {
		t.Errorf("getRecommendedMemory(usage, 0) = %v, want %v", got, usage)
	}
	// no OOM, usage below floor → floor wins
	if got := getRecommendedMemory(s, 10*1024*1024, 0); got != s.MinMemoryBytes {
		t.Errorf("getRecommendedMemory(below-floor, 0) = %v, want floor %v", got, s.MinMemoryBytes)
	}
	// OOM observed → max(usage, oom*factor)
	oom := 300.0 * 1024 * 1024
	want := oom * 1.5
	if got := getRecommendedMemory(s, usage, oom); got != want {
		t.Errorf("getRecommendedMemory(usage, oom) = %v, want %v", got, want)
	}
}

func TestPctChange(t *testing.T) {
	if got := pctChange(120, 100); math.Abs(got-20) > 1e-9 {
		t.Errorf("pctChange(120,100) = %v, want 20", got)
	}
	if got := pctChange(80, 100); math.Abs(got-20) > 1e-9 {
		t.Errorf("pctChange(80,100) = %v, want 20", got)
	}
	// no current request → 100 (hits max-change guard, matches original)
	if got := pctChange(50, 0); got != 100 {
		t.Errorf("pctChange(50,0) = %v, want 100", got)
	}
}

func TestQuantileFraction(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{99.99, "1"}, // round(0.9999, 2) = 1.0 → peak
		{99, "0.99"},
		{95, "0.95"},
		{50, "0.5"},
	}
	for _, c := range cases {
		if got := quantileFraction(c.in); got != c.want {
			t.Errorf("quantileFraction(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseSettings(t *testing.T) {
	raw := map[string]any{
		"default_min_cpu":                0.01,
		"default_min_memory":             float64(100), // MiB
		"oom_kill_increase_factor":       1.5,
		"change_threshold":               float64(10),
		"cpu_analysis_percentile":        float64(0), // NB Algo → default
		"memory_analysis_percentile":     float64(95),
		"default_analysis_duration_hour": float64(24),
		"recommend_only":                 false,
		"identifier":                     "tenant/acct/wf/uuid",
	}
	s, err := parseSettings(raw)
	if err != nil {
		t.Fatalf("parseSettings error: %v", err)
	}
	if s.MinMemoryBytes != 100*1024*1024 {
		t.Errorf("MinMemoryBytes = %v, want %v (100 MiB → bytes)", s.MinMemoryBytes, 100*1024*1024)
	}
	if s.CPUPercentile != defaultPercentile {
		t.Errorf("CPUPercentile = %v, want default %v (0 → NB Algo)", s.CPUPercentile, defaultPercentile)
	}
	if s.MemoryPercentile != 95 {
		t.Errorf("MemoryPercentile = %v, want 95", s.MemoryPercentile)
	}
	if s.MaxChangeThreshold != defaultMaxChangeThreshold {
		t.Errorf("MaxChangeThreshold = %v, want default %v (not sent)", s.MaxChangeThreshold, defaultMaxChangeThreshold)
	}
	if s.ChangeThreshold != 10 {
		t.Errorf("ChangeThreshold = %v, want 10", s.ChangeThreshold)
	}
	if s.Identifier != "tenant/acct/wf/uuid" {
		t.Errorf("Identifier = %q", s.Identifier)
	}
}

func TestParseApplications(t *testing.T) {
	raw := []any{
		map[string]any{"name": "ad", "namespace": "demo", "kind": "Deployment"},
	}
	apps, err := parseApplications(raw)
	if err != nil {
		t.Fatalf("parseApplications error: %v", err)
	}
	if len(apps) != 1 || apps[0].Name != "ad" || apps[0].Kind != "Deployment" {
		t.Fatalf("unexpected apps: %+v", apps)
	}

	// missing kind → error
	if _, err := parseApplications([]any{map[string]any{"name": "ad", "namespace": "demo"}}); err == nil {
		t.Error("expected error for application missing kind")
	}
}

func TestSetContainerResources(t *testing.T) {
	c := map[string]any{
		"name": "app",
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "100m", "memory": "64Mi"},
			"limits":   map[string]any{"cpu": "200m", "memory": "128Mi"},
		},
	}
	// new cpu request, drop cpu limit, bump memory
	setContainerResources(c, "250m", "200Mi", "", "280Mi")

	res := c["resources"].(map[string]any)
	req := res["requests"].(map[string]any)
	if req["cpu"] != "250m" || req["memory"] != "200Mi" {
		t.Errorf("requests = %+v", req)
	}
	lim, _ := res["limits"].(map[string]any)
	if lim == nil {
		t.Fatalf("limits dropped entirely, want memory limit retained")
	}
	if _, ok := lim["cpu"]; ok {
		t.Errorf("cpu limit should be removed, got %+v", lim)
	}
	if lim["memory"] != "280Mi" {
		t.Errorf("memory limit = %v, want 280Mi", lim["memory"])
	}
}
