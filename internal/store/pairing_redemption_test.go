package store

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/owndevices"
)

// spentLink seeds a link redeemed at 2000 and the redemption that would
// complete it: the owner's existing device, relabeled, and its pending
// session.
func spentLink(t *testing.T) (*Store, PairingRedemption) {
	t.Helper()
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	seedPairingLink(t, s, owner, []byte("hash-1"), 9000)
	if _, err := s.RedeemPairingLink([]byte("hash-1"), 2000, "thumb"); err != nil {
		t.Fatalf("RedeemPairingLink: %v", err)
	}
	pending := newTestSession("sess-pending", owner, device, key, 9000)
	pending.ActivatedAt = 0
	relabeled := device
	relabeled.Label, relabeled.Platform = "Renamed by the redemption", "windows"
	return s, PairingRedemption{LinkID: "link-1", DeviceSession: DeviceSession{Device: relabeled, Session: pending,
		Refresh: RefreshGrant{Hash: []byte("refresh-1"), CreatedAt: 2000, ExpiresAt: 90000}}}
}

// onNewDevice is r redeemed by a key this store has never seen: a new
// device row.
func onNewDevice(t *testing.T, r PairingRedemption) PairingRedemption {
	t.Helper()
	device, err := NewPairedDevice(r.Session.UserID, "New laptop", "desktop", "linux", "thumb-new", "bearer")
	if err != nil {
		t.Fatal(err)
	}
	r.Device, r.NewDevice, r.Session.DeviceID = device, true, device.ID
	return r
}

// eachDevice runs test for a redemption on the existing device and on a new
// one.
func eachDevice(t *testing.T, test func(t *testing.T, s *Store, r PairingRedemption)) {
	t.Run("existing device", func(t *testing.T) {
		s, r := spentLink(t)
		test(t, s, r)
	})
	t.Run("new device", func(t *testing.T) {
		s, r := spentLink(t)
		test(t, s, onNewDevice(t, r))
	})
}

// requireNothingRedeemed checks that a redemption that failed left no
// session, no refresh secret, no link naming them, and no new or renamed
// device.
func requireNothingRedeemed(t *testing.T, s *Store, r PairingRedemption) {
	t.Helper()
	if got, err := s.GetSession(r.Session.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a failed redemption left its session (%+v, %v)", got, err)
	}
	var secrets int
	if err := s.db.QueryRow(`SELECT count(*) FROM refresh_secrets WHERE session_id = ?`, r.Session.ID).Scan(&secrets); err != nil || secrets != 0 {
		t.Fatalf("a failed redemption left %d refresh secrets (%v)", secrets, err)
	}
	if link, err := s.GetPairingLink("link-1"); err != nil || link.SessionID != "" || link.DeviceID != "" {
		t.Fatalf("a failed redemption left the link naming rows (%+v, %v)", link, err)
	}
	device, err := s.GetDevice(r.Device.ID)
	switch {
	case r.NewDevice && !errors.Is(err, sql.ErrNoRows):
		t.Fatalf("a failed redemption left its new device (%+v, %v)", device, err)
	case !r.NewDevice && (err != nil || device.Label != "This Desktop" || device.Platform != "linux"):
		t.Fatalf("a failed redemption renamed its device (%+v, %v)", device, err)
	}
}

// A redemption whose refresh secret cannot be written writes nothing else.
func TestCompletePairingRedemptionWritesNothingWhenItsRefreshSecretFails(t *testing.T) {
	eachDevice(t, func(t *testing.T, s *Store, r PairingRedemption) {
		mustExec(t, s.db, `CREATE TRIGGER reject_refresh BEFORE INSERT ON refresh_secrets BEGIN SELECT RAISE(ABORT, 'test refresh failure'); END`)
		if err := s.CompletePairingRedemption(r); err == nil {
			t.Fatal("a redemption whose refresh secret failed reported success")
		}
		requireNothingRedeemed(t, s, r)
	})
}

