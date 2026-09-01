package app

import (
	"fmt"

	"agent-overflow/internal/network"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/transport"
)

// GetNetworkSettings returns the persisted network preferences — the
// bind toggle, the canonical domain, the DNS hook, the external
// certificate pair, the tailnet toggle and its coordination server —
// plus what only the running process knows: the share URLs, this launch's
// token, the certificate status, and the tailnet node's status.
// Everything server-derived is recomputed on every call, so a rebind, a
// renewal or a node joining reflects on the next read. The screen polls
// this while an issuance or a sign-in is in flight; there is no push
// channel for it.
//
//ao:scope host
func (a *App) GetNetworkSettings() (network.Settings, error) {
	return network.FromServer(a.transportServer.Load(), a.persistedNetworkSettings()), nil
}

// SetNetworkSettings persists the network preferences and applies the
// ones the transport holds live.
//
// The bind: false → true rebinds to 0.0.0.0:<port> so LAN clients can
// reach the app; true → false rebinds back to 127.0.0.1:<port>. The port
// is reused so a previously-shared URL stays valid (only the host
// changes). On rebind failure the transport state is unchanged (Rebind
// is state-intact on error) and the persisted setting is rolled back, so
// a subsequent GetNetworkSettings returns the actual transport state.
//
// The canonical domain: applied to the listener as the one DNS name its
// Host guard accepts besides the loopback spellings, and to the origin
// allow-list as the https origins a page served under that name has. The
// certificate for it is NOT obtained here — that is the reconciler in
// app_domaincert.go, kicked at the end of this call, because a DNS-01
// exchange outlives any RPC.
//
// The tailnet: the toggle and its coordination server are persisted and
// the reconciler is kicked, exactly like the certificate half and for the
// same reason — a node's first bring-up ends in an interactive sign-in,
// which outlives any RPC. Nothing about the node is done inline here.
//
// Origin allow-list: a LAN bind requires an explicit allow-list, so a
// page loaded from some other origin cannot open a socket here with a
// token it happened to learn. On bind-all=true the list contains
// loopback variants plus the discovered LAN IP; a canonical domain adds
// its own https origins on either bind.
//
//ao:scope settings:write
//ao:stepup
func (a *App) SetNetworkSettings(s network.Settings) (network.Settings, error) {
	if a.settings == nil {
		return network.Settings{}, fmt.Errorf("settings service unavailable")
	}
	srv := a.transportServer.Load()
	if srv == nil {
		return network.Settings{}, fmt.Errorf("transport server unavailable")
	}

	prev := a.settings.Get().Network
	next := settings.NetworkSettings{
		BindAll:           s.BindAll,
		CanonicalDomain:   s.CanonicalDomain,
		ACMEDNSHook:       s.ACMEDNSHook,
		ExternalCertFile:  s.ExternalCertFile,
		ExternalKeyFile:   s.ExternalKeyFile,
		TailnetEnabled:    s.TailnetEnabled,
		TailnetControlURL: s.TailnetControlURL,
	}
	if _, err := a.settings.SetNetwork(next); err != nil {
		return network.Settings{}, fmt.Errorf("persist network settings: %w", err)
	}
	// Read back what was stored rather than trusting the input: the
	// service trims and lower-cases, and the domain the listener compares
	// a Host header against must be the same spelling the certificate was
	// ordered for.
	stored := a.settings.Get().Network

	// Compute the LAN IP once so the URL we report and the origin we
	// allow-list use the same value — otherwise the user could see a URL
	// their browser can't reach without an origin failure.
	lanIP := ""
	if stored.BindAll {
		lanIP = network.DiscoverLocalLANIP()
	}
	originPatterns := network.OriginPatterns(stored.BindAll, lanIP, stored.CanonicalDomain)
	srv.SetCanonicalHost(stored.CanonicalDomain)

	if prev.BindAll != stored.BindAll {
		port := portFromAddr(srv.Addr())
		addr := fmt.Sprintf("%s:%d", network.BindHost(stored.BindAll), port)
		if err := srv.Rebind(addr, &transport.RebindOptions{OriginPatterns: originPatterns}); err != nil {
			// Roll the file back so we don't lie about the transport
			// state. The rollback uses the previously-persisted value,
			// not the patch input, so a partial write doesn't strand
			// bind-all=true in settings while the transport runs on
			// loopback. Rebind is state-intact on failure (the transport
			// never moved), so the settings rollback plus the two live
			// values set above is the whole undo.
			srv.SetCanonicalHost(prev.CanonicalDomain)
			if _, rbErr := a.settings.SetNetwork(prev); rbErr != nil {
				return network.Settings{}, fmt.Errorf("rebind failed: %w (rollback also failed: %v)", err, rbErr)
			}
			return network.Settings{}, fmt.Errorf("rebind transport: %w", err)
		}
	} else {
		// No address change, so no listener swap: rotate the allow-list
		// in place. A domain edit must not drop every open socket.
		srv.SetOriginPatterns(originPatterns)
	}

	// The certificate half runs on its own goroutine. Kicking it here is
	// what makes "save the domain" and "obtain a certificate for it" one
	// act from the user's side without making them one call.
	a.kickDomainCertificate()

	// The tailnet half is the same arrangement for the same reason: the
	// node's first bring-up ends in an interactive sign-in, so "turn it
	// on" and "be on the tailnet" cannot be one call. The screen polls the
	// status and shows the link.
	a.kickTailnet()

	return network.FromServerWithLAN(srv, a.networkSettingsFrom(stored), lanIP), nil
}

