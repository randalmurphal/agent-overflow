package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"strconv"
	"sync"
	"testing"
)

func seedPairingLink(t *testing.T, s *Store, owner User, hash []byte, expires int64) PairingLink {
	t.Helper()
	link := PairingLink{
		ID:           "link-1",
		UserID:       owner.ID,
		Scopes:       []string{"threads:read", "files:read"},
		BindingClass: "device-bound",
		DeviceClass:  "phone",
		CreatedAt:    1000,
		ExpiresAt:    expires,
	}
	if err := s.CreatePairingLink(link, hash); err != nil {
		t.Fatalf("CreatePairingLink: %v", err)
	}
	return link
}

// seedPendingSession writes session, unactivated, the way a redemption does:
// on its existing device, named by a redeemed link (link-1, which the test
// must not have created).
func seedPendingSession(t *testing.T, s *Store, owner User, session Session) PairingLink {
	t.Helper()
	seedPairingLink(t, s, owner, []byte("hash-pending"), session.ExpiresAt)
	link, _ := redeemOnto(t, s, []byte("hash-pending"), session.CreatedAt, session)
	return link
}

// redeemOnto spends the link hash names at at for session's existing device
// and completes the redemption with session, unactivated, on the link's
// terms. A device with no key is given one first, as a redemption needs.
func redeemOnto(t *testing.T, s *Store, hash []byte, at int64, session Session) (PairingLink, Session) {
	t.Helper()
	device, err := s.GetDevice(session.DeviceID)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if device.KeyThumbprint == "" {
		device.KeyThumbprint = "thumb-" + device.ID
		if err := s.SetDeviceKeyThumbprint(device.ID, device.KeyThumbprint); err != nil {
			t.Fatalf("SetDeviceKeyThumbprint: %v", err)
		}
	}
	link, err := s.RedeemPairingLink(hash, at, device.KeyThumbprint)
	if err != nil {
		t.Fatalf("RedeemPairingLink: %v", err)
	}
	session.UserID, session.BindingClass, session.Scopes, session.ActivatedAt = link.UserID, link.BindingClass, link.Scopes, 0
	if err := s.CompletePairingRedemption(PairingRedemption{LinkID: link.ID,
		DeviceSession: DeviceSession{Device: device, Session: session}}); err != nil {
		t.Fatalf("CompletePairingRedemption: %v", err)
	}
	link.DeviceID, link.SessionID = device.ID, session.ID
	return link, session
}

// accessUntil is a confirmation's access expiry, the same for every class.
func accessUntil(at int64) func(string) int64 {
	return func(string) int64 { return at }
}

func TestRetirePendingPairingsRollsBackIfLinkRetirementFails(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	seedPairingLink(t, s, owner, []byte("hash"), 9000)
	link, session := redeemOnto(t, s, []byte("hash"), 2000, newTestSession("pending", owner, device, key, 9000))
	if _, err := s.db.Exec(`CREATE TRIGGER reject_pairing_retirement BEFORE UPDATE OF canceled_at ON pairing_links BEGIN SELECT RAISE(ABORT, 'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.RetirePendingPairings(3000); err == nil {
		t.Fatal("injected transaction failure was ignored")
	}
	after, err := s.GetSession(session.ID)
	if err != nil || after.RevokedAt != 0 {
		t.Fatal("session revocation escaped rollback", err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER reject_pairing_retirement`); err != nil {
		t.Fatal(err)
	}
	if err := s.RetirePendingPairings(3001); err != nil {
		t.Fatal(err)
	}
	after, err = s.GetSession(session.ID)
	if err != nil || after.RevokedAt != 3001 {
		t.Fatal("pending session not revoked", err)
	}
	retired, err := s.GetPairingLink(link.ID)
	if err != nil || retired.CanceledAt != 3001 {
		t.Fatal("pending link not retired", err)
	}
}

