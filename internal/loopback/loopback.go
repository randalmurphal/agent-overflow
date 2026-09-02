// Package loopback holds this app's loopback policy: the predicates that
// decide whether a host, authority, or peer address names this machine,
// and the dialer that reaches it (dial.go). Stdlib-only and importable
// from anywhere, because the packages that need it — transport,
// clientmode, browser, claudetui, cdpclient, harnessrpc, devscan — have
// no other reason to know about each other.
//
// The predicates are deliberately DIFFERENT, and the differences are the
// point. What existed before this package was seven private copies of
// four semantics, each one a couple of lines and each one convincing
// on its own, so the answer to "is this loopback" depended on which file
// you were reading. Consolidating removes the copies; it does not merge
// the policies, because a Host header, a kernel peer address, and a
// user-typed endpoint are asking different questions:
//
//   - HostHeader is a policy, not a classification. It answers only for
//     three literal spellings and refuses every DNS name, including one
//     that resolves to 127.0.0.1 — which is the entire mechanism it
//     exists for.
//   - PeerAddress is a classification of something the kernel reported,
//     so it accepts every loopback address form and needs no name
//     handling at all.
//   - EndpointHostname and EndpointAuthority classify a URL the user or
//     a config file supplied. They accept the name "localhost" (a person
//     writing an endpoint means the machine, not a rebind attempt) and
//     any loopback literal, and they differ from each other only in
//     whether the input may carry a port.
//
// Two related checks deliberately live elsewhere:
//
//   - internal/observability/pprofserve validates its BIND address as a
//     loopback IP literal, refusing names outright. A bind address is
//     resolved at bind time, so accepting a name there would mean
//     accepting whatever it resolves to then; that is a stricter rule
//     than any predicate here and it belongs beside the bind.
//   - internal/devserverprobe dials loopback too, but from a URL it was
//     handed rather than a port this process chose: it validates that the
//     URL is loopback at all and keeps the literal the URL named. Dialer
//     discards the host on purpose, so one of the two would have to give
//     up what it exists for.
//   - internal/triage's normalizeLoopbackHost is a normalizer, not a
//     predicate: it rewrites wildcard bind addresses to "localhost" and
//     returns a canonical spelling for a dev-server URL. Sharing a
//     predicate with it would mean sharing only the easy half.
//
// The frontend has its own counterpart, isLoopbackHostname in
// frontend/src/lib/transport/bootstrap.ts, which decides whether a
// refused page credential is retryable. It stays there — a different
// language with no way to call this one — and it is deliberately WIDER
// than EndpointHostname: it also accepts any *.localhost name, because a
// document host is a browser's own idea of where it is, not something
// this process is deciding to trust.
package loopback

import (
	"net"
	"net/netip"
	"strings"
)