// RenewCanonicalDomainCert asks the reconciler to check the canonical
// domain's certificate now: load an external pair that changed, or order
// one when there is none or it is inside the renewal window. Returns
// immediately with the current status — the work happens on the
// reconciler's goroutine and the screen polls GetNetworkSettings while
// `tls.renewing` is set, because a DNS-01 exchange takes minutes and no
// RPC may wait that long.
//
// No step-up: this call carries no argument and changes no
// configuration — it re-runs what the daily timer would have run anyway,
// against settings that were themselves written through the
// step-up-gated SetNetworkSettings, so demanding a second proof would
// gate the retry of an act that was already proved.
//
//ao:scope host
func (a *App) RenewCanonicalDomainCert() (network.Settings, error) {
	if a.settings == nil {
		return network.Settings{}, fmt.Errorf("settings service unavailable")
	}
	stored := a.settings.Get().Network
	if stored.CanonicalDomain == "" {
		return network.Settings{}, fmt.Errorf("no canonical domain is configured, so there is no certificate to renew")
	}
	if !a.kickDomainCertificate() {
		return network.Settings{}, fmt.Errorf("the certificate reconciler is not running")
	}
	return network.FromServer(a.transportServer.Load(), a.networkSettingsFrom(stored)), nil
}

// persistedNetworkSettings is the wire record built from what is stored
// plus the observed certificate status, with the server-derived fields
// left for network.FromServer to fill.
func (a *App) persistedNetworkSettings() network.Settings {
	return a.networkSettingsFrom(a.currentSettings().Network)
}

func (a *App) networkSettingsFrom(stored settings.NetworkSettings) network.Settings {
	return network.Settings{
		BindAll:         stored.BindAll,
		CanonicalDomain: stored.CanonicalDomain,
		// OrEmpty so an unconfigured hook is `[]` on the wire rather than
		// `null`: the field is a list the screen renders, and a client
		// should not have to coalesce one absent value per read.
		ACMEDNSHook:       slicesx.OrEmpty(stored.ACMEDNSHook),
		ExternalCertFile:  stored.ExternalCertFile,
		ExternalKeyFile:   stored.ExternalKeyFile,
		TailnetEnabled:    stored.TailnetEnabled,
		TailnetControlURL: stored.TailnetControlURL,
		TLS:               a.domainCertStatus(),
		Tailnet:           a.tailnetStatus(),
	}
}
