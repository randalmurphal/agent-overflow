package store

import (
	"agent-overflow/internal/owndevices"
	"database/sql"
	"encoding/base64"
	"errors"
	"testing"
)

func ownKey() string { return base64.RawURLEncoding.EncodeToString(make([]byte, 32)) }
func approvedOwnPair(t *testing.T) (*Store, PairingLink) {
	t.Helper()
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	session := newTestSession("own-session", owner, device, key, 9000)
	if e := s.CreateSession(session); e != nil {
		t.Fatal(e)
	}
	link := PairingLink{ID: "own-link", UserID: owner.ID, Scopes: []string{"threads:read"}, BindingClass: "device-bound", DeviceClass: "phone", Purpose: "own-device", CreatedAt: 1000, ExpiresAt: 9000}
	if e := s.CreatePairingLink(link, []byte("hash-own")); e != nil {
		t.Fatal(e)
	}
	if _, e := s.RedeemPairingLink([]byte("hash-own"), 2000, ownKey()); e != nil {
		t.Fatal(e)
	}
	if e := s.AttachPairingRedemption(link.ID, device.ID, session.ID); e != nil {
		t.Fatal(e)
	}
	if e := s.PrepareOwnPairing(link.ID); e != nil {
		t.Fatal(e)
	}
	link, e := s.ConfirmPairingLink(link.ID, 2100)
	if e != nil {
		t.Fatal(e)
	}
	return s, link
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
	if _, _, e = s.MergeOwnDevices([]owndevices.Member{m}, 2200, ""); e != nil {
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
	if _, _, e = s.MergeOwnDevices([]owndevices.Member{m}, 2200, ""); e == nil {
		t.Fatal("revocation failure was ignored")
	}
	if held, e := s.OwnDeviceForSession(link.SessionID); e != nil || held.Removed {
		t.Fatal("membership committed despite failed revocation", held, e)
	}
	if _, e = s.db.Exec(`DROP TRIGGER refuse_own_revocation`); e != nil {
		t.Fatal(e)
	}
	if _, ids, e := s.MergeOwnDevices([]owndevices.Member{m}, 2200, ""); e != nil || len(ids) != 1 {
		t.Fatal(ids, e)
	}
	if _, e = s.OwnDeviceForSession(link.SessionID); !errors.Is(e, sql.ErrNoRows) {
		t.Fatal("removed membership remained admitted", e)
	}
}
