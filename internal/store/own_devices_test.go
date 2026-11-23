package store

import (
	"agent-overflow/internal/owndevices"
	"database/sql"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"
)

func ownKey() string { return base64.RawURLEncoding.EncodeToString(make([]byte, 32)) }
func approvedOwnPair(t *testing.T) (*Store, PairingLink) {
	t.Helper()
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	if e := s.SetDeviceKeyThumbprint(device.ID, ownKey()); e != nil {
		t.Fatal(e)
	}
	link := PairingLink{ID: "own-link", UserID: owner.ID, Scopes: []string{"threads:read"}, BindingClass: "device-bound", DeviceClass: "phone", Purpose: "own-device", CreatedAt: 1000, ExpiresAt: 9000}
	if e := s.CreatePairingLink(link, []byte("hash-own")); e != nil {
		t.Fatal(e)
	}
	redeemOnto(t, s, []byte("hash-own"), 2000, newTestSession("own-session", owner, device, key, 9000))
	confirmed, e := s.ConfirmRedeemedPairing(link.ID, 2100, accessUntil(9000))
	if e != nil {
		t.Fatal(e)
	}
	return s, confirmed.Link
}
func TestOwnAdmissionRecoveryCannotRestoreLaterRemoval(t *testing.T) {
	s, link := approvedOwnPair(t)
	if e := s.AdmitOwnPairing(link); e != nil {
		t.Fatal(e)
	}
	m, e := s.OwnDeviceForSession(link.SessionID)
	if e != nil {
		t.Fatal(e)
	}
	// A crash after confirmation but before membership commit is equivalent to
	// this missing projection. The durable confirmation still names generation1.
	if _, e = s.db.Exec(`DELETE FROM own_device_sessions WHERE session_id=?`, link.SessionID); e != nil {
		t.Fatal(e)
	}
	if e = s.AdmitOwnPairing(link); e != nil {
		t.Fatal(e)
	}
	m.Removed = true
	if _, _, _, e = s.MergeOwnDevices([]owndevices.Member{m}, 2200, ""); e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.Exec(`DELETE FROM own_device_sessions WHERE session_id=?`, link.SessionID); e != nil {
		t.Fatal(e)
	}
	if e = s.AdmitOwnPairing(link); e == nil {
		t.Fatal("old confirmed admission restored revoked membership")
	}
}
func TestOwnMembershipAndSessionRevocationAreAtomic(t *testing.T) {
	s, link := approvedOwnPair(t)
	if e := s.AdmitOwnPairing(link); e != nil {
		t.Fatal(e)
	}
	m, e := s.OwnDevice(ownKey())
	if e != nil {
		t.Fatal(e)
	}
	m.Removed = true
	if _, e = s.db.Exec(`CREATE TRIGGER refuse_own_revocation BEFORE UPDATE OF revoked_at ON sessions BEGIN SELECT RAISE(ABORT,'disk failure'); END`); e != nil {
		t.Fatal(e)
	}
	if _, _, _, e = s.MergeOwnDevices([]owndevices.Member{m}, 2200, ""); e == nil {
		t.Fatal("revocation failure was ignored")
	}
	if held, e := s.OwnDeviceForSession(link.SessionID); e != nil || held.Removed {
		t.Fatal("membership committed despite failed revocation", held, e)
	}
	if _, e = s.db.Exec(`DROP TRIGGER refuse_own_revocation`); e != nil {
		t.Fatal(e)
	}
	if _, ids, _, e := s.MergeOwnDevices([]owndevices.Member{m}, 2200, ""); e != nil || len(ids) != 1 {
		t.Fatal(ids, e)
	}
	if _, e = s.OwnDeviceForSession(link.SessionID); !errors.Is(e, sql.ErrNoRows) {
		t.Fatal("removed membership remained admitted", e)
	}
}

