package network

import (
	"fmt"
	"net"
	"sort"
	"strings"

	"agent-overflow/internal/transport"
)

// Settings is the wire-shaped network preferences a settings UI reads
// / writes. Mirrors settings.NetworkSettings (which only persists
// user-controlled state) plus server-derived URL + Token fields the
// user can copy. The URL and Token are read-only on Set — the server
// owns those.
type Settings struct {
	// BindAll, when true, asks the transport server to listen on the
	// LAN-reachable bind (0.0.0.0) so other devices on the network
	// can reach the app. Default false keeps the server on
	// 127.0.0.1.
	BindAll bool `json:"bindAll"`

	// CanonicalDomain is the one HTTPS name this backend answers to
	// (docs/specs/remote-access.md §7). Bare hostname, no scheme, no
	// port. Empty means the backend is reached by address only.
	CanonicalDomain string `json:"canonicalDomain"`

	// ACMEDNSHook is the argv of the command that publishes and removes
	// the DNS-01 challenge record. Empty means this backend never orders
	// a certificate — the name is served by whatever terminates in front
	// of it, or not over TLS at all.
	ACMEDNSHook []string `json:"acmeDnsHook"`

	// ExternalCertFile / ExternalKeyFile are absolute paths to a
	// certificate this backend did not obtain. Both or neither. The pair
	// wins over issuance: with it set, nothing is ordered.
	ExternalCertFile string `json:"externalCertFile"`
	ExternalKeyFile  string `json:"externalKeyFile"`

	// TLS is read-only status, filled by the app from what is actually
	// loaded. Ignored on Set — nothing here is a preference.
	TLS TLSStatus `json:"tls"`

	// URL is the http://host:port/?t=<ticket> URL the user can paste
	// into a remote browser. The `t` is a ONE-TIME page ticket, spent
	// by that browser's first bootstrap exchange for a cookie, so a
	// copied URL loads the page once and grants nothing further — a
	// networked page still has to pair. Server-derived: when BindAll is
	// true and a non-loopback interface IP is discoverable, the URL
	// points at the LAN IP; otherwise it falls back to the server's
	// own Addr.
	URL string `json:"url"`

	// Token is this launch's session credential — what a client that is
	// not a browser presents (`agent-overflow --connect
	// ws://host:port/ws?token=<value>`, the `ao-harness` CLI). Browsers
	// never use it: the URL above carries a one-time page ticket instead
	// and the browser ends up holding a cookie. Surfaced for debugging /
	// advanced wiring; the user shouldn't normally need to touch it.
	Token string `json:"token"`

	// Insecure is true when the URL above traverses an untrusted
	// network in cleartext. That is any LAN bind whose URL is still
	// http://: the ticket on it and every byte the paired device
	// exchanges afterwards are readable by anything on the same
	// Wi-Fi. The frontend renders a warning banner when Insecure is
	// true so the user knows to front the bind with Tailscale Serve,
	// an SSH tunnel, or a reverse proxy before sharing. A URL that came
	// out https:// — which happens only when a certificate for the
	// canonical domain is actually loaded — is not flagged.
	Insecure bool `json:"insecure"`
}

// The values TLSStatus.Serving takes. They answer one question — what
// does this listener present for the canonical domain — and the UI
// renders each as its own sentence.
const (
	// TLSServingNone: no certificate at all. The listener answers
	// cleartext only and nothing can pin it.
	TLSServingNone = "none"
	// TLSServingSelfSigned: the install's own certificate, which a
	// paired Go client pins by fingerprint and no browser accepts.
	TLSServingSelfSigned = "self-signed"
	// TLSServingACME: a certificate this backend obtained for the
	// canonical domain and renews.
	TLSServingACME = "acme"
	// TLSServingExternal: the user's own certificate files.
	TLSServingExternal = "external"
)

// TLSStatus is what the Network settings screen shows about the
// certificate half. Read-only in both directions: every field is
// observed, none is a preference, and there is no push channel — the
// screen re-reads GetNetworkSettings.
type TLSStatus struct {
	// Serving is one of the constants above.
	Serving string `json:"serving"`

	// NotAfter is when the certificate for the canonical domain expires,
	// in unix milliseconds. Zero when none is loaded. The self-signed
	// certificate's expiry is deliberately not reported here: it is ten
	// years out and nothing renews it.
	NotAfter int64 `json:"notAfter"`

	// Renewing is true while an issuance or renewal is in flight. The
	// screen polls while it is set, which is why issuance is not an RPC
	// that blocks — a DNS-01 round trip outlives any call timeout.
	Renewing bool `json:"renewing"`

	// LastError is the last issuance or load failure, verbatim and
	// naming its stage. Cleared by the next success. Errors are
	// user-facing state, not log entries.
	LastError string `json:"lastError"`

	// SelfSignedFingerprint is the `sha256:<hex>` a paired Go client
	// pins, the same string the pairing payload carries. Shown so the
	// two can be compared by eye when a pin is refused.
	SelfSignedFingerprint string `json:"selfSignedFingerprint"`
}

