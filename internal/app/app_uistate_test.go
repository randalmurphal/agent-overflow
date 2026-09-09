package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/identity"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// clientCtx is the context a bound method sees on a connection that named
// no session and declared the given screen — every launch-credential client
// (the harness CLI, the e2e rig, a `--connect` stub with no forwarded
// credential).
func clientCtx(deviceID string) context.Context {
	ctx, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{
		Client: transport.ClientIdentity{DeviceID: deviceID},
	})
	return ctx
}

// sessionCtx is the context a bound method sees on a connection that
// presented a durable session, optionally alongside a declared screen.
func sessionCtx(sessionID, clientDeviceID string) context.Context {
	ctx, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{
		Client:    transport.ClientIdentity{DeviceID: clientDeviceID},
		SessionID: sessionID,
	})
	return ctx
}

// pairDevice drives one BROWSER-class device through the whole pairing
// exchange the way the device side does.
func pairDevice(t *testing.T, app *App, label, thumbprint string) (store.Device, store.Session) {
	t.Helper()
	return pairDeviceOfClass(t, app, identity.DeviceBrowser, label, thumbprint)
}

// pairDeviceOfClass is pairDevice with the device class named — mint, redeem
// with a key thumbprint, confirm — returning the live device and session rows
// it produced. The class is a parameter because it is what the settings
// device tier resolves class defaults from
// (internal/settings/classdefaults.go), so a test about a phone has to be
// able to pair one.
func pairDeviceOfClass(
	t *testing.T, app *App, class identity.DeviceClass, label, thumbprint string,
) (store.Device, store.Session) {
	t.Helper()
	state := app.identityState()
	if state == nil {
		t.Fatal("identity is not wired")
	}
	link, err := state.sessions.MintPairingLink(identity.PairingRequest{
		UserID:       state.owner.ID,
		DeviceClass:  class,
		BindingClass: identity.BindingDeviceBound,
		Scopes:       identity.Scopes,
	})
	if err != nil {
		t.Fatalf("MintPairingLink: %v", err)
	}
	redemption, reason := state.sessions.RedeemPairing(identity.RedemptionRequest{
		Token: link.Token, Proof: identity.DeviceProof{Value: thumbprint}, Label: label, Platform: "linux",
	})
	if reason.Refused() {
		t.Fatalf("RedeemPairing: %s", reason.Code())
	}
	if _, err := state.sessions.ConfirmPairing(link.Link.ID); err != nil {
		t.Fatalf("ConfirmPairing: %v", err)
	}
	device, err := app.store.GetDevice(redemption.DeviceID)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	session, err := app.store.GetSession(redemption.Tokens.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	return device, session
}

// localChannelSession is the session this backend mints for its own page
// channel, which every local client presents.
func localChannelSession(t *testing.T, app *App) store.Session {
	t.Helper()
	state := app.identityState()
	session, _, err := state.sessions.EnsureLocalChannelSession(state.owner.ID)
	if err != nil {
		t.Fatalf("EnsureLocalChannelSession: %v", err)
	}
	return session
}

func TestValidClientID(t *testing.T) {
	valid := []string{
		"11111111-2222-3333-4444-555555555555", // uuid shape (Go + crypto.randomUUID)
		"abcd1234",                             // minimum length
		strings.Repeat("a", 64),                // maximum length
	}
	for _, id := range valid {
		if !validClientID(id) {
			t.Errorf("validClientID(%q) = false, want true", id)
		}
	}

	invalid := []string{
		"",
		"short",                  // under 8 chars
		strings.Repeat("a", 65),  // over 64 chars
		"has space in the id",    // space
		"under_score-in-here-ok", // underscore not in charset
		"client:injected-scope",  // colon must never reach scope building
		"../../etc/passwd-xxxx",  // path-ish chars
	}
	for _, id := range invalid {
		if validClientID(id) {
			t.Errorf("validClientID(%q) = true, want false", id)
		}
	}
}

// TestUIStateScope_PairedSessionGetsTheDeviceBucket — a paired device is
// the unit device-tier state is scoped to, and the screen's declared client
// id must not move that bucket.
func TestUIStateScope_PairedSessionGetsTheDeviceBucket(t *testing.T) {
	app := identityApp(t)
	device, session := pairDevice(t, app, "A browser", "thumb-browser")

	if err := app.store.SetUIState("device:"+device.ID, map[string]string{"sidebar:width": "312"}); err != nil {
		t.Fatalf("seed the device bucket: %v", err)
	}
	if err := app.store.SetUIState("client:aaaaaaaa-1111-2222-3333-444444444444", map[string]string{"sidebar:width": "999"}); err != nil {
		t.Fatalf("seed the client bucket: %v", err)
	}

	// Two screens declaring different client ids on the SAME session read
	// the device bucket, never the bucket their declared id would name.
	for _, clientID := range []string{"aaaaaaaa-1111-2222-3333-444444444444", "bbbbbbbb-1111-2222-3333-444444444444"} {
		got, err := app.GetUIState(sessionCtx(session.ID, clientID))
		if err != nil {
			t.Fatalf("GetUIState as %s: %v", clientID, err)
		}
		if got["sidebar:width"] != "312" {
			t.Fatalf("screen %s read %v, want the device bucket", clientID, got)
		}
	}
}

// TestUIStateScope_LocalChannelKeepsPerScreenBuckets — the local channel
// session names the BACKEND's own channel, not one screen, so two screens
// sharing it must keep two buckets.
func TestUIStateScope_LocalChannelKeepsPerScreenBuckets(t *testing.T) {
	app := identityApp(t)
	session := localChannelSession(t, app)
	const screenA = "11111111-2222-3333-4444-555555555555"
	const screenB = "99999999-8888-7777-6666-555555555555"

	for scope, width := range map[string]string{
		"client:" + screenA:          "312",
		"client:" + screenB:          "250",
		"device:" + session.DeviceID: "100",
	} {
		if err := app.store.SetUIState(scope, map[string]string{"sidebar:width": width}); err != nil {
			t.Fatalf("seed %s: %v", scope, err)
		}
	}

	gotA, err := app.GetUIState(sessionCtx(session.ID, screenA))
	if err != nil {
		t.Fatalf("GetUIState A: %v", err)
	}
	if gotA["sidebar:width"] != "312" {
		t.Fatalf("screen A read %v, want 312", gotA)
	}
	gotB, err := app.GetUIState(sessionCtx(session.ID, screenB))
	if err != nil {
		t.Fatalf("GetUIState B: %v", err)
	}
	if gotB["sidebar:width"] != "250" {
		t.Fatalf("screen B read %v, want 250", gotB)
	}
}

// TestUIStateScope_RevokedSessionIsRefused — falling through to the client
// scope would hand a revoked device a working bucket by ignoring the
// credential it presented.
func TestUIStateScope_RevokedSessionIsRefused(t *testing.T) {
	app := identityApp(t)
	_, session := pairDevice(t, app, "A browser", "thumb-browser")
	if _, err := app.identityState().sessions.RevokeSession(session.ID); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}

	ctx := sessionCtx(session.ID, "11111111-2222-3333-4444-555555555555")
	if _, err := app.GetUIState(ctx); err == nil {
		t.Fatal("GetUIState on a revoked session: want error, got nil")
	}
}

