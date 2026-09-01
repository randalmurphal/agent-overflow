package app

import (
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/network"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/tailnet"
)

// The app-side half of the tailnet feature: the persisted preference, the
// status the settings screen reads, and the one act that deletes the
// node's identity. Bringing a node UP is exercised in internal/tailnet
// against an in-process coordination server; nothing here touches a
// network, and the cases below are the reasons why it does not have to.

// newTailnetTestApp is newNetworkTestApp plus the config root the
// reconciler would have been handed at boot, without starting its
// goroutine — every case here calls the reconciler directly, so a loop
// racing it would only make failures intermittent.
func newTailnetTestApp(t *testing.T) (*App, string) {
	t.Helper()
	app, _ := newNetworkTestApp(t)
	root := t.TempDir()
	app.tailnet.mu.Lock()
	app.tailnet.dir = root
	app.tailnet.mu.Unlock()
	return app, root
}

// TestTailnetIsOffByDefaultAndBuildsNothing is the opt-in property read
// from the outside: a fresh install reports the feature off, and a
// reconcile pass over that state constructs nothing and writes nothing.
func TestTailnetIsOffByDefaultAndBuildsNothing(t *testing.T) {
	app, root := newTailnetTestApp(t)

	got, err := app.GetNetworkSettings()
	if err != nil {
		t.Fatalf("GetNetworkSettings: %v", err)
	}
	if got.TailnetEnabled || got.TailnetControlURL != "" {
		t.Fatalf("a fresh install reports tailnetEnabled=%v controlUrl=%q", got.TailnetEnabled, got.TailnetControlURL)
	}
	if got.Tailnet.Running || got.Tailnet.State != "" || got.Tailnet.HasState {
		t.Fatalf("a fresh install reports tailnet status %+v", got.Tailnet)
	}
	if got.Tailnet.URL != "" {
		t.Fatalf("a URL is published for a node that does not exist: %q", got.Tailnet.URL)
	}
	if got.Tailnet.IPs == nil {
		t.Error("tailnet.ips serialises as null; the screen renders a list and should not coalesce one per read")
	}

	app.reconcileTailnet()

	if _, err := os.Stat(tailnet.StateDir(root)); !os.IsNotExist(err) {
		t.Errorf("a reconcile pass with the feature off created state (stat error %v)", err)
	}
	app.tailnet.mu.Lock()
	node := app.tailnet.node
	app.tailnet.mu.Unlock()
	if node != nil {
		t.Error("a reconcile pass with the feature off constructed a node")
	}
}

// TestSetNetworkSettingsCarriesTheTailnetPreference pins that the toggle
// rides the existing step-up-gated write rather than a second RPC, and
// comes back on the read the screen polls.
func TestSetNetworkSettingsCarriesTheTailnetPreference(t *testing.T) {
	app, _ := newTailnetTestApp(t)

	saved, err := app.SetNetworkSettings(network.Settings{
		TailnetEnabled:    true,
		TailnetControlURL: "https://headscale.example/",
	})
	if err != nil {
		t.Fatalf("SetNetworkSettings: %v", err)
	}
	if !saved.TailnetEnabled || saved.TailnetControlURL != "https://headscale.example/" {
		t.Fatalf("the write answered %+v", saved)
	}

	read, err := app.GetNetworkSettings()
	if err != nil {
		t.Fatalf("GetNetworkSettings: %v", err)
	}
	if !read.TailnetEnabled || read.TailnetControlURL != "https://headscale.example/" {
		t.Fatalf("the read answered tailnetEnabled=%v controlUrl=%q", read.TailnetEnabled, read.TailnetControlURL)
	}
	if stored := app.settings.Get().Network; !stored.WantsTailnet() {
		t.Fatal("the preference did not reach the settings file")
	}

	// An unusable coordination server is refused at the write, where the
	// person who typed it is still looking at it.
	if _, err := app.SetNetworkSettings(network.Settings{
		TailnetEnabled:    true,
		TailnetControlURL: "headscale.example",
	}); err == nil {
		t.Fatal("SetNetworkSettings accepted a control URL with no scheme")
	}
	if stored := app.settings.Get().Network; stored.TailnetControlURL != "https://headscale.example/" {
		t.Fatalf("a refused write changed the stored control URL to %q", stored.TailnetControlURL)
	}
}