func TestPairingLinkRoundTrips(t *testing.T) {
	s, owner, _ := seedOwnerDevice(t)
	seedPairingLink(t, s, owner, []byte("hash-1"), 9000)

	got, err := s.GetPairingLink("link-1")
	if err != nil {
		t.Fatalf("GetPairingLink: %v", err)
	}
	if got.UserID != owner.ID || got.BindingClass != "device-bound" || got.DeviceClass != "phone" {
		t.Fatalf("link did not round-trip: %+v", got)
	}
	if len(got.Scopes) != 2 || got.Scopes[0] != "threads:read" {
		t.Fatalf("scopes = %v", got.Scopes)
	}
	if got.Redeemed() || got.Settled() {
		t.Fatalf("a fresh link reported itself redeemed/settled: %+v", got)
	}
	if got.CertFingerprint != "" {
		t.Fatalf("cert fingerprint = %q, want none for a link minted without one", got.CertFingerprint)
	}
}

// TestPairingLinkRedemptionIsSingleUse — the whole admission story rests on
// the CAS predicate. Two redemptions of one token must produce exactly one
// device, whichever order they reach SQLite in.
func TestPairingLinkRedemptionIsSingleUse(t *testing.T) {
	s, owner, _ := seedOwnerDevice(t)
	seedPairingLink(t, s, owner, []byte("hash-1"), 9000)

	first, err := s.RedeemPairingLink([]byte("hash-1"), 2000, "thumb-a")
	if err != nil {
		t.Fatalf("first RedeemPairingLink: %v", err)
	}
	if first.KeyThumbprint != "thumb-a" {
		t.Fatalf("redemption did not record the key: %+v", first)
	}
	if err := attachPairingRedemption(s.db, "link-1", "dev-a", "sess-a"); err != nil {
		t.Fatalf("AttachPairingRedemption: %v", err)
	}
	if !first.Redeemed() {
		t.Fatal("redeemed link did not report itself redeemed")
	}
	if _, err := s.RedeemPairingLink([]byte("hash-1"), 2001, "thumb-b"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second redemption err = %v, want sql.ErrNoRows", err)
	}
	after, err := s.GetPairingLink("link-1")
	if err != nil {
		t.Fatalf("GetPairingLink: %v", err)
	}
	if after.DeviceID != "dev-a" || after.KeyThumbprint != "thumb-a" {
		t.Fatalf("second redemption overwrote the first: %+v", after)
	}
}

// TestPairingLinkRedemptionUnderConcurrency — the same property under a
// real race rather than in sequence. Exactly one caller may win.
func TestPairingLinkRedemptionUnderConcurrency(t *testing.T) {
	s, owner, _ := seedOwnerDevice(t)
	seedPairingLink(t, s, owner, []byte("hash-1"), 9000)

	const racers = 8
	var wg sync.WaitGroup
	won := make([]bool, racers)
	wg.Add(racers)
	for i := range racers {
		go func() {
			defer wg.Done()
			_, err := s.RedeemPairingLink([]byte("hash-1"), 2000, "thumb")
			won[i] = err == nil
		}()
	}
	wg.Wait()
	winners := 0
	for _, ok := range won {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d racers redeemed one link; exactly one may win", winners)
	}
}

func TestPairingLinkRedemptionRefusesAnExpiredWindow(t *testing.T) {
	s, owner, _ := seedOwnerDevice(t)
	seedPairingLink(t, s, owner, []byte("hash-1"), 3000)

	if _, err := s.RedeemPairingLink([]byte("hash-1"), 3000, "thumb"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("redemption at the expiry instant err = %v, want sql.ErrNoRows", err)
	}
	if _, err := s.RedeemPairingLink([]byte("hash-1"), 2999, "thumb"); err != nil {
		t.Fatalf("redemption inside the window: %v", err)
	}
}

func TestPairingLinkUnknownTokenIsIndistinguishableFromASpentOne(t *testing.T) {
	s, owner, _ := seedOwnerDevice(t)
	seedPairingLink(t, s, owner, []byte("hash-1"), 9000)
	if _, err := s.RedeemPairingLink([]byte("hash-1"), 2000, "thumb"); err != nil {
		t.Fatalf("RedeemPairingLink: %v", err)
	}
	spent := s.mustRedeemErr(t, []byte("hash-1"))
	never := s.mustRedeemErr(t, []byte("hash-never-existed"))
	if spent.Error() != never.Error() {
		t.Fatalf("a spent token answered %v and an unknown one %v; the two must not be distinguishable",
			spent, never)
	}
}

func (s *Store) mustRedeemErr(t *testing.T, hash []byte) error {
	t.Helper()
	_, err := s.RedeemPairingLink(hash, 5000, "thumb")
	if err == nil {
		t.Fatal("redemption succeeded where a refusal was required")
	}
	return err
}

