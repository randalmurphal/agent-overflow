package main

import (
	"fmt"
	"net"
	"sort"
	"strings"

	"agent-overflow/internal/transport"
)

// NetworkSettings is the wire-shaped network preferences a settings
// UI reads / writes. Mirrors settings.NetworkSettings (which only
// persists user-controlled state) plus server-derived URL + Token
// fields the user can copy. The URL and Token are read-only on
// SetNetworkSettings — the server owns those.
type NetworkSettings struct {
	// BindAll, when true, asks the transport server to listen on the
	// LAN-reachable bind (0.0.0.0) so other devices on the network can
	// reach the app. Default false keeps the server on 127.0.0.1.
	BindAll bool `json:"bindAll"`

	// URL is the http://host:port/?t=<token> URL the user can paste
	// into a remote browser. Server-derived: when BindAll is true and
	// a non-loopback interface IP is discoverable, the URL points at
	// the LAN IP; otherwise it falls back to the server's own Addr.
	URL string `json:"url"`

	// Token is the current ephemeral auth token. Surfaced for
	// debugging / advanced wiring; the user shouldn't normally need
	// to touch it directly.
	Token string `json:"token"`

	// Insecure is true when the URL above traverses an untrusted
	// network in cleartext. Today that's any LAN bind: the URL is
	// http://, the token is in the query string, and a network
	// observer on the same Wi-Fi can read both. The frontend renders
	// a warning banner when Insecure is true so the user knows to
	// front the bind with Tailscale Serve, an SSH tunnel, or a
	// reverse proxy before sharing.
	Insecure bool `json:"insecure"`
}

// GetNetworkSettings returns the current persisted bind-all preference
// plus the server-derived URL and token. The URL and token are
// recomputed on every call so a rebind (e.g. via SetNetworkSettings)
// reflects immediately on the next read.
func (a *App) GetNetworkSettings() (NetworkSettings, error) {
	cfg := a.currentSettings()
	return a.networkSettingsFor(cfg.Network.BindAll), nil
}

// SetNetworkSettings persists the new bind-all preference and rebinds
// the transport server. Going false → true rebinds to 0.0.0.0:<port>
// so LAN clients can reach the app; true → false rebinds back to
// 127.0.0.1:<port>. The port is reused so a previously-shared URL
// stays valid (only the host changes).
//
// On rebind failure the transport state is unchanged (Rebind is
// state-intact on error) and the persisted setting is rolled back so
// a subsequent GetNetworkSettings returns the actual transport state.
// Returns the post-rebind NetworkSettings so the UI can update the URL
// display in one round trip.
//
// Origin allow-list: a LAN bind requires an explicit allow-list so a
// stray browser tab on the LAN can't WebSocket-hijack a leaked token
// (CSWSH). On bind-all=true the list contains loopback variants plus
// the discovered LAN IP; on bind-all=false the list is empty (loopback
// has no browser-origin to validate, and InsecureSkipVerify is fine).
func (a *App) SetNetworkSettings(s NetworkSettings) (NetworkSettings, error) {
	if a.settings == nil {
		return NetworkSettings{}, fmt.Errorf("settings service unavailable")
	}
	srv := a.transportServer.Load()
	if srv == nil {
		return NetworkSettings{}, fmt.Errorf("transport server unavailable")
	}

	prevCfg := a.settings.Get()
	if prevCfg.Network.BindAll == s.BindAll {
		// No change — return the current snapshot without touching the
		// transport. Keeps the binding idempotent for UIs that fire
		// SetNetworkSettings on every render.
		return a.networkSettingsFor(prevCfg.Network.BindAll), nil
	}

	patch := map[string]any{
		"network": map[string]any{
			"bindAll": s.BindAll,
		},
	}
	if _, err := a.settings.Update(patch); err != nil {
		return NetworkSettings{}, fmt.Errorf("persist network settings: %w", err)
	}

	port := portFromAddr(srv.Addr())
	host := networkBindHost(s.BindAll)
	addr := fmt.Sprintf("%s:%d", host, port)

	// Compute the LAN IP once so the URL we report and the origin we
	// allow-list use the same value — otherwise the user could see a
	// URL their browser can't reach without an origin failure.
	lanIP := ""
	if s.BindAll {
		lanIP = discoverLocalLANIP()
	}
	originPatterns := networkOriginPatterns(s.BindAll, lanIP)

	if err := srv.Rebind(addr, &transport.RebindOptions{OriginPatterns: originPatterns}); err != nil {
		// Roll the file back so we don't lie about the transport state.
		// The rollback uses the previously-persisted value, not the
		// patch input, so a partial Update doesn't strand bind-all=true
		// in settings while the transport runs on loopback. Rebind is
		// state-intact on failure (the transport never moved), so the
		// settings rollback is the only state we need to undo.
		rollback := map[string]any{
			"network": map[string]any{
				"bindAll": prevCfg.Network.BindAll,
			},
		}
		if _, rbErr := a.settings.Update(rollback); rbErr != nil {
			return NetworkSettings{}, fmt.Errorf("rebind failed: %w (rollback also failed: %v)", err, rbErr)
		}
		return NetworkSettings{}, fmt.Errorf("rebind transport: %w", err)
	}

	return a.networkSettingsForWithLAN(s.BindAll, lanIP), nil
}

