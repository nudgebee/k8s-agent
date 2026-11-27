package grafana

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// blockedPrefixes are the addresses an API-proxy request may never reach:
// the agent's own loopback, link-local (which holds the cloud instance
// metadata service on AWS, GCP, Azure and OpenStack), and the metadata or
// host-agent addresses some clouds put elsewhere. In-cluster (private)
// addresses stay reachable — reaching in-cluster APIs is what the proxy is for.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),          // "this host"; 0.0.0.0 connects to loopback on Linux
	netip.MustParsePrefix("127.0.0.0/8"),        // loopback
	netip.MustParsePrefix("169.254.0.0/16"),     // link-local, instance metadata
	netip.MustParsePrefix("100.100.100.200/32"), // Alibaba Cloud metadata
	netip.MustParsePrefix("168.63.129.16/32"),   // Azure host agent (WireServer)
	netip.MustParsePrefix("::/128"),             // unspecified
	netip.MustParsePrefix("::1/128"),            // loopback
	netip.MustParsePrefix("fe80::/10"),          // link-local
	netip.MustParsePrefix("fd00:ec2::254/128"),  // AWS instance metadata over IPv6
}

// blockedUpstream reports whether a dial target is one of blockedPrefixes.
func blockedUpstream(addr netip.Addr) bool {
	// netip.Prefix.Contains is false for any zoned address, so "[::1%lo]"
	// would get past the loopback prefix without dropping the zone first.
	addr = addr.Unmap().WithZone("")
	for _, p := range blockedPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// refuseBlocked is a net.Dialer Control hook. It runs on the address the
// dialer is about to connect to, after DNS resolution and for every redirect
// hop.
func refuseBlocked(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("api proxy: unexpected dial address %q", address)
	}
	if blockedUpstream(ap.Addr()) {
		return fmt.Errorf("api proxy: upstream address %s is not allowed", ap.Addr())
	}
	return nil
}

// newAPIClient returns the HTTP client for API-proxy requests. It ignores
// HTTP(S)_PROXY: through a forward proxy the dialer would only see the
// proxy's address, not the upstream's.
func newAPIClient() *http.Client {
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: refuseBlocked}
	// Built here rather than cloned from http.DefaultTransport, so nothing
	// another package sets on the default (a wrapper, a custom TLS dialer)
	// decides how this client connects.
	t := &http.Transport{
		DialContext:           d.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Timeout: 60 * time.Second, Transport: t}
}
