package store

import (
	"database/sql"
	"errors"
	"testing"
)

// Revocation is absolute (docs/specs/remote-access.md §2). These three
// pin the store half of it: what a re-revoke reaches, what a mint may
// write, and what a rotation or confirmation may keep alive.

// TestRevokeDeviceReSweepsAnAlreadyRevokedDevice — the incident. A session
// row that exists un-revoked for an ALREADY-revoked device was left live
// by the `moved == 0` early return, which made every later revoke a silent
// no-op and put that session out of the device surface's reach forever.
func TestRevokeDeviceReSweepsAnAlreadyRevokedDevice(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)

	if err := s.CreateSession(newTestSession("sess-first", owner, device, key, 9000)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	ids, err := s.RevokeDevice(device.ID, 2000)
	if err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	if len(ids.SessionIDs) != 1 || !ids.DeviceMoved {
		t.Fatalf("first revoke = %+v", ids)
	}

	// The straggler: an un-revoked session row for the already-revoked
	// device. Written behind CreateSession's back, because no production
	// path may produce one any more — but a database written by a build
	// that predates that gate can hold one, and reaching it is the whole
	// point of re-sweeping.
	if err := s.CreateSession(newTestSession("sess-straggler", owner, device, key, 9000)); err == nil {
		t.Fatal("CreateSession minted a session for a revoked device")
	}
	if _, err := s.db.Exec(
		`INSERT INTO sessions (id, user_id, device_id, binding_class, scopes,
			signing_key_id, created_at, expires_at, revoked_at, last_seen_at, activated_at)
		 VALUES ('sess-straggler', ?, ?, 'device-bound', '["threads:read"]', ?, 1000, 9000, NULL, 0, 1000)`,
		owner.ID, device.ID, key.ID); err != nil {
		t.Fatalf("stage the straggler: %v", err)
	}
	again, err := s.RevokeDevice(device.ID, 3000)
	if err != nil {
		t.Fatalf("second RevokeDevice: %v", err)
	}
	if len(again.SessionIDs) != 1 || again.SessionIDs[0] != "sess-straggler" {
		t.Fatalf("re-revoke swept %v, want [sess-straggler]", again.SessionIDs)
	}
	if again.DeviceMoved {
		t.Fatal("re-revoke reported the device row as having moved twice")
	}
	got, err := s.GetSession("sess-straggler")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.RevokedAt == 0 {
		t.Fatal("the straggler is still live after a second device revocation")
	}
	live, err := s.ListLiveSessions(1)
	if err != nil {
		t.Fatalf("ListLiveSessions: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("a revoked device still holds live sessions: %v", live)
	}
}

// TestCreateSessionRefusesARevokedDevice — the other end: the row cannot
// be written at all. The predicate is inside the INSERT, so a revocation
// committing between a caller's device read and its mint refuses the mint
// rather than racing it.
func TestCreateSessionRefusesARevokedDevice(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	if _, err := s.RevokeDevice(device.ID, 2000); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	err := s.CreateSession(newTestSession("sess-after", owner, device, key, 9000))
	if !errors.Is(err, ErrDeviceRevoked) {
		t.Fatalf("CreateSession for a revoked device = %v, want ErrDeviceRevoked", err)
	}
	if err := s.CreateSession(newTestSession("sess-nodevice", owner, Device{ID: "no-such-device"}, key, 9000)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("CreateSession for a missing device = %v, want sql.ErrNoRows", err)
	}
}

// TestActivateAndExtendRefuseARevokedDevice — a pairing confirmation must
// not turn an inert row into a live credential, and a rotation must not
// move a window a revocation just closed. Both predicates are in the
// UPDATE, for the same reason CreateSession's is in the INSERT.
func TestActivateAndExtendRefuseARevokedDevice(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)

	pending := newTestSession("sess-pending", owner, device, key, 9000)
	pending.ActivatedAt = 0
	if err := s.CreateSession(pending); err != nil {
		t.Fatalf("CreateSession(pending): %v", err)
	}
	if err := s.CreateSession(newTestSession("sess-live", owner, device, key, 9000)); err != nil {
		t.Fatalf("CreateSession(live): %v", err)
	}
	if _, err := s.RevokeDevice(device.ID, 2000); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	// Un-revoke both rows so the ONLY thing refusing them is the device.
	if _, err := s.db.Exec(`UPDATE sessions SET revoked_at = NULL`); err != nil {
		t.Fatalf("stage the stragglers: %v", err)
	}

	moved, err := s.ActivateSession("sess-pending", 2500, 9000)
	if err != nil {
		t.Fatalf("ActivateSession: %v", err)
	}
	if moved {
		t.Fatal("ActivateSession confirmed a pairing whose device was revoked")
	}
	moved, err = s.ExtendSession("sess-live", 20000, 2500)
	if err != nil {
		t.Fatalf("ExtendSession: %v", err)
	}
	if moved {
		t.Fatal("ExtendSession renewed a revoked device's session")
	}
}

// TestSessionReadsCarryTheDeviceRevocation — every session read joins the
// device, so `Session.Live` answers the conjunction rather than half of
// it. A query that reached `sessions` without the join would fail to scan,
// which is what keeps this from being a convention.
func TestSessionReadsCarryTheDeviceRevocation(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	if err := s.CreateSession(newTestSession("sess-1", owner, device, key, 9000)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := s.RevokeDevice(device.ID, 2000); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE sessions SET revoked_at = NULL WHERE id = 'sess-1'`); err != nil {
		t.Fatalf("stage the straggler: %v", err)
	}

	got, err := s.GetSession("sess-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.DeviceRevokedAt != 2000 {
		t.Fatalf("GetSession deviceRevokedAt = %d, want 2000", got.DeviceRevokedAt)
	}
	if got.Live(1) {
		t.Fatal("a session whose device is revoked reported itself live")
	}
	if got.AwaitingConfirmation() {
		t.Fatal("a revoked device has no confirmation to await")
	}
	listed, err := s.ListSessionsForDevice(device.ID)
	if err != nil {
		t.Fatalf("ListSessionsForDevice: %v", err)
	}
	if len(listed) != 1 || listed[0].DeviceRevokedAt != 2000 {
		t.Fatalf("ListSessionsForDevice lost the device stamp: %+v", listed)
	}
	live, err := s.ListLiveSessions(1)
	if err != nil {
		t.Fatalf("ListLiveSessions: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("ListLiveSessions returned a revoked device's session: %v", live)
	}
}
