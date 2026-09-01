package app

import (
	"net"
	"net/url"
	"testing"

	"agent-overflow/internal/network"
	"agent-overflow/internal/settings"
)

// The listen-port half of Settings → Network: a port the operator names,
// applied through the same Rebind the bind toggle uses, with the
// transport-port cache kept naming where the listener actually is.

// freeLoopbackPort binds and immediately releases a port, yielding a
// number that is very likely still free on the next bind.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe for a free port: %v", err)
	}
	port := portFromAddr(listener.Addr().String())
	if err := listener.Close(); err != nil {
		t.Fatalf("release probe listener: %v", err)
	}
	return port
}

func TestSetNetworkSettings_MovesTheListenerToTheChosenPort(t *testing.T) {
	app, srv := newNetworkTestApp(t)
	before := portFromAddr(srv.Addr())
	chosen := freeLoopbackPort(t)
	if chosen == before {
		t.Skip("the probe port collided with the bound one; nothing to move")
	}

	got, err := app.SetNetworkSettings(network.Settings{ListenPort: chosen})
	if err != nil {
		t.Fatalf("SetNetworkSettings: %v", err)
	}
	if got.ListenPort != chosen {
		t.Fatalf("ListenPort = %d, want %d", got.ListenPort, chosen)
	}
	if bound := portFromAddr(srv.Addr()); bound != chosen {
		t.Fatalf("the listener is on port %d, want %d", bound, chosen)
	}
	if persisted := app.settings.Get().Network.ListenPort; persisted != chosen {
		t.Fatalf("persisted listenPort = %d, want %d", persisted, chosen)
	}
	// The share URL is recomputed from the listener, so it names the new
	// port with no second read.
	parsed, err := url.Parse(got.URL)
	if err != nil {
		t.Fatalf("parse share URL %q: %v", got.URL, err)
	}
	if parsed.Port() != intToPortString(chosen) {
		t.Fatalf("share URL %q does not name the new port %d", got.URL, chosen)
	}
}

// The port and the bind host are one address, so changing both is one
// rebind rather than two.
func TestSetNetworkSettings_ChangesPortAndBindTogether(t *testing.T) {
	app, srv := newNetworkTestApp(t)
	chosen := freeLoopbackPort(t)

	if _, err := app.SetNetworkSettings(network.Settings{BindAll: true, ListenPort: chosen}); err != nil {
		t.Fatalf("SetNetworkSettings: %v", err)
	}
	host, port, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	if host != "0.0.0.0" && host != "::" {
		t.Fatalf("bind host = %q, want the wide bind", host)
	}
	if port != intToPortString(chosen) {
		t.Fatalf("port = %s, want %d", port, chosen)
	}
}

// Clearing the port back to automatic does NOT move the listener. It is
// already on a port, every share URL and every paired device's stored
// endpoint names it, and jumping somewhere else to express "no
// preference" would break all of them to change nothing.
func TestSetNetworkSettings_ClearingThePortLeavesTheListenerAlone(t *testing.T) {
	app, srv := newNetworkTestApp(t)
	chosen := freeLoopbackPort(t)
	if _, err := app.SetNetworkSettings(network.Settings{ListenPort: chosen}); err != nil {
		t.Fatalf("SetNetworkSettings pinning: %v", err)
	}

	got, err := app.SetNetworkSettings(network.Settings{ListenPort: 0})
	if err != nil {
		t.Fatalf("SetNetworkSettings clearing: %v", err)
	}
	if got.ListenPort != 0 {
		t.Fatalf("ListenPort = %d, want 0 after clearing", got.ListenPort)
	}
	if bound := portFromAddr(srv.Addr()); bound != chosen {
		t.Fatalf("clearing the port moved the listener from %d to %d", chosen, bound)
	}
}