// A redemption whose link cannot name it writes nothing.
func TestCompletePairingRedemptionWritesNothingWhenItsAttachFails(t *testing.T) {
	eachDevice(t, func(t *testing.T, s *Store, r PairingRedemption) {
		mustExec(t, s.db, `CREATE TRIGGER reject_attach BEFORE UPDATE OF session_id ON pairing_links BEGIN SELECT RAISE(ABORT, 'test attach failure'); END`)
		if err := s.CompletePairingRedemption(r); err == nil {
			t.Fatal("a redemption whose attach failed reported success")
		}
		requireNothingRedeemed(t, s, r)
	})
}

// A cancellation that reaches the spent link before the redemption completes
// leaves the redemption nothing to write.
func TestCompletePairingRedemptionAfterACancellationWritesNothing(t *testing.T) {
	eachDevice(t, func(t *testing.T, s *Store, r PairingRedemption) {
		if _, err := s.CancelPairingLink("link-1", 2100); err != nil {
			t.Fatalf("CancelPairingLink: %v", err)
		}
		if err := s.CompletePairingRedemption(r); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("a redemption of a cancelled link = %v, want sql.ErrNoRows", err)
		}
		requireNothingRedeemed(t, s, r)
	})
}

func TestCompletePairingRedemptionWritesTheDeviceAndSessionWithItsLink(t *testing.T) {
	eachDevice(t, func(t *testing.T, s *Store, r PairingRedemption) {
		if err := s.CompletePairingRedemption(r); err != nil {
			t.Fatalf("CompletePairingRedemption: %v", err)
		}
		link, err := s.GetPairingLink("link-1")
		if err != nil || link.SessionID != r.Session.ID || link.DeviceID != r.Device.ID || !link.Ready() {
			t.Fatalf("the link after its redemption = %+v (%v)", link, err)
		}
		if device, err := s.GetDevice(r.Device.ID); err != nil || device.Label != r.Device.Label || device.Platform != r.Device.Platform {
			t.Fatalf("the redemption's device = %+v (%v), want %+v", device, err, r.Device)
		}
		if session, err := s.GetSession(r.Session.ID); err != nil || session.ActivatedAt != 0 {
			t.Fatalf("the redemption's session = %+v (%v), want written and unactivated", session, err)
		}
		if _, err := s.GetRefreshSecretByHash([]byte("refresh-1")); err != nil {
			t.Fatalf("the redemption's refresh secret: %v", err)
		}
	})
}

// A live session is written with its device by CreateDeviceSession; a
// pending one is refused there, because it exists only with its link.
func TestCreateDeviceSessionWritesOnlyALiveSession(t *testing.T) {
	eachDevice(t, func(t *testing.T, s *Store, r PairingRedemption) {
		if err := s.CreateDeviceSession(r.DeviceSession); err == nil {
			t.Fatal("CreateDeviceSession wrote a pending session")
		}
		requireNothingRedeemed(t, s, r)
		live := r.DeviceSession
		live.Session.ActivatedAt = live.Session.CreatedAt
		if err := s.CreateDeviceSession(live); err != nil {
			t.Fatalf("CreateDeviceSession: %v", err)
		}
		if device, err := s.GetDevice(live.Device.ID); err != nil || device.Label != live.Device.Label || device.Platform != live.Device.Platform {
			t.Fatalf("the session's device = %+v (%v), want %+v", device, err, live.Device)
		}
		if session, err := s.GetSession(live.Session.ID); err != nil || session.ActivatedAt == 0 {
			t.Fatalf("the written session = %+v (%v), want live", session, err)
		}
		if _, err := s.GetRefreshSecretByHash([]byte("refresh-1")); err != nil {
			t.Fatalf("the session's refresh secret: %v", err)
		}
	})
}

