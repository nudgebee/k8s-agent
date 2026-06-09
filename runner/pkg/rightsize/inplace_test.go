package rightsize

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestK8sAtLeastInPlace(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"v1.33.0", true},
		{"v1.33.11-eks-40737a8", true},
		{"v1.35.3-gke.1389002", true},
		{"1.34.0+", true},
		{"v2.0.1", true},
		{"v1.32.9", false},
		{"v1.27.0", false},
		{"1.30.2", false},
		{"", false},
		{"garbage", false},
	}
	for _, c := range cases {
		if got := k8sAtLeastInPlace(c.in); got != c.want {
			t.Errorf("k8sAtLeastInPlace(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestBuildResizePatch(t *testing.T) {
	targets := []inPlaceTarget{
		{name: "app", reqCPU: "0.5", reqMem: "663Mi", limCPU: "", limMem: "928Mi"},
	}
	b, err := buildResizePatch(targets)
	if err != nil {
		t.Fatalf("buildResizePatch error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	containers := parsed["spec"].(map[string]any)["containers"].([]any)
	if len(containers) != 1 {
		t.Fatalf("want 1 container, got %d", len(containers))
	}
	c0 := containers[0].(map[string]any)
	if c0["name"] != "app" {
		t.Errorf("name = %v, want app", c0["name"])
	}
	res := c0["resources"].(map[string]any)
	reqs := res["requests"].(map[string]any)
	if reqs["cpu"] != "0.5" || reqs["memory"] != "663Mi" {
		t.Errorf("requests = %v", reqs)
	}
	lims := res["limits"].(map[string]any)
	if lims["memory"] != "928Mi" {
		t.Errorf("limits.memory = %v, want 928Mi", lims["memory"])
	}
	if _, ok := lims["cpu"]; ok {
		t.Errorf("empty cpu limit must be omitted, got %v", lims["cpu"])
	}

	// All-empty target → no usable resources → error.
	if _, err := buildResizePatch([]inPlaceTarget{{name: "x"}}); err == nil {
		t.Errorf("expected error for empty target")
	}
}

func TestResizeState(t *testing.T) {
	mk := func(ctype, reason string) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
			{Type: corev1.PodConditionType(ctype), Status: corev1.ConditionTrue, Reason: reason},
		}}}
	}
	cases := []struct {
		pod  *corev1.Pod
		want string
	}{
		{&corev1.Pod{}, "done"},
		{mk("PodResizePending", "Infeasible"), "infeasible"},
		{mk("PodResizePending", "Deferred"), "pending"},
		{mk("PodResizeInProgress", ""), "inprogress"},
		{mk("PodResizeInProgress", "Error"), "infeasible"},
	}
	for _, c := range cases {
		if got := resizeState(c.pod); got != c.want {
			t.Errorf("resizeState() = %q, want %q", got, c.want)
		}
	}

	// A condition with Status != True is ignored → done.
	p := &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
		{Type: "PodResizePending", Status: corev1.ConditionFalse, Reason: "Infeasible"},
	}}}
	if got := resizeState(p); got != "done" {
		t.Errorf("resizeState(False condition) = %q, want done", got)
	}
}

func TestParseSettingsInPlaceDefault(t *testing.T) {
	// Absent → defaults true.
	if s, _ := parseSettings(map[string]any{}); !s.InPlace {
		t.Errorf("in_place absent should default to true")
	}
	// Explicit false respected.
	if s, _ := parseSettings(map[string]any{"in_place": false}); s.InPlace {
		t.Errorf("in_place=false should disable in-place")
	}
}
