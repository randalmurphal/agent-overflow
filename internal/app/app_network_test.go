package app

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/identity"
	"agent-overflow/internal/network"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// atTheMachine is the ctx a bound method sees for a call made from THIS
// machine — the embedded webview, the harness, `ao-harness`, `--connect`
// on the same host. Every case below that reads a URL or a token needs it:
// the credential half of network.Settings is withheld from a caller that
// is not host-present, and a bare context.Background() carries the zero
// proof, which is that caller (app_network.go networkSettingsForCaller).
func atTheMachine() context.Context {
	return callFrom("", true)
}

// newNetworkTestApp wires the minimum App + transport + dispatcher
// needed for GetNetworkSettings / SetNetworkSettings tests. The
// dispatcher is empty — we never dial it via WS — but transport.New
// requires non-nil Dispatcher and EventBus.
func newNetworkTestApp(t *testing.T) (*App, *transport.Server) {
	t.Helper()

	app := &App{
		settings: settings.NewService(t.TempDir()),
	}
	srv := startTestTransportServer(t)
	app.SetTransportServer(srv)
	return app, srv
}

// startTestTransportServer boots a loopback transport server and shuts it down
// with the test. It is shared with the AO-credential tests, which need a live
// server only because a session's credential is minted from its URL.
func startTestTransportServer(t *testing.T) *transport.Server {
	t.Helper()
	srv, err := transport.New(transport.Config{
		Dispatcher: transport.NewDispatcher(),
		EventBus:   transport.NewEventBus(8),
		Token:      "test-network-token",
	})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("transport.Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv
}

func TestGetNetworkSettings_DefaultsToLoopback(t *testing.T) {
	app, srv := newNetworkTestApp(t)

	got, err := app.GetNetworkSettings(atTheMachine())
	if err != nil {
		t.Fatalf("GetNetworkSettings: %v", err)
	}
	if got.BindAll {
		t.Fatalf("BindAll = true, want false (default)")
	}
	if got.Token != srv.Token() {
		t.Fatalf("Token = %q, want %q", got.Token, srv.Token())
	}
	if !strings.Contains(got.URL, "127.0.0.1") {
		t.Fatalf("URL = %q, want loopback host", got.URL)
	}
	// The shared URL carries a one-time page ticket, never the session
	// token: what a user copies out of this panel buys one browser
	// session and cannot be replayed.
	if strings.Contains(got.URL, srv.Token()) {
		t.Fatalf("URL = %q carries the session token", got.URL)
	}
	if !strings.Contains(got.URL, "?"+transport.PageTicketParam+"=") {
		t.Fatalf("URL = %q carries no page ticket", got.URL)
	}
}

// TestSetNetworkSettings_TogglesAndPersistsBindAll proves the binding
// flips both directions: false→true rebinds to 0.0.0.0:<same-port>,
// true→false rebinds back to 127.0.0.1:<same-port>. The port is
// preserved so any URL the user already shared keeps working at the
// host-only differing address.
func TestSetNetworkSettings_TogglesAndPersistsBindAll(t *testing.T) {
	app, srv := newNetworkTestApp(t)
	originalPort := portFromAddr(srv.Addr())

	got, err := app.SetNetworkSettings(atTheMachine(), network.Settings{BindAll: true})
	if err != nil {
		t.Fatalf("SetNetworkSettings(true): %v", err)
	}
	if !got.BindAll {
		t.Fatalf("BindAll = false, want true after toggle")
	}
	// The transport addr must now be 0.0.0.0 (or [::]) on the same port.
	host, port, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("split addr after toggle: %v", err)
	}
	if host != "0.0.0.0" && host != "::" {
		t.Fatalf("bind host = %q, want 0.0.0.0 after BindAll=true", host)
	}
	if port != intToPortString(originalPort) {
		t.Fatalf("port changed across rebind: was %d, now %s", originalPort, port)
	}

	// Settings file persisted the change — a fresh service over the
	// same dir should reload it.
	persisted := app.settings.Get()
	if !persisted.Network.BindAll {
		t.Fatalf("persisted Network.BindAll = false, want true")
	}

	// Toggle back; verify rebind to loopback.
	got, err = app.SetNetworkSettings(atTheMachine(), network.Settings{BindAll: false})
	if err != nil {
		t.Fatalf("SetNetworkSettings(false): %v", err)
	}
	if got.BindAll {
		t.Fatalf("BindAll = true, want false after second toggle")
	}
	host, port, err = net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("split addr after second toggle: %v", err)
	}
	if host != "127.0.0.1" {
		t.Fatalf("bind host = %q, want 127.0.0.1 after BindAll=false", host)
	}
	if port != intToPortString(originalPort) {
		t.Fatalf("port changed across second rebind: was %d, now %s", originalPort, port)
	}
}