func TestConfirmRedeemedPairingRequiresAnAttachedRedemptionAndHappensOnce(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	seedPairingLink(t, s, owner, []byte("hash-1"), 9000)

	if _, err := s.ConfirmRedeemedPairing("link-1", 2500, accessUntil(900000)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("confirming an unredeemed link err = %v, want sql.ErrNoRows", err)
	}
	if err := s.SetDeviceKeyThumbprint(device.ID, "thumb"); err != nil {
		t.Fatal(err)
	}
	device.KeyThumbprint = "thumb"
	if _, err := s.RedeemPairingLink([]byte("hash-1"), 2000, "thumb"); err != nil {
		t.Fatalf("RedeemPairingLink: %v", err)
	}
	// Spent, but no session attached yet: there is nothing to admit.
	if _, err := s.ConfirmRedeemedPairing("link-1", 2500, accessUntil(900000)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("confirming a redemption with no session err = %v, want sql.ErrNoRows", err)
	}
	pending := newTestSession("sess-pending", owner, device, key, 9000)
	pending.ActivatedAt, pending.Scopes = 0, []string{"threads:read", "files:read"}
	if err := s.CompletePairingRedemption(PairingRedemption{LinkID: "link-1", DeviceSession: DeviceSession{Device: device, Session: pending}}); err != nil {
		t.Fatalf("CompletePairingRedemption: %v", err)
	}
	confirmed, err := s.ConfirmRedeemedPairing("link-1", 2500, accessUntil(900000))
	if err != nil {
		t.Fatalf("ConfirmRedeemedPairing: %v", err)
	}
	if confirmed.Link.ConfirmedAt != 2500 || !confirmed.Link.Settled() {
		t.Fatalf("confirmation did not settle the link: %+v", confirmed)
	}
	if session, err := s.GetSession(pending.ID); err != nil || session.ActivatedAt != 2500 || session.ExpiresAt != 900000 {
		t.Fatalf("confirmation did not activate the session with its access window (%+v, %v)", session, err)
	}
	if _, err := s.ConfirmRedeemedPairing("link-1", 3000, accessUntil(900000)); !errors.Is(err, ErrPairingAlreadyConfirmed) {
		t.Fatalf("second confirmation err = %v, want ErrPairingAlreadyConfirmed", err)
	}
	after, _ := s.GetPairingLink("link-1")
	if after.ConfirmedAt != 2500 {
		t.Fatalf("second confirmation moved the stamp to %d", after.ConfirmedAt)
	}
}

