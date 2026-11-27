package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/nudgebee/nudgebee-agent/pkg/dispatch"
	"github.com/nudgebee/nudgebee-agent/pkg/identity"
)

func okHandler(context.Context, map[string]any) (any, error) { return "ran", nil }

// A split set with only a reader: writer and exec are disabled.
func readerOnlySet(t *testing.T) *identity.Set {
	t.Helper()
	base := &rest.Config{Host: "https://192.0.2.1"}
	s, err := identity.NewSplit(base, fake.NewClientset(), nil, identity.Config{Namespace: "nb", ReaderSA: "t-runner-reader"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestGuard_DisabledWriterAndExecRefuse(t *testing.T) {
	ids := readerOnlySet(t)
	handlers := map[string]dispatch.Handler{
		"delete_pod":             okHandler,
		"pod_bash_enricher":      okHandler,
		"continuous_rightsizing": okHandler,
		"get_resource":           okHandler,
	}
	guardIdentityActions(handlers, ids)

	for _, name := range []string{"delete_pod", "pod_bash_enricher"} {
		if _, err := handlers[name](context.Background(), nil); err == nil || !strings.Contains(err.Error(), "disabled on this agent") {
			t.Errorf("%s: err = %v, want disabled", name, err)
		}
	}
	// Recommend-only rightsizing is a read; apply is a change.
	if out, err := handlers["continuous_rightsizing"](context.Background(), map[string]any{"settings": map[string]any{"recommend_only": true}}); err != nil || out != "ran" {
		t.Errorf("recommend-only rightsizing: %v, %v", out, err)
	}
	if _, err := handlers["continuous_rightsizing"](context.Background(), map[string]any{}); err == nil {
		t.Error("rightsizing apply must be refused without a writer")
	}
	if out, err := handlers["get_resource"](context.Background(), nil); err != nil || out != "ran" {
		t.Errorf("read action wrapped: %v, %v", out, err)
	}
}

func TestGuard_LegacyUntouched(t *testing.T) {
	ids := identity.Legacy(fake.NewClientset(), nil, &rest.Config{})
	handlers := map[string]dispatch.Handler{"delete_pod": okHandler}
	guardIdentityActions(handlers, ids)
	if out, err := handlers["delete_pod"](context.Background(), nil); err != nil || out != "ran" {
		t.Fatalf("legacy delete_pod: %v, %v", out, err)
	}
}

func TestReadyz(t *testing.T) {
	legacy := identity.Legacy(fake.NewClientset(), nil, &rest.Config{})
	rec := httptest.NewRecorder()
	readyzHandler(legacy)(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 200 {
		t.Fatalf("legacy readyz = %d", rec.Code)
	}
	// Split, reader never checked: not ready.
	rec = httptest.NewRecorder()
	readyzHandler(readerOnlySet(t))(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 503 {
		t.Fatalf("unchecked reader readyz = %d, want 503", rec.Code)
	}
}

func TestIdentityTelemetry(t *testing.T) {
	if got := identityMode(identity.Legacy(fake.NewClientset(), nil, &rest.Config{})); got != "legacy" {
		t.Fatalf("mode = %q", got)
	}
	if got := identityMode(readerOnlySet(t)); got != "split" {
		t.Fatalf("mode = %q", got)
	}
}