// TestNetworkSettings_InsecureFlag pins the LAN-bind-plaintext
// warning surface: when BindAll is on and the URL is http://, the
// Insecure flag is true so the frontend can render a "use Tailscale
// / SSH tunnel" warning before the user shares the URL on an
// untrusted network. Loopback URLs are also http:// but never
// traverse a network — they stay safe and are not flagged.
func TestNetworkSettings_InsecureFlag(t *testing.T) {
	app, _ := newNetworkTestApp(t)

	// Default: loopback bind. http:// but not insecure (it stays on
	// the same machine).
	got, err := app.GetNetworkSettings(atTheMachine())
	if err != nil {
		t.Fatalf("GetNetworkSettings: %v", err)
	}
	if got.Insecure {
		t.Fatalf("loopback bind: Insecure = true, want false")
	}

	// Toggle to LAN bind: still http://, now Insecure. Skip the
	// test if no LAN IP discoverable (CI sandbox); we only assert
	// when the URL actually contains a non-loopback host.
	got, err = app.SetNetworkSettings(atTheMachine(), network.Settings{BindAll: true})
	if err != nil {
		t.Fatalf("SetNetworkSettings(true): %v", err)
	}
	if !strings.HasPrefix(got.URL, "http://") {
		t.Fatalf("LAN URL not http://: %q", got.URL)
	}
	if !got.Insecure {
		t.Fatalf("LAN bind: Insecure = false, want true (URL %q traverses LAN in cleartext)", got.URL)
	}

	// Toggle back: Insecure clears.
	got, err = app.SetNetworkSettings(atTheMachine(), network.Settings{BindAll: false})
	if err != nil {
		t.Fatalf("SetNetworkSettings(false): %v", err)
	}
	if got.Insecure {
		t.Fatalf("after toggle to loopback: Insecure = true, want false")
	}
}

// TestSetNetworkSettings_NoOpWhenUnchanged verifies the binding is
// idempotent — calling SetNetworkSettings with the same flag twice
// in a row doesn't churn the transport (which would interrupt
// in-flight connections for no reason).
func TestSetNetworkSettings_NoOpWhenUnchanged(t *testing.T) {
	app, srv := newNetworkTestApp(t)
	originalAddr := srv.Addr()

	if _, err := app.SetNetworkSettings(atTheMachine(), network.Settings{BindAll: false}); err != nil {
		t.Fatalf("SetNetworkSettings(false) on default: %v", err)
	}
	if srv.Addr() != originalAddr {
		t.Fatalf("addr changed on no-op set: was %q, now %q", originalAddr, srv.Addr())
	}
}

// The domain is applied to the live listener in the same call that
// persists it: the Host header carrying that name is answered, and the
// page origins under it may open a socket. Neither waits for a restart,
// and neither is a rebind — an open connection must survive a user
// typing their domain into the settings screen.
func TestSetNetworkSettings_AppliesTheCanonicalDomainLive(t *testing.T) {
	app, srv := newNetworkTestApp(t)
	addr := srv.Addr()

	got, err := app.SetNetworkSettings(atTheMachine(), network.Settings{
		CanonicalDomain: "  Backend.Example  ",
		ACMEDNSHook:     []string{"dns-hook", "--zone", "example"},
	})
	if err != nil {
		t.Fatalf("SetNetworkSettings: %v", err)
	}
	if got.CanonicalDomain != "backend.example" {
		t.Fatalf("canonicalDomain = %q, want the stored spelling", got.CanonicalDomain)
	}
	if srv.CanonicalHost() != "backend.example" {
		t.Fatalf("the listener answers to %q, want backend.example", srv.CanonicalHost())
	}
	if srv.Addr() != addr {
		t.Fatalf("a domain edit rebound the listener: %q -> %q", addr, srv.Addr())
	}
	if !app.settings.Get().Network.WantsACME() {
		t.Fatal("the stored settings do not describe a backend that wants issuance")
	}
	// Nothing was issued, so the share URL is unchanged and the status
	// says what is actually presented rather than what was configured.
	if !strings.Contains(got.URL, "127.0.0.1") {
		t.Fatalf("URL = %q, want the loopback address while no certificate is loaded", got.URL)
	}
	if got.TLS.Serving != network.TLSServingNone {
		t.Fatalf("serving = %q, want %q", got.TLS.Serving, network.TLSServingNone)
	}

	// Clearing the domain withdraws the name from the listener too.
	if _, err := app.SetNetworkSettings(atTheMachine(), network.Settings{}); err != nil {
		t.Fatalf("SetNetworkSettings(clear): %v", err)
	}
	if srv.CanonicalHost() != "" {
		t.Fatalf("the listener still answers to %q after the domain was cleared", srv.CanonicalHost())
	}
}

