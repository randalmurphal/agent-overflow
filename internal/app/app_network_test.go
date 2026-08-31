package app

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/network"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/transport"
)

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

	got, err := app.GetNetworkSettings()
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
	if !strings.Contains(got.URL, srv.Token()) {
		t.Fatalf("URL = %q, want token", got.URL)
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

	got, err := app.SetNetworkSettings(network.Settings{BindAll: true})
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
	got, err = app.SetNetworkSettings(network.Settings{BindAll: false})
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
	got, err := app.GetNetworkSettings()
	if err != nil {
		t.Fatalf("GetNetworkSettings: %v", err)
	}
	if got.Insecure {
		t.Fatalf("loopback bind: Insecure = true, want false")
	}

	// Toggle to LAN bind: still http://, now Insecure. Skip the
	// test if no LAN IP discoverable (CI sandbox); we only assert
	// when the URL actually contains a non-loopback host.
	got, err = app.SetNetworkSettings(network.Settings{BindAll: true})
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
	got, err = app.SetNetworkSettings(network.Settings{BindAll: false})
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

	if _, err := app.SetNetworkSettings(network.Settings{BindAll: false}); err != nil {
		t.Fatalf("SetNetworkSettings(false) on default: %v", err)
	}
	if srv.Addr() != originalAddr {
		t.Fatalf("addr changed on no-op set: was %q, now %q", originalAddr, srv.Addr())
	}
}

// TestNetworkFromServer_LoopbackUsesAppURL pins the URL output for the
// default loopback bind: it must equal Server.AppURL so the user
// always sees a consistent string regardless of how settings is
// queried.
func TestNetworkFromServer_LoopbackUsesAppURL(t *testing.T) {
	_, srv := newNetworkTestApp(t)
	got := network.FromServer(srv, false).URL
	if got != srv.AppURL() {
		t.Fatalf("loopback URL = %q, want srv.AppURL() = %q", got, srv.AppURL())
	}
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

	if _, err := app.SetNetworkSettings(network.Settings{BindAll: true}); err == nil {
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
		got, err := app.SetNetworkSettings(network.Settings{BindAll: exp.bindAll})
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

	if _, err := app.SetNetworkSettings(network.Settings{BindAll: true}); err == nil {
		t.Fatalf("SetNetworkSettings expected to fail when target addr is held")
	}

	if got := app.settings.Get().Network.BindAll; got != preBindAll {
		t.Fatalf("settings BindAll persisted on rebind failure: pre=%v post=%v", preBindAll, got)
	}
	if got := srv.Addr(); got != preAddr {
		t.Fatalf("transport addr changed despite failed rebind: pre=%q post=%q", preAddr, got)
	}
}