// BindHost returns the bind interface for the given LAN toggle.
// Loopback (127.0.0.1) keeps the server local; 0.0.0.0 listens on
// every interface so any LAN-reachable IP routes to it.
func BindHost(bindAll bool) string {
	if bindAll {
		return "0.0.0.0"
	}
	return "127.0.0.1"
}

// OriginPatterns returns the extra origins the WS upgrade accepts
// beyond the one it is addressed to, for the given LAN toggle and
// discovered LAN IP.
//
// On loopback the list is nil, which is not "accept anything": the
// upgrade always accepts a request carrying no Origin (every client
// that is not a browser) and a request whose Origin is this listener's
// own authority, and refuses every other Origin. That refusal is what
// keeps a page served by some other port on this machine from opening
// an authenticated socket with the cookie the browser would attach for
// it — cookies are scoped by host, not by port. See
// transport.OriginAllowed.
//
// On LAN bind the list adds the spellings a shared URL can legitimately
// be opened under: the loopback aliases and the discovered LAN IP. A
// browser tab from any other origin still cannot open a socket here.
//
// A canonical domain adds its https origins whatever the bind is, and
// the port-bearing spelling is the load-bearing one: a page served at
// https://<domain> through something that terminates TLS in front of
// this backend reaches the socket over cleartext, so the request
// authority this server computes is http://<domain> and would not match
// the page's own origin. Naming the origin is how that deployment is
// answered rather than half-broken. It does NOT relax the Host guard —
// that reads the bind address and the canonical name, not this list.
func OriginPatterns(bindAll bool, lanIP, canonicalDomain string) []string {
	var patterns []string
	if bindAll {
		patterns = append(patterns,
			"http://127.0.0.1:*",
			"http://localhost:*",
		)
		if lanIP != "" {
			patterns = append(patterns, fmt.Sprintf("http://%s:*", lanIP))
		}
	}
	if canonicalDomain != "" {
		patterns = append(patterns,
			fmt.Sprintf("https://%s", canonicalDomain),
			fmt.Sprintf("https://%s:*", canonicalDomain),
		)
	}
	return patterns
}

// AppURLWithLAN renders the URL using a caller-supplied LAN IP so
// the discovery function isn't called twice in a Set flow (once for
// the allow-list, once for the URL).
//
// Three renderings, in order of what the user can actually paste
// somewhere useful: the canonical domain when this backend holds a
// certificate for it (real HTTPS, which is the only form a browser
// opens without a trust warning), the LAN IP, then loopback.
//
// The https branch asks the LISTENER whether it can complete a handshake
// for that exact name (Server.ServesDomain), not the observed TLS status
// beside it. The two agree in the settled state and differ in the one
// that matters: a user who just changed their domain has a settings
// record naming the new one while the old certificate is still what is
// loaded, and publishing https:// for a name nothing answers on is worse
// than publishing the http:// URL that works.
//
// Every branch carries a freshly minted one-time page ticket: the
// loopback one from Server.AppURL, the others minted here because only
// this function knows the authority to name. Each render of the share
// panel therefore hands out a URL that opens one browser session — a
// second device needs the panel read again.
func AppURLWithLAN(srv *transport.Server, s Settings, lanIP string) string {
	loopbackURL := srv.AppURL()
	if s.CanonicalDomain != "" && srv.ServesDomain(s.CanonicalDomain) {
		if url, ok := ticketedURL(srv, "https", authorityFor(srv, s.CanonicalDomain, "443")); ok {
			return url
		}
	}
	if !s.BindAll || lanIP == "" {
		// Couldn't find a LAN-reachable address, or none was asked for
		// — surface the loopback URL so the user at least gets
		// something they can paste into a local browser, and the UI can
		// hint that LAN discovery failed via the BindAll=true flag.
		return loopbackURL
	}
	_, port, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		return loopbackURL
	}
	if url, ok := ticketedURL(srv, "http", net.JoinHostPort(lanIP, port)); ok {
		return url
	}
	return loopbackURL
}

// authorityFor spells host[:port] for a URL, dropping the port when it
// is the scheme's default — a domain fronted on 443 reads as the name
// alone, which is what the user would type.
func authorityFor(srv *transport.Server, host, defaultPort string) string {
	_, port, err := net.SplitHostPort(srv.Addr())
	if err != nil || port == "" || port == defaultPort {
		return host
	}
	return net.JoinHostPort(host, port)
}

// ticketedURL mints the one-time page ticket and renders the URL, or
// reports false so the caller can fall back to one that already has a
// ticket on it.
func ticketedURL(srv *transport.Server, scheme, authority string) (string, bool) {
	ticket, err := srv.MintPageTicket()
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("%s://%s/?%s=%s", scheme, authority, transport.PageTicketParam, ticket), true
}

