// Package redact strips credentials from datasource URLs before they leave
// the process — in the telemetry report the UI renders, and in logs.
//
// Operators configure basic auth inline (https://user:pass@host) and rarely
// percent-encode the password, so it may contain '/', '?', '#', ':' or '@'.
// No parser can tell where such a userinfo ends: url.Parse reads
// "https://admin:1234/x@host" as host "admin", port 1234. Both helpers here
// therefore cut through the LAST '@', which removes the whole userinfo
// whatever it contains. The cost is over-trimming a URL with an '@' in its
// path or query — datasource endpoints don't have one in practice, and what
// remains is still a suffix of the URL, never part of the secret.
package redact

import (
	"net/url"
	"regexp"
	"strings"
)

// scheme matches a URL scheme (RFC 3986 §3.1).
var scheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*$`)

// URL removes the userinfo from a URL or from a scheme-less
// `user:pass@host[:port]` address (the form CLICKHOUSE_HOST accepts).
// Values without an '@' are returned unchanged.
func URL(raw string) string {
	at := strings.LastIndex(raw, "@")
	if at < 0 {
		return raw
	}
	// Keep the scheme only when it is one: a scheme-less value whose
	// password contains "://" must not have that mistaken for a scheme.
	if i := strings.Index(raw, "://"); i >= 0 && i+3 <= at && scheme.MatchString(raw[:i]) {
		return raw[:i+3] + raw[at+1:]
	}
	return raw[at+1:]
}

// urlUserinfo matches a URL's scheme and everything up to the last '@' in the
// same whitespace- or quote-delimited token.
var urlUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^\s"'<>]*@`)

// secretParam matches a credential carried in a query string — the
// ClickHouse HTTP client, for one, sends `password=` there.
var secretParam = regexp.MustCompile(`(?i)([?&](?:password|passwd|pwd|secret|token|access_token|api_key|apikey)=)[^&#\s"'<>]*`)

// Text applies URL to every URL quoted in free text, e.g. an error like
// `Get "https://user:***@host/-/healthy": dial tcp ...`, and masks credential
// query parameters. net/http masks a userinfo password as "***" but keeps the
// username, other clients don't mask at all, and nothing masks the query.
func Text(s string) string {
	if strings.Contains(s, "@") {
		s = urlUserinfo.ReplaceAllString(s, "$1")
	}
	return secretParam.ReplaceAllString(s, "${1}REDACTED")
}

// MisparsedMessage replaces a datasource's connection error when Misparsed
// reports its URL: the client's own error can quote the misread pieces of the
// credentials (e.g. "lookup <username>: no such host"), and the fix the
// operator needs is the encoding, not the symptom.
const MisparsedMessage = "the URL's inline credentials contain an unencoded '/', '?', '#' or other reserved character, so they are not read as credentials; percent-encode them"

// Misparsed reports whether raw carries inline credentials that an HTTP
// client will not read as credentials. Go takes the userinfo from the
// authority only — up to the first '/', '?' or '#' — so an unencoded one of
// those in the password moves the cut: the request goes to a wrong host
// derived from the username, and errors quote fragments of the credentials
// outside of any URL, where Text cannot find them.
func Misparsed(raw string) bool {
	at := strings.LastIndex(raw, "@")
	if at < 0 {
		return false
	}
	start, full := 0, raw
	if i := strings.Index(raw, "://"); i >= 0 && i+3 <= at && scheme.MatchString(raw[:i]) {
		start = i + 3
	} else {
		full = "http://" + raw // scheme-less host form; clients add one
	}
	if strings.ContainsAny(raw[start:at], "/?#") {
		return true
	}
	_, err := url.Parse(full)
	return err != nil
}
