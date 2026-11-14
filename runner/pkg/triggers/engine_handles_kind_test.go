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

// Handles narrows HandlesKind to an operation: every builtin matcher reads
// updates only, so the runner's own watch encodes no creates or deletes.
func TestEngine_HandlesKindAndOperation(t *testing.T) {
	e := NewEngine(Builtins(), time.Now())
	for _, k := range []string{"Pod", "Job", "Node", "Service", "Deployment", "DaemonSet", "StatefulSet", "Ingress", "ConfigMap", "Rollout"} {
		if !e.Handles(k, "update") {
			t.Errorf("Handles(%q, update) = false", k)
		}
		for _, op := range []string{"create", "delete"} {
			if e.Handles(k, op) {
				t.Errorf("Handles(%q, %s) = true; no builtin matcher reads it", k, op)
			}
		}
	}
	if !NewEngine([]MatcherSpec{{Name: "any", Kind: "Pod"}}, time.Now()).Handles("Pod", "delete") {
		t.Error("a matcher with no Operations must accept every operation")
	}
}
