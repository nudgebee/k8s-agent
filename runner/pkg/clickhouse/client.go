// Package clickhouse is a thin HTTP client for the ClickHouse query interface.
// We use the HTTP API (port 8123 default) rather than the native binary
// protocol so we can stay on net/http and avoid pulling in clickhouse-go.
//
// We translate JSONCompact responses into the
// {data, columns, column_types, error} shape that backend callers expect.
package clickhouse

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client is a ClickHouse HTTP client. Concurrency-safe via the underlying http.Client.
type Client struct {
	BaseURL  string
	User     string
	Password string
	Database string
	HTTP     *http.Client
}

// Config carries the env-derived settings.
type Config struct {
	Host       string // CLICKHOUSE_HOST (default "localhost"); may include "host:port" or a URL
	Port       int    // CLICKHOUSE_PORT (default 8123)
	User       string // CLICKHOUSE_USER
	Password   string // CLICKHOUSE_PASSWORD
	Database   string // CLICKHOUSE_DB (default "default")
	SSLEnabled bool   // CLICKHOUSE_SSL_ENABLED
}

// New builds a Client from Config. Returns nil if Host is empty so callers
// can treat unconfigured CH as "feature off".
//
// Scheme picking: if Host already includes a scheme ("https://…"), we honour
// it. Otherwise SSLEnabled flips http→https. Matches the legacy behaviour
// when CLICKHOUSE_HOST is set to a URL vs a bare host.
func New(cfg Config) *Client {
	scheme, host, port := normalizeHost(cfg.Host, cfg.Port)
	if host == "" {
		return nil
	}
	if scheme == "" {
		scheme = "http"
		if cfg.SSLEnabled {
			scheme = "https"
		}
	}
	db := cfg.Database
	if db == "" {
		db = "default"
	}
	return &Client{
		BaseURL:  fmt.Sprintf("%s://%s:%d", scheme, host, port),
		User:     cfg.User,
		Password: cfg.Password,
		Database: db,
		HTTP:     &http.Client{Timeout: 60 * time.Second},
	}
}

// QueryResult mirrors the QueryResult wire shape. All four fields are
// emitted unconditionally so callers can distinguish empty rows from a
// missing field.
type QueryResult struct {
	Data        [][]any  `json:"data"`
	Columns     []string `json:"columns"`
	ColumnTypes []string `json:"column_types"`
	Error       *string  `json:"error"`
}

// Query runs the SQL via /?database=...&user=...&password=... POST. The
// legacy equivalent is db.run_query(query, values). `values` (positional
// bind params) are spliced into the query the same way clickhouse-connect
// does — we accept them but rely on ClickHouse's parameterized-query
// support via param_<name>.
//
// For simplicity here we don't expand values (api-server's caller never sends
// any, the query is already final SQL by the time it reaches us). Empty
// `values` slice is the common case; we error if non-empty.
func (c *Client) Query(ctx context.Context, query string, values []any) (*QueryResult, error) {
	if c == nil {
		return errResult("clickhouse: not configured"), nil
	}
	if len(values) > 0 {
		return errResult("clickhouse: parameterized values not supported by HTTP client"), nil
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return errResult("clickhouse: empty query"), nil
	}
	// Append FORMAT JSONCompact unless the caller already has one. JSONCompact
	// gives us {meta:[{name,type}], data:[[...]], rows:N} which we map directly.
	if !strings.Contains(strings.ToUpper(q), "FORMAT ") {
		q += " FORMAT JSONCompact"
	}

	v := url.Values{}
	v.Set("database", c.Database)
	if c.User != "" {
		v.Set("user", c.User)
	}
	if c.Password != "" {
		v.Set("password", c.Password)
	}
	endpoint := c.BaseURL + "/?" + v.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBufferString(q))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		// Connection-level errors surface as a result.error so callers see a
		// uniform shape even when the host is unreachable.
		return errResult(fmt.Sprintf("clickhouse: %v", err)), nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return errResult(fmt.Sprintf("clickhouse: HTTP %d: %s", resp.StatusCode, truncate(string(body), 1024))), nil
	}

	var raw struct {
		Meta []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"meta"`
		Data [][]any `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return errResult(fmt.Sprintf("clickhouse: parse JSONCompact: %v", err)), nil
	}
	out := &QueryResult{
		Data:        raw.Data,
		Columns:     make([]string, len(raw.Meta)),
		ColumnTypes: make([]string, len(raw.Meta)),
	}
	for i, m := range raw.Meta {
		out.Columns[i] = m.Name
		// `column_types` maps to ClickHouse's base_type which strips
		// nullable wrapping (e.g. "Int32" not "Nullable(Int32)").
		out.ColumnTypes[i] = baseType(m.Type)
	}
	return out, nil
}

// errResult wraps a string error in a QueryResult so the wire shape is uniform.
func errResult(msg string) *QueryResult {
	s := msg
	return &QueryResult{
		Data:        [][]any{},
		Columns:     []string{},
		ColumnTypes: []string{},
		Error:       &s,
	}
}

// baseType strips the "Nullable(...)" wrapper. Mirrors clickhouse-connect's
// ColumnType.base_type.
func baseType(t string) string {
	if strings.HasPrefix(t, "Nullable(") && strings.HasSuffix(t, ")") {
		return t[len("Nullable(") : len(t)-1]
	}
	return t
}

// normalizeHost handles the URL-or-host-port shapes accepted. host can be:
//   - "host"
//   - "host:port"
//   - "http://host:port" / "https://..."
//
// Returns the scheme (empty if not in input), bare hostname, and resolved port.
func normalizeHost(host string, defaultPort int) (string, string, int) {
	if host == "" {
		return "", "", 0
	}
	if defaultPort == 0 {
		defaultPort = 8123
	}
	if strings.Contains(host, "://") {
		u, err := url.Parse(host)
		if err == nil {
			h := u.Hostname()
			if u.Port() != "" {
				p, _ := strconv.Atoi(u.Port())
				return u.Scheme, h, p
			}
			return u.Scheme, h, defaultPort
		}
	}
	if i := strings.LastIndex(host, ":"); i > 0 {
		h := host[:i]
		p, err := strconv.Atoi(host[i+1:])
		if err == nil {
			return "", h, p
		}
	}
	return "", host, defaultPort
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
