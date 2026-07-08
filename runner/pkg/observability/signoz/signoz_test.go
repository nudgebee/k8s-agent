package signoz

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestQueryRange_PostsParamsAsBody(t *testing.T) {
	var path, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, &http.Client{Timeout: 5 * time.Second})
	c.APIKey = "test-key"
	_, err := c.QueryRange(context.Background(), map[string]any{"start": 1, "end": 2})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/api/v3/query_range" {
		t.Errorf("path = %q", path)
	}
	if !strings.Contains(body, `"start":1`) {
		t.Errorf("body = %s", body)
	}
}

func TestAPIKey_SetAsCustomHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("SIGNOZ-API-KEY")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := New(srv.URL, nil)
	c.APIKey = "abc123"
	_, _ = c.LabelSuggest(context.Background(), map[string]any{"key": "service"})
	if got != "abc123" {
		t.Errorf("SIGNOZ-API-KEY = %q", got)
	}
}

func TestUserPassword_LoginsAndSetsBearer(t *testing.T) {
	var logins int
	var authHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/login" {
			logins++
			_, _ = w.Write([]byte(`{"accessJwt":"jwt-tok","accessJwtExpiry":9999999999}`))
			return
		}
		authHeader = r.Header.Get("Authorization")
		if r.Header.Get("SIGNOZ-API-KEY") != "" {
			t.Errorf("unexpected api key header on password auth")
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New(srv.URL, nil)
	c.User = "u@x.com"
	c.Password = "secret"
	for i := 0; i < 3; i++ {
		if _, err := c.QueryRange(context.Background(), map[string]any{}); err != nil {
			t.Fatal(err)
		}
	}
	if authHeader != "Bearer jwt-tok" {
		t.Errorf("Authorization = %q", authHeader)
	}
	if logins != 1 {
		t.Errorf("expected token to be cached (1 login), got %d", logins)
	}
}

func TestAPIKey_TakesPrecedenceOverPassword(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/login" {
			t.Errorf("should not login when API key is set")
		}
		if r.Header.Get("SIGNOZ-API-KEY") != "k" {
			t.Errorf("SIGNOZ-API-KEY = %q", r.Header.Get("SIGNOZ-API-KEY"))
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := New(srv.URL, nil)
	c.APIKey = "k"
	c.User = "u"
	c.Password = "p"
	if _, err := c.QueryRange(context.Background(), map[string]any{}); err != nil {
		t.Fatal(err)
	}
}

func TestEachEndpoint_RoutesCorrectly(t *testing.T) {
	cases := []struct {
		name string
		path string
		fn   func(c *Client) error
	}{
		{"query_range", "/api/v3/query_range", func(c *Client) error {
			_, err := c.QueryRange(context.Background(), map[string]any{})
			return err
		}},
		{"label_suggest", "/api/v3/autocomplete/attribute_keys", func(c *Client) error {
			_, err := c.LabelSuggest(context.Background(), map[string]any{})
			return err
		}},
		{"value_suggest", "/api/v3/autocomplete/attribute_values", func(c *Client) error {
			_, err := c.ValueSuggest(context.Background(), map[string]any{})
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var path string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path = r.URL.Path
				_, _ = w.Write([]byte(`{}`))
			}))
			defer srv.Close()
			cli := New(srv.URL, nil)
			if err := c.fn(cli); err != nil {
				t.Fatal(err)
			}
			if path != c.path {
				t.Errorf("path = %q; want %q", path, c.path)
			}
		})
	}
}

func TestPost_RequiresParams(t *testing.T) {
	c := New("http://x", nil)
	if _, err := c.QueryRange(context.Background(), nil); err == nil {
		t.Error("expected error for nil params")
	}
}

func TestPost_PropagatesHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, nil)
	_, err := c.QueryRange(context.Background(), map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("expected HTTP 400, got %v", err)
	}
}

func TestHandlers_AllRegistered(t *testing.T) {
	hs := Handlers(New("http://x", nil))
	for _, want := range []string{"signoz_query_range", "signoz_label_suggest", "signoz_value_suggest"} {
		if _, ok := hs[want]; !ok {
			t.Errorf("missing %s", want)
		}
	}
}

func TestHandlers_DispatchEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	hs := Handlers(New(srv.URL, nil))
	if _, err := hs["signoz_query_range"](context.Background(), map[string]any{"x": 1}); err != nil {
		t.Fatal(err)
	}
}
