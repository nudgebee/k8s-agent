package discovery

import "encoding/base64"

// basicAuth produces the Authorization header value the collector expects:
// a bare base64 string with NO "Basic " prefix. The collector's auth
// handler reads the header verbatim and base64-decodes; prepending
// "Basic " causes a 401 "Invalid secret" because the decoded string
// still has the prefix bytes.
func basicAuth(secret string) string {
	return base64.StdEncoding.EncodeToString([]byte(secret))
}
