package network

import (
	"net"

	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/transport"
)

// ComputerRoutes advertises listeners without minting a page ticket. The
// caller supplies current LAN discovery so tests never depend on host NICs.
func ComputerRoutes(srv *transport.Server, s Settings, lanIP string) []computerroute.Route {
	if srv == nil {
		return nil
	}
	var routes []computerroute.Route
	if s.Tailnet.Running && s.Tailnet.HTTPS && s.Tailnet.DNSName != "" {
		routes = append(routes, computerroute.Route{Endpoint: "https://" + s.Tailnet.DNSName})
	}
	host, port, err := net.SplitHostPort(srv.Addr())
	bound := net.ParseIP(host)
	// A saved toggle can momentarily precede its rebind. Advertise the LAN
	// only when the listener actually accepts it; loopback is never a route
	// to send to another computer.
	if err == nil && s.BindAll && bound != nil && !bound.IsLoopback() {
		if ip := net.ParseIP(lanIP); ip != nil && (ip.IsPrivate() || ip.IsLinkLocalUnicast() || isTailscaleCGNAT(ip)) && (bound.IsUnspecified() || bound.Equal(ip)) && s.TLS.SelfSignedFingerprint != "" {
			routes = append(routes, computerroute.Route{Endpoint: "https://" + net.JoinHostPort(ip.String(), port), CertFingerprint: s.TLS.SelfSignedFingerprint})
		}
		if s.CanonicalDomain != "" && srv.ServesDomain(s.CanonicalDomain) {
			routes = append(routes, computerroute.Route{Endpoint: "https://" + authorityFor(srv, s.CanonicalDomain, "443")})
		}
	}
	return computerroute.Merge(nil, routes)
}
