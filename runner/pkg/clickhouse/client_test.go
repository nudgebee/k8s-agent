package clickhouse

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestQuery_HappyPath verifies that the HTTP body is the SQL string with
// FORMAT JSONCompact appended, the query string carries only the database, the
// credentials go as Basic auth, and the response maps cleanly to
// QueryResult{data, columns, column_types}.
func TestQuery_HappyPath(t *testing.T) {
	var got struct {
		body       string
		path       string
		user, pass string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		got.body = string(buf[:n])
		got.path = r.URL.RawQuery
		got.user, got.pass, _ = r.BasicAuth()
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
		t.Errorf("query string = %q; want only database=d, no credentials", got.path)
	}
	if got.user != "u" || got.pass != "p" {
		t.Errorf("basic auth = %q/%q; want u/p", got.user, got.pass)
	}
}

// Query appends FORMAT JSONCompact unless the statement ends with a FORMAT clause
// of its own. "FORMAT " elsewhere, such as in a filter on span text, is not one.
func TestQuery_AppendsFormatUnlessStatementEndsWithOne(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		body = string(buf[:n])
		_, _ = w.Write([]byte(`{"meta":[{"name":"x","type":"UInt8"}],"data":[[1]]}`))
	}))
	defer srv.Close()
	c := New(Config{Host: strings.TrimPrefix(srv.URL, "http://")})

	for query, want := range map[string]string{
		"SELECT 1 FROM t WHERE SpanName = 'invalid format string'":            "SELECT 1 FROM t WHERE SpanName = 'invalid format string' FORMAT JSONCompact",
		"SELECT 1 FROM t WHERE positionCaseInsensitive(Body, 'FORMAT x') > 0": "SELECT 1 FROM t WHERE positionCaseInsensitive(Body, 'FORMAT x') > 0 FORMAT JSONCompact",
		"SELECT 1 SETTINGS max_execution_time = 5 FORMAT JSONCompact":         "SELECT 1 SETTINGS max_execution_time = 5 FORMAT JSONCompact",
		"select 1 format JSONEachRow;":                                        "select 1 format JSONEachRow;",
		"SELECT 1 FORMAT JSONCompact\n":                                       "SELECT 1 FORMAT JSONCompact",
		"SELECT 1 FORMAT JSONCompact SETTINGS max_threads = 2, readonly = 1":  "SELECT 1 FORMAT JSONCompact SETTINGS max_threads = 2, readonly = 1",
		"SELECT 1 FROM t WHERE x = 'FORMAT a SETTINGS b'":                     "SELECT 1 FROM t WHERE x = 'FORMAT a SETTINGS b' FORMAT JSONCompact",
		// A SETTINGS value may be a quoted string, which can hold a comma, "="
		// or an escaped quote.
		"SELECT 1 FORMAT JSONCompact SETTINGS join_algorithm = 'hash'":                                           "SELECT 1 FORMAT JSONCompact SETTINGS join_algorithm = 'hash'",
		"SELECT 1 FORMAT JSONCompact SETTINGS max_threads = 2, join_algorithm = 'hash', use_query_cache = false": "SELECT 1 FORMAT JSONCompact SETTINGS max_threads = 2, join_algorithm = 'hash', use_query_cache = false",
		"SELECT 1 FORMAT JSONCompact SETTINGS log_comment = 'a, b = c', max_threads = 1":                         "SELECT 1 FORMAT JSONCompact SETTINGS log_comment = 'a, b = c', max_threads = 1",
		"SELECT 1 FORMAT JSONCompact SETTINGS log_comment = 'it''s \\'quoted\\''":                                "SELECT 1 FORMAT JSONCompact SETTINGS log_comment = 'it''s \\'quoted\\''",
		// A string literal that ends the statement and only contains such a
		// clause is not one.
		"SELECT 'FORMAT x SETTINGS y = ''z'''":   "SELECT 'FORMAT x SETTINGS y = ''z''' FORMAT JSONCompact",
		"SELECT 'FORMAT x SETTINGS y = \\'z\\''": "SELECT 'FORMAT x SETTINGS y = \\'z\\'' FORMAT JSONCompact",
	} {
		if res, err := c.Query(context.Background(), query, nil); err != nil || res.Error != nil {
			t.Fatalf("Query(%q) = %+v, %v", query, res, err)
		}
		if body != want {
			t.Errorf("Query(%q) sent %q; want %q", query, body, want)
		}
	}
}

