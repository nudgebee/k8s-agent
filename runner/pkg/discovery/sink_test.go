package discovery

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSink_PostsExpectedEnvelope(t *testing.T) {
	got := make(chan *http.Request, 1)
	gotBody := make(chan []byte, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- r
		gotBody <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewSink(srv.URL, "secret", "acc-1", "cluster-x", slog.Default())
	env := &Envelope{
		Type:         TypeService,
		Data:         []any{map[string]any{"name": "frontend"}},
		FullLoad:     true,
		IsFirstBatch: true,
		IsLastBatch:  true,
	}
	if err := s.Post(context.Background(), env); err != nil {
		t.Fatalf("Post: %v", err)
	}

	r := <-got
	if r.URL.Path != "/v1/k8s/discovery" {
		t.Errorf("path = %q", r.URL.Path)
	}
	if r.Header.Get("X-NB-Account-Id") != "acc-1" {
		t.Errorf("X-NB-Account-Id = %q", r.Header.Get("X-NB-Account-Id"))
	}
	if r.Header.Get("X-NB-Cluster") != "cluster-x" {
		t.Errorf("X-NB-Cluster = %q", r.Header.Get("X-NB-Cluster"))
	}
	// Bare base64, no "Basic " prefix — matches the legacy sink format.
	if r.Header.Get("Authorization") == "" || strings.HasPrefix(r.Header.Get("Authorization"), "Basic ") {
		t.Errorf("Authorization = %q (expected bare base64, no Basic prefix)", r.Header.Get("Authorization"))
	}

	var decoded Envelope
	if err := json.Unmarshal(<-gotBody, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Type != TypeService || !decoded.FullLoad || !decoded.IsFirstBatch || !decoded.IsLastBatch {
		t.Errorf("envelope = %+v", decoded)
	}
}

func TestSink_GzipsLargePayloads(t *testing.T) {
	gotEncoding := make(chan string, 1)
	gotBody := make(chan []byte, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding <- r.Header.Get("Content-Encoding")
		body, _ := io.ReadAll(r.Body)
		gotBody <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Build a >16 KB payload.
	data := make([]any, 0, 200)
	for i := 0; i < 200; i++ {
		data = append(data, map[string]any{"name": strings.Repeat("x", 200)})
	}

	s := NewSink(srv.URL, "", "", "", slog.Default())
	if err := s.Post(context.Background(), &Envelope{Type: TypeService, Data: data}); err != nil {
		t.Fatalf("Post: %v", err)
	}

	if enc := <-gotEncoding; enc != "gzip" {
		t.Errorf("Content-Encoding = %q; want gzip", enc)
	}

	// Verify it actually decompresses.
	gz, err := gzip.NewReader(strings.NewReader(string(<-gotBody)))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer func() { _ = gz.Close() }()
	if _, err := io.ReadAll(gz); err != nil {
		t.Fatalf("read decompressed: %v", err)
	}
}

func TestSink_PropagatesHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("oops"))
	}))
	defer srv.Close()

	s := NewSink(srv.URL, "", "", "", slog.Default())
	err := s.Post(context.Background(), &Envelope{Type: TypeService})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("expected HTTP 500 error, got %v", err)
	}
}
