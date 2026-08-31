package store

import (
	"database/sql"
	"errors"
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
		t.Fatalf("cert fingerprint = %q, want empty until phase 5 fills it", got.CertFingerprint)
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
	if err := s.AttachPairingRedemption("link-1", "dev-a", "sess-a"); err != nil {
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

func TestConfirmPairingLinkRequiresARedemptionAndHappensOnce(t *testing.T) {
	s, owner, _ := seedOwnerDevice(t)
	seedPairingLink(t, s, owner, []byte("hash-1"), 9000)

	if _, err := s.ConfirmPairingLink("link-1", 2500); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("confirming an unredeemed link err = %v, want sql.ErrNoRows", err)
	}
	if _, err := s.RedeemPairingLink([]byte("hash-1"), 2000, "thumb"); err != nil {
		t.Fatalf("RedeemPairingLink: %v", err)
	}
	confirmed, err := s.ConfirmPairingLink("link-1", 2500)
	if err != nil {
		t.Fatalf("ConfirmPairingLink: %v", err)
	}
	if confirmed.ConfirmedAt != 2500 || !confirmed.Settled() {
		t.Fatalf("confirmation did not settle the link: %+v", confirmed)
	}
	if _, err := s.ConfirmPairingLink("link-1", 3000); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second confirmation err = %v, want sql.ErrNoRows", err)
	}
	after, _ := s.GetPairingLink("link-1")
	if after.ConfirmedAt != 2500 {
		t.Fatalf("second confirmation moved the stamp to %d", after.ConfirmedAt)
	}
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
	if err := s.AttachPairingRedemption("link-1", "dev", "sess"); err != nil {
		t.Fatalf("AttachPairingRedemption: %v", err)
	}
	canceled, err := s.CancelPairingLink("link-1", 2600)
	if err != nil {
		t.Fatalf("CancelPairingLink: %v", err)
	}
	if canceled.SessionID != "sess" {
		t.Fatalf("cancel lost the session the redemption created: %+v", canceled)
	}
	if _, err := s.ConfirmPairingLink("link-1", 2700); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("a canceled link accepted a confirmation")
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

func seedRefreshSession(t *testing.T) (*Store, Session) {
	t.Helper()
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	session := newTestSession("sess-1", owner, device, key, 900000)
	if err := s.CreateSession(session); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return s, session
}

func TestRefreshSecretRotationSpendsThePredecessor(t *testing.T) {
	s, session := seedRefreshSession(t)
	first, err := s.CreateRefreshSecret(session.ID, []byte("secret-1"), 1000, 90000)
	if err != nil {
		t.Fatalf("CreateRefreshSecret: %v", err)
	}
	spent, err := s.ConsumeRefreshSecret([]byte("secret-1"), 2000, "thumb")
	if err != nil {
		t.Fatalf("ConsumeRefreshSecret: %v", err)
	}
	if spent.ID != first.ID || !spent.Spent() || spent.ConsumedBy != "thumb" {
		t.Fatalf("consumption did not record the spend: %+v", spent)
	}
	if _, err := s.ConsumeRefreshSecret([]byte("secret-1"), 2500, "thumb"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second consumption err = %v, want sql.ErrNoRows", err)
	}
}

// TestSpentRefreshSecretStaysReadable — the reuse detector is exactly this
// row surviving its own consumption. If the spend deleted it, a replay
// would read as a credential that never existed.
func TestSpentRefreshSecretStaysReadable(t *testing.T) {
	s, session := seedRefreshSession(t)
	if _, err := s.CreateRefreshSecret(session.ID, []byte("secret-1"), 1000, 90000); err != nil {
		t.Fatalf("CreateRefreshSecret: %v", err)
	}
	if _, err := s.ConsumeRefreshSecret([]byte("secret-1"), 2000, "thumb"); err != nil {
		t.Fatalf("ConsumeRefreshSecret: %v", err)
	}
	read, err := s.GetRefreshSecretByHash([]byte("secret-1"))
	if err != nil {
		t.Fatalf("GetRefreshSecretByHash: %v", err)
	}
	if !read.Spent() || read.SessionID != session.ID {
		t.Fatalf("spent secret did not survive readable: %+v", read)
	}
	if _, err := s.GetRefreshSecretByHash([]byte("never-issued")); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("an unissued hash read as %v, want sql.ErrNoRows", err)
	}
}

func TestConsumeRefreshSecretRefusesAnExpiredOne(t *testing.T) {
	s, session := seedRefreshSession(t)
	if _, err := s.CreateRefreshSecret(session.ID, []byte("secret-1"), 1000, 3000); err != nil {
		t.Fatalf("CreateRefreshSecret: %v", err)
	}
	if _, err := s.ConsumeRefreshSecret([]byte("secret-1"), 3000, "thumb"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("an expired refresh secret was exchanged")
	}
	read, err := s.GetRefreshSecretByHash([]byte("secret-1"))
	if err != nil {
		t.Fatalf("GetRefreshSecretByHash: %v", err)
	}
	if read.Spent() {
		t.Fatal("a refused expired secret was marked spent, which would report a later retry as a reuse")
	}
}