// TestForgetTailnetNodeRefusesWhileEnabled is the ordering rule. Deleting
// the identity under a live node leaves this process holding one nothing
// on disk records, and the owner's admin panel showing a device with no
// way back.
func TestForgetTailnetNodeRefusesWhileEnabled(t *testing.T) {
	app, root := newTailnetTestApp(t)
	seedTailnetState(t, root)

	if _, err := app.settings.SetNetwork(settings.NetworkSettings{TailnetEnabled: true}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, err := app.ForgetTailnetNode(); err == nil {
		t.Fatal("ForgetTailnetNode ran while the feature was enabled")
	}
	if _, err := os.Stat(tailnet.StateDir(root)); err != nil {
		t.Fatalf("the refused call still removed state: %v", err)
	}
}

// TestForgetTailnetNodeRemovesTheIdentityOnceDisabled covers the act
// itself, and the status field that makes it offerable only when there is
// something to forget.
func TestForgetTailnetNodeRemovesTheIdentityOnceDisabled(t *testing.T) {
	app, root := newTailnetTestApp(t)
	seedTailnetState(t, root)

	before, err := app.GetNetworkSettings()
	if err != nil {
		t.Fatalf("GetNetworkSettings: %v", err)
	}
	if !before.Tailnet.HasState {
		t.Fatal("a node identity on disk is not reported, so the forget affordance would never appear")
	}

	after, err := app.ForgetTailnetNode()
	if err != nil {
		t.Fatalf("ForgetTailnetNode: %v", err)
	}
	if after.Tailnet.HasState {
		t.Error("the status still reports an identity after it was removed")
	}
	if _, err := os.Stat(tailnet.StateDir(root)); !os.IsNotExist(err) {
		t.Errorf("the state directory survived the forget (stat error %v)", err)
	}

	// Idempotent, so the button does not have to be disabled the instant
	// it is pressed.
	if _, err := app.ForgetTailnetNode(); err != nil {
		t.Errorf("a second forget: %v", err)
	}
}

// TestTailnetFailuresAreUserFacingState pins principle 5 for this
// feature: a bring-up that could not happen is carried on the status the
// screen renders, verbatim, and cleared by the next settled pass.
func TestTailnetFailuresAreUserFacingState(t *testing.T) {
	app, _ := newNetworkTestApp(t)
	// No config root, which is the one bring-up failure reachable without
	// a network: there is nowhere to keep the node's identity.
	if _, err := app.settings.SetNetwork(settings.NetworkSettings{TailnetEnabled: true}); err != nil {
		t.Fatalf("enable: %v", err)
	}

	app.reconcileTailnet()

	got, err := app.GetNetworkSettings()
	if err != nil {
		t.Fatalf("GetNetworkSettings: %v", err)
	}
	if got.Tailnet.LastError == "" {
		t.Fatal("a failed bring-up left no user-facing error")
	}

	// The backoff climbs with consecutive failures and stops at the
	// ceiling, so a control server that is down does not become a busy
	// loop.
	first := app.tailnetRetryDelay()
	for range 20 {
		app.reconcileTailnet()
	}
	last := app.tailnetRetryDelay()
	if last <= first {
		t.Errorf("the retry delay did not grow with repeated failures: %v then %v", first, last)
	}
	if last > tailnetRetryCeiling {
		t.Errorf("the retry delay passed its ceiling: %v", last)
	}

	// Turning the feature off settles it: there is nothing left to fail.
	if _, err := app.settings.SetNetwork(settings.NetworkSettings{}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	app.reconcileTailnet()
	got, err = app.GetNetworkSettings()
	if err != nil {
		t.Fatalf("GetNetworkSettings: %v", err)
	}
	if got.Tailnet.LastError != "" {
		t.Errorf("the error survived a disable: %q", got.Tailnet.LastError)
	}
}

// seedTailnetState writes the shape tsnet leaves behind, so a case can
// exercise "there is an identity here" without bringing a node up.
func seedTailnetState(t *testing.T, root string) {
	t.Helper()
	dir := tailnet.StateDir(root)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("seed the state directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tailscaled.state"), []byte("node key"), 0o600); err != nil {
		t.Fatalf("seed node state: %v", err)
	}
}
