package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	chclient "github.com/nudgebee/nudgebee-agent/pkg/clickhouse"
)

// chTables is a ClickHouse stand-in whose system.tables read answers with one
// row when present, none otherwise, or an HTTP 500 when broken.
func chTables(t *testing.T, present, broken bool) *chclient.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if broken {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("Code: 159. DB::Exception: Timeout exceeded"))
			return
		}
		data := `[]`
		if present {
			data = `[[1]]`
		}
		_, _ = w.Write([]byte(`{"meta":[{"name":"1","type":"UInt8"}],"data":` + data + `}`))
	}))
	t.Cleanup(srv.Close)
	return chclient.New(chclient.Config{Host: strings.TrimPrefix(srv.URL, "http://")})
}

func TestClickhouseLogsStatus_TablePresent(t *testing.T) {
	ok, reason, table := clickhouseLogsStatus(context.Background(), chTables(t, true, false), "logs_db", "app_logs")

	if !ok || reason != "" || table != "logs_db.app_logs" {
		t.Errorf("got %v/%q/%q; want true/\"\"/logs_db.app_logs", ok, reason, table)
	}
}

// The reason is shown in the UI, so it says what was checked: the table, not
// whether logs are arriving.
func TestClickhouseLogsStatus_TableMissing(t *testing.T) {
	ok, reason, table := clickhouseLogsStatus(context.Background(), chTables(t, false, false), "logs_db", "app_logs")

	if ok || table != "logs_db.app_logs" {
		t.Errorf("got %v/%q; want false/logs_db.app_logs", ok, table)
	}
	if !strings.Contains(reason, "logs_db.app_logs is not present") {
		t.Errorf("reason = %q; want it to name the table that is not present", reason)
	}
}

func TestClickhouseLogsStatus_QueryFailure(t *testing.T) {
	ok, reason, table := clickhouseLogsStatus(context.Background(), chTables(t, false, true), "default", "otel_logs")

	if ok || table != "default.otel_logs" || !strings.Contains(reason, "Timeout exceeded") {
		t.Errorf("got %v/%q/%q; want false, the server's error, default.otel_logs", ok, reason, table)
	}
}

// No client (CLICKHOUSE_HOST empty or CLICKHOUSE_ENABLED false) still reports
// the field, false, with a reason and no table.
func TestClickhouseLogsStatus_NoClient(t *testing.T) {
	ok, reason, table := clickhouseLogsStatus(context.Background(), nil, "default", "otel_logs")

	if ok || table != "" || !strings.Contains(reason, "CLICKHOUSE_HOST") {
		t.Errorf("got %v/%q/%q; want false, a reason naming CLICKHOUSE_HOST, no table", ok, reason, table)
	}
}