// A refused domain never reaches the listener, and never reaches the
// file either: one write path, one set of rules.
func TestSetNetworkSettings_RefusesADomainThatCannotBeServed(t *testing.T) {
	app, srv := newNetworkTestApp(t)

	if _, err := app.SetNetworkSettings(atTheMachine(), network.Settings{CanonicalDomain: "https://backend.example/"}); err == nil {
		t.Fatal("a URL was accepted as the canonical domain")
	}
	if srv.CanonicalHost() != "" {
		t.Fatalf("the listener took %q from a refused write", srv.CanonicalHost())
	}
	if app.settings.Get().Network.CanonicalDomain != "" {
		t.Fatal("a refused write still persisted the domain")
	}
}

// The manual button is a kick, not a synchronous issuance: it answers
// with the current status and refuses only what it can answer for
// immediately.
func TestRenewCanonicalDomainCert_RefusesWithNoDomain(t *testing.T) {
	app, _ := newNetworkTestApp(t)

	if _, err := app.RenewCanonicalDomainCert(atTheMachine()); err == nil {
		t.Fatal("a renewal was accepted with no canonical domain configured")
	}

	if _, err := app.SetNetworkSettings(atTheMachine(), network.Settings{
		CanonicalDomain: "backend.example",
		ACMEDNSHook:     []string{"dns-hook"},
	}); err != nil {
		t.Fatalf("SetNetworkSettings: %v", err)
	}
	// The reconciler is started by Start, which this fixture does not
	// run, so the call reports that rather than pretending it queued
	// work nothing will pick up.
	if _, err := app.RenewCanonicalDomainCert(atTheMachine()); err == nil {
		t.Fatal("a renewal was accepted with no reconciler running")
	}
}

// TestNetworkFromServer_LoopbackUsesAppURL pins the URL output for the
// default loopback bind: it is Server.AppURL's, so the user sees the
// same address however settings is queried.
//
// It is not the same STRING twice: every render mints its own one-time
// page ticket, so two reads of the panel hand out two independently
// openable URLs rather than one that a second reader finds spent.
func TestNetworkFromServer_LoopbackUsesAppURL(t *testing.T) {
	_, srv := newNetworkTestApp(t)
	first := network.FromServer(srv, network.Settings{}).URL
	second := network.FromServer(srv, network.Settings{}).URL
	if originOf(t, first) != originOf(t, srv.AppURL()) {
		t.Fatalf("loopback URL = %q, want the server's own origin", first)
	}
	if first == second {
		t.Fatalf("two renders handed out the same URL: %q", first)
	}
}

// originOf reduces a page URL to scheme://host:port.
func originOf(t *testing.T, pageURL string) string {
	t.Helper()
	parsed, err := url.Parse(pageURL)
	if err != nil {
		t.Fatalf("parse %q: %v", pageURL, err)
	}
	return parsed.Scheme + "://" + parsed.Host
}

// intToPortString matches what SplitHostPort produces — base-10
// integer with no leading zeros — so a string == string compare
// lines up exactly.
func intToPortString(p int) string {
	return strconv.Itoa(p)
}

// TestSetNetworkSettings_TransportUnavailable proves the binding
// fails fast when the transport server pointer is nil — the App
// was wired without SetTransportServer (a startup ordering bug, or
// a partial boot). The persisted settings must NOT change in that
// case; otherwise the next boot would honor a flag the transport
// never actually applied.
func TestSetNetworkSettings_TransportUnavailable(t *testing.T) {
	app := &App{settings: settings.NewService(t.TempDir())}
	prev := app.settings.Get().Network.BindAll

	if _, err := app.SetNetworkSettings(atTheMachine(), network.Settings{BindAll: true}); err == nil {
		t.Fatalf("SetNetworkSettings without transport should error")
	}

	if got := app.settings.Get().Network.BindAll; got != prev {
		t.Fatalf("Network.BindAll persisted despite transport-unavailable error: was %v, now %v", prev, got)
	}
}