// readyLink seeds a redeemed link whose pending session is attached.
func readyLink(t *testing.T) (*Store, Session) {
	t.Helper()
	s, r := spentLink(t)
	if err := s.CompletePairingRedemption(r); err != nil {
		t.Fatalf("CompletePairingRedemption: %v", err)
	}
	return s, r.Session
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

// A personal pairing whose membership can no longer be admitted cancels the
// link and revokes its session in the confirmation's transaction, and admits
// nothing.
func TestConfirmRedeemedPairingWhoseMembershipIsRefusedCancelsInstead(t *testing.T) {
	for _, refused := range []struct {
		name, purpose, want string
		// before makes the membership inadmissible.
		before func(t *testing.T, s *Store)
	}{
		// No member holds the generation the introduction was issued for.
		{"an introduction no longer authorized", "own-introduction", "no longer authorized", func(*testing.T, *Store) {}},
		// The catalog is full, so a new member cannot join it.
		{"a full catalog", "own-device", "too many own devices", func(t *testing.T, s *Store) {
			mustExec(t, s.db, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
				INSERT INTO own_devices(key_thumbprint, member, generation, removed) SELECT 'member-' || i, '{}', 1, 0 FROM n`, owndevices.MaxMembers)
		}},
	} {
		t.Run(refused.name, func(t *testing.T) {
			s, owner, device := seedOwnerDevice(t)
			key := seedSigningKey(t, s)
			link := PairingLink{ID: "own-link", UserID: owner.ID, Scopes: []string{"threads:read"}, BindingClass: "device-bound",
				DeviceClass: "desktop", Purpose: refused.purpose, MemberGeneration: 1, CreatedAt: 1000, ExpiresAt: 9000}
			if err := s.CreatePairingLink(link, []byte("hash-own")); err != nil {
				t.Fatal(err)
			}
			if _, err := s.RedeemPairingLink([]byte("hash-own"), 2000, "thumb-own"); err != nil {
				t.Fatal(err)
			}
			pending := newTestSession("sess-own", owner, device, key, 9000)
			pending.ActivatedAt = 0
			if err := s.CompletePairingRedemption(PairingRedemption{LinkID: link.ID, DeviceSession: DeviceSession{Device: device, Session: pending}}); err != nil {
				t.Fatal(err)
			}
			refused.before(t, s)
			confirmation, err := s.ConfirmRedeemedPairing(link.ID, 2500, 900000)
			if !errors.Is(err, ErrPairingIncomplete) || !strings.Contains(err.Error(), refused.want) {
				t.Fatalf("a confirmation whose membership is refused = %v, want ErrPairingIncomplete for the admission", err)
			}
			if !confirmation.Canceled.SessionRevoked {
				t.Fatalf("the incomplete confirmation reported %+v, want its session revoked", confirmation.Canceled)
			}
			if got, err := s.GetPairingLink(link.ID); err != nil || got.ConfirmedAt != 0 || got.CanceledAt == 0 {
				t.Fatalf("the link = %+v (%v), want cancelled and unconfirmed", got, err)
			}
			if session, err := s.GetSession(pending.ID); err != nil || session.ActivatedAt != 0 || session.RevokedAt == 0 {
				t.Fatalf("the session = %+v (%v), want revoked and unactivated", session, err)
			}
			var admitted int
			if err := s.db.QueryRow(`SELECT count(*) FROM own_device_sessions WHERE session_id = ?`, pending.ID).Scan(&admitted); err != nil || admitted != 0 {
				t.Fatalf("%d memberships admitted for the refused pairing (%v)", admitted, err)
			}
		})
	}
}

// A redemption confirmed in its own transaction is an own-device
// introduction's alone, and one whose confirmation is refused writes nothing.
func TestCompletePairingRedemptionConfirmsOnlyAnIntroduction(t *testing.T) {
	for _, purpose := range []string{"", "own-device", "own-introduction"} {
		t.Run("purpose "+purpose, func(t *testing.T) {
			eachDevice(t, func(t *testing.T, s *Store, r PairingRedemption) {
				// spentLink's link is ordinary; give it the purpose under test.
				// No member holds the generation an introduction was issued
				// for, so its confirmation is refused.
				mustExec(t, s.db, `UPDATE pairing_links SET purpose = ?, member_generation = 1 WHERE id = 'link-1'`, purpose)
				r.ConfirmedAt, r.AccessExpiresAt = 2500, 900000
				err := s.CompletePairingRedemption(r)
				switch {
				case purpose == "own-introduction" && !errors.Is(err, ErrPairingIncomplete):
					t.Fatalf("an introduction whose membership is refused = %v, want ErrPairingIncomplete", err)
				case purpose != "own-introduction" && (err == nil || !strings.Contains(err.Error(), "confirmed by its owner")):
					t.Fatalf("a %q redemption confirmed with its write = %v, want refused", purpose, err)
				}
				requireNothingRedeemed(t, s, r)
			})
		})
	}
}
