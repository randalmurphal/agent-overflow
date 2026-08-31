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
	// network in cleartext. Today that's any LAN bind: the URL is
	// http://, so the ticket on it and every byte the paired device
	// exchanges afterwards are readable by anything on the same
	// Wi-Fi. The frontend renders a warning banner when Insecure is
	// true so the user knows to front the bind with Tailscale Serve,
	// an SSH tunnel, or a reverse proxy before sharing.
	Insecure bool `json:"insecure"`
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
func OriginPatterns(bindAll bool, lanIP string) []string {
	if !bindAll {
		return nil
	}
	patterns := []string{
		"http://127.0.0.1:*",
		"http://localhost:*",
	}
	if lanIP != "" {
		patterns = append(patterns, fmt.Sprintf("http://%s:*", lanIP))
	}
	return patterns
}

// AppURLWithLAN renders the URL using a caller-supplied LAN IP so
// the discovery function isn't called twice in a Set flow (once for
// the allow-list, once for the URL).
//
// Both branches carry a freshly minted one-time page ticket: the
// loopback one from Server.AppURL, the LAN one minted here because only
// this function knows the interface address to name. Each render of the
// share panel therefore hands out a URL that opens one browser session
// — a second device needs the panel read again.
func AppURLWithLAN(srv *transport.Server, bindAll bool, lanIP string) string {
	loopback := srv.AppURL()
	if !bindAll {
		return loopback
	}
	addr := srv.Addr()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return loopback
	}
	if lanIP == "" {
		// Couldn't find a LAN-reachable address — surface the
		// loopback URL so the user at least gets something they can
		// paste into a local browser, and the UI can hint that LAN
		// discovery failed via the BindAll=true flag.
		return loopback
	}
	ticket, err := srv.MintPageTicket()
	if err != nil {
		return loopback
	}
	return fmt.Sprintf("http://%s:%s/?%s=%s", lanIP, port, transport.PageTicketParam, ticket)
}

// FromServer builds a Settings record for the given bind-all flag
// using the live transport server (addr + token). Caller is expected
// to provide the persisted BindAll value; the URL / Token / Insecure
// fields are server-derived.
func FromServer(srv *transport.Server, bindAll bool) Settings {
	lanIP := ""
	if bindAll {
		lanIP = DiscoverLocalLANIP()
	}
	return FromServerWithLAN(srv, bindAll, lanIP)
}

// FromServerWithLAN is the primitive form used by callers that
// already know which LAN IP to embed in the URL (post-rebind, where
// the IP was computed once for both the allow-list and the URL).
func FromServerWithLAN(srv *transport.Server, bindAll bool, lanIP string) Settings {
	out := Settings{BindAll: bindAll}
	if srv == nil {
		return out
	}
	out.Token = srv.Token()
	out.URL = AppURLWithLAN(srv, bindAll, lanIP)
	// LAN bind URLs are http:// today — the token traverses the
	// network in cleartext. Surface that to the UI so the user sees
	// a clear "front this with Tailscale / SSH tunnel" warning
	// before sharing the URL on an untrusted network. Loopback URLs
	// are also http:// but stay on the same machine, so they aren't
	// flagged.
	if bindAll && strings.HasPrefix(out.URL, "http://") {
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