// A repeat confirmation answers from whether the confirmed device can use
// its access at that moment, by its access or by renewal: allowed while
// either has time left and its renewal is unspent, ended once both expired,
// its renewal was spent, or its session was revoked. Nothing is written.
func TestARepeatConfirmationAnswersFromWhetherTheDeviceCanUseItsAccess(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	seedPairingLink(t, s, owner, []byte("hash-1"), 9000)
	if err := s.SetDeviceKeyThumbprint(device.ID, "thumb"); err != nil {
		t.Fatal(err)
	}
	device.KeyThumbprint = "thumb"
	if _, err := s.RedeemPairingLink([]byte("hash-1"), 2000, "thumb"); err != nil {
		t.Fatal(err)
	}
	pending := newTestSession("sess-pending", owner, device, key, 9000)
	pending.ActivatedAt, pending.Scopes = 0, []string{"threads:read", "files:read"}
	// Access until 10000 once confirmed; renewal until 50000.
	refresh := RefreshGrant{Hash: []byte("refresh-1"), CreatedAt: 2000, ExpiresAt: 50000}
	if err := s.CompletePairingRedemption(PairingRedemption{LinkID: "link-1", DeviceSession: DeviceSession{Device: device, Session: pending, Refresh: refresh}}); err != nil {
		t.Fatal(err)
	}
	if access, err := s.PairingAccessOf("link-1", 2400); err != nil || access != PairingNotConfirmed {
		t.Fatalf("before confirmation the access = %v (%v), want PairingNotConfirmed", access, err)
	}
	if _, err := s.ConfirmRedeemedPairing("link-1", 2500, accessUntil(10000)); err != nil {
		t.Fatal(err)
	}
	repeat := func(at int64, wantAccess PairingAccess, wantErr error, when string) {
		t.Helper()
		if access, err := s.PairingAccessOf("link-1", at); err != nil || access != wantAccess {
			t.Fatalf("%s: the access = %v (%v), want %v", when, access, err, wantAccess)
		}
		if _, err := s.ConfirmRedeemedPairing("link-1", at, accessUntil(at+10000)); !errors.Is(err, wantErr) {
			t.Fatalf("%s: a repeat confirmation = %v, want %v", when, err, wantErr)
		}
	}
	repeat(5000, PairingUsable, ErrPairingAlreadyConfirmed, "inside its access")
	repeat(20000, PairingUsable, ErrPairingAlreadyConfirmed, "past its access, inside its renewal")
	repeat(60000, PairingAccessEnded, ErrPairingAccessEnded, "past its access and its renewal")
	if session, err := s.GetSession(pending.ID); err != nil || session.ExpiresAt != 10000 {
		t.Fatalf("the repeat confirmations moved the session to %+v (%v)", session, err)
	}
	if _, err := s.db.Exec(`UPDATE refresh_secrets SET consumed_at = 3000 WHERE session_id = ?`, pending.ID); err != nil {
		t.Fatal(err)
	}
	repeat(20000, PairingAccessEnded, ErrPairingAccessEnded, "past its access, with its renewal spent")
	repeat(5000, PairingUsable, ErrPairingAlreadyConfirmed, "inside its access, with its renewal spent")
	if _, _, err := s.RevokeSession(pending.ID, 5000); err != nil {
		t.Fatal(err)
	}
	repeat(5000, PairingAccessEnded, ErrPairingAccessEnded, "inside its access, with its session revoked")
}

// TestCancelPairingLinkWorksAfterRedemption — the refusal case the
// verification number exists for is a link a device the owner does not
// recognise has ALREADY redeemed. Cancelling only before redemption would
// leave that case with nothing to do.
func TestCancelPairingLinkWorksAfterRedemption(t *testing.T) {
	s, owner, _ := seedOwnerDevice(t)
	seedPairingLink(t, s, owner, []byte("hash-1"), 9000)
	if _, err := s.RedeemPairingLink([]byte("hash-1"), 2000, "thumb"); err != nil {
		t.Fatalf("RedeemPairingLink: %v", err)
	}
	if err := attachPairingRedemption(s.db, "link-1", "dev", "sess"); err != nil {
		t.Fatalf("AttachPairingRedemption: %v", err)
	}
	canceled, err := s.CancelPairingLink("link-1", 2600)
	if err != nil {
		t.Fatalf("CancelPairingLink: %v", err)
	}
	if canceled.Link.SessionID != "sess" {
		t.Fatalf("cancel lost the session the redemption created: %+v", canceled)
	}
	if _, err := s.ConfirmRedeemedPairing("link-1", 2700, accessUntil(900000)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("a canceled link accepted a confirmation")
	}
}

// A cancellation ends the link and its redemption's pending session
// together: a failed session revocation leaves the link open for the
// retry, and the retry ends both.
func TestCancelPairingLinkEndsItsPendingSessionInTheSameTransaction(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	seedPairingLink(t, s, owner, []byte("hash-1"), 9000)
	_, pending := redeemOnto(t, s, []byte("hash-1"), 2000, newTestSession("sess-pending", owner, device, key, 9000))
	mustExec(t, s.db, `CREATE TRIGGER reject_session_revoke BEFORE UPDATE OF revoked_at ON sessions BEGIN SELECT RAISE(ABORT, 'test revocation failure'); END`)
	if _, err := s.CancelPairingLink("link-1", 2600); err == nil {
		t.Fatal("a cancellation whose session revocation failed reported success")
	}
	if link, err := s.GetPairingLink("link-1"); err != nil || link.CanceledAt != 0 {
		t.Fatalf("a failed revocation left the link canceled (%+v, %v)", link, err)
	}
	mustExec(t, s.db, `DROP TRIGGER reject_session_revoke`)
	canceled, err := s.CancelPairingLink("link-1", 2700)
	if err != nil || !canceled.SessionRevoked {
		t.Fatalf("retry = %+v, %v; want the link and its session ended", canceled, err)
	}
	session, err := s.GetSession(pending.ID)
	if err != nil || session.RevokedAt != 2700 {
		t.Fatalf("pending session after the retry = %+v, %v", session, err)
	}
}

