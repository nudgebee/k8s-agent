package clickhouse

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestQuery_HappyPath verifies that the HTTP body is the SQL string with
// FORMAT JSONCompact appended, the query string carries only the database,
// and the response maps cleanly to QueryResult{data, columns, column_types}.
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
	if got.path != "database=d" {
		t.Errorf("query string = %q; want only database=d", got.path)
	}
}

// credsSeen is what a stub server saw of the credentials on one request.
type credsSeen struct {
	query          url.Values
	user, password string
	basic          bool
}

func credsServer(t *testing.T, body string) (*httptest.Server, *credsSeen) {
	t.Helper()
	seen := &credsSeen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.query = r.URL.Query()
		seen.user, seen.password, seen.basic = r.BasicAuth()
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

// The password must never be in the URL: *url.Error quotes the whole URL, and
// transport errors are logged (and, with the log agent on, land in otel_logs).
func assertBasicAuthOnly(t *testing.T, seen *credsSeen) {
	t.Helper()
	for _, k := range []string{"user", "password"} {
		if seen.query.Has(k) {
			t.Errorf("URL carries %q: %v", k, seen.query)
		}
	}
	if seen.query.Get("database") != "d" {
		t.Errorf("URL database = %q; want d", seen.query.Get("database"))
	}
	if !seen.basic || seen.user != "u" || seen.password != "S3cretPW" {
		t.Errorf("basic auth = (%q, %q, %v); want (u, S3cretPW, true)", seen.user, seen.password, seen.basic)
	}
}

func TestQuery_SendsCredentialsAsBasicAuth(t *testing.T) {
	srv, seen := credsServer(t, `{"meta":[{"name":"x","type":"UInt8"}],"data":[[1]]}`)
	c := New(Config{Host: strings.TrimPrefix(srv.URL, "http://"), User: "u", Password: "S3cretPW", Database: "d"})
	res, err := c.Query(context.Background(), "SELECT 1", nil)
	if err != nil || res.Error != nil {
		t.Fatalf("query: %v %v", err, res.Error)
	}
	assertBasicAuthOnly(t, seen)
}

func TestExec_SendsCredentialsAsBasicAuth(t *testing.T) {
	srv, seen := credsServer(t, "")
	c := New(Config{Host: strings.TrimPrefix(srv.URL, "http://"), User: "u", Password: "S3cretPW", Database: "d"})
	if err := c.Exec(context.Background(), "CREATE TABLE t (x UInt8) ENGINE = Memory"); err != nil {
		t.Fatalf("exec: %v", err)
	}
	assertBasicAuthOnly(t, seen)
}

// Same users as the old URL params: none sent → no header (ClickHouse uses
// `default`); a password alone was `default`'s password, so it still is.
func TestRequest_AuthDefaultsMatchURLParams(t *testing.T) {
	srv, seen := credsServer(t, "")
	host := strings.TrimPrefix(srv.URL, "http://")

	if err := New(Config{Host: host}).Exec(context.Background(), "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if seen.basic {
		t.Errorf("no credentials configured, but sent basic auth (%q, %q)", seen.user, seen.password)
	}

	if err := New(Config{Host: host, Password: "pw"}).Exec(context.Background(), "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if !seen.basic || seen.user != "default" || seen.password != "pw" {
		t.Errorf("password only: basic auth = (%q, %q, %v); want (default, pw, true)", seen.user, seen.password, seen.basic)
	}
}

// closedPortClient points at a port nothing listens on, so every request fails
// in the transport, where Go's error quotes the request URL.
func closedPortClient(t *testing.T) *Client {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return New(Config{Host: addr, User: "u", Password: "S3cretPW", Database: "d"})
}

func TestQuery_TransportErrorOmitsPassword(t *testing.T) {
	res, err := closedPortClient(t).Query(context.Background(), "SELECT 1", nil)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.Error == nil {
		t.Fatal("want a transport error on a closed port")
	}
	if strings.Contains(*res.Error, "S3cretPW") {
		t.Errorf("error leaks the password: %s", *res.Error)
	}
}

func TestExec_TransportErrorOmitsPassword(t *testing.T) {
	err := closedPortClient(t).Exec(context.Background(), "CREATE TABLE t (x UInt8) ENGINE = Memory")
	if err == nil {
		t.Fatal("want a transport error on a closed port")
	}
	if strings.Contains(err.Error(), "S3cretPW") {
		t.Errorf("error leaks the password: %v", err)
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

func TestExec_EmptyBodyIsSuccess(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.WriteHeader(http.StatusOK) // ClickHouse sends no body for DDL
	}))
	defer srv.Close()
	c := New(Config{Host: strings.TrimPrefix(srv.URL, "http://")})

	if err := c.Exec(context.Background(), "CREATE TABLE t (x UInt8) ENGINE = Memory"); err != nil {
		t.Fatalf("Exec on empty 200: %v", err)
	}
	if strings.Contains(got, "FORMAT") {
		t.Errorf("Exec must send the statement verbatim, got %q", got)
	}
}

func TestExec_HTTPErrorCarriesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("Code: 62. DB::Exception: Syntax error"))
	}))
	defer srv.Close()
	c := New(Config{Host: strings.TrimPrefix(srv.URL, "http://")})

	err := c.Exec(context.Background(), "CREATE TABLE")
	if err == nil || !strings.Contains(err.Error(), "Syntax error") || !strings.Contains(err.Error(), "400") {
		t.Fatalf("want error with status and body, got %v", err)
	}
}

func TestExec_NilClientAndEmptyStatement(t *testing.T) {
	var c *Client
	if err := c.Exec(context.Background(), "SELECT 1"); err == nil {
		t.Error("nil client must error")
	}
	c = New(Config{Host: "localhost"})
	if err := c.Exec(context.Background(), "   "); err == nil {
		t.Error("empty statement must error")
	}
}
