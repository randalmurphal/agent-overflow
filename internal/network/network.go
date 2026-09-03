package network

import (
	"fmt"
	"net"
	"sort"
	"strconv"
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

	// ListenPort is the port the transport binds, or 0 for automatic
	// (settings.NetworkSettings.ListenPort argues the choice). It is a
	// PREFERENCE, not an observation: the port actually bound right now
	// is the authority in URL below, and the two differ for exactly as
	// long as it takes a saved change to rebind.
	ListenPort int `json:"listenPort"`

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

	// TailnetEnabled asks this backend to join the owner's tailnet as its
	// own node (docs/specs/remote-access.md §7). Off by default; turning
	// it on is what makes the app reachable from outside the LAN without
	// a public listener or a tunnel.
	TailnetEnabled bool `json:"tailnetEnabled"`

	// TailnetControlURL is the coordination server the node registers
	// with. Empty means the Tailscale service, which is what nearly every
	// install wants; a self-hosted control plane is why it is settable.
	TailnetControlURL string `json:"tailnetControlUrl"`

	// TLS is read-only status, filled by the app from what is actually
	// loaded. Ignored on Set — nothing here is a preference.
	TLS TLSStatus `json:"tls"`

	// Tailnet is read-only status about the node, on the same terms as
	// TLS: every field is observed, none is a preference, and the screen
	// re-reads GetNetworkSettings rather than subscribing.
	Tailnet TailnetStatus `json:"tailnet"`

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

// TailnetStatus is what the Network settings screen shows about the
// tailnet node. Read-only in both directions, like TLSStatus.
//
// The state vocabulary is Tailscale's own, carried verbatim rather than
// mapped onto a local set. Mapping would mean a table that has to be
// extended before this backend can even describe a state its dependency
// added, and "the node reports a state this build does not have a
// sentence for" is better shown than swallowed.
type TailnetStatus struct {
	// Running is true when the node is on the tailnet and answering.
	// Derived from State, and carried separately so the screen does not
	// have to know which spelling means "up".
	Running bool `json:"running"`

	// State is the node's backend state — "NeedsLogin", "Starting",
	// "Running", "Stopped". Empty when the feature is off, which is not
	// a state the node can be in.
	State string `json:"state"`

	// AuthURL is the sign-in link to open while the node waits for the
	// owner to approve it, and empty otherwise. Single use: it is
	// published only while it is live, and cleared the moment the node
	// joins.
	AuthURL string `json:"authUrl"`

	// DNSName is the node's MagicDNS name — what the owner types to
	// reach this backend from any of their devices.
	DNSName string `json:"dnsName"`

	// IPs are the node's tailnet addresses. Shown because a tailnet with
	// MagicDNS turned off has nothing but these.
	IPs []string `json:"ips"`

	// URL is the address to open on another device on the same tailnet,
	// carrying a one-time page ticket like the LAN URL above.
	//
	// There is deliberately no Insecure flag beside it. An http:// URL
	// here is not the same act as an http:// URL on a LAN: every byte of
	// it crosses an encrypted, authenticated WireGuard link between two
	// devices the owner enrolled, so warning about it would teach the
	// user to ignore a warning that does mean something on the LAN URL.
	URL string `json:"url"`

	// HTTPS is true when the node also answers TLS on its ts.net name,
	// which needs HTTPS enabled in the tailnet's admin panel. False
	// means the URL above is http:// and the reason is the tailnet's
	// settings, not this backend's.
	HTTPS bool `json:"https"`

	// HasState is true when a node identity exists on disk. It is what
	// makes "forget this node" offerable only when there is something to
	// forget — and, while the feature is off, the only sign that this
	// backend is still a device in the owner's tailnet admin panel.
	HasState bool `json:"hasState"`

	// LastError is the last node failure, verbatim. Cleared by the next
	// success. Errors are user-facing state, not log entries.
	LastError string `json:"lastError"`
}

// tailnetURL renders the address to open on another tailnet device, or
// "" when there is nothing reachable to publish.
//
// Same one-time-ticket rule as the LAN URL, and the same consequence:
// each read of the share panel hands out a URL that opens one browser
// session. It mints only while the node is actually up, so an install
// with the feature off spends no ticket at all.
func tailnetURL(srv *transport.Server, s TailnetStatus) string {
	if !s.Running || s.DNSName == "" {
		return ""
	}
	if s.HTTPS {
		// tsnet answers TLS on 443 for the node's own name, so the URL is
		// the bare name — which is the whole point of the ts.net
		// certificate.
		if url, ok := ticketedURL(srv, "https", s.DNSName); ok {
			return url
		}
		return ""
	}
	_, port, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		return ""
	}
	if url, ok := ticketedURL(srv, "http", net.JoinHostPort(s.DNSName, port)); ok {
		return url
	}
	return ""
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
// beyond the one it is addressed to, for the given LAN toggle, the
// discovered LAN IP, and the PORT this listener actually bound.
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
// EVERY PATTERN NAMES ONE PORT, and that is the whole point of the port
// parameter. Until wave 9 the LAN entries were `http://127.0.0.1:*`,
// `http://localhost:*` and `http://<lanIP>:*`, so a document served by
// ANY port on this machine named an origin the allow-list accepted, and
// the browser attached this backend's page cookie to the handshake it
// opened. No case ever needed the wildcard: it was written when the list
// was handed to the WebSocket library's own matcher and the bound port
// was simply not threaded down to the helper. The port is load-bearing
// now for a second reason as well — this machine's preview listeners
// (docs/specs/remote-access.md §7, "the port gateway") serve
// agent-authored bytes on OTHER ports of these same hosts.
//
// Each host is named under both schemes because the listener answers
// both on the one port it binds: with a certificate configured it
// classifies each connection by its first byte (transport's TLS sniffer),
// so `https://<host>:<port>` and `http://<host>:<port>` are two spellings
// of the same listener rather than two surfaces.
//
// A canonical domain adds its https origins whatever the bind is, and
// the port-bearing spelling is the load-bearing one: a page served at
// https://<domain> through something that terminates TLS in front of
// this backend reaches the socket over cleartext, so the request
// authority this server computes is http://<domain> and would not match
// the page's own origin. Naming the origin is how that deployment is
// answered rather than half-broken. It does NOT relax the Host guard —
// that reads the bind address and the canonical name, not this list.
//
// A port outside 1-65535 is a bind this process has not resolved yet, so
// every port-bearing pattern is DROPPED rather than guessed at. The
// caller's remaining admission — the request's own authority — is exact
// by construction, so the failure mode is a refusal, never a widening.
func OriginPatterns(bindAll bool, lanIP, canonicalDomain string, port int) []string {
	known := port > 0 && port <= 65535
	var patterns []string
	if bindAll && known {
		hosts := []string{"127.0.0.1", "localhost"}
		if lanIP != "" {
			hosts = append(hosts, lanIP)
		}
		for _, host := range hosts {
			authority := net.JoinHostPort(host, strconv.Itoa(port))
			patterns = append(patterns, "http://"+authority, "https://"+authority)
		}
	}
	if canonicalDomain != "" {
		// The bare name first: a proxy fronting this backend on 443 is
		// the deployment the canonical domain exists for, and a browser
		// spells that origin with no port at all.
		patterns = append(patterns, "https://"+canonicalDomain)
		if known {
			patterns = append(patterns,
				"https://"+net.JoinHostPort(canonicalDomain, strconv.Itoa(port)))
		}
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
//
// This is the form for a caller at the machine. FromServerRedacted below
// is the form for every other caller, and argues field by field what the
// difference is.
func FromServer(srv *transport.Server, s Settings) Settings {
	lanIP := ""
	if s.BindAll {
		lanIP = DiscoverLocalLANIP()
	}
	return FromServerWithLAN(srv, s, lanIP)
}

// FromServerRedacted builds the wire record for a caller that is NOT at
// this machine: everything a remote owner needs in order to read and
// change how this backend is exposed, with the four server-derived fields
// left empty.
//
// Redaction by never MINTING, not by blanking afterwards, which is why
// there is deliberately no *transport.Server parameter: nothing this
// function can produce needs one, so nothing it can produce can spend a
// ticket. Two of the withheld fields are one-time page tickets drawn from
// a book of sixteen, so building the full record and then clearing it
// would spend the owner's own supply — evicting the URL they just copied
// at their own screen — to hand the remote caller nothing.
//
// What is withheld, and why each:
//
//   - Token is this LAUNCH's session credential. A device holding it can
//     attach as the backend's own local channel: unattributed, and
//     withdrawable only by restarting the process. It is the one field
//     here that must never leave the machine.
//   - URL and Tailnet.URL each carry a one-time page ticket. Neither
//     grants anything on its own — a ticket loads the page and nothing
//     more — but both are drawn from that bounded book, and both are
//     addresses this caller already knows how to reach.
//   - Insecure describes the URL above, and there is no URL.
//
// What is KEPT is what remote administration is FOR: the bind toggle, the
// port, the canonical domain, the DNS hook argv, the external certificate
// pair, the tailnet toggle and its control URL, and both read-only status
// blocks. Tailnet.AuthURL in particular stays — it is the tailscale
// sign-in link the node publishes while it waits to be approved, which is
// precisely what a remote owner needs in order to approve it, and it is
// not a page ticket.
func FromServerRedacted(s Settings) Settings {
	out := s
	out.URL = ""
	out.Token = ""
	out.Insecure = false
	out.Tailnet.URL = ""
	return out
}

// FromServerWithLAN is the primitive form used by callers that
// already know which LAN IP to embed in the URL (post-rebind, where
// the IP was computed once for both the allow-list and the URL).
//
// It starts from the redacted record so the withheld set is declared
// once: a fifth server-derived field is added by clearing it there and
// filling it here, and a server this process does not have yet leaves the
// caller with exactly what an off-host caller would have seen.
func FromServerWithLAN(srv *transport.Server, s Settings, lanIP string) Settings {
	out := FromServerRedacted(s)
	if srv == nil {
		return out
	}
	out.Token = srv.Token()
	out.URL = AppURLWithLAN(srv, s, lanIP)
	out.Tailnet.URL = tailnetURL(srv, s.Tailnet)
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