// TestSetNetworkSettings_BindAllTrueFalseTrueCycle exercises the
// listener swap three times in a row to surface any state retention
// from previous rebinds (origin patterns reverting, listener leak,
// addr churn). Each toggle must land on the right bind host.
func TestSetNetworkSettings_BindAllTrueFalseTrueCycle(t *testing.T) {
	app, srv := newNetworkTestApp(t)
	originalPort := portFromAddr(srv.Addr())

	expectations := []struct {
		bindAll bool
		host    string
	}{
		{bindAll: true, host: "0.0.0.0"},
		{bindAll: false, host: "127.0.0.1"},
		{bindAll: true, host: "0.0.0.0"},
	}

	for i, exp := range expectations {
		got, err := app.SetNetworkSettings(atTheMachine(), network.Settings{BindAll: exp.bindAll})
		if err != nil {
			t.Fatalf("step %d SetNetworkSettings(%v): %v", i, exp.bindAll, err)
		}
		if got.BindAll != exp.bindAll {
			t.Fatalf("step %d BindAll = %v, want %v", i, got.BindAll, exp.bindAll)
		}
		host, port, err := net.SplitHostPort(srv.Addr())
		if err != nil {
			t.Fatalf("step %d split addr: %v", i, err)
		}
		// IPv6-only hosts may render 0.0.0.0 as "::" — accept either.
		if exp.host == "0.0.0.0" {
			if host != "0.0.0.0" && host != "::" {
				t.Fatalf("step %d bind host = %q, want %q", i, host, exp.host)
			}
		} else if host != exp.host {
			t.Fatalf("step %d bind host = %q, want %q", i, host, exp.host)
		}
		if port != intToPortString(originalPort) {
			t.Fatalf("step %d port changed: was %d, now %s", i, originalPort, port)
		}
	}
}

// TestSetNetworkSettings_RebindFailureRollsBack drives the failure
// path: a Rebind that can't bind (someone else holds the address)
// must leave the settings file at its previous value, AND must
// leave the transport state untouched (Rebind's state-intact
// contract).
//
// We engineer the failure by holding a foreign listener on the
// exact 0.0.0.0:<live-port> the toggle will ask for. The kernel
// refuses the duplicate bind, Rebind fails before mutating
// anything, and SetNetworkSettings rolls the persisted flag back.
func TestSetNetworkSettings_RebindFailureRollsBack(t *testing.T) {
	app, srv := newNetworkTestApp(t)

	preAddr := srv.Addr()
	preBindAll := app.settings.Get().Network.BindAll

	// SetNetworkSettings preserves the port across the toggle, so
	// the rebind addr will be 0.0.0.0:<currentPort>. Grab that addr
	// first to force the rebind to fail — the kernel won't let us
	// bind 0.0.0.0:N while another listener already holds it.
	livePort := portFromAddr(preAddr)
	blocker, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", livePort))
	if err != nil {
		// Some hosts won't allow this dual-bind because
		// 127.0.0.1:livePort is already held by the live server.
		// Skip rather than fail — the state-intact contract is
		// also covered directly by the transport-package test.
		t.Skipf("could not stage blocker on 0.0.0.0:%d: %v", livePort, err)
	}
	defer blocker.Close()

	if _, err := app.SetNetworkSettings(atTheMachine(), network.Settings{BindAll: true}); err == nil {
		t.Fatalf("SetNetworkSettings expected to fail when target addr is held")
	}

	if got := app.settings.Get().Network.BindAll; got != preBindAll {
		t.Fatalf("settings BindAll persisted on rebind failure: pre=%v post=%v", preBindAll, got)
	}
	if got := srv.Addr(); got != preAddr {
		t.Fatalf("transport addr changed despite failed rebind: pre=%q post=%q", preAddr, got)
	}
}

// ---------------------------------------------------------------------
// The credential half does not leave the machine.
//
// GetNetworkSettings answers `access:admin`, which is what makes
// Settings → Network reachable from a paired admin device at all — the
// `host` annotation it carried before refused every one of them. What that
// device must NOT be handed is this launch's token (a holder attaches as
// the backend's own local channel: unattributed, and withdrawable only by
// restarting the process) or either ticket-bearing share URL.
// ---------------------------------------------------------------------

