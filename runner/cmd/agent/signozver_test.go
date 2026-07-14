package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchSignozVersion(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"ok", http.StatusOK, `{"version":"v0.51.0","ee":"Y","setupCompleted":true}`, "v0.51.0"},
		{"empty version field", http.StatusOK, `{"ee":"Y"}`, ""},
		{"non-200", http.StatusInternalServerError, `boom`, ""},
		{"bad json", http.StatusOK, `not json`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/version" {
					t.Errorf("unexpected path %q", r.URL.Path)
				}
				if r.Method != http.MethodGet {
					t.Errorf("expected GET, got %s", r.Method)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			got := fetchSignozVersion(ctx, srv.Client(), srv.URL)
			if got != tt.want {
				t.Errorf("fetchSignozVersion = %q, want %q", got, tt.want)
			}
		})
	}
}

// A dead endpoint must yield "" rather than blocking or panicking.
func TestFetchSignozVersion_Unreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if got := fetchSignozVersion(ctx, &http.Client{Timeout: time.Second}, "http://127.0.0.1:1"); got != "" {
		t.Errorf("expected empty version for unreachable host, got %q", got)
	}
}