func TestCancelPairingLinkBlocksALaterRedemption(t *testing.T) {
	s, owner, _ := seedOwnerDevice(t)
	seedPairingLink(t, s, owner, []byte("hash-1"), 9000)
	if _, err := s.CancelPairingLink("link-1", 1500); err != nil {
		t.Fatalf("CancelPairingLink: %v", err)
	}
	if _, err := s.RedeemPairingLink([]byte("hash-1"), 2000, "thumb"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("a canceled link was still redeemable")
	}
}

func TestDeletePairingLinksKeepsRedeemedOnes(t *testing.T) {
	s, owner, _ := seedOwnerDevice(t)
	seedPairingLink(t, s, owner, []byte("hash-1"), 2000)
	second := PairingLink{
		ID: "link-2", UserID: owner.ID, Scopes: []string{"threads:read"},
		BindingClass: "device-bound", DeviceClass: "browser",
		CreatedAt: 1000, ExpiresAt: 2000,
	}
	if err := s.CreatePairingLink(second, []byte("hash-2")); err != nil {
		t.Fatalf("CreatePairingLink: %v", err)
	}
	if _, err := s.RedeemPairingLink([]byte("hash-2"), 1500, "thumb"); err != nil {
		t.Fatalf("RedeemPairingLink: %v", err)
	}
	dropped, err := s.DeletePairingLinksExpiredBefore(5000)
	if err != nil {
		t.Fatalf("DeletePairingLinksExpiredBefore: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("dropped %d links, want only the unredeemed one", dropped)
	}
	if _, err := s.GetPairingLink("link-2"); err != nil {
		t.Fatalf("the redeemed link went with the prune: %v", err)
	}
}

// refreshHash is the digest a renewal stores for the secret named name.
func refreshHash(name string) []byte {
	digest := sha256.Sum256([]byte(name))
	return digest[:]
}

// seedRefreshSession writes a live session with its first refresh secret,
// hashed from name and valid until expires, in the transaction production
// issues one with.
func seedRefreshSession(t *testing.T, name string, expires int64) (*Store, Session) {
	t.Helper()
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	session := newTestSession("sess-1", owner, device, key, 900000)
	if err := s.CreateDeviceSession(DeviceSession{Device: device, Session: session,
		Refresh: RefreshGrant{Hash: refreshHash(name), CreatedAt: 1000, ExpiresAt: expires}}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return s, session
}

// renew spends session's secret old and issues next at at, as a renewal from
// a client that lets the backend choose its next secret does.
func renew(s *Store, session Session, old, next string, at int64) error {
	_, err := s.RotateRefreshSecret(context.Background(), RefreshRotation{
		SessionID: session.ID, DeviceID: session.DeviceID, OldHash: refreshHash(old), NextHash: refreshHash(next),
		Now: at, AccessUntil: at + 1000000, RefreshUntil: at + 2000000,
	})
	return err
}

func TestRefreshRotationSpendsThePredecessor(t *testing.T) {
	s, session := seedRefreshSession(t, "secret-1", 90000)
	if err := renew(s, session, "secret-1", "secret-2", 2000); err != nil {
		t.Fatalf("renew: %v", err)
	}
	spent, err := s.GetRefreshSecretByHash(refreshHash("secret-1"))
	if err != nil || !spent.Spent() || spent.ConsumedBy != session.DeviceID {
		t.Fatalf("the renewal did not record the spend: %+v (%v)", spent, err)
	}
	if err := renew(s, session, "secret-1", "secret-3", 2500); !errors.Is(err, ErrRefreshReuse) {
		t.Fatalf("a second renewal with the spent secret = %v, want ErrRefreshReuse", err)
	}
}

// TestSpentRefreshSecretStaysReadable — the reuse detector is exactly this
// row surviving its own consumption. If the spend deleted it, a replay
// would read as a credential that never existed.
func TestSpentRefreshSecretStaysReadable(t *testing.T) {
	s, session := seedRefreshSession(t, "secret-1", 90000)
	if err := renew(s, session, "secret-1", "secret-2", 2000); err != nil {
		t.Fatalf("renew: %v", err)
	}
	read, err := s.GetRefreshSecretByHash(refreshHash("secret-1"))
	if err != nil {
		t.Fatalf("GetRefreshSecretByHash: %v", err)
	}
	if !read.Spent() || read.SessionID != session.ID {
		t.Fatalf("spent secret did not survive readable: %+v", read)
	}
	if _, err := s.GetRefreshSecretByHash(refreshHash("never-issued")); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("an unissued hash read as %v, want sql.ErrNoRows", err)
	}
}

func TestRefreshRotationRefusesAnExpiredSecret(t *testing.T) {
	s, session := seedRefreshSession(t, "secret-1", 3000)
	if err := renew(s, session, "secret-1", "secret-2", 3000); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("an expired refresh secret was exchanged: %v", err)
	}
	read, err := s.GetRefreshSecretByHash(refreshHash("secret-1"))
	if err != nil {
		t.Fatalf("GetRefreshSecretByHash: %v", err)
	}
	if read.Spent() {
		t.Fatal("a refused expired secret was marked spent, which would report a later retry as a reuse")
	}
}