// TestUIStateScope_AnonymousConnectionHasNoBucket — no session and no
// declared screen is an error. An anonymous bucket would be one every
// anonymous connection shares.
func TestUIStateScope_AnonymousConnectionHasNoBucket(t *testing.T) {
	app := identityApp(t)
	for _, ctx := range []context.Context{
		context.Background(), // not a transport connection at all
		clientCtx(""),        // a connection that declared nothing
		clientCtx("client:injected-scope"),
	} {
		if _, err := app.GetUIState(ctx); err == nil {
			t.Fatal("GetUIState with no identity: want error, got nil")
		}
	}
}

// TestUIStateBindings_ReadsOnlyTheCallersBucket covers the
// launch-credential path: no session, a declared screen, today's buckets.
func TestUIStateBindings_ReadsOnlyTheCallersBucket(t *testing.T) {
	app := newTestAppWithStore(t)
	const clientA = "11111111-2222-3333-4444-555555555555"
	const clientB = "99999999-8888-7777-6666-555555555555"

	if err := app.store.SetUIState("client:"+clientA, map[string]string{"sidebar:width": "312"}); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if err := app.store.SetUIState("client:"+clientB, map[string]string{"sidebar:width": "250"}); err != nil {
		t.Fatalf("seed B: %v", err)
	}

	got, err := app.GetUIState(clientCtx(clientA))
	if err != nil {
		t.Fatalf("GetUIState: %v", err)
	}
	if got["sidebar:width"] != "312" {
		t.Fatalf("client A bucket = %v, want sidebar:width=312", got)
	}
	gotB, err := app.GetUIState(clientCtx(clientB))
	if err != nil {
		t.Fatalf("GetUIState B: %v", err)
	}
	if gotB["sidebar:width"] != "250" {
		t.Fatalf("client B bucket = %v, want sidebar:width=250", gotB)
	}
}

