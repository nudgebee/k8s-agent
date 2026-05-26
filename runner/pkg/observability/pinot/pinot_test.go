package pinot

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestQuery_PostsSQL(t *testing.T) {
	var path, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		b := make([]byte, 256)
		n, _ := r.Body.Read(b)
		body = string(b[:n])
		_, _ = w.Write([]byte(`{"resultTable":{}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, &http.Client{Timeout: 5 * time.Second})
	_, err := c.Query(context.Background(), "SELECT * FROM logs LIMIT 10")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/sql" {
		t.Errorf("path = %q; want /sql", path)
	}
	if !strings.Contains(body, "SELECT * FROM logs") {
		t.Errorf("body = %s", body)
	}
}

func TestQuery_RequiresSQL(t *testing.T) {
	c := New("http://x", nil)
	if _, err := c.Query(context.Background(), ""); err == nil {
		t.Error("expected error for empty sql")
	}
}

func TestTables_GETRoute(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{"tables":["logs"]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, nil)
	if _, err := c.Tables(context.Background()); err != nil {
		t.Fatal(err)
	}
	if path != "/tables" {
		t.Errorf("path = %q; want /tables", path)
	}
}

func TestSchema_GETRoute(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{"schemaName":"logs"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, nil)
	if _, err := c.Schema(context.Background(), "logs"); err != nil {
		t.Fatal(err)
	}
	if path != "/schemas/logs" {
		t.Errorf("path = %q; want /schemas/logs", path)
	}
}

func TestSchema_RequiresTable(t *testing.T) {
	c := New("http://x", nil)
	if _, err := c.Schema(context.Background(), ""); err == nil {
		t.Error("expected error for empty table")
	}
}

func TestBearerAuth_SetOnRequests(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"tables":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, nil)
	c.AuthToken = "tok123"
	if _, err := c.Tables(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != "Bearer tok123" {
		t.Errorf("Authorization = %q; want Bearer tok123", got)
	}
}

func TestBasicAuth_SetWhenNoToken(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"tables":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, nil)
	c.Username = "admin"
	c.Password = "secret"
	if _, err := c.Tables(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "Basic ") {
		t.Errorf("Authorization = %q; want Basic ...", got)
	}
}

func TestHTTPError_Propagated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad query"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, nil)
	_, err := c.Query(context.Background(), "SELECT 1")
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("expected HTTP 400 error, got %v", err)
	}
}

func TestHandlers_AllRegistered(t *testing.T) {
	hs := Handlers(New("http://x", nil))
	for _, want := range []string{"pinot_query", "pinot_tables", "pinot_schema"} {
		if _, ok := hs[want]; !ok {
			t.Errorf("missing handler: %s", want)
		}
	}
}

func TestHandlers_QueryDispatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"resultTable":{"rows":[]}}`))
	}))
	defer srv.Close()

	hs := Handlers(New(srv.URL, nil))
	if _, err := hs["pinot_query"](context.Background(), map[string]any{"sql": "SELECT 1"}); err != nil {
		t.Fatal(err)
	}
}

func TestHandlers_TablesDispatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tables":[]}`))
	}))
	defer srv.Close()

	hs := Handlers(New(srv.URL, nil))
	if _, err := hs["pinot_tables"](context.Background(), map[string]any{}); err != nil {
		t.Fatal(err)
	}
}

func TestHandlers_SchemaDispatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"schemaName":"logs"}`))
	}))
	defer srv.Close()

	hs := Handlers(New(srv.URL, nil))
	if _, err := hs["pinot_schema"](context.Background(), map[string]any{"table": "logs"}); err != nil {
		t.Fatal(err)
	}
}