func TestRefreshRotationUnderConcurrency(t *testing.T) {
	s, session := seedRefreshSession(t, "secret-1", 90000)
	const racers = 8
	var wg sync.WaitGroup
	won := make([]bool, racers)
	wg.Add(racers)
	for i := range racers {
		go func() {
			defer wg.Done()
			won[i] = renew(s, session, "secret-1", "next-"+strconv.Itoa(i), 2000) == nil
		}()
	}
	wg.Wait()
	winners := 0
	for _, ok := range won {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d racers spent one refresh secret; exactly one may win", winners)
	}
}

func TestEndRefreshFamilyClearsTheOutstandingChain(t *testing.T) {
	s, session := seedRefreshSession(t, "s-1", 90000)
	if err := renew(s, session, "s-1", "s-2", 1500); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if err := renew(s, session, "s-2", "s-3", 1600); err != nil {
		t.Fatalf("renew: %v", err)
	}
	ended, err := s.EndRefreshFamily(session.ID, 2000, "reuse-detected")
	if err != nil {
		t.Fatalf("EndRefreshFamily: %v", err)
	}
	if ended.Outstanding != 1 || !ended.Revoked {
		t.Fatalf("ended %+v, want the 1 outstanding secret spent and the session revoked", ended)
	}
	if after, err := s.GetSession(session.ID); err != nil || after.RevokedAt != 2000 {
		t.Fatalf("the session after its family ended = %+v (%v), want revoked", after, err)
	}
	chain, err := s.ListRefreshSecretsForSession(session.ID)
	if err != nil {
		t.Fatalf("ListRefreshSecretsForSession: %v", err)
	}
	if len(chain) != 3 {
		t.Fatalf("chain has %d rows, want 3", len(chain))
	}
	for _, secret := range chain {
		if !secret.Spent() {
			t.Fatalf("secret %s survived the family spend", secret.ID)
		}
	}
	first, _ := s.GetRefreshSecretByHash(refreshHash("s-1"))
	if first.ConsumedBy != session.DeviceID {
		t.Fatalf("the family spend rewrote an earlier attribution to %q", first.ConsumedBy)
	}
}

// A family end whose session revocation fails spends no secret either, so
// the caller's retry or report starts from the family as it was.
func TestEndRefreshFamilyWhoseRevocationFailsSpendsNothing(t *testing.T) {
	s, session := seedRefreshSession(t, "s-1", 90000)
	mustExec(t, s.db, `CREATE TRIGGER reject_revoke BEFORE UPDATE OF revoked_at ON sessions BEGIN SELECT RAISE(ABORT, 'test revocation failure'); END`)
	if _, err := s.EndRefreshFamily(session.ID, 2000, "reuse-detected"); err == nil {
		t.Fatal("a family end whose revocation failed reported success")
	}
	if held, err := s.GetRefreshSecretByHash(refreshHash("s-1")); err != nil || held.Spent() {
		t.Fatalf("the secret after a failed family end = %+v (%v), want unspent", held, err)
	}
	if after, err := s.GetSession(session.ID); err != nil || after.RevokedAt != 0 {
		t.Fatalf("the session after a failed family end = %+v (%v), want live", after, err)
	}
}

