package redact

import "testing"

func TestURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"http://prometheus.monitoring.svc:9090", "http://prometheus.monitoring.svc:9090"},
		{"clickhouse.svc:8123", "clickhouse.svc:8123"},
		{"https://user:secret@example.com/prom", "https://example.com/prom"},
		{"https://user@example.com:8443/loki", "https://example.com:8443/loki"},
		{"https://u:p%40ss@example.com/x", "https://example.com/x"},
		{"admin:secret@clickhouse.svc:8123", "clickhouse.svc:8123"},
		// Unencoded reserved characters in the password. url.Parse reads the
		// first two as host:port and finds no userinfo at all.
		{"https://admin:1234/abc@host.example.com/x", "https://host.example.com/x"},
		{"https://admin:12#ab@host.example.com", "https://host.example.com"},
		{"https://user:12?ss@host.example.com/x", "https://host.example.com/x"},
		{"https://user:pa?ss@loki.example.com", "https://loki.example.com"},
		{"https://user:p@ss@loki.example.com", "https://loki.example.com"},
		{"user:pa?ss@ch.example.com", "ch.example.com"},
		{"default:secret@clickhouse.example.com/db?user=a@b.test", "b.test"},
		// "://" inside a scheme-less password is not a scheme.
		{"user:a://b@ch.example.com", "ch.example.com"},
		// Documented over-trim: an '@' in the path or query is
		// indistinguishable from one in an unencoded password.
		{"http://example.com/path?email=a@b.test", "http://b.test"},
	}
	for _, tc := range cases {
		if got := URL(tc.in); got != tc.want {
			t.Errorf("URL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"HTTP 401", "HTTP 401"},
		{"contact admin@example.com", "contact admin@example.com"},
		{
			`prometheus get /api/v1/query: Get "https://user:***@prom.example.com/api/v1/query": dial tcp 192.0.2.1:443: i/o timeout`,
			`prometheus get /api/v1/query: Get "https://prom.example.com/api/v1/query": dial tcp 192.0.2.1:443: i/o timeout`,
		},
		{`Get "https://user:pa/ss@host.example.com/x": EOF`, `Get "https://host.example.com/x": EOF`},
		{"dial https://user:p@ss@host.example.com: x", "dial https://host.example.com: x"},
		{
			`clickhouse: Post "http://ch.example.com:8123/?database=default&password=s3cr3t&user=default": EOF`,
			`clickhouse: Post "http://ch.example.com:8123/?database=default&password=REDACTED&user=default": EOF`,
		},
		{"https://h.example.com/q?TOKEN=abc#frag", "https://h.example.com/q?TOKEN=REDACTED#frag"},
		{"https://h.example.com/q?tokens=1", "https://h.example.com/q?tokens=1"},
		{
			"a http://u:p@a.example.com and https://b.example.com/x",
			"a http://a.example.com and https://b.example.com/x",
		},
	}
	for _, tc := range cases {
		if got := Text(tc.in); got != tc.want {
			t.Errorf("Text(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMisparsed(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"http://prometheus.monitoring.svc:9090", false},
		{"https://user:secret@example.com/prom", false},
		{"https://u:p%2Fss@example.com/x", false},
		{"admin:secret@clickhouse.svc:8123", false},
		{"https://admin:1234/abc@host.example.com/x", true},
		{"https://admin:12#ab@host.example.com", true},
		{"https://user:pa?ss@loki.example.com", true},
		{"user:pa/ss@ch.example.com", true},
		{"https://user:pa ss@loki.example.com", true},
		// An '@' only in the query is caught too: Go reads no userinfo, so
		// if it was meant as a password it is not being sent.
		{"http://example.com/path?email=a@b.test", true},
	}
	for _, tc := range cases {
		if got := Misparsed(tc.in); got != tc.want {
			t.Errorf("Misparsed(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