// offHostAdminApp is an identity-wired App on a live transport, plus a
// paired session holding the two grants this surface is reached through.
func offHostAdminApp(t *testing.T) (*App, store.Session) {
	t.Helper()
	app := identityApp(t)
	app.SetTransportServer(startTestTransportServer(t))
	session := pairSessionWithScopes(t, app, "thumb-network-admin", []identity.Scope{
		identity.ScopeAccessAdmin,
		identity.ScopeSettingsWrite,
	})
	// The precondition the rest of the file rests on: the gate ADMITS this
	// device's read. Were GetNetworkSettings still `host`, every assertion
	// below would be about an answer nobody could ask for.
	if refusal := transport.AuthorizeSessionMethod(
		session.Scopes, "GetNetworkSettings", transport.CallerProof{},
	); refusal != nil {
		t.Fatalf("an access:admin session is refused GetNetworkSettings: %+v", refusal)
	}
	return app, session
}

// withheldFrom is the whole redaction stated once: the full record with
// the four server-derived fields cleared. Comparing against it — rather
// than against a list of fields somebody remembered — is what makes a
// field added later fail here instead of travelling silently.
func withheldFrom(full network.Settings) network.Settings {
	want := full
	want.URL = ""
	want.Token = ""
	want.Insecure = false
	want.Tailnet.URL = ""
	return want
}

func TestGetNetworkSettingsWithholdsCredentialsFromAnOffHostAdmin(t *testing.T) {
	app, session := offHostAdminApp(t)

	// A configuration worth reading remotely, written from the machine.
	if _, err := app.SetNetworkSettings(atTheMachine(), network.Settings{
		BindAll:         true,
		CanonicalDomain: "backend.example",
		ACMEDNSHook:     []string{"dnstool", "--zone", "example.com"},
	}); err != nil {
		t.Fatalf("SetNetworkSettings: %v", err)
	}

	atMachine, err := app.GetNetworkSettings(atTheMachine())
	if err != nil {
		t.Fatalf("GetNetworkSettings (host): %v", err)
	}
	if atMachine.Token == "" {
		t.Fatal("the owner's own screen was not given this launch's token")
	}
	if atMachine.URL == "" {
		t.Fatal("the owner's own screen was not given a share URL")
	}

	remote, err := app.GetNetworkSettings(callFrom(session.ID, false))
	if err != nil {
		t.Fatalf("GetNetworkSettings (off-host admin): %v", err)
	}
	if remote.Token != "" {
		t.Errorf("Token = %q reached a device that is not this machine", remote.Token)
	}
	if remote.URL != "" {
		t.Errorf("URL = %q reached a device that is not this machine", remote.URL)
	}
	if remote.Tailnet.URL != "" {
		t.Errorf("Tailnet.URL = %q reached a device that is not this machine", remote.Tailnet.URL)
	}
	if remote.Insecure {
		t.Error("Insecure is set on a record carrying no URL to describe")
	}

	// Everything else is what the screen exists to show and change.
	if !remote.BindAll || remote.CanonicalDomain != "backend.example" {
		t.Fatalf("the remote admin lost the settings it came for: %+v", remote)
	}
	if want := withheldFrom(atMachine); !reflect.DeepEqual(remote, want) {
		t.Fatalf("redacted record\n got %+v\nwant %+v", remote, want)
	}
}

// The tailnet's sign-in link travels, and it is the field most likely to
// be swept up by a blunter rule: it is a URL, it is single use, and it
// looks like the two that are withheld. It is not a page ticket — it is
// the link the owner opens to APPROVE this machine, so withholding it
// would leave a remote owner able to enable the feature and unable to
// finish it.
func TestRedactionKeepsTheTailnetSignInLink(t *testing.T) {
	app, session := offHostAdminApp(t)

	staged := network.Settings{
		TailnetEnabled: true,
		Tailnet: network.TailnetStatus{
			State:   "NeedsLogin",
			AuthURL: "https://login.tailscale.com/a/0123456789abcdef",
			URL:     "http://node.example.ts.net:1234/?t=a-page-ticket",
		},
	}

	remote := app.networkSettingsForCaller(callFrom(session.ID, false), staged)
	if remote.Tailnet.AuthURL != staged.Tailnet.AuthURL {
		t.Errorf("AuthURL = %q, want the sign-in link to travel", remote.Tailnet.AuthURL)
	}
	if remote.Tailnet.State != "NeedsLogin" {
		t.Errorf("State = %q, want the node's own word for what it is doing", remote.Tailnet.State)
	}
	if remote.Tailnet.URL != "" {
		t.Errorf("Tailnet.URL = %q, want the ticketed address withheld", remote.Tailnet.URL)
	}
}

