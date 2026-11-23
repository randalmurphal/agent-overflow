package store

import (
	"database/sql"
	"errors"
	"testing"
)

// spentLink seeds a link redeemed at 2000 whose pending session is not yet
// written, and the pending session a redemption would write for it.
func spentLink(t *testing.T) (*Store, Session) {
	t.Helper()
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	seedPairingLink(t, s, owner, []byte("hash-1"), 9000)
	if _, err := s.RedeemPairingLink([]byte("hash-1"), 2000, "thumb"); err != nil {
		t.Fatalf("RedeemPairingLink: %v", err)
	}
	pending := newTestSession("sess-pending", owner, device, key, 9000)
	pending.ActivatedAt = 0
	return s, pending
}

func redemptionOf(session Session) PairingRedemption {
	return PairingRedemption{LinkID: "link-1", Session: session, Refresh: RefreshGrant{Hash: []byte("refresh-1"), CreatedAt: 2000, ExpiresAt: 90000}}
}

// requireNoPendingSession checks that a redemption that failed left neither
// its session, its refresh secret, nor a link naming them.
func requireNoPendingSession(t *testing.T, s *Store, session Session) {
	t.Helper()
	if got, err := s.GetSession(session.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a failed redemption left its session (%+v, %v)", got, err)
	}
	var secrets int
	if err := s.db.QueryRow(`SELECT count(*) FROM refresh_secrets WHERE session_id = ?`, session.ID).Scan(&secrets); err != nil || secrets != 0 {
		t.Fatalf("a failed redemption left %d refresh secrets (%v)", secrets, err)
	}
	if link, err := s.GetPairingLink("link-1"); err != nil || link.SessionID != "" || link.DeviceID != "" {
		t.Fatalf("a failed redemption left the link naming rows (%+v, %v)", link, err)
	}
}

// A redemption whose refresh secret cannot be written writes no session
// either.
func TestCompletePairingRedemptionWritesNothingWhenItsRefreshSecretFails(t *testing.T) {
	s, pending := spentLink(t)
	mustExec(t, s.db, `CREATE TRIGGER reject_refresh BEFORE INSERT ON refresh_secrets BEGIN SELECT RAISE(ABORT, 'test refresh failure'); END`)
	if err := s.CompletePairingRedemption(redemptionOf(pending)); err == nil {
		t.Fatal("a redemption whose refresh secret failed reported success")
	}
	requireNoPendingSession(t, s, pending)
}

// A redemption whose link cannot name it writes no session.
func TestCompletePairingRedemptionWritesNothingWhenItsAttachFails(t *testing.T) {
	s, pending := spentLink(t)
	mustExec(t, s.db, `CREATE TRIGGER reject_attach BEFORE UPDATE OF session_id ON pairing_links BEGIN SELECT RAISE(ABORT, 'test attach failure'); END`)
	if err := s.CompletePairingRedemption(redemptionOf(pending)); err == nil {
		t.Fatal("a redemption whose attach failed reported success")
	}
	requireNoPendingSession(t, s, pending)
}

// A cancellation that reaches the spent link before the redemption completes
// leaves the redemption nothing to write.
func TestCompletePairingRedemptionAfterACancellationWritesNoSession(t *testing.T) {
	s, pending := spentLink(t)
	if _, err := s.CancelPairingLink("link-1", 2100); err != nil {
		t.Fatalf("CancelPairingLink: %v", err)
	}
	if err := s.CompletePairingRedemption(redemptionOf(pending)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a redemption of a cancelled link = %v, want sql.ErrNoRows", err)
	}
	requireNoPendingSession(t, s, pending)
}

func TestCompletePairingRedemptionWritesTheSessionWithItsLink(t *testing.T) {
	s, pending := spentLink(t)
	if err := s.CompletePairingRedemption(redemptionOf(pending)); err != nil {
		t.Fatalf("CompletePairingRedemption: %v", err)
	}
	link, err := s.GetPairingLink("link-1")
	if err != nil || link.SessionID != pending.ID || link.DeviceID != pending.DeviceID || !link.Ready() {
		t.Fatalf("the link after its redemption = %+v (%v)", link, err)
	}
	if session, err := s.GetSession(pending.ID); err != nil || session.ActivatedAt != 0 {
		t.Fatalf("the redemption's session = %+v (%v), want written and unactivated", session, err)
	}
	if _, err := s.GetRefreshSecretByHash([]byte("refresh-1")); err != nil {
		t.Fatalf("the redemption's refresh secret: %v", err)
	}
}

