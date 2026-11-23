package identity

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"agent-overflow/internal/owndevices"
	"agent-overflow/internal/store"
)

// withholdSigningKey removes the signing key's row, so a Sessions that holds
// no key in memory cannot sign; restore puts it back.
func withholdSigningKey(t *testing.T, raw *sql.DB) (restore func()) {
	t.Helper()
	var id string
	var secret []byte
	var created int64
	if err := raw.QueryRow(`SELECT id, secret, created_at FROM signing_keys`).Scan(&id, &secret, &created); err != nil {
		t.Fatalf("read the signing key: %v", err)
	}
	if _, err := raw.Exec(`DELETE FROM signing_keys`); err != nil {
		t.Fatalf("withhold the signing key: %v", err)
	}
	return func() {
		if _, err := raw.Exec(`INSERT INTO signing_keys (id, secret, created_at) VALUES (?, ?, ?)`, id, secret, created); err != nil {
			t.Fatalf("restore the signing key: %v", err)
		}
	}
}

// restartedSessions is a second session core over st, as a restarted backend
// has, holding no signing key in memory, on sessions' clock.
func restartedSessions(t *testing.T, sessions *Sessions, st *store.Store) *Sessions {
	t.Helper()
	restarted, err := NewSessions(st, testBackendID)
	if err != nil {
		t.Fatal(err)
	}
	restarted.now = sessions.now
	return restarted
}

func mustNextSecret(t *testing.T) string {
	t.Helper()
	next, _, err := newRefreshSecret()
	if err != nil {
		t.Fatal(err)
	}
	return next
}

// A renewal whose new credential cannot be signed still classifies what it
// was presented: a reuse ends the family and a superseded receipt answers
// superseded, as they would with the key at hand.
func TestARenewalThatCannotSignStillClassifiesItsPresentation(t *testing.T) {
	t.Run("reuse", func(t *testing.T) {
		sessions, st, raw, owner, _ := stragglerFixture(t)
		_, first := pairedDevice(t, sessions, owner, "thumb-reuse")
		if _, reason := sessions.Refresh(RefreshRequest{Secret: first.RefreshSecret, NextSecret: mustNextSecret(t), Proof: bearerProof("thumb-reuse")}); reason.Refused() {
			t.Fatal(reason)
		}
		withholdSigningKey(t, raw)
		restarted := restartedSessions(t, sessions, st)
		// The spent secret with a successor it never recorded.
		if _, reason := restarted.Refresh(RefreshRequest{Secret: first.RefreshSecret, NextSecret: mustNextSecret(t), Proof: bearerProof("thumb-reuse")}); reason != ReasonRevokedSession {
			t.Fatalf("a reuse while the key is withheld = %s, want %s", reason, ReasonRevokedSession)
		}
		if session, err := st.GetSession(first.SessionID); err != nil || session.RevokedAt == 0 {
			t.Fatalf("the family after a reuse = %+v (%v), want its session revoked", session, err)
		}
	})
	t.Run("superseded", func(t *testing.T) {
		sessions, st, raw, owner, _ := stragglerFixture(t)
		_, first := pairedDevice(t, sessions, owner, "thumb-superseded")
		next := mustNextSecret(t)
		lost := RefreshRequest{Secret: first.RefreshSecret, NextSecret: next, Proof: bearerProof("thumb-superseded")}
		second, reason := sessions.Refresh(lost)
		if reason.Refused() {
			t.Fatal(reason)
		}
		if _, reason := sessions.Refresh(RefreshRequest{Secret: second.RefreshSecret, NextSecret: mustNextSecret(t), Proof: bearerProof("thumb-superseded")}); reason.Refused() {
			t.Fatal(reason)
		}
		withholdSigningKey(t, raw)
		restarted := restartedSessions(t, sessions, st)
		if _, reason := restarted.Refresh(lost); reason != ReasonRefreshSuperseded {
			t.Fatalf("a superseded receipt while the key is withheld = %s, want %s", reason, ReasonRefreshSuperseded)
		}
		if session, err := st.GetSession(first.SessionID); err != nil || session.RevokedAt != 0 {
			t.Fatalf("a superseded receipt = %+v (%v), want the session live", session, err)
		}
	})
}

