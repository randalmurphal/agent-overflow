package app

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
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

// pairDevice drives one device through the whole pairing exchange the way
// the device side does — mint, redeem with a key thumbprint, confirm — and
// returns the live device and session rows it produced.
func pairDevice(t *testing.T, app *App, label, thumbprint string) (store.Device, store.Session) {
	t.Helper()
	state := app.identityState()
	if state == nil {
		t.Fatal("identity is not wired")
	}
	link, err := state.sessions.MintPairingLink(identity.PairingRequest{
		UserID:       state.owner.ID,
		DeviceClass:  identity.DeviceBrowser,
		BindingClass: identity.BindingDeviceBound,
		Scopes:       identity.Scopes,
	})
	if err != nil {
		t.Fatalf("MintPairingLink: %v", err)
	}
	redemption, reason := state.sessions.RedeemPairing(identity.RedemptionRequest{
		Token: link.Token, KeyThumbprint: thumbprint, Label: label, Platform: "linux",
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

	if err := app.SetUIState(sessionCtx(session.ID, "aaaaaaaa-1111-2222-3333-444444444444"),
		map[string]string{"sidebar:width": "312"}); err != nil {
		t.Fatalf("SetUIState: %v", err)
	}
	bucket, err := app.store.GetUIState("device:" + device.ID)
	if err != nil {
		t.Fatalf("read the device bucket: %v", err)
	}
	if bucket["sidebar:width"] != "312" {
		t.Fatalf("device bucket = %v, want the write", bucket)
	}

	// A second screen declaring a different client id on the SAME session
	// reads the same bucket: the device is the scope, not the screen.
	got, err := app.GetUIState(sessionCtx(session.ID, "bbbbbbbb-1111-2222-3333-444444444444"))
	if err != nil {
		t.Fatalf("GetUIState: %v", err)
	}
	if got["sidebar:width"] != "312" {
		t.Fatalf("second screen read %v, want the device bucket", got)
	}
	// And nothing landed in a client-keyed bucket.
	stray, err := app.store.GetUIState("client:aaaaaaaa-1111-2222-3333-444444444444")
	if err != nil {
		t.Fatalf("read the client bucket: %v", err)
	}
	if len(stray) != 0 {
		t.Fatalf("a paired session also wrote a client bucket: %v", stray)
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

	if err := app.SetUIState(sessionCtx(session.ID, screenA),
		map[string]string{"sidebar:width": "312"}); err != nil {
		t.Fatalf("SetUIState A: %v", err)
	}
	if err := app.SetUIState(sessionCtx(session.ID, screenB),
		map[string]string{"sidebar:width": "250"}); err != nil {
		t.Fatalf("SetUIState B: %v", err)
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
	if _, err := app.store.GetUIState("device:" + session.DeviceID); err != nil {
		t.Fatalf("read the channel device bucket: %v", err)
	}
	channelBucket, _ := app.store.GetUIState("device:" + session.DeviceID)
	if len(channelBucket) != 0 {
		t.Fatalf("the local channel collapsed two screens into %v", channelBucket)
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
	if err := app.SetUIState(ctx, map[string]string{"a": "1"}); err == nil {
		t.Fatal("SetUIState on a revoked session: want error, got nil")
	}
	if err := app.DeleteUIState(ctx, []string{"a"}); err == nil {
		t.Fatal("DeleteUIState on a revoked session: want error, got nil")
	}
}

// TestUIStateScope_AnonymousConnectionHasNoBucket — no session and no
// declared screen is an error on all three methods. An anonymous bucket
// would be one every anonymous connection shares.
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
		if err := app.SetUIState(ctx, map[string]string{"a": "1"}); err == nil {
			t.Fatal("SetUIState with no identity: want error, got nil")
		}
		if err := app.DeleteUIState(ctx, []string{"a"}); err == nil {
			t.Fatal("DeleteUIState with no identity: want error, got nil")
		}
	}
}

// TestUIStateBindings_RoundTripAndScopeIsolation covers the
// launch-credential path: no session, a declared screen, today's buckets.
func TestUIStateBindings_RoundTripAndScopeIsolation(t *testing.T) {
	app := newTestAppWithStore(t)
	const clientA = "11111111-2222-3333-4444-555555555555"
	const clientB = "99999999-8888-7777-6666-555555555555"

	if err := app.SetUIState(clientCtx(clientA), map[string]string{"sidebar:width": "312"}); err != nil {
		t.Fatalf("SetUIState: %v", err)
	}
	if err := app.SetUIState(clientCtx(clientB), map[string]string{"sidebar:width": "250"}); err != nil {
		t.Fatalf("SetUIState B: %v", err)
	}

	got, err := app.GetUIState(clientCtx(clientA))
	if err != nil {
		t.Fatalf("GetUIState: %v", err)
	}
	if got["sidebar:width"] != "312" {
		t.Fatalf("client A bucket = %v, want sidebar:width=312", got)
	}

	if err := app.DeleteUIState(clientCtx(clientA), []string{"sidebar:width"}); err != nil {
		t.Fatalf("DeleteUIState: %v", err)
	}
	got, err = app.GetUIState(clientCtx(clientA))
	if err != nil {
		t.Fatalf("GetUIState after delete: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("client A bucket after delete = %v, want empty", got)
	}
	// B's identical key is untouched — buckets are per-client.
	gotB, err := app.GetUIState(clientCtx(clientB))
	if err != nil {
		t.Fatalf("GetUIState B: %v", err)
	}
	if gotB["sidebar:width"] != "250" {
		t.Fatalf("client B bucket = %v, want sidebar:width=250", gotB)
	}
}

func TestUIStateBindings_RejectsOversizeInput(t *testing.T) {
	app := newTestAppWithStore(t)
	ctx := clientCtx("11111111-2222-3333-4444-555555555555")

	if err := app.SetUIState(ctx, map[string]string{
		"too-big": strings.Repeat("v", maxUIStateValueLen+1),
	}); err == nil {
		t.Fatal("SetUIState with oversize value: want error, got nil")
	}
	if err := app.SetUIState(ctx, map[string]string{
		strings.Repeat("k", maxUIStateKeyLen+1): "v",
	}); err == nil {
		t.Fatal("SetUIState with oversize key: want error, got nil")
	}

	oversizeBatch := make(map[string]string, maxUIStateBatch+1)
	for i := 0; i <= maxUIStateBatch; i++ {
		oversizeBatch["key-"+strconv.Itoa(i)] = "v"
	}
	if err := app.SetUIState(ctx, oversizeBatch); err == nil {
		t.Fatal("SetUIState with oversize batch: want error, got nil")
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
	if err := app.SetUIState(ctx, map[string]string{"a": "1"}); err == nil {
		t.Fatal("SetUIState with nil store: want error, got nil")
	}
	if err := app.DeleteUIState(ctx, []string{"a"}); err == nil {
		t.Fatal("DeleteUIState with nil store: want error, got nil")
	}
}