func TestConsumeRefreshSecretUnderConcurrency(t *testing.T) {
	s, session := seedRefreshSession(t)
	if _, err := s.CreateRefreshSecret(session.ID, []byte("secret-1"), 1000, 90000); err != nil {
		t.Fatalf("CreateRefreshSecret: %v", err)
	}
	const racers = 8
	var wg sync.WaitGroup
	won := make([]bool, racers)
	wg.Add(racers)
	for i := range racers {
		go func() {
			defer wg.Done()
			_, err := s.ConsumeRefreshSecret([]byte("secret-1"), 2000, "thumb")
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
		t.Fatalf("%d racers spent one refresh secret; exactly one may win", winners)
	}
}

func TestSpendRefreshSecretsForSessionClearsTheOutstandingChain(t *testing.T) {
	s, session := seedRefreshSession(t)
	for _, hash := range [][]byte{[]byte("s-1"), []byte("s-2"), []byte("s-3")} {
		if _, err := s.CreateRefreshSecret(session.ID, hash, 1000, 90000); err != nil {
			t.Fatalf("CreateRefreshSecret: %v", err)
		}
	}
	if _, err := s.ConsumeRefreshSecret([]byte("s-1"), 1500, "thumb"); err != nil {
		t.Fatalf("ConsumeRefreshSecret: %v", err)
	}
	moved, err := s.SpendRefreshSecretsForSession(session.ID, 2000, "reuse-detected")
	if err != nil {
		t.Fatalf("SpendRefreshSecretsForSession: %v", err)
	}
	if moved != 2 {
		t.Fatalf("spent %d secrets, want the 2 still outstanding", moved)
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
	first, _ := s.GetRefreshSecretByHash([]byte("s-1"))
	if first.ConsumedBy != "thumb" {
		t.Fatalf("the family spend rewrote an earlier attribution to %q", first.ConsumedBy)
	}
}

func TestRefreshSecretsCascadeWithTheirSession(t *testing.T) {
	s, session := seedRefreshSession(t)
	if _, err := s.CreateRefreshSecret(session.ID, []byte("secret-1"), 1000, 90000); err != nil {
		t.Fatalf("CreateRefreshSecret: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, session.ID); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if _, err := s.GetRefreshSecretByHash([]byte("secret-1")); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("a refresh secret outlived the session it renews")
	}
}

func TestDeleteRefreshSecretsKeepsSpentOnesInsideTheirWindow(t *testing.T) {
	s, session := seedRefreshSession(t)
	if _, err := s.CreateRefreshSecret(session.ID, []byte("live"), 1000, 90000); err != nil {
		t.Fatalf("CreateRefreshSecret: %v", err)
	}
	if _, err := s.CreateRefreshSecret(session.ID, []byte("stale"), 1000, 3000); err != nil {
		t.Fatalf("CreateRefreshSecret: %v", err)
	}
	if _, err := s.ConsumeRefreshSecret([]byte("live"), 2000, "thumb"); err != nil {
		t.Fatalf("ConsumeRefreshSecret: %v", err)
	}
	dropped, err := s.DeleteRefreshSecretsExpiredBefore(5000)
	if err != nil {
		t.Fatalf("DeleteRefreshSecretsExpiredBefore: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("dropped %d secrets, want only the expired one", dropped)
	}
	if _, err := s.GetRefreshSecretByHash([]byte("live")); err != nil {
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
	if err := s.CreateSession(pending); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
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
	moved, err := s.ActivateSession("sess-pending", 3000, 950000)
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
	if again, _ := s.ActivateSession("sess-pending", 4000, 999999); again {
		t.Fatal("a second activation moved the stamp")
	}
}

func TestActivateSessionRefusesARevokedRow(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	pending := newTestSession("sess-pending", owner, device, key, 900000)
	pending.ActivatedAt = 0
	if err := s.CreateSession(pending); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := s.RevokeSession("sess-pending", 2000); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	moved, err := s.ActivateSession("sess-pending", 3000, 950000)
	if err != nil {
		t.Fatalf("ActivateSession: %v", err)
	}
	if moved {
		t.Fatal("a confirmation undid a revocation")
	}
}

func TestExtendSessionOnlyMovesALiveWindowForward(t *testing.T) {
	s, session := seedRefreshSession(t)
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
	if _, err := s.RevokeSession(session.ID, 2500); err != nil {
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
		if err := s.CreateSession(session); err != nil {
			t.Fatalf("CreateSession %s: %v", id, err)
		}
	}
	if _, err := s.RevokeSession("revoked", 2500); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	dropped, err := s.DeleteSessionsExpiredBefore(5000)
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

	if err := s.AttachPairingRedemption("link-1", "dev", "sess"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("attaching to an unredeemed link err = %v, want sql.ErrNoRows", err)
	}
	if _, err := s.RedeemPairingLink([]byte("hash-1"), 2000, "thumb"); err != nil {
		t.Fatalf("RedeemPairingLink: %v", err)
	}
	if err := s.AttachPairingRedemption("link-1", "dev", "sess"); err != nil {
		t.Fatalf("AttachPairingRedemption: %v", err)
	}
	if err := s.AttachPairingRedemption("link-1", "other-dev", "other-sess"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second attach err = %v, want sql.ErrNoRows", err)
	}
	link, _ := s.GetPairingLink("link-1")
	if link.DeviceID != "dev" || link.SessionID != "sess" {
		t.Fatalf("second attach re-pointed the link: %+v", link)
	}
}