// HostHeader reports whether an HTTP Host header names loopback, under
// the strict spelling policy the request-validation guard needs.
//
// It accepts exactly "127.0.0.1", "localhost" and "::1", case-insensitive,
// each with an optional port and the IPv6 form bracketed. Everything else
// is refused: other addresses in 127.0.0.0/8, IPv4-mapped IPv6 loopback,
// and every DNS name — including one whose A record is 127.0.0.1, which
// is the case this predicate exists to refuse. A page navigated to
// http://some.name:<our-port>/ would otherwise reach a server that
// believes it is only answering this machine.
//
// An empty Host is refused. HTTP/1.1 requires one, so a request without
// it is hand-built, and admitting it would be a hole in the same guard.
//
// Callers: internal/transport's loopbackHostGuard, which wraps
// /bootstrap.json, /ws, /pageurl and /rpc whenever the origin allow-list
// is empty; and internal/clientmode's loopbackOnly, which wraps the
// --connect stub's routes. Both rely on the name refusal specifically —
// PeerAddress would answer "yes" for such a request, because the packets
// really did arrive over the loopback interface.
func HostHeader(host string) bool {
	if host == "" {
		return false
	}
	hostOnly, ok := hostPart(host)
	if !ok {
		return false
	}
	switch strings.ToLower(hostOnly) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// PeerAddress reports whether an accepted connection's peer is on a
// loopback interface, reading the "ip:port" form Go puts in
// http.Request.RemoteAddr and net.Conn.RemoteAddr.
//
// It accepts every loopback address form, because it is classifying an
// address the kernel produced rather than a string somebody typed: all
// of 127.0.0.0/8, ::1, and IPv4-mapped ::ffff:127.0.0.1. Names are not
// accepted and do not need to be — the kernel reports addresses.
//
// Unparseable input is refused. httptest sometimes leaves RemoteAddr
// empty, and failing closed means a synthetic request cannot reach a
// privileged surface by being malformed.
//
// This trusts the kernel to report a true peer. A reverse proxy on this
// host makes every remote caller look local, so the documented
// deployment is to proxy from a different host.
//
// Callers: internal/transport (the per-connection flag that gates
// host-tooling receivers, error-text redaction and the step-up proof,
// plus the /rpc route's own check), internal/app's bindingAdmitsPeer
// (which compares a session's binding class against the peer that
// presented it),
// internal/browser's MCP endpoint, and internal/provider/claudetui's
// hook relay and upstream gateway. For the last three it is the entire
// admission decision, not one check among several.
func PeerAddress(remoteAddr string) bool {
	if remoteAddr == "" {
		return false
	}
	// The canonical form parses in one pass.
	if addrPort, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return addrPort.Addr().IsLoopback()
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return addr.IsLoopback()
}

// EndpointHostname reports whether a bare hostname — url.URL.Hostname(),
// which has already dropped the port and the IPv6 brackets — names this
// machine.
//
// It accepts "localhost" case-insensitively and any address literal that
// is loopback, which is wider than HostHeader on purpose: this input is
// a URL somebody configured, and a person writing "localhost" means this
// machine. It is not a request-validation boundary and must never be
// used as one.
//
// Callers: internal/cdpclient, deciding whether two DevTools target URLs
// address the same page across a 127.0.0.1/localhost spelling
// difference; internal/harnessrpc, doing the same for harness page URLs.
func EndpointHostname(hostname string) bool {
	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(hostname)
	return err == nil && addr.IsLoopback()
}

// EndpointAuthority is EndpointHostname for an authority that may carry
// a port: url.URL.Host, or a "host:port" out of configuration.
//
// It cannot be folded into EndpointHostname, and the reason is a real
// input rather than a hypothetical one. An authority spells IPv6 with
// brackets, so a bare unbracketed "::1" is malformed here and is
// refused — while EndpointHostname must accept exactly that, because
// url.URL.Hostname() strips the brackets before its caller sees it. One
// function cannot answer both without being wrong for one of them.
//
// Caller: internal/clientmode, classifying the --connect upstream URL to
// decide whether the stub is fronting a remote backend.
func EndpointAuthority(authority string) bool {
	if authority == "" {
		return false
	}
	hostOnly, ok := hostPart(authority)
	if !ok {
		return false
	}
	return EndpointHostname(hostOnly)
}

// hostPart splits the host out of an authority, and reports false when
// the input is not one.
//
// net.SplitHostPort fails on a bare host, which is legitimate, and on a
// malformed one, which is not, so the two are told apart by hand: a
// colon means either a bracketed IPv6 with no port ("[::1]") or
// something malformed, and malformed is refused rather than guessed at.
func hostPart(authority string) (string, bool) {
	if host, _, err := net.SplitHostPort(authority); err == nil {
		return host, true
	}
	if !strings.Contains(authority, ":") {
		return authority, true
	}
	if strings.HasPrefix(authority, "[") && strings.HasSuffix(authority, "]") {
		return strings.TrimSuffix(strings.TrimPrefix(authority, "["), "]"), true
	}
	return "", false
}