func TestRefreshSecretsCascadeWithTheirSession(t *testing.T) {
	s, session := seedRefreshSession(t, "secret-1", 90000)
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, session.ID); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if _, err := s.GetRefreshSecretByHash(refreshHash("secret-1")); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("a refresh secret outlived the session it renews")
	}
}

func TestDeleteRefreshSecretsKeepsSpentOnesInsideTheirWindow(t *testing.T) {
	s, session := seedRefreshSession(t, "live", 90000)
	if err := renew(s, session, "live", "live-next", 2000); err != nil {
		t.Fatalf("renew: %v", err)
	}
	device, err := s.GetDevice(session.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	other := session
	other.ID = "sess-2"
	if err := s.CreateDeviceSession(DeviceSession{Device: device, Session: other,
		Refresh: RefreshGrant{Hash: refreshHash("stale"), CreatedAt: 1000, ExpiresAt: 3000}}); err != nil {
		t.Fatalf("seed the second session: %v", err)
	}
	dropped, err := s.DeleteRefreshSecretsExpiredBefore(5000)
	if err != nil {
		t.Fatalf("DeleteRefreshSecretsExpiredBefore: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("dropped %d secrets, want only the expired one", dropped)
	}
	if _, err := s.GetRefreshSecretByHash(refreshHash("live")); err != nil {
		t.Fatalf("a spent secret inside its window was pruned, which would blind the reuse detector: %v", err)
	}
}

func TestEnsureChannelDeviceResolvesToOneRow(t *testing.T) {
	s, owner, _ := seedOwnerDevice(t)
	first, err := s.EnsureChannelDevice(owner.ID, "local", "This computer", "desktop", "linux")
	if err != nil {
		t.Fatalf("first EnsureChannelDevice: %v", err)
	}
	second, err := s.EnsureChannelDevice(owner.ID, "local", "A Different Label", "desktop", "linux")
	if err != nil {
		t.Fatalf("second EnsureChannelDevice: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("channel device id moved across boots: %q -> %q", first.ID, second.ID)
	}
	if second.Label != "This computer" {
		t.Fatalf("second call rewrote the label to %q", second.Label)
	}
	if second.Channel != "local" {
		t.Fatalf("channel = %q", second.Channel)
	}
}

func TestEnsureChannelDeviceUnderConcurrency(t *testing.T) {
	s, owner, _ := seedOwnerDevice(t)
	const racers = 8
	var wg sync.WaitGroup
	ids := make([]string, racers)
	errs := make([]error, racers)
	wg.Add(racers)
	for i := range racers {
		go func() {
			defer wg.Done()
			device, err := s.EnsureChannelDevice(owner.ID, "local", "This computer", "desktop", "linux")
			ids[i], errs[i] = device.ID, err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v", i, err)
		}
		if ids[i] != ids[0] {
			t.Fatalf("racers resolved different channel devices: %q vs %q", ids[i], ids[0])
		}
	}
}

func TestPairedDevicesShareTheEmptyChannel(t *testing.T) {
	s, owner, _ := seedOwnerDevice(t)
	if _, err := s.CreateDevice(owner.ID, "Phone", "phone", "ios"); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	if _, err := s.CreateDevice(owner.ID, "Laptop", "browser", "macos"); err != nil {
		t.Fatalf("second CreateDevice: %v", err)
	}
	devices, err := s.ListDevicesForUser(owner.ID)
	if err != nil {
		t.Fatalf("ListDevicesForUser: %v", err)
	}
	if len(devices) != 3 {
		t.Fatalf("listed %d devices, want 3", len(devices))
	}
	for _, device := range devices {
		if device.Channel != "" {
			t.Fatalf("paired device %s carries channel %q", device.Label, device.Channel)
		}
	}
}

func TestSessionActivationGatesLiveness(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	pending := newTestSession("sess-pending", owner, device, key, 900000)
	pending.ActivatedAt = 0
	seedPendingSession(t, s, owner, pending)
	read, err := s.GetSession("sess-pending")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if read.Live(2000) {
		t.Fatal("an unconfirmed session reported itself live")
	}
	if !read.AwaitingConfirmation() {
		t.Fatal("an unconfirmed session did not report itself awaiting confirmation")
	}
	live, err := s.ListLiveSessions(2000)
	if err != nil {
		t.Fatalf("ListLiveSessions: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("ListLiveSessions returned %d unconfirmed rows", len(live))
	}
	moved, err := activateSession(s.db, "sess-pending", 3000, 950000)
	if err != nil {
		t.Fatalf("ActivateSession: %v", err)
	}
	if !moved {
		t.Fatal("activation reported no move")
	}
	read, _ = s.GetSession("sess-pending")
	if !read.Live(3500) || read.ExpiresAt != 950000 {
		t.Fatalf("activation did not open the window: %+v", read)
	}
	if again, _ := activateSession(s.db, "sess-pending", 4000, 999999); again {
		t.Fatal("a second activation moved the stamp")
	}
}

func TestActivateSessionRefusesARevokedRow(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	pending := newTestSession("sess-pending", owner, device, key, 900000)
	pending.ActivatedAt = 0
	seedPendingSession(t, s, owner, pending)
	if _, _, err := s.RevokeSession("sess-pending", 2000); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	moved, err := activateSession(s.db, "sess-pending", 3000, 950000)
	if err != nil {
		t.Fatalf("ActivateSession: %v", err)
	}
	if moved {
		t.Fatal("a confirmation undid a revocation")
	}
}

func TestExtendSessionOnlyMovesALiveWindowForward(t *testing.T) {
	s, session := seedRefreshSession(t, "secret-1", 90000)
	moved, err := s.ExtendSession(session.ID, 950000, 2000)
	if err != nil {
		t.Fatalf("ExtendSession: %v", err)
	}
	if !moved {
		t.Fatal("extending a live session reported no move")
	}
	backwards, err := s.ExtendSession(session.ID, 940000, 2000)
	if err != nil {
		t.Fatalf("ExtendSession backwards: %v", err)
	}
	if backwards {
		t.Fatal("a stale renewal shortened a live window")
	}
	if _, _, err := s.RevokeSession(session.ID, 2500); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	revived, err := s.ExtendSession(session.ID, 990000, 3000)
	if err != nil {
		t.Fatalf("ExtendSession after revoke: %v", err)
	}
	if revived {
		t.Fatal("a renewal resurrected a revoked session")
	}
}

func TestDeleteSessionsExpiredBeforeKeepsLiveAndRevokedRows(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	for id, expires := range map[string]int64{"old": 2000, "live": 900000, "revoked": 900000} {
		session := newTestSession(id, owner, device, key, expires)
		if err := seedLiveSession(s, session); err != nil {
			t.Fatalf("seed session %s: %v", id, err)
		}
	}
	if _, _, err := s.RevokeSession("revoked", 2500); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	dropped, _, err := s.DeleteSessionsExpiredBefore(5000)
	if err != nil {
		t.Fatalf("DeleteSessionsExpiredBefore: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("dropped %d sessions, want only the expired one", dropped)
	}
	if _, err := s.GetSession("revoked"); err != nil {
		t.Fatalf("the revocation record went with the prune: %v", err)
	}
	if _, err := s.GetSession("old"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("the expired session survived the prune")
	}
}

// TestAttachPairingRedemptionHappensOnce — the attach is what makes a
// redeemed link name the rows it produced. A second call must not be able
// to re-point a link at a different device.
func TestAttachPairingRedemptionHappensOnce(t *testing.T) {
	s, owner, _ := seedOwnerDevice(t)
	seedPairingLink(t, s, owner, []byte("hash-1"), 9000)

	if err := attachPairingRedemption(s.db, "link-1", "dev", "sess"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("attaching to an unredeemed link err = %v, want sql.ErrNoRows", err)
	}
	if _, err := s.RedeemPairingLink([]byte("hash-1"), 2000, "thumb"); err != nil {
		t.Fatalf("RedeemPairingLink: %v", err)
	}
	if err := attachPairingRedemption(s.db, "link-1", "dev", "sess"); err != nil {
		t.Fatalf("AttachPairingRedemption: %v", err)
	}
	if err := attachPairingRedemption(s.db, "link-1", "other-dev", "other-sess"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second attach err = %v, want sql.ErrNoRows", err)
	}
	link, _ := s.GetPairingLink("link-1")
	if link.DeviceID != "dev" || link.SessionID != "sess" {
		t.Fatalf("second attach re-pointed the link: %+v", link)
	}
}