func TestMigrateUIStateFromSettings_MovesLegacyKeysOnce(t *testing.T) {
	app := newTestAppWithStore(t)
	configDir := t.TempDir()

	legacy := `{
		"theme": "dark",
		"paneLayout": {"version":1,"panes":[{"paneId":"p1","threadId":"t1","ratio":1}]},
		"collapsedProjects": ["proj-a","proj-b"]
	}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(legacy), 0o600); err != nil {
		t.Fatalf("write settings.json: %v", err)
	}

	migrateUIStateFromSettings(configDir, app.store)

	clientID := EnsureClientIDIn(configDir)
	if clientID == "" {
		t.Fatal("ensureClientIDIn returned empty id")
	}
	bucket, err := app.store.GetUIState("client:" + clientID)
	if err != nil {
		t.Fatalf("GetUIState: %v", err)
	}
	if !strings.Contains(bucket["paneLayout"], `"paneId":"p1"`) {
		t.Fatalf("paneLayout not migrated: %q", bucket["paneLayout"])
	}
	if !strings.Contains(bucket["sidebar:collapsedProjects"], "proj-a") {
		t.Fatalf("collapsedProjects not migrated: %q", bucket["sidebar:collapsedProjects"])
	}
	// theme is a real settings field, not view state — must not move.
	if _, ok := bucket["theme"]; ok {
		t.Fatal("theme leaked into the ui_state bucket")
	}

	// Re-running (settings.json still holds the stale keys until its
	// next sparse save) must not overwrite newer bucket values.
	if err := app.store.SetUIState("client:"+clientID, map[string]string{
		"sidebar:collapsedProjects": `["only-c"]`,
	}); err != nil {
		t.Fatalf("SetUIState: %v", err)
	}
	migrateUIStateFromSettings(configDir, app.store)
	bucket, err = app.store.GetUIState("client:" + clientID)
	if err != nil {
		t.Fatalf("GetUIState after rerun: %v", err)
	}
	if bucket["sidebar:collapsedProjects"] != `["only-c"]` {
		t.Fatalf("rerun clobbered newer bucket value: %q", bucket["sidebar:collapsedProjects"])
	}
}

func TestMigrateUIStateFromSettings_NoFileIsNoOp(t *testing.T) {
	app := newTestAppWithStore(t)
	configDir := t.TempDir()

	migrateUIStateFromSettings(configDir, app.store)

	clientID := EnsureClientIDIn(configDir)
	bucket, err := app.store.GetUIState("client:" + clientID)
	if err != nil {
		t.Fatalf("GetUIState: %v", err)
	}
	if len(bucket) != 0 {
		t.Fatalf("bucket = %v, want empty when settings.json is absent", bucket)
	}
}

func TestUIStateBindings_NilStore(t *testing.T) {
	app := &App{}
	ctx := clientCtx("11111111-2222-3333-4444-555555555555")
	if _, err := app.GetUIState(ctx); err == nil {
		t.Fatal("GetUIState with nil store: want error, got nil")
	}
}
