package alerts

import "encoding/base64"

// basicAuth produces the Authorization header value the backend collector
// expects: a bare base64 string with NO "Basic " prefix. Adding "Basic "
// causes a 401 from the collector's auth handler.
func basicAuth(secret string) string {
	return base64.StdEncoding.EncodeToString([]byte(secret))
}
