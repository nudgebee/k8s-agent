package main

import (
	"context"
	"testing"
	"time"
)

func TestInAPIGroup(t *testing.T) {
	cases := []struct {
		apiVersion, group string
		want              bool
	}{
		{"serving.knative.dev/v1", "serving.knative.dev", true},
		{"serving.knative.dev/v1beta1", "serving.knative.dev", true}, // any version
		{"v1", "serving.knative.dev", false},                         // core Service
		{"v1", "", true},
		{"", "", true},
		{"", "cert-manager.io", false},
	}
	for _, c := range cases {
		if got := inAPIGroup(c.apiVersion, c.group); got != c.want {
			t.Errorf("inAPIGroup(%q, %q) = %v, want %v", c.apiVersion, c.group, got, c.want)
		}
	}
}

// The cache indexes events by bare kind, so a Knative Service and a core
// Service of the same name share a bucket; the group must separate them.
func TestCachedEventsLister_FiltersByGroup(t *testing.T) {
	now := time.Now()
	core := testEvent("prod", "e1", "Service", "web", "CoreServiceEvent", now)
	core.InvolvedObject.APIVersion = "v1"
	kn := testEvent("prod", "e2", "Service", "web", "KnativeEvent", now.Add(-time.Minute))
	kn.InvolvedObject.APIVersion = "serving.knative.dev/v1"
	l, _ := startCachedLister(t, core, kn)

	got, _ := l.ListEvents(context.Background(), "prod", "serving.knative.dev", "Service", "web", 10)
	if r := reasons(got); len(r) != 1 || r[0] != "KnativeEvent" {
		t.Errorf("group lookup = %v, want only the Knative event", r)
	}
	got, _ = l.ListEvents(context.Background(), "prod", "", "Service", "web", 10)
	if len(got) != 2 {
		t.Errorf("no group = every apiVersion, got %v", reasons(got))
	}
}
