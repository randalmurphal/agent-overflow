package app

import (
	"context"
	"fmt"

	"agent-overflow/internal/network"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/transport"
)

// networkSettingsForCaller is the ONE place the host-present redaction is
// applied, and every bound method that hands a network.Settings back to a
// caller goes through it or through its WithLAN sibling below.
//
// The pick reads the proof the transport resolved for THIS call
// (internal/transport/connstate.go), never a second question asked here:
// one call, one answer, and the same answer the per-RPC gate compared. A
// context nothing installed a proof on answers the zero proof and lands on
// the redacted branch, which is both the safe default and the honest one —
// an in-process caller proved nothing about a peer.
//
// It is applied on the host-scoped methods too, where the gate has already
// refused every off-host caller. One rule wherever this shape leaves the
// process beats a per-method judgement about whether some other check
// happened to cover it; the two remaining callers of the full variant are
// deliberate and say so at their call sites (pairingPageURL in
// app_access.go, ServeEndpoints in bootstrap.go).
func (a *App) networkSettingsForCaller(ctx context.Context, s network.Settings) network.Settings {
	if !transport.CallerProofFromContext(ctx).HostPresent {
		return network.FromServerRedacted(s)
	}
	return network.FromServer(a.transportServer.Load(), s)
}

// networkSettingsForCallerWithLAN is the same pick for SetNetworkSettings,
// which discovered the LAN IP once for its origin allow-list and must not
// discover it a second time — the URL it reports and the origin it allows
// have to name the same address (internal/network/AGENTS.md).
func (a *App) networkSettingsForCallerWithLAN(
	ctx context.Context, srv *transport.Server, s network.Settings, lanIP string,
) network.Settings {
	if !transport.CallerProofFromContext(ctx).HostPresent {
		return network.FromServerRedacted(s)
	}
	return network.FromServerWithLAN(srv, s, lanIP)
}

// GetNetworkSettings returns the persisted network preferences — the
// bind toggle, the canonical domain, the DNS hook, the external
// certificate pair, the tailnet toggle and its coordination server —
// plus what only the running process knows: the certificate status, the
// tailnet node's status, and — for a caller at the machine — the share
// URLs and this launch's token. Everything server-derived is recomputed on
// every call, so a rebind, a renewal or a node joining reflects on the
// next read. The screen polls this while an issuance or a sign-in is in
// flight; there is no push channel for it.
//
// `access:admin` rather than `host`, and the two halves of that decision
// are separate. WHO may read it: managing how a backend is exposed is what
// a paired admin device is for, and a `host` annotation refused every one
// of them — leaving Settings → Remote access unreachable from any device that
// was not the machine itself. WHAT they read: the credential half is
// withheld from a caller that is not host-present, which
// network.FromServerRedacted argues field by field. The bind toggle, the
// domain, the certificate status and the tailnet node — including its
// sign-in link — are what remote administration needs, and they travel.
//
//ao:scope access:admin
//ao:route home
func (a *App) GetNetworkSettings(ctx context.Context) (network.Settings, error) {
	return a.networkSettingsForCaller(ctx, a.persistedNetworkSettings()), nil
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
// The answer it returns is picked for the caller on the same terms as
// GetNetworkSettings: this call is step-up reachable from a paired device,
// and its return carried the launch token to it until that pick existed.
//
//ao:scope settings:write
//ao:route home
//ao:stepup
func (a *App) SetNetworkSettings(ctx context.Context, s network.Settings) (network.Settings, error) {
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
		ListenPort:        s.ListenPort,
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
	// Every pattern names ONE port (internal/network.OriginPatterns), so
	// the list cannot be built until the port this listener will answer on
	// is settled. Both branches below know it, and the rebind branch
	// re-checks the number it actually got.
	originPatterns := func(port int) []string {
		return network.OriginPatterns(stored.BindAll, lanIP, stored.CanonicalDomain, port)
	}
	srv.SetCanonicalHost(stored.CanonicalDomain)

	// The address has two halves and either can move. The HOST comes from
	// the bind toggle; the PORT is the saved one, or where the listener
	// already is when nothing is saved.
	//
	// Clearing the port back to 0 ("automatic") deliberately does NOT move
	// the listener. The listener is already on a port, every share URL and
	// every paired device's stored endpoint names it, and jumping to some
	// other number to express "no preference" would break all of them to
	// change nothing the operator asked to change. It means "stop pinning
	// this", and the re-pin below is what makes the next boot agree.
	currentPort := portFromAddr(srv.Addr())
	wantPort := stored.ListenPort
	if wantPort == 0 {
		wantPort = currentPort
	}
	if prev.BindAll != stored.BindAll || wantPort != currentPort {
		addr := fmt.Sprintf("%s:%d", network.BindHost(stored.BindAll), wantPort)
		if err := srv.Rebind(addr, &transport.RebindOptions{OriginPatterns: originPatterns(wantPort)}); err != nil {
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
		if bound := portFromAddr(srv.Addr()); bound != wantPort {
			// An ephemeral bind (wantPort was 0 because nothing is saved
			// and the listener had no address to read) landed somewhere
			// only the listener knows. The list handed to Rebind names a
			// port nothing answers on, so replace it with the real one
			// rather than leave every LAN page refused.
			srv.SetOriginPatterns(originPatterns(bound))
		}
	} else {
		// No address change, so no listener swap: rotate the allow-list
		// in place. A domain edit must not drop every open socket.
		srv.SetOriginPatterns(originPatterns(currentPort))
	}

	// Keep the transport-port cache naming where this listener actually
	// is, whenever the operator touched the port field. That file is a
	// cache of the previous bind, and the two ways it could go stale are
	// both this call: pinning a new port (the cache still holds the old
	// one) and clearing the pin (the cache holds a port from before it was
	// ever set, and the next boot would silently move there).
	if prev.ListenPort != stored.ListenPort {
		a.recordBoundPort(portFromAddr(srv.Addr()))
		a.publishLocalControl()
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

	return a.networkSettingsForCallerWithLAN(ctx, srv, a.networkSettingsFrom(stored), lanIP), nil
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
//ao:route home
func (a *App) RenewCanonicalDomainCert(ctx context.Context) (network.Settings, error) {
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
	return a.networkSettingsForCaller(ctx, a.networkSettingsFrom(stored)), nil
}

// recordBoundPort tells the executable where this listener ended up, when
// there is anything listening for that. A zero port is a bound address
// this process could not parse, which is not a fact worth persisting.
func (a *App) recordBoundPort(port int) {
	if a.boundPortRecorder == nil || port == 0 {
		return
	}
	a.boundPortRecorder(port)
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
		ListenPort:      stored.ListenPort,
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