// The transport-port cache is the executable's record of the previous
// bind. Both ways it could go stale are this call, so both are covered:
// pinning a new port, and clearing the pin (where the cache would
// otherwise still hold a number from before the operator ever set one,
// and the next boot would silently move there).
func TestSetNetworkSettings_RecordsTheBoundPortWhenTheOperatorTouchesIt(t *testing.T) {
	app, srv := newNetworkTestApp(t)
	var recorded []int
	app.boundPortRecorder = func(port int) { recorded = append(recorded, port) }

	chosen := freeLoopbackPort(t)
	if chosen == portFromAddr(srv.Addr()) {
		t.Skip("the probe port collided with the bound one; nothing to move")
	}
	if _, err := app.SetNetworkSettings(network.Settings{ListenPort: chosen}); err != nil {
		t.Fatalf("SetNetworkSettings pinning: %v", err)
	}
	if len(recorded) != 1 || recorded[0] != chosen {
		t.Fatalf("recorded = %v, want one entry naming the new port %d", recorded, chosen)
	}

	if _, err := app.SetNetworkSettings(network.Settings{ListenPort: 0}); err != nil {
		t.Fatalf("SetNetworkSettings clearing: %v", err)
	}
	if len(recorded) != 2 || recorded[1] != chosen {
		t.Fatalf("recorded = %v, want the clear to record where the listener stayed (%d)", recorded, chosen)
	}
}

// A save that did not touch the port writes nothing to the cache: the
// listener has not moved, so there is nothing new to record.
func TestSetNetworkSettings_LeavesTheCacheAloneForAnUnrelatedSave(t *testing.T) {
	app, _ := newNetworkTestApp(t)
	var recorded []int
	app.boundPortRecorder = func(port int) { recorded = append(recorded, port) }

	if _, err := app.SetNetworkSettings(network.Settings{CanonicalDomain: "ao.example.com"}); err != nil {
		t.Fatalf("SetNetworkSettings: %v", err)
	}
	if len(recorded) != 0 {
		t.Fatalf("recorded = %v on a save that never touched the port", recorded)
	}
}

// A port already held by something else fails the rebind, and the whole
// undo runs: the transport never moved (Rebind is state-intact) and the
// settings file is rolled back so the screen's next read describes the
// listener that is actually there.
func TestSetNetworkSettings_PortRebindFailureRollsBack(t *testing.T) {
	app, srv := newNetworkTestApp(t)
	before := portFromAddr(srv.Addr())

	squatter, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy a port: %v", err)
	}
	defer squatter.Close()
	taken := portFromAddr(squatter.Addr().String())

	var recorded []int
	app.boundPortRecorder = func(port int) { recorded = append(recorded, port) }

	if _, err := app.SetNetworkSettings(network.Settings{ListenPort: taken}); err == nil {
		t.Fatal("SetNetworkSettings accepted a port another process holds")
	}
	if bound := portFromAddr(srv.Addr()); bound != before {
		t.Fatalf("the listener moved to %d on a failed rebind, want it left on %d", bound, before)
	}
	if persisted := app.settings.Get().Network.ListenPort; persisted != 0 {
		t.Fatalf("persisted listenPort = %d after a failed rebind, want the rolled-back 0", persisted)
	}
	if len(recorded) != 0 {
		t.Fatalf("recorded = %v after a failed rebind", recorded)
	}
}

// The refusal happens in settings validation, before anything is written
// or rebound: an out-of-range number is not a port the transport should
// ever be asked for.
func TestSetNetworkSettings_RefusesAnUnusablePort(t *testing.T) {
	app, srv := newNetworkTestApp(t)
	before := portFromAddr(srv.Addr())

	for _, port := range []int{-1, settings.MaxListenPort + 1} {
		if _, err := app.SetNetworkSettings(network.Settings{ListenPort: port}); err == nil {
			t.Errorf("SetNetworkSettings accepted listenPort %d", port)
		}
	}
	if bound := portFromAddr(srv.Addr()); bound != before {
		t.Fatalf("a refused port still moved the listener from %d to %d", before, bound)
	}
}

// A nil recorder is every fixture and every boot that resolved no config
// directory. Touching the port there must not panic.
func TestSetNetworkSettings_ToleratesNoBoundPortRecorder(t *testing.T) {
	app, _ := newNetworkTestApp(t)
	app.boundPortRecorder = nil

	if _, err := app.SetNetworkSettings(network.Settings{ListenPort: freeLoopbackPort(t)}); err != nil {
		t.Fatalf("SetNetworkSettings with no recorder installed: %v", err)
	}
}
