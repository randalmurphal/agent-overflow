package identity

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"

	"agent-overflow/internal/owndevices"
)

// An own-device introduction is confirmed in its redemption's transaction,
// so one whose confirmation is refused or fails to write adds or renames no
// device and leaves no session, refresh secret or membership. The spent link
// is cancelled, and a refusal is recorded as that cancellation.
func TestAnOwnIntroductionWhoseConfirmationFailsLeavesNothing(t *testing.T) {
	for _, failing := range []struct {
		name string
		// trigger is formatted with the sponsor's key thumbprint.
		trigger string
		refused bool
	}{
		// The sponsor leaves the group while the redemption is written: the
		// confirmation's own admission check refuses it.
		{"membership refused", `CREATE TRIGGER fail_intro AFTER UPDATE OF session_id ON pairing_links
			BEGIN UPDATE own_devices SET removed = 1 WHERE key_thumbprint = '%s'; END`, true},
		{"activation write", `CREATE TRIGGER fail_intro BEFORE UPDATE OF activated_at ON sessions
			BEGIN SELECT RAISE(ABORT, 'injected activation failure %s'); END`, false},
	} {
		for _, known := range []bool{false, true} {
			name := failing.name + " on a new device"
			if known {
				name = failing.name + " on a known device"
			}
			t.Run(name, func(t *testing.T) {
				s, _, raw, owner, _ := stragglerFixture(t)
				sponsor, target := newSigningDevice(t), newSigningDevice(t)
				personalPair(t, s, owner, sponsor)
				if _, err := s.MergeOwnDevices([]owndevices.Member{{KeyThumbprint: target.thumbprint(), Generation: 1}}); err != nil {
					t.Fatal(err)
				}
				if known {
					redeemOwn(t, s, ownIntro(t, s, owner, sponsor, target), target)
				}
				link := ownIntro(t, s, owner, sponsor, target)
				before := countSignInWrites(t, raw)
				beforeMembers, beforeConfirmed := countIntroductionWrites(t, raw, link.Link.ID)
				if _, err := raw.Exec(fmt.Sprintf(failing.trigger, sponsor.thumbprint())); err != nil {
					t.Fatalf("install the failure: %v", err)
				}

				_, reason := s.RedeemPairing(RedemptionRequest{
					Token: link.Token, Label: "Renamed phone", Platform: "windows",
					Proof: target.proof(t, "POST", "/auth/pair", link.Token+target.thumbprint(), s.now()),
				})
				if reason != ReasonUnknownCredential {
					t.Fatalf("an introduction whose %s = %s, want %s", failing.name, reason, ReasonUnknownCredential)
				}
				after := countSignInWrites(t, raw)
				if after.devices != before.devices || !slices.Equal(after.labels, before.labels) {
					t.Errorf("a failed introduction changed the devices: %v, want %v", after.labels, before.labels)
				}
				if after.sessions != before.sessions || after.refreshSecrets != before.refreshSecrets {
					t.Errorf("a failed introduction left %d sessions and %d refresh secrets, want %d and %d",
						after.sessions, after.refreshSecrets, before.sessions, before.refreshSecrets)
				}
				afterMembers, afterConfirmed := countIntroductionWrites(t, raw, link.Link.ID)
				if afterMembers != beforeMembers || afterConfirmed != beforeConfirmed || after.mintAudits != before.mintAudits {
					t.Errorf("a failed introduction admitted %d memberships and recorded %d confirmations and %d mints, want %d, %d and %d",
						afterMembers, afterConfirmed, after.mintAudits, beforeMembers, beforeConfirmed, before.mintAudits)
				}
				stored, err := s.store.GetPairingLink(link.Link.ID)
				if err != nil || stored.CanceledAt == 0 || stored.ConfirmedAt != 0 || stored.SessionID != "" {
					t.Fatalf("the spent introduction = %+v (%v), want cancelled and naming nothing", stored, err)
				}
				canceled := 0
				entries, err := s.store.ListRecentAuthAudit(100)
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if entry.Event == string(AuditPairingCanceled) && entry.Detail == link.Link.ID {
						canceled++
					}
				}
				if want := map[bool]int{true: 1, false: 0}[failing.refused]; canceled != want {
					t.Fatalf("the introduction recorded %d cancellations, want %d", canceled, want)
				}
			})
		}
	}
}

