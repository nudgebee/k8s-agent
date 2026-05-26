package clickhouse

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestQuery_HappyPath verifies that the HTTP body is the SQL string with
// FORMAT JSONCompact appended, query-string carries db/user/password, and
// the response maps cleanly to QueryResult{data, columns, column_types}.
func TestQuery_HappyPath(t *testing.T) {
	var got struct {
		body string
		path string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		got.body = string(buf[:n])
		got.path = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"meta":[{"name":"count","type":"UInt64"}],"data":[[42]]}`))
	}))
	defer srv.Close()

	c := New(Config{Host: strings.TrimPrefix(srv.URL, "http://"), User: "u", Password: "p", Database: "d"})
	if c == nil {
		t.Fatal("New returned nil")
	}
	res, err := c.Query(context.Background(), "SELECT count() FROM t", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Error != nil {
		t.Errorf("error = %q; want nil", *res.Error)
	}
	if len(res.Data) != 1 || res.Columns[0] != "count" || res.ColumnTypes[0] != "UInt64" {
		t.Errorf("unexpected result: %+v", res)
	}
	if !strings.Contains(got.body, "FORMAT JSONCompact") {
		t.Errorf("expected FORMAT JSONCompact appended; body = %q", got.body)
	}
	if !strings.Contains(got.path, "database=d") || !strings.Contains(got.path, "user=u") {
		t.Errorf("query string missing creds: %q", got.path)
	}
}

// TestQuery_ErrorsAreReportedAsResultError checks that connection-level errors
// surface as result.error rather than a Go error. api-server callers expect a
// uniform shape.
func TestQuery_ConnectionErrorAsResultError(t *testing.T) {
	c := &Client{BaseURL: "http://127.0.0.1:1", Database: "d", HTTP: &http.Client{}}
	res, err := c.Query(context.Background(), "SELECT 1", nil)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.Error == nil {
		t.Fatal("expected result.error; got nil")
	}
}

func TestQuery_HTTPErrorAsResultError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Code: 47. DB::Exception: Unknown table", 500)
	}))
	defer srv.Close()
	c := New(Config{Host: strings.TrimPrefix(srv.URL, "http://"), Database: "d"})
	res, _ := c.Query(context.Background(), "SELECT * FROM nope", nil)
	if res.Error == nil {
		t.Fatal("expected result.error for HTTP 500")
	}
	if !strings.Contains(*res.Error, "HTTP 500") {
		t.Errorf("error doesn't mention HTTP 500: %q", *res.Error)
	}
}

func TestNew_NormalizesHostAndPort(t *testing.T) {
	cases := []struct {
		host, want string
	}{
		{"clickhouse.svc:9000", "http://clickhouse.svc:9000"},
		{"https://example.com:8443", "https://example.com:8443"},
		{"clickhouse.svc", "http://clickhouse.svc:8123"},
	}
	for _, c := range cases {
		got := New(Config{Host: c.host}).BaseURL
		if got != c.want {
			t.Errorf("New(host=%q).BaseURL = %q; want %q", c.host, got, c.want)
		}
	}
}

func TestQuery_ParameterizedRejected(t *testing.T) {
	c := New(Config{Host: "h"})
	res, _ := c.Query(context.Background(), "SELECT 1", []any{1, 2})
	if res.Error == nil || !strings.Contains(*res.Error, "parameterized") {
		t.Errorf("expected parameterized-rejection error; got %+v", res.Error)
	}
}

func TestBaseTypeStripsNullable(t *testing.T) {
	if baseType("Nullable(Int32)") != "Int32" {
		t.Error("baseType failed to strip Nullable")
	}
	if baseType("UInt64") != "UInt64" {
		t.Error("baseType modified bare type")
	}
}