// networkSettingsFor builds the wire-shaped NetworkSettings for the
// given bind-all flag, reading the live transport addr + token. Used
// by Get and the post-rebind error path. Discovers a LAN IP for the
// URL when bind-all is on; pass through networkSettingsForWithLAN when
// the caller already has a discovered IP they want to reuse.
func (a *App) networkSettingsFor(bindAll bool) NetworkSettings {
	lanIP := ""
	if bindAll {
		lanIP = discoverLocalLANIP()
	}
	return a.networkSettingsForWithLAN(bindAll, lanIP)
}

// networkSettingsForWithLAN is the primitive form used by callers that
// already know which LAN IP to embed in the URL (post-rebind, where
// the IP was computed once for both the allow-list and the URL).
func (a *App) networkSettingsForWithLAN(bindAll bool, lanIP string) NetworkSettings {
	out := NetworkSettings{BindAll: bindAll}
	srv := a.transportServer.Load()
	if srv == nil {
		return out
	}
	out.Token = srv.Token()
	out.URL = networkAppURLWithLAN(srv, bindAll, lanIP)
	// LAN bind URLs are http:// today — the token traverses the network
	// in cleartext. Surface that to the UI so the user sees a clear
	// "front this with Tailscale / SSH tunnel" warning before sharing
	// the URL on an untrusted network. Loopback URLs are also http://
	// but stay on the same machine, so they aren't flagged.
	if bindAll && strings.HasPrefix(out.URL, "http://") {
		out.Insecure = true
	}
	return out
}

// networkBindHost returns the bind interface for the given LAN toggle.
// Loopback (127.0.0.1) keeps the server local; 0.0.0.0 listens on
// every interface so any LAN-reachable IP routes to it.
func networkBindHost(bindAll bool) string {
	if bindAll {
		return "0.0.0.0"
	}
	return "127.0.0.1"
}