// The leak this closed. SetNetworkSettings is step-up reachable from a
// paired device (it carries //ao:stepup, and a passkey assertion is a
// proof a remote owner can produce), and its RETURN carried the launch
// token to that device — so the write a phone made handed it the one
// credential the read is careful never to.
func TestSetNetworkSettingsWithholdsCredentialsFromAnOffHostAdmin(t *testing.T) {
	app, session := offHostAdminApp(t)

	// The remote owner's own step-up proof, which is what reaches this
	// method from a device that is not the machine.
	written, err := app.SetNetworkSettings(callSteppedUp(session.ID), network.Settings{
		BindAll:         true,
		CanonicalDomain: "backend.example",
	})
	if err != nil {
		t.Fatalf("SetNetworkSettings (off-host admin): %v", err)
	}
	if written.Token != "" {
		t.Errorf("Token = %q rode the write's answer to a device that is not this machine", written.Token)
	}
	if written.URL != "" {
		t.Errorf("URL = %q rode the write's answer to a device that is not this machine", written.URL)
	}
	if written.Tailnet.URL != "" {
		t.Errorf("Tailnet.URL = %q rode the write's answer off-host", written.Tailnet.URL)
	}
	// The write still ANSWERED with what it did, or the screen has nothing
	// to paint the result from.
	if !written.BindAll || written.CanonicalDomain != "backend.example" {
		t.Fatalf("the write's answer lost what it wrote: %+v", written)
	}

	// The same write from the machine still carries both, so the case
	// above is the redaction and not a regression in the write itself.
	atMachine, err := app.SetNetworkSettings(atTheMachine(), network.Settings{
		BindAll:         true,
		CanonicalDomain: "backend.example",
	})
	if err != nil {
		t.Fatalf("SetNetworkSettings (host): %v", err)
	}
	if atMachine.Token == "" || atMachine.URL == "" {
		t.Fatalf("the owner's own screen lost the share panel: %+v", atMachine)
	}
}

// ForgetTailnetNode and RenewCanonicalDomainCert return the same shape and
// go through the same pick. Both are `host`-scoped, so the gate already
// refuses an off-host session — this is the belt to that braces, and the
// reason the rule is applied wherever the shape leaves the process rather
// than per method.
func TestEveryNetworkSettingsAnswerGoesThroughThePick(t *testing.T) {
	app, session := offHostAdminApp(t)
	remote := callFrom(session.ID, false)
	// The config root the reconciler would have been handed at boot, so
	// ForgetTailnetNode reaches its answer rather than refusing for want of
	// a directory. Its goroutine stays unstarted: nothing here needs a node.
	app.tailnet.mu.Lock()
	app.tailnet.dir = t.TempDir()
	app.tailnet.mu.Unlock()

	if _, err := app.SetNetworkSettings(atTheMachine(), network.Settings{
		CanonicalDomain: "backend.example",
		ACMEDNSHook:     []string{"dnstool"},
	}); err != nil {
		t.Fatalf("SetNetworkSettings: %v", err)
	}

	// The reconciler is not running in this fixture, so the renewal
	// refuses before it formats anything — which is why the pick is
	// asserted through the formatter it shares rather than through a
	// return this fixture cannot produce.
	if _, err := app.RenewCanonicalDomainCert(remote); err == nil {
		t.Fatal("a renewal was accepted with no reconciler running")
	}

	forgotten, err := app.ForgetTailnetNode(remote)
	if err != nil {
		t.Fatalf("ForgetTailnetNode: %v", err)
	}
	if forgotten.Token != "" || forgotten.URL != "" || forgotten.Tailnet.URL != "" {
		t.Fatalf("ForgetTailnetNode answered an off-host caller with credentials: %+v", forgotten)
	}
	if forgotten.CanonicalDomain != "backend.example" {
		t.Fatalf("the answer lost the settings beside them: %+v", forgotten)
	}
}