// A detected reuse whose family end cannot be written changes nothing and
// says so: the device is told to try later, the session stays live, the
// outstanding secret unspent, and no reuse is recorded; the next
// presentation of the spent secret ends the family.
func TestAReuseWhoseFamilyEndFailsChangesNothing(t *testing.T) {
	sessions, st, raw, owner, _ := stragglerFixture(t)
	_, first := pairedDevice(t, sessions, owner, "thumb-family")
	second, reason := sessions.Refresh(RefreshRequest{Secret: first.RefreshSecret, Proof: bearerProof("thumb-family")})
	if reason.Refused() {
		t.Fatal(reason)
	}
	if _, err := raw.Exec(`CREATE TRIGGER reject_revoke BEFORE UPDATE OF revoked_at ON sessions BEGIN SELECT RAISE(ABORT, 'injected revocation failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, reason := sessions.Refresh(RefreshRequest{Secret: first.RefreshSecret, Proof: bearerProof("thumb-family")}); reason != ReasonTemporarilyUnavailable {
		t.Fatalf("a reuse whose family end failed = %s, want %s", reason, ReasonTemporarilyUnavailable)
	}
	if session, err := st.GetSession(first.SessionID); err != nil || session.RevokedAt != 0 {
		t.Fatalf("the session after the failed end = %+v (%v), want live", session, err)
	}
	digest := hashRefreshSecret(second.RefreshSecret)
	if held, err := st.GetRefreshSecretByHash(digest[:]); err != nil || held.Spent() {
		t.Fatalf("the outstanding secret after the failed end = %+v (%v), want unspent", held, err)
	}
	if n := countAudit(t, st, AuditRefreshReuseDetected, ""); n != 0 {
		t.Fatalf("a failed family end recorded %d reuses, want none", n)
	}
	if _, err := raw.Exec(`DROP TRIGGER reject_revoke`); err != nil {
		t.Fatal(err)
	}
	if _, reason := sessions.Refresh(RefreshRequest{Secret: first.RefreshSecret, Proof: bearerProof("thumb-family")}); reason != ReasonRevokedSession {
		t.Fatalf("the reuse after storage recovered = %s, want %s", reason, ReasonRevokedSession)
	}
	if session, err := st.GetSession(first.SessionID); err != nil || session.RevokedAt == 0 {
		t.Fatalf("the session after the family ended = %+v (%v), want revoked", session, err)
	}
}

// A confirmation whose device was revoked and then removed ends the pairing
// in its own transaction: the owner is told it was not completed, and the
// link is canceled rather than left approvable.
func TestAConfirmationWhoseDeviceIsGoneEndsThePairing(t *testing.T) {
	sessions, st, _, owner, _ := newFixture(t)
	link := mustMintLink(t, sessions, owner)
	redemption := mustRedeem(t, sessions, link.Token, "thumb-forgotten")
	if _, err := sessions.RevokeDevice(redemption.DeviceID); err != nil {
		t.Fatal(err)
	}
	if deleted, _, err := st.DeleteDevice(redemption.DeviceID); err != nil || !deleted {
		t.Fatalf("forget the device = %v, %v", deleted, err)
	}
	if _, err := sessions.ConfirmPairing(link.Link.ID); !errors.Is(err, ErrPairingNotCompleted) {
		t.Fatalf("confirming a pairing whose device is gone = %v, want ErrPairingNotCompleted", err)
	}
	if stored, err := st.GetPairingLink(link.Link.ID); err != nil || stored.CanceledAt == 0 || stored.ConfirmedAt != 0 {
		t.Fatalf("the pairing = %+v (%v), want it canceled", stored, err)
	}
	if _, err := sessions.ConfirmPairing(link.Link.ID); !errors.Is(err, ErrPairingRefused) {
		t.Fatalf("confirming it again = %v, want ErrPairingRefused", err)
	}
}

// An introduction whose membership is refused and whose cancellation fails
// records no cancellation as done: the access audit shows the spent link
// left unsettled instead.
func TestAnIntroductionWhoseCancellationFailsRecordsNoCancellation(t *testing.T) {
	s, _, raw, owner, _ := stragglerFixture(t)
	sponsor, target := newSigningDevice(t), newSigningDevice(t)
	personalPair(t, s, owner, sponsor)
	if _, err := s.MergeOwnDevices([]owndevices.Member{{KeyThumbprint: target.thumbprint(), Generation: 1}}); err != nil {
		t.Fatal(err)
	}
	link := ownIntro(t, s, owner, sponsor, target)
	for _, trigger := range []string{
		fmt.Sprintf(`CREATE TRIGGER refuse_intro AFTER UPDATE OF session_id ON pairing_links
			BEGIN UPDATE own_devices SET removed = 1 WHERE key_thumbprint = '%s'; END`, sponsor.thumbprint()),
		`CREATE TRIGGER reject_cancel BEFORE UPDATE OF canceled_at ON pairing_links BEGIN SELECT RAISE(ABORT, 'injected cancellation failure'); END`,
	} {
		if _, err := raw.Exec(trigger); err != nil {
			t.Fatal(err)
		}
	}
	if _, reason := s.RedeemPairing(RedemptionRequest{
		Token: link.Token, Proof: target.proof(t, "POST", "/auth/pair", link.Token+target.thumbprint(), s.now()),
	}); reason != ReasonUnknownCredential {
		t.Fatalf("a refused introduction = %s, want %s", reason, ReasonUnknownCredential)
	}
	if n := countAudit(t, s.store, AuditPairingCanceled, store.AuthAuditOutcomeAllowed); n != 0 {
		t.Fatalf("a failed cancellation was recorded as %d cancellations", n)
	}
	if n := countAudit(t, s.store, AuditPairingCanceled, store.AuthAuditOutcomeRefused); n != 1 {
		t.Fatalf("a failed cancellation left %d refused entries, want 1", n)
	}
}

// countAudit counts the audit entries of event, of outcome when it is set.
func countAudit(t *testing.T, st *store.Store, event AuditEvent, outcome string) int {
	t.Helper()
	entries, err := st.ListRecentAuthAudit(200)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, entry := range entries {
		if entry.Event == string(event) && (outcome == "" || entry.Outcome == outcome) {
			n++
		}
	}
	return n
}

// A personal admission a restart finishes is revoked only when the
// membership refuses it; a storage failure leaves the approved session for
// the next start, which then finishes it.
func TestRecoveringAnAdmissionRevokesOnlyARefusal(t *testing.T) {
	s, st, raw, owner, _ := stragglerFixture(t)
	member := newSigningDevice(t)
	paired := personalPair(t, s, owner, member)
	// What a stop between the activation and the membership leaves.
	if _, err := raw.Exec(`DELETE FROM own_device_sessions WHERE session_id = ?`, paired.Tokens.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TRIGGER reject_membership BEFORE INSERT ON own_device_sessions BEGIN SELECT RAISE(ABORT, 'injected membership failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverOwnAdmissions(); err == nil || !strings.Contains(err.Error(), "injected membership failure") {
		t.Fatalf("recovery with a failing membership write = %v, want that failure", err)
	}
	if session, err := st.GetSession(paired.Tokens.SessionID); err != nil || session.RevokedAt != 0 {
		t.Fatalf("the approved session after a storage failure = %+v (%v), want it kept", session, err)
	}
	if _, err := raw.Exec(`DROP TRIGGER reject_membership`); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverOwnAdmissions(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.OwnDeviceForSession(paired.Tokens.SessionID); err != nil {
		t.Fatalf("the next recovery did not finish the admission: %v", err)
	}
	if session, err := st.GetSession(paired.Tokens.SessionID); err != nil || session.RevokedAt != 0 {
		t.Fatalf("the finished admission's session = %+v (%v), want live", session, err)
	}
}

// A marked link is a computer pairing window's invitation, admitted only
// while a window holds it; a session core with no gate has no window, so it
// refuses the link unspent.
func TestAMarkedLinkWithoutAGateIsRefused(t *testing.T) {
	sessions, st, _, owner, _ := newFixture(t)
	link, err := sessions.MintPairingLink(PairingRequest{
		UserID: owner.ID, DeviceClass: DevicePhone, BindingClass: BindingDeviceBound,
		Scopes: []Scope{ScopeThreadsRead}, Bootstrap: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, reason := sessions.RedeemPairing(RedemptionRequest{Token: link.Token, Proof: bearerProof("thumb-marked")}); reason != ReasonUnknownCredential {
		t.Fatalf("redeeming a marked link with no gate = %s, want %s", reason, ReasonUnknownCredential)
	}
	if stored, err := st.GetPairingLink(link.Link.ID); err != nil || stored.RedeemedAt != 0 {
		t.Fatalf("the marked link = %+v (%v), want it unspent", stored, err)
	}
}