// A redemption derives its verification number before it writes anything,
// so a failure to derive it leaves the link unspent and nothing written,
// and a redemption that commits is never then refused. The device is told
// to present the same link again.
func TestARedemptionThatCannotDeriveItsNumberSpendsNothing(t *testing.T) {
	s, st, raw, owner, _ := stragglerFixture(t)
	link := mustMintLink(t, s, owner)
	before := countSignInWrites(t, raw)
	// The number is keyed by the signing key: with none stored and none
	// mintable, it cannot be derived.
	for _, statement := range []string{
		`DELETE FROM signing_keys`,
		`CREATE TRIGGER fail_key BEFORE INSERT ON signing_keys BEGIN SELECT RAISE(ABORT, 'injected key failure'); END`,
	} {
		if _, err := raw.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if _, reason := s.RedeemPairing(RedemptionRequest{Token: link.Token, Proof: bearerProof("thumb-number")}); reason != ReasonTemporarilyUnavailable {
		t.Fatalf("a redemption that cannot derive its number = %s, want %s", reason, ReasonTemporarilyUnavailable)
	}
	if stored, err := st.GetPairingLink(link.Link.ID); err != nil || stored.RedeemedAt != 0 || stored.Settled() {
		t.Fatalf("the link after the failure = %+v (%v), want unspent", stored, err)
	}
	if after := countSignInWrites(t, raw); after.devices != before.devices || after.sessions != before.sessions {
		t.Fatalf("the failure wrote %d devices and %d sessions, want %d and %d", after.devices, after.sessions, before.devices, before.sessions)
	}
	if _, err := raw.Exec(`DROP TRIGGER fail_key`); err != nil {
		t.Fatal(err)
	}
	if _, reason := s.RedeemPairing(RedemptionRequest{Token: link.Token, Proof: bearerProof("thumb-number")}); reason.Refused() {
		t.Fatalf("the retry after the key recovered = %s", reason)
	}
}

// A storage failure before the link is spent, reading the link or spending
// it, leaves the link as it was: the device is told to present the same link
// again, and that presentation redeems it.
func TestARedemptionThatFailsBeforeTheSpendSaysToTryTheSameLinkAgain(t *testing.T) {
	for _, failing := range []struct{ name, inject, recover string }{
		{"the link's read", `ALTER TABLE pairing_links RENAME COLUMN token_hash TO token_hash_moved`,
			`ALTER TABLE pairing_links RENAME COLUMN token_hash_moved TO token_hash`},
		{"the spend", `CREATE TRIGGER fail_spend BEFORE UPDATE OF redeemed_at ON pairing_links BEGIN SELECT RAISE(ABORT, 'injected spend failure'); END`,
			`DROP TRIGGER fail_spend`},
	} {
		t.Run(failing.name, func(t *testing.T) {
			s, st, raw, owner, _ := stragglerFixture(t)
			link := mustMintLink(t, s, owner)
			if _, err := raw.Exec(failing.inject); err != nil {
				t.Fatalf("install the failure: %v", err)
			}
			if _, reason := s.RedeemPairing(RedemptionRequest{Token: link.Token, Proof: bearerProof("thumb-retry")}); reason != ReasonTemporarilyUnavailable {
				t.Fatalf("a redemption whose %s failed = %s, want %s", failing.name, reason, ReasonTemporarilyUnavailable)
			}
			if _, err := raw.Exec(failing.recover); err != nil {
				t.Fatalf("remove the failure: %v", err)
			}
			if stored, err := st.GetPairingLink(link.Link.ID); err != nil || stored.RedeemedAt != 0 || stored.Settled() {
				t.Fatalf("the link after the failure = %+v (%v), want unspent", stored, err)
			}
			if _, reason := s.RedeemPairing(RedemptionRequest{Token: link.Token, Proof: bearerProof("thumb-retry")}); reason.Refused() {
				t.Fatalf("the same link presented again = %s", reason)
			}
		})
	}
}

// refusingGate refuses every link, as the computer pairing book refuses an
// invitation whose window is gone.
type refusingGate struct{}

func (refusingGate) Admit(string, bool, func() error) error { return ErrPairingRefused }

// failingGate fails to decide, which refuses nothing.
type failingGate struct{}

func (failingGate) Admit(string, bool, func() error) error {
	return errors.New("injected gate failure")
}

// A link that admits nothing keeps its refusal: never "try the same link
// again", which could not work. Only a gate that fails to decide is this
// computer's to retry.
func TestARefusedRedemptionIsNeverToldToTryTheSameLinkAgain(t *testing.T) {
	s, _, _, owner, _ := stragglerFixture(t)
	redeem := func(token string) Reason {
		_, reason := s.RedeemPairing(RedemptionRequest{Token: token, Proof: bearerProof("thumb-refused")})
		return reason
	}
	if reason := redeem("no-such-link"); reason != ReasonUnknownCredential {
		t.Fatalf("an unknown link = %s, want %s", reason, ReasonUnknownCredential)
	}
	spent := mustMintLink(t, s, owner)
	if reason := redeem(spent.Token); reason.Refused() {
		t.Fatal(reason)
	}
	if reason := redeem(spent.Token); reason != ReasonUnknownCredential {
		t.Fatalf("a spent link = %s, want %s", reason, ReasonUnknownCredential)
	}
	gated := mustMintLink(t, s, owner)
	s.SetPairingGate(refusingGate{})
	if reason := redeem(gated.Token); reason != ReasonUnknownCredential {
		t.Fatalf("a link the gate refuses = %s, want %s", reason, ReasonUnknownCredential)
	}
	s.SetPairingGate(failingGate{})
	if reason := redeem(gated.Token); reason != ReasonTemporarilyUnavailable {
		t.Fatalf("a link the gate failed to decide = %s, want %s", reason, ReasonTemporarilyUnavailable)
	}
}

// countIntroductionWrites counts the sessions admitted to the group and the
// confirmations recorded for linkID.
func countIntroductionWrites(t *testing.T, raw *sql.DB, linkID string) (members, confirmed int) {
	t.Helper()
	if err := raw.QueryRow(`SELECT COUNT(*) FROM own_device_sessions`).Scan(&members); err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM auth_audit WHERE event = ? AND detail = ?`,
		string(AuditPairingConfirmed), linkID).Scan(&confirmed); err != nil {
		t.Fatalf("count confirmations: %v", err)
	}
	return members, confirmed
}

// An introduction that succeeds hands out a credential that is not waiting
// on anyone, and records the mint, the confirmation and the redemption.
func TestAnOwnIntroductionIsConfirmedWithItsRedemption(t *testing.T) {
	s, _, _, owner, _ := newFixture(t)
	sponsor, target := newSigningDevice(t), newSigningDevice(t)
	personalPair(t, s, owner, sponsor)
	if _, err := s.MergeOwnDevices([]owndevices.Member{{KeyThumbprint: target.thumbprint(), Generation: 1}}); err != nil {
		t.Fatal(err)
	}
	link := ownIntro(t, s, owner, sponsor, target)
	r := redeemOwn(t, s, link, target)
	if r.Tokens.AwaitingConfirmation {
		t.Fatal("an introduction's credential must not wait on a confirmation")
	}
	session, reason := s.Live(r.Tokens.SessionID)
	if reason.Refused() {
		t.Fatalf("the introduced session = %s, want live", reason)
	}
	if want := RenewablePolicyFor(DevicePhone).Access.Milliseconds(); session.ExpiresAt-session.ActivatedAt != want {
		t.Fatalf("the introduced session's window = %dms, want the phone access window %dms", session.ExpiresAt-session.ActivatedAt, want)
	}
	if _, err := s.store.OwnDeviceForSession(session.ID); err != nil {
		t.Fatalf("the introduced session is not a member: %v", err)
	}
	entries, err := s.store.ListAuthAuditForDevice(r.DeviceID, 20)
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	for _, entry := range entries {
		if entry.SessionID == session.ID {
			events = append(events, entry.Event)
		}
	}
	if want := []string{string(AuditPairingRedeemed), string(AuditPairingConfirmed), string(AuditSessionMinted)}; !slices.Equal(events, want) {
		t.Fatalf("the introduction's audit entries, newest first = %v, want %v", events, want)
	}
}
