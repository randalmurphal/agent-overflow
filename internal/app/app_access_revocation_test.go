package app

import (
	"testing"

	"agent-overflow/internal/identity"
)

// Revocation is absolute (docs/specs/remote-access.md §2). These drive the
// real pairing exchange through the App fixtures and then ask every
// surface a paired device reaches — the per-request resolver, the per-RPC
// liveness hook, the scope gate, and the RPC that performs the revoke —
// what it answers afterwards.
//
// The incident: a paired browser was revoked, kept full access, and every
// later revoke was a silent no-op.

// TestRevokedDeviceIsRefusedByEveryHook — one revoke, and the session it
// held stops resolving on all three transport hooks at once. They share
// Sessions.Live, which is exactly why there is nothing to check per hook.
func TestRevokedDeviceIsRefusedByEveryHook(t *testing.T) {
	app := accessApp(t)
	const thumbprint = "thumb-revoked-hooks"
	grant := pairedDeviceGrant(t, app, thumbprint)
	session, err := app.store.GetSession(grant.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}

	if !SessionLive(app, session.ID) {
		t.Fatal("a freshly paired session is not live")
	}
	if scopes, refusal := SessionScopes(app, session.ID); refusal != "" || len(scopes) == 0 {
		t.Fatalf("SessionScopes before revoke = %v / %q", scopes, refusal)
	}
	if _, ok := SessionForRequest(app, requestWith(t, grant.Credential, thumbprint)); !ok {
		t.Fatal("a live paired credential was refused")
	}

	revoked, err := app.RevokeAccessDevice(session.DeviceID)
	if err != nil {
		t.Fatalf("RevokeAccessDevice: %v", err)
	}
	if !revoked.DeviceMoved || revoked.SessionsEnded != 1 {
		t.Fatalf("RevokeAccessDevice = %+v, want the device and its one session", revoked)
	}

	if SessionLive(app, session.ID) {
		t.Fatal("SessionLive still admits a revoked device's session")
	}
	if _, refusal := SessionScopes(app, session.ID); refusal != identity.ReasonRevokedSession.Code() {
		t.Fatalf("SessionScopes after revoke = %q, want revoked_session", refusal)
	}
	if _, ok := SessionForRequest(app, requestWith(t, grant.Credential, thumbprint)); ok {
		t.Fatal("SessionForRequest still resolves a revoked device's credential")
	}
}

// TestReRevokingADeviceStillSweepsAndCloses — the doctrine RevokeSession
// already carried, now on the device path. A second revoke is not a no-op:
// it re-sweeps whatever is un-revoked and closes whatever is open, because
// a credential that survived a first revocation is exactly the one worth
// reaching. The early return that made it a no-op is the incident.
func TestReRevokingADeviceStillSweepsAndCloses(t *testing.T) {
	app := accessApp(t)
	conns := &recordingConns{}
	AttachSessionConns(app, conns)
	device, first := pairDevice(t, app, "A browser", "thumb-re-revoke")

	revoked, err := app.RevokeAccessDevice(device.ID)
	if err != nil {
		t.Fatalf("first RevokeAccessDevice: %v", err)
	}
	if !revoked.DeviceMoved || revoked.SessionsEnded != 1 || revoked.ConnectionsClosed != 1 {
		t.Fatalf("first revoke = %+v, want the device, 1 session, 1 connection", revoked)
	}
	if !conns.sawClose(first.ID) {
		t.Fatal("the first revoke left the device's socket open")
	}

	// A second revoke of an already-revoked device: nothing left to sweep,
	// and the call says so instead of reporting the same success the first
	// one did.
	again, err := app.RevokeAccessDevice(device.ID)
	if err != nil {
		t.Fatalf("second RevokeAccessDevice: %v", err)
	}
	if again.DeviceMoved || again.SessionsEnded != 0 || again.ConnectionsClosed != 0 {
		t.Fatalf("second revoke = %+v, want nothing moved", again)
	}
}

// TestARevokedDeviceCannotRegainAccess walks the ways back in a person
// would actually try, in order. None may work while the device row is
// revoked; restoring it is the one deliberate remedy, and even that hands
// back no credential.
func TestARevokedDeviceCannotRegainAccess(t *testing.T) {
	app := accessApp(t)
	device, session := pairDevice(t, app, "A browser", "thumb-regain")
	if _, err := app.RevokeAccessDevice(device.ID); err != nil {
		t.Fatalf("RevokeAccessDevice: %v", err)
	}
	state := app.identityState()

	// Re-pair with the same key.
	_, token := mintLink(t, app, identity.DeviceBrowser)
	if _, reason := state.sessions.RedeemPairing(identity.RedemptionRequest{
		Token: token, Proof: identity.DeviceProof{Value: "thumb-regain"},
	}); reason != identity.ReasonRevokedDevice {
		t.Fatalf("re-pairing a revoked device = %s, want revoked_device", reason)
	}

	// Mint a fresh session straight onto the device row.
	if _, _, err := state.sessions.Mint(identity.MintRequest{
		UserID: device.UserID, DeviceID: device.ID,
		BindingClass: identity.BindingDeviceBound, Scopes: identity.Scopes,
		TTL: identity.PairingConfirmWindow,
	}); err == nil {
		t.Fatal("Mint issued a fresh credential on a revoked device row")
	}

	// Restore re-admits the KEY to pairing and moves no credential, so the
	// session the revoke ended stays ended.
	if err := app.RestoreAccessDevice(device.ID); err != nil {
		t.Fatalf("RestoreAccessDevice: %v", err)
	}
	if SessionLive(app, session.ID) {
		t.Fatal("restoring the device brought its revoked session back")
	}
}
