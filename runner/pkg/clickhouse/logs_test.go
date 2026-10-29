package clickhouse

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// tablesStub answers system.tables reads with the given rows and records the SQL.
func tablesStub(t *testing.T, rows [][]any, status int) (*Client, *[]string) {
	t.Helper()
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		queries = append(queries, string(body))
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte("Code: 241. DB::Exception: Memory limit exceeded"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		writeRows(w, "1", rows)
	}))
	t.Cleanup(srv.Close)
	return New(Config{Host: strings.TrimPrefix(srv.URL, "http://"), Database: "default"}), &queries
}

func TestLogsTablePresent_TableExists(t *testing.T) {
	c, queries := tablesStub(t, [][]any{{1}}, http.StatusOK)

	ok, err := LogsTablePresent(context.Background(), c, "logs_db", "otel_logs")

	if err != nil || !ok {
		t.Fatalf("LogsTablePresent = %v, %v; want true, nil", ok, err)
	}
	q := (*queries)[0]
	for _, want := range []string{"system.tables", "database = 'logs_db'", "name = 'otel_logs'"} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q lacks %q", q, want)
		}
	}
}

func TestLogsTablePresent_TableMissing(t *testing.T) {
	c, _ := tablesStub(t, nil, http.StatusOK)

	ok, err := LogsTablePresent(context.Background(), c, "default", "otel_logs")

	if err != nil || ok {
		t.Fatalf("LogsTablePresent = %v, %v; want false, nil", ok, err)
	}
}

// The database and table come from chart values, so a quote in either must stay
// inside the string literal.
func TestLogsTablePresent_EscapesNames(t *testing.T) {
	c, queries := tablesStub(t, nil, http.StatusOK)

	_, _ = LogsTablePresent(context.Background(), c, "d'b", "t'b")

	if q := (*queries)[0]; !strings.Contains(q, `database = 'd\'b'`) || !strings.Contains(q, `name = 't\'b'`) {
		t.Errorf("names not escaped: %s", q)
	}
}

func TestLogsTablePresent_ServerErrorIsError(t *testing.T) {
	c, _ := tablesStub(t, nil, http.StatusInternalServerError)

	ok, err := LogsTablePresent(context.Background(), c, "default", "otel_logs")

	if ok || err == nil || !strings.Contains(err.Error(), "Memory limit exceeded") {
		t.Fatalf("LogsTablePresent = %v, %v; want false and the server's error", ok, err)
	}
}

// A connection error repeats the URL. The reason reaches the backend and the
// UI, so it must not carry the password; the client keeps credentials out of
// the URL.
func TestLogsTablePresent_ConnectionErrorHidesPassword(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	host := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()
	c := New(Config{Host: host, Database: "default", User: "default", Password: "s3cr3t-pw"})

	ok, err := LogsTablePresent(context.Background(), c, "default", "otel_logs")

	if ok || err == nil {
		t.Fatalf("LogsTablePresent = %v, %v; want false and an error", ok, err)
	}
	if strings.Contains(err.Error(), "s3cr3t-pw") {
		t.Errorf("error leaks the password: %v", err)
	}
}

func TestLogsTablePresent_NilClient(t *testing.T) {
	ok, err := LogsTablePresent(context.Background(), nil, "default", "otel_logs")

	if ok || err == nil {
		t.Fatalf("LogsTablePresent = %v, %v; want false and an error", ok, err)
	}
}
