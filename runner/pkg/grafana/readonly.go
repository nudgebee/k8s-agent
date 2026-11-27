package grafana

import (
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// promReadPath matches the Prometheus HTTP API endpoints that only read, at
// the end of a path so a prefixed backend (Thanos, Mimir, a Grafana
// datasource proxy) matches too. They accept POST with a form body, which the
// Prometheus Go client prefers for queries.
var promReadPath = regexp.MustCompile(`/api/v1/(query|query_range|query_exemplars|series|labels|label/[^/]+/values|format_query|parse_query|metadata|targets/metadata)$`)

// IsReadRequest reports whether a proxied HTTP request only reads, given its
// X-NB-Request-Type ("Grafana", "APIProxy", "Prometheus"), method and URL.
//
// An API-proxy request never counts as a read: its caller picks the upstream
// host, so even a GET can reach any address the agent can, and it needs a
// relay signature like a write. For Grafana and Prometheus, GET and HEAD read
// and POST reads only for Prometheus query endpoints and Grafana's query
// endpoints. Everything else counts as a write: other methods, admin and write
// endpoints, a URL that is not a plain absolute path (the proxy appends it to
// its base URL as a string, so "@host/..." or "//host/..." would change the
// host), and for POST a path with "." or ".." segments or encoded characters.
func IsReadRequest(requestType, method, rawURL string) bool {
	switch requestType {
	case "", "Grafana", "Prometheus":
	default:
		return false
	}
	if !isPlainPath(rawURL) {
		return false
	}
	switch method {
	case http.MethodGet, http.MethodHead:
		return true
	case http.MethodPost:
	default:
		return false
	}
	u, _ := url.Parse(rawURL)
	if u.RawPath != "" || u.Path == "" || path.Clean(u.Path) != u.Path {
		return false
	}
	p := u.Path
	if requestType == "Prometheus" {
		return promReadPath.MatchString(p)
	}
	if p == "/api/ds/query" {
		return true
	}
	// Grafana datasource proxy / resource calls to a Prometheus datasource.
	return (strings.HasPrefix(p, "/api/datasources/proxy/") || strings.HasPrefix(p, "/api/datasources/uid/")) &&
		promReadPath.MatchString(p)
}

// isPlainPath reports whether rawURL is an absolute path that keeps the host
// of whatever base URL it is appended to.
func isPlainPath(rawURL string) bool {
	if !strings.HasPrefix(rawURL, "/") || strings.HasPrefix(rawURL, "//") || strings.ContainsRune(rawURL, '\\') {
		return false
	}
	u, err := url.Parse(rawURL)
	return err == nil && u.Scheme == "" && u.Host == "" && u.User == nil
}