// networkOriginPatterns returns the WS-upgrade origin allow-list for
// the given LAN toggle and discovered LAN IP. On loopback the list is
// nil — the upgrader treats nil as "InsecureSkipVerify", appropriate
// for 127.0.0.1 where there's no LAN-attached browser origin to
// validate. On LAN bind the list explicitly enumerates loopback
// variants plus the discovered LAN IP so a browser tab from any other
// origin (a malicious LAN peer, a leaked-token URL pasted into a
// foreign page) can't open a WS to this server.
func networkOriginPatterns(bindAll bool, lanIP string) []string {
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

// networkAppURL returns the URL the user should share. For loopback
// binds we use the server's own Addr (already 127.0.0.1:<port>). For
// LAN binds we replace the unspecified host (0.0.0.0 / ::) with a
// discovered private LAN IP. Discovery falls back to the loopback URL
// if no interface address fits — the user gets something pasteable
// either way, even if it only works locally.
func networkAppURL(srv *transport.Server, bindAll bool) string {
	lanIP := ""
	if bindAll {
		lanIP = discoverLocalLANIP()
	}
	return networkAppURLWithLAN(srv, bindAll, lanIP)
}

// networkAppURLWithLAN renders the URL using a caller-supplied LAN IP
// so the discovery function isn't called twice in SetNetworkSettings
// (once for the allow-list, once for the URL).
func networkAppURLWithLAN(srv *transport.Server, bindAll bool, lanIP string) string {
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
		// Couldn't find a LAN-reachable address — surface the loopback
		// URL so the user at least gets something they can paste into
		// a local browser, and the UI can hint that LAN discovery
		// failed via the BindAll=true flag.
		return loopback
	}
	return fmt.Sprintf("http://%s:%s/?t=%s", lanIP, port, srv.Token())
}

// netInterfaces is the iface enumeration hook. Production binds it to
// net.Interfaces; tests substitute a fake to exercise the iteration
// order, range filters, and Tailscale fallback without depending on
// the host's real network configuration.
var netInterfaces = net.Interfaces

// netInterfaceAddrs is the per-iface address hook paired with
// netInterfaces. Splitting the lookups (rather than embedding the
// addresses on a synthetic interface struct) keeps the production path
// using the standard library directly, which avoids a slow path for
// the default case.
var netInterfaceAddrs = func(iface net.Interface) ([]net.Addr, error) {
	return iface.Addrs()
}

// discoverLocalLANIP returns a deterministic LAN IPv4 to publish in the
// share URL. Iterates interfaces sorted by Index ascending so a
// multi-homed host always returns the same answer across runs (the
// Go runtime's net.Interfaces order is not specified). Preference
// order:
//
//  1. RFC1918 / link-local IPv4 ("traditional" LAN). Most users want
//     this — it's the address other devices on their home network or
//     office subnet will route to.
//  2. Tailscale CGNAT IPv4 (100.64.0.0/10). Tailscale assigns each
//     node a CGNAT address; if the user is on Tailscale at all, that
//     IP is typically the intended remote-access path. Picking it over
//     "no LAN URL" lets the LAN-bind toggle do the right thing on
//     Tailscale-only hosts.
//  3. Empty string — caller falls back to the loopback URL.
//
// The function never returns a public IPv4. A cloud VM toggling LAN-
// bind shouldn't auto-publish its public address — without TLS, that
// invites a user to share a token over an open port.
func discoverLocalLANIP() string {
	ifaces, err := netInterfaces()
	if err != nil {
		return ""
	}

	// Sort for determinism. The Go runtime makes no guarantee about
	// net.Interfaces ordering across runs (it's typically kernel-order,
	// but a multi-homed reboot can shuffle indices). Sorting by Index
	// pins the result so the user sees the same URL across restarts —
	// or at least a stable choice that only changes when their network
	// configuration genuinely changes.
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
		addrs, err := netInterfaceAddrs(iface)
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
				// IPv6 LAN URLs are uncommon for the casual paste-into-
				// browser path; skipping keeps the UI string short.
				continue
			}
			if ip.IsPrivate() || ip.IsLinkLocalUnicast() {
				return ip.String()
			}
			if isTailscaleCGNAT(ip) && tailscaleFallback == "" {
				// Stash the first Tailscale IP we see but keep scanning —
				// a real RFC1918 address (if one exists) wins.
				tailscaleFallback = ip.String()
			}
		}
	}
	return tailscaleFallback
}

// isTailscaleCGNAT reports whether the IPv4 falls inside the
// 100.64.0.0/10 carrier-grade NAT range Tailscale uses for its mesh.
// net.IP doesn't have a built-in for this — RFC6598 isn't a "private"
// range in the IsPrivate() sense — so we hand-check the prefix.
func isTailscaleCGNAT(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	return v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127
}
