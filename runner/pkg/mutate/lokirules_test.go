package mutate

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

func TestLokiRules_CreateOrReplace_PostsYAML(t *testing.T) {
	var path, contentType, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		contentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	m := New(fake.NewClientset(), "", nil)
	m.SetLokiRules(srv.URL, nil)
	yaml := `groups:
- name: g
  rules:
  - alert: X
    expr: rate({job="nginx"}[1m]) > 5
`
	if _, err := m.CreateOrReplaceLokiAlertRule(context.Background(), "tenant-1", yaml); err != nil {
		t.Fatal(err)
	}
	if path != "/loki/api/v1/rules/tenant-1" {
		t.Errorf("path = %q", path)
	}
	if contentType != "application/yaml" {
		t.Errorf("Content-Type = %q", contentType)
	}
	if !strings.Contains(body, "rate(") {
		t.Errorf("body = %s", body)
	}
}

func TestLokiRules_Delete_BuildsPath(t *testing.T) {
	var path, method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		method = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	m := New(fake.NewClientset(), "", nil)
	m.SetLokiRules(srv.URL, nil)
	if _, err := m.DeleteLokiAlertRule(context.Background(), "tenant-1", "group-1"); err != nil {
		t.Fatal(err)
	}
	if path != "/loki/api/v1/rules/tenant-1/group-1" {
		t.Errorf("path = %q", path)
	}
	if method != http.MethodDelete {
		t.Errorf("method = %q", method)
	}
}

func TestLokiRules_NoURL(t *testing.T) {
	m := New(fake.NewClientset(), "", nil)
	if _, err := m.CreateOrReplaceLokiAlertRule(context.Background(), "n", "body"); err == nil {
		t.Error("expected error when LokiRulesURL not configured")
	}
	if _, err := m.DeleteLokiAlertRule(context.Background(), "n", "g"); err == nil {
		t.Error("expected error when LokiRulesURL not configured")
	}
}

func TestLokiRules_Validates(t *testing.T) {
	m := New(fake.NewClientset(), "", nil)
	m.SetLokiRules("http://x", nil)
	if _, err := m.CreateOrReplaceLokiAlertRule(context.Background(), "", "x"); err == nil {
		t.Error("missing namespace should error")
	}
	if _, err := m.CreateOrReplaceLokiAlertRule(context.Background(), "n", ""); err == nil {
		t.Error("missing body should error")
	}
	if _, err := m.DeleteLokiAlertRule(context.Background(), "n", ""); err == nil {
		t.Error("missing group should error")
	}
}

func TestLokiRules_PropagatesHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`bad rule`))
	}))
	defer srv.Close()
	m := New(fake.NewClientset(), "", nil)
	m.SetLokiRules(srv.URL, nil)
	_, err := m.CreateOrReplaceLokiAlertRule(context.Background(), "n", "x")
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("expected HTTP 400 error, got %v", err)
	}
}

func TestLokiRules_HandlersRegistered(t *testing.T) {
	m := New(fake.NewClientset(), "", nil)
	if _, ok := Handlers(m)["create_loki_alert_rule"]; ok {
		t.Error("should NOT register loki rule actions without LokiRulesURL")
	}
	m.SetLokiRules("http://x", nil)
	hs := Handlers(m)
	for _, want := range []string{"create_loki_alert_rule", "update_loki_alert_rule", "delete_loki_alert_rule"} {
		if _, ok := hs[want]; !ok {
			t.Errorf("missing %s", want)
		}
	}
}