// With no user set, Query authenticates as ClickHouse's "default" user, and a
// password with characters a header value can't carry still goes through.
func TestQuery_DefaultUserAndAnyPassword(t *testing.T) {
	var user, pass string
	var ok bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok = r.BasicAuth()
		_, _ = w.Write([]byte(`{"meta":[{"name":"x","type":"UInt8"}],"data":[[1]]}`))
	}))
	defer srv.Close()

	c := New(Config{Host: strings.TrimPrefix(srv.URL, "http://"), Password: "p\tw\x01"})
	res, err := c.Query(context.Background(), "SELECT 1", nil)
	if err != nil || res.Error != nil {
		t.Fatalf("Query = %+v, %v; want success", res, err)
	}
	if !ok || user != "default" || pass != "p\tw\x01" {
		t.Errorf("basic auth = %q/%q (ok=%v); want default/the password", user, pass, ok)
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

// A DDL statement answers 200 with an empty body. That must read as success
// with no rows, not as a JSON parse failure.
func TestQuery_EmptyBodyIsEmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := New(Config{Host: strings.TrimPrefix(srv.URL, "http://"), Database: "d"})
	res, err := c.Query(context.Background(), "ALTER TABLE t MODIFY COLUMN c String", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Error != nil {
		t.Fatalf("error = %q; want nil", *res.Error)
	}
	if len(res.Data) != 0 || len(res.Columns) != 0 || len(res.ColumnTypes) != 0 {
		t.Errorf("unexpected result: %+v", res)
	}
}

// A connection dropped before the promised body arrives must not read as an
// empty, successful result.
func TestQuery_TruncatedBodyIsResultError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := New(Config{Host: strings.TrimPrefix(srv.URL, "http://"), Database: "d"})
	res, _ := c.Query(context.Background(), "ALTER TABLE t MODIFY COLUMN c String", nil)
	if res.Error == nil || !strings.Contains(*res.Error, "read body") {
		t.Fatalf("want a read body error; got %+v", res)
	}
}

// A 200 body that is present but not JSON is still a parse failure.
func TestQuery_MalformedBodyIsResultError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("Ok.\n"))
	}))
	defer srv.Close()
	c := New(Config{Host: strings.TrimPrefix(srv.URL, "http://"), Database: "d"})
	res, _ := c.Query(context.Background(), "SELECT 1", nil)
	if res.Error == nil || !strings.Contains(*res.Error, "parse JSONCompact") {
		t.Fatalf("want a parse JSONCompact error; got %+v", res)
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

// Every Query error quotes the request URL, which carries any userinfo from
// CLICKHOUSE_HOST (the password itself goes as Basic auth). Those errors reach
// logs, action results and the telemetry report.
func TestQuery_ErrorsHideCredentials(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()
	for _, host := range []string{
		addr,                              // connection refused
		"http://hostuser:hostpw@" + addr,  // userinfo in the host URL
		"http://hostuser:ho/stpw@" + addr, // unencoded '/': fails to parse
	} {
		c := New(Config{Host: host, Database: "default", User: "default", Password: "s3cr3t-pw"})
		res, err := c.Query(context.Background(), "SELECT 1", nil)
		msg := ""
		switch {
		case err != nil:
			msg = err.Error()
		case res != nil && res.Error != nil:
			msg = *res.Error
		default:
			t.Fatalf("host %q: Query succeeded against a closed port", host)
		}
		for _, secret := range []string{"s3cr3t-pw", "hostpw", "stpw"} {
			if strings.Contains(msg, secret) {
				t.Errorf("host %q: error leaks %q: %s", host, secret, msg)
			}
		}
	}
}
