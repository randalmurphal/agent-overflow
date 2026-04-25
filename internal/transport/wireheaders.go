package transport

import (
	"net"
	"net/http"
	"strings"
)

// WriteSecurityHeaders writes the standard security headers used by
// every HTTP-side response in this package and in clientmode (which
// imports this helper). The set is deliberately small and conservative:
//
//   - X-Content-Type-Options: nosniff blocks MIME-confusion attacks.
//   - X-Frame-Options: DENY blocks clickjacking via a hostile parent.
//   - Referrer-Policy: no-referrer keeps the bootstrap token out of
//     outbound referers if the SPA navigates externally.
//
// Cache-Control is intentionally NOT set here — callers pick the right
// caching policy per route (immutable for hashed assets, no-store for
// the SPA shell + bootstrap, no-cache for the SPA fallback).
func WriteSecurityHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
}

// IsLoopbackHost reports whether the request's Host header names a
// loopback interface. Used by the DNS-rebinding defence in both the
// embedded-webview server (server.go) and the --connect stub
// (clientmode.go) so the same rule applies in both deployments.
//
// Accepts: "127.0.0.1", "127.0.0.1:<port>", "localhost",
// "localhost:<port>", "[::1]", "[::1]:<port>". Empty Host is rejected
// (HTTP/1.1 requires it; allowing empty would punch a hole in the
// rebind defence for handcrafted clients).
//
// Anything else, including non-loopback IPv4/IPv6 literals or arbitrary
// DNS names that happen to resolve to 127.0.0.1, is rejected.
func IsLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	hostOnly, _, err := net.SplitHostPort(host)
	if err != nil {
		// SplitHostPort fails on bare hosts and malformed inputs.
		// A bracketed IPv6 with no port (`[::1]`) is the legitimate
		// no-port case; anything else with a stray colon is malformed
		// and we refuse rather than guess.
		if strings.Contains(host, ":") {
			if !(strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]")) {
				return false
			}
			hostOnly = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
		} else {
			hostOnly = host
		}
	}
	switch strings.ToLower(hostOnly) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}