// FromServer builds the wire record from the persisted half plus what
// the live transport server derives (URL, token, and whether that URL is
// safe to share). The caller supplies everything a user chose — the bind
// toggle, the domain, the hook, the external pair — and the observed TLS
// status, since only the app knows what is actually loaded.
func FromServer(srv *transport.Server, s Settings) Settings {
	lanIP := ""
	if s.BindAll {
		lanIP = DiscoverLocalLANIP()
	}
	return FromServerWithLAN(srv, s, lanIP)
}

// FromServerWithLAN is the primitive form used by callers that
// already know which LAN IP to embed in the URL (post-rebind, where
// the IP was computed once for both the allow-list and the URL).
func FromServerWithLAN(srv *transport.Server, s Settings, lanIP string) Settings {
	out := s
	out.URL = ""
	out.Token = ""
	out.Insecure = false
	if srv == nil {
		return out
	}
	out.Token = srv.Token()
	out.URL = AppURLWithLAN(srv, s, lanIP)
	// A LAN bind whose URL is http:// carries the token and every later
	// byte in cleartext. Surface that to the UI so the user sees a clear
	// "front this with Tailscale / SSH tunnel" warning before sharing
	// the URL on an untrusted network. Loopback URLs are also http://
	// but stay on the same machine, so they aren't flagged — and an
	// https:// URL is only ever produced when a certificate for the
	// canonical domain is loaded, which is the case this exists to stop
	// warning about.
	if s.BindAll && strings.HasPrefix(out.URL, "http://") {
		out.Insecure = true
	}
	return out
}

// Interfaces is the iface enumeration hook. Production binds it to
// net.Interfaces; tests substitute a fake to exercise the iteration
// order, range filters, and Tailscale fallback without depending on
// the host's real network configuration.
var Interfaces = net.Interfaces

// InterfaceAddrs is the per-iface address hook paired with
// Interfaces. Splitting the lookups (rather than embedding the
// addresses on a synthetic interface struct) keeps the production
// path using the standard library directly, which avoids a slow
// path for the default case.
var InterfaceAddrs = func(iface net.Interface) ([]net.Addr, error) {
	return iface.Addrs()
}

// DiscoverLocalLANIP returns a deterministic LAN IPv4 to publish in
// the share URL. Iterates interfaces sorted by Index ascending so a
// multi-homed host always returns the same answer across runs (the
// Go runtime's net.Interfaces order is not specified). Preference
// order:
//
//  1. RFC1918 / link-local IPv4 ("traditional" LAN). Most users
//     want this — it's the address other devices on their home
//     network or office subnet will route to.
//  2. Tailscale CGNAT IPv4 (100.64.0.0/10). Tailscale assigns each
//     node a CGNAT address; if the user is on Tailscale at all,
//     that IP is typically the intended remote-access path.
//     Picking it over "no LAN URL" lets the LAN-bind toggle do the
//     right thing on Tailscale-only hosts.
//  3. Empty string — caller falls back to the loopback URL.
//
// The function never returns a public IPv4. A cloud VM toggling
// LAN-bind shouldn't auto-publish its public address — without
// TLS, that invites a user to share a token over an open port.
func DiscoverLocalLANIP() string {
	ifaces, err := Interfaces()
	if err != nil {
		return ""
	}

	// Sort for determinism. The Go runtime makes no guarantee
	// about net.Interfaces ordering across runs (it's typically
	// kernel-order, but a multi-homed reboot can shuffle indices).
	// Sorting by Index pins the result so the user sees the same
	// URL across restarts — or at least a stable choice that only
	// changes when their network configuration genuinely changes.
	sorted := make([]net.Interface, len(ifaces))
	copy(sorted, ifaces)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Index < sorted[j].Index
	})

	var tailscaleFallback string
	for _, iface := range sorted {
		// Skip down / loopback interfaces — neither helps a LAN peer.
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := InterfaceAddrs(iface)
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil {
				// IPv6 LAN URLs are uncommon for the casual paste-
				// into-browser path; skipping keeps the UI string short.
				continue
			}
			if ip.IsPrivate() || ip.IsLinkLocalUnicast() {
				return ip.String()
			}
			if isTailscaleCGNAT(ip) && tailscaleFallback == "" {
				// Stash the first Tailscale IP we see but keep
				// scanning — a real RFC1918 address (if one
				// exists) wins.
				tailscaleFallback = ip.String()
			}
		}
	}
	return tailscaleFallback
}

// isTailscaleCGNAT reports whether the IPv4 falls inside the
// 100.64.0.0/10 carrier-grade NAT range Tailscale uses for its
// mesh. net.IP doesn't have a built-in for this — RFC6598 isn't a
// "private" range in the IsPrivate() sense — so we hand-check the
// prefix.
func isTailscaleCGNAT(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	return v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127
}
