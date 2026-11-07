package triggers

import (
	"testing"
	"time"
)

func TestEngine_HandlesKind(t *testing.T) {
	e := NewEngine(Builtins(), time.Now())
	for _, k := range []string{"Pod", "pod", "Job", "Node", "Service", "Deployment", "configmap", "Rollout"} {
		if !e.HandlesKind(k) {
			t.Errorf("HandlesKind(%q) = false; a builtin matcher reads it", k)
		}
	}
	for _, k := range []string{"ReplicaSet", "event", "namespace", "ClusterRole", ""} {
		if e.HandlesKind(k) {
			t.Errorf("HandlesKind(%q) = true; no builtin matcher reads it", k)
		}
	}
	if !NewEngine([]MatcherSpec{{Name: "any", Kind: "Any"}}, time.Now()).HandlesKind("ReplicaSet") {
		t.Error(`a Kind "Any" matcher must accept every kind`)
	}
}