// readyLink seeds a redeemed link whose pending session is attached.
func readyLink(t *testing.T) (*Store, Session) {
	t.Helper()
	s, pending := spentLink(t)
	if err := s.CompletePairingRedemption(redemptionOf(pending)); err != nil {
		t.Fatalf("CompletePairingRedemption: %v", err)
	}
	return s, pending
}

// A confirmation whose write fails leaves the link and its session as they
// were, whichever statement failed, and a retry completes it.
func TestConfirmRedeemedPairingWhoseWriteFailsChangesNothing(t *testing.T) {
	for _, failing := range []struct{ name, trigger string }{
		{"activation", `CREATE TRIGGER reject_confirm BEFORE UPDATE OF activated_at ON sessions BEGIN SELECT RAISE(ABORT, 'test activation failure'); END`},
		{"confirmation", `CREATE TRIGGER reject_confirm BEFORE UPDATE OF confirmed_at ON pairing_links BEGIN SELECT RAISE(ABORT, 'test confirmation failure'); END`},
	} {
		t.Run(failing.name, func(t *testing.T) {
			s, pending := readyLink(t)
			mustExec(t, s.db, failing.trigger)
			if _, err := s.ConfirmRedeemedPairing("link-1", 2500, 900000); err == nil || errors.Is(err, ErrPairingIncomplete) {
				t.Fatalf("a confirmation whose %s failed = %v, want the storage error", failing.name, err)
			}
			if link, err := s.GetPairingLink("link-1"); err != nil || link.Settled() || !link.Ready() {
				t.Fatalf("a failed confirmation moved the link (%+v, %v)", link, err)
			}
			if session, err := s.GetSession(pending.ID); err != nil || session.ActivatedAt != 0 || session.RevokedAt != 0 {
				t.Fatalf("a failed confirmation moved the session (%+v, %v)", session, err)
			}
			mustExec(t, s.db, `DROP TRIGGER reject_confirm`)
			if _, err := s.ConfirmRedeemedPairing("link-1", 2600, 900000); err != nil {
				t.Fatalf("the retry after storage recovered: %v", err)
			}
			if session, err := s.GetSession(pending.ID); err != nil || session.ActivatedAt != 2600 {
				t.Fatalf("the retry did not activate the session (%+v, %v)", session, err)
			}
		})
	}
}

// A confirmation whose session can no longer be activated settles the
// pairing as cancelled, with its session revoked, in the same transaction.
func TestConfirmRedeemedPairingThatCannotActivateCancelsInstead(t *testing.T) {
	s, pending := readyLink(t)
	// Admitted at the session's own deadline: too late to activate.
	confirmation, err := s.ConfirmRedeemedPairing("link-1", pending.ExpiresAt, 900000)
	if !errors.Is(err, ErrPairingIncomplete) {
		t.Fatalf("a confirmation of a lapsed session = %v, want ErrPairingIncomplete", err)
	}
	if !confirmation.Canceled.SessionRevoked || confirmation.Canceled.Link.SessionID != pending.ID {
		t.Fatalf("the incomplete confirmation reported %+v, want its session revoked", confirmation.Canceled)
	}
	if link, err := s.GetPairingLink("link-1"); err != nil || link.ConfirmedAt != 0 || link.CanceledAt != pending.ExpiresAt {
		t.Fatalf("an incomplete confirmation left the link %+v (%v), want cancelled", link, err)
	}
	if session, err := s.GetSession(pending.ID); err != nil || session.ActivatedAt != 0 || session.RevokedAt == 0 {
		t.Fatalf("an incomplete confirmation left the session %+v (%v), want revoked and unactivated", session, err)
	}
}

// The deadline a confirmation is judged against is its admission's: one
// admitted before the session's deadline activates it whenever its write
// runs.
func TestConfirmRedeemedPairingJudgesTheDeadlineAtAdmission(t *testing.T) {
	s, pending := readyLink(t)
	if _, err := s.ConfirmRedeemedPairing("link-1", pending.ExpiresAt-1, 900000); err != nil {
		t.Fatalf("a confirmation admitted before the deadline: %v", err)
	}
	if session, err := s.GetSession(pending.ID); err != nil || session.ActivatedAt != pending.ExpiresAt-1 || session.ExpiresAt != 900000 {
		t.Fatalf("the session = %+v (%v), want activated with its access window", session, err)
	}
}
