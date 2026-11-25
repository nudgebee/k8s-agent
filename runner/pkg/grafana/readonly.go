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
// GET and HEAD read. POST reads only for Prometheus query endpoints and
// Grafana's query endpoints; everything else (other methods, admin and write
// endpoints, API-proxy POSTs, a path with "." or ".." segments or encoded
// characters, anything but a plain absolute path) counts as a write.
func IsReadRequest(requestType, method, rawURL string) bool {
	switch method {
	case http.MethodGet, http.MethodHead:
		return true
	case http.MethodPost:
	default:
		return false
	}
	// The proxy appends rawURL to its base URL as a string, so it must be a
	// plain absolute path: "@host/..." or "//host/..." would change the host.
	if !strings.HasPrefix(rawURL, "/") || strings.HasPrefix(rawURL, "//") {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.RawPath != "" || u.Path == "" || path.Clean(u.Path) != u.Path {
		return false
	}
	p := u.Path
	switch requestType {
	case "Prometheus":
		return promReadPath.MatchString(p)
	case "", "Grafana":
		if p == "/api/ds/query" {
			return true
		}
		// Grafana datasource proxy / resource calls to a Prometheus datasource.
		return (strings.HasPrefix(p, "/api/datasources/proxy/") || strings.HasPrefix(p, "/api/datasources/uid/")) &&
			promReadPath.MatchString(p)
	}
	return false
}