// Removing this serving host from its group cancels its unconfirmed
// personal invitation with the pending session its redemption created, in
// the same transaction, and reports that session for teardown.
func TestRemovingThisHostCancelsItsPersonalInvitationWithItsPendingSession(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	link := PairingLink{ID: "own-pending", UserID: owner.ID, Scopes: []string{"threads:read"}, BindingClass: "device-bound", DeviceClass: "desktop", Purpose: "own-device", CreatedAt: 1000, ExpiresAt: 9000}
	if err := s.CreatePairingLink(link, []byte("hash-own-pending")); err != nil {
		t.Fatal(err)
	}
	_, pending := redeemOnto(t, s, []byte("hash-own-pending"), 2000, newTestSession("sess-own-pending", owner, device, key, 9000))

	const backendID = "93fd7522-831e-4401-8aa0-6dca4e65bcb0"
	hostKey := make([]byte, 32)
	hostKey[0] = 3
	host := owndevices.Member{KeyThumbprint: base64.RawURLEncoding.EncodeToString(hostKey), BackendID: backendID, Generation: 1}
	if _, _, _, err := s.MergeOwnDevices([]owndevices.Member{host}, 2100, backendID); err != nil {
		t.Fatal(err)
	}
	host.Removed = true
	_, revoked, _, err := s.MergeOwnDevices([]owndevices.Member{host}, 2200, backendID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(revoked, pending.ID) {
		t.Fatalf("removing this host revoked %v, want the invitation's pending session %s", revoked, pending.ID)
	}
	if got, err := s.GetPairingLink(link.ID); err != nil || got.CanceledAt != 2200 {
		t.Fatalf("the personal invitation = %+v (%v), want canceled", got, err)
	}
	if session, err := s.GetSession(pending.ID); err != nil || session.RevokedAt != 2200 {
		t.Fatalf("the invitation's pending session = %+v (%v), want revoked with it", session, err)
	}
}

// A cancellation never ends an activated session: an introduction admitted
// without its device ever renewing is canceled, and its session stays as the
// group's own rules leave it.
func TestCancelingAnAdmittedIntroductionKeepsItsSession(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	link := PairingLink{ID: "intro", UserID: owner.ID, Scopes: []string{"threads:read"}, BindingClass: "device-bound", DeviceClass: "desktop", Purpose: "own-introduction", CreatedAt: 1000, ExpiresAt: 9000}
	if err := s.CreatePairingLink(link, []byte("hash-intro")); err != nil {
		t.Fatal(err)
	}
	_, session := redeemOnto(t, s, []byte("hash-intro"), 2000, newTestSession("sess-intro", owner, device, key, 9000))
	mustExec(t, s.db, `UPDATE sessions SET activated_at = 2050 WHERE id = ?`, session.ID)
	mustExec(t, s.db, `UPDATE pairing_links SET confirmed_at = 2050 WHERE id = ?`, link.ID)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	canceled, err := cancelPairingLinksTx(tx, 2100, `id = ?`, link.ID)
	if err != nil || canceled.links != 1 || len(canceled.revoked) != 0 {
		t.Fatalf("canceling the admitted introduction = %+v (%v), want it canceled and no session revoked", canceled, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if after, err := s.GetSession(session.ID); err != nil || after.RevokedAt != 0 {
		t.Fatalf("the admitted introduction's session = %+v (%v), want it live", after, err)
	}
}

// A confirmed link whose session an older build never activated is settled
// at startup: its session is revoked, the link stays confirmed, and no
// personal admission is left to finish for it, whether or not the older
// build had written the membership.
func TestStartupRevokesTheUnactivatedSessionOfAConfirmedLink(t *testing.T) {
	for name, withMembership := range map[string]bool{"ordinary": false, "personal with its membership": true} {
		t.Run(name, func(t *testing.T) { startupRevokesUnactivatedSession(t, withMembership) })
	}
}

func startupRevokesUnactivatedSession(t *testing.T, withMembership bool) {
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	link := PairingLink{ID: "confirmed-pending", UserID: owner.ID, Scopes: []string{"threads:read"}, BindingClass: "device-bound", DeviceClass: "desktop", Purpose: "own-device", MemberGeneration: 1, CreatedAt: 1000, ExpiresAt: 9000}
	if err := s.CreatePairingLink(link, []byte("hash-confirmed-pending")); err != nil {
		t.Fatal(err)
	}
	_, pending := redeemOnto(t, s, []byte("hash-confirmed-pending"), 2000, newTestSession("sess-confirmed-pending", owner, device, key, 9000))
	// What an older build left when it stopped between the confirmation and
	// the activation.
	mustExec(t, s.db, `UPDATE pairing_links SET confirmed_at = 2100 WHERE id = ?`, link.ID)
	if withMembership {
		mustExec(t, s.db, `INSERT INTO own_devices (key_thumbprint, member, generation, removed) VALUES (?, '{}', 1, 0)`, ownKey())
		mustExec(t, s.db, `INSERT INTO own_device_sessions (session_id, key_thumbprint, generation) VALUES (?, ?, 1)`, pending.ID, ownKey())
	}
	if err := s.RetirePendingPairings(3000); err != nil {
		t.Fatal(err)
	}
	if session, err := s.GetSession(pending.ID); err != nil || session.RevokedAt != 3000 {
		t.Fatalf("the confirmed link's unactivated session = %+v (%v), want revoked", session, err)
	}
	if got, err := s.GetPairingLink(link.ID); err != nil || got.ConfirmedAt != 2100 || got.CanceledAt != 0 {
		t.Fatalf("the confirmed link = %+v (%v), want it confirmed and uncanceled", got, err)
	}
	incomplete, err := s.IncompleteOwnAdmissions()
	if err != nil || len(incomplete) != 0 {
		t.Fatalf("admissions left to finish = %+v (%v), want none", incomplete, err)
	}
}

// A link an older build canceled without revoking the session its
// redemption created leaves that session pending; startup revokes it and
// keeps the link's stamp.
func TestStartupRevokesThePendingSessionOfACanceledLink(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	link := PairingLink{ID: "canceled-pending", UserID: owner.ID, Scopes: []string{"threads:read"}, BindingClass: "device-bound", DeviceClass: "desktop", CreatedAt: 1000, ExpiresAt: 9000}
	if err := s.CreatePairingLink(link, []byte("hash-canceled-pending")); err != nil {
		t.Fatal(err)
	}
	_, pending := redeemOnto(t, s, []byte("hash-canceled-pending"), 2000, newTestSession("sess-canceled-pending", owner, device, key, 9000))
	mustExec(t, s.db, `UPDATE pairing_links SET canceled_at = 2100 WHERE id = ?`, link.ID)
	if err := s.RetirePendingPairings(3000); err != nil {
		t.Fatal(err)
	}
	if session, err := s.GetSession(pending.ID); err != nil || session.RevokedAt != 3000 {
		t.Fatalf("the canceled link's pending session = %+v (%v), want revoked", session, err)
	}
	if got, err := s.GetPairingLink(link.ID); err != nil || got.CanceledAt != 2100 {
		t.Fatalf("the canceled link = %+v (%v), want its stamp kept", got, err)
	}
}

// Startup's retirement reads no link row: a link whose scopes are valid JSON
// of a shape this build does not read is still canceled, with its pending
// session, and recovery goes on.
func TestStartupRetiresALinkWhoseScopesItCannotRead(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	link := PairingLink{ID: "odd-scopes", UserID: owner.ID, Scopes: []string{"threads:read"}, BindingClass: "device-bound", DeviceClass: "desktop", CreatedAt: 1000, ExpiresAt: 9000}
	if err := s.CreatePairingLink(link, []byte("hash-odd-scopes")); err != nil {
		t.Fatal(err)
	}
	_, pending := redeemOnto(t, s, []byte("hash-odd-scopes"), 2000, newTestSession("sess-odd-scopes", owner, device, key, 9000))
	mustExec(t, s.db, `UPDATE pairing_links SET scopes = '{"threads":"read"}' WHERE id = ?`, link.ID)
	if err := s.RetirePendingPairings(3000); err != nil {
		t.Fatalf("startup stopped on a link whose scopes it cannot read: %v", err)
	}
	var canceledAt int64
	if err := s.db.QueryRow(`SELECT canceled_at FROM pairing_links WHERE id = ?`, link.ID).Scan(&canceledAt); err != nil || canceledAt != 3000 {
		t.Fatalf("the link's canceled_at = %d (%v), want 3000", canceledAt, err)
	}
	if session, err := s.GetSession(pending.ID); err != nil || session.RevokedAt != 3000 {
		t.Fatalf("its pending session = %+v (%v), want revoked", session, err)
	}
}

// Startup's one sweep of pending sessions starts from the sessions never
// activated and finds each one's link through the index, so a store's
// settled link history is not read: the plan searches pairing_links and
// never scans it, and the sweep still revokes the pending session.
func TestStartupSweepReadsNoSettledLinkHistory(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	mustExec(t, s.db, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 2000)
		INSERT INTO pairing_links (id, user_id, token_hash, scopes, binding_class, device_class, created_at, expires_at, redeemed_at, confirmed_at, session_id)
		SELECT 'settled-' || i, ?, randomblob(32), '[]', 'device-bound', 'desktop', 1000, 9000, 1500, 1600, 'gone-' || i FROM n`, owner.ID)
	link := PairingLink{ID: "pending", UserID: owner.ID, Scopes: []string{"threads:read"}, BindingClass: "device-bound", DeviceClass: "desktop", CreatedAt: 1000, ExpiresAt: 9000}
	if err := s.CreatePairingLink(link, []byte("hash-pending")); err != nil {
		t.Fatal(err)
	}
	_, pending := redeemOnto(t, s, []byte("hash-pending"), 2000, newTestSession("sess-pending", owner, device, key, 9000))
	mustExec(t, s.db, `ANALYZE`)
	plan := queryPlanText(t, s.db, `EXPLAIN QUERY PLAN UPDATE sessions SET revoked_at = 1
		WHERE revoked_at IS NULL AND (`+pendingLinkSessionsSQL+`) RETURNING id, device_id`)
	if !strings.Contains(plan, "SEARCH l USING COVERING INDEX idx_pairing_links_session") || strings.Contains(plan, "SCAN pairing_links") || strings.Contains(plan, "SCAN l") {
		t.Fatalf("startup's sweep reads the link history:\n%s", plan)
	}
	if err := s.RetirePendingPairings(3000); err != nil {
		t.Fatal(err)
	}
	if session, err := s.GetSession(pending.ID); err != nil || session.RevokedAt != 3000 {
		t.Fatalf("the pending session = %+v (%v), want revoked", session, err)
	}
}
