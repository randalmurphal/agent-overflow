package identity

import (
	"database/sql"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"

	_ "modernc.org/sqlite"
)

// stragglerFixture is newFixture plus a second handle on the same database
// file, so a test can stage the one row shape no production path is
// allowed to produce: a session that is un-revoked while its device is
// revoked. Staged directly on purpose — the question is what every CONSULT
// does about such a row, not how one got there.
func stragglerFixture(t *testing.T) (*Sessions, *store.Store, *sql.DB, store.User, store.Device) {
	t.Helper()
	path := storetest.ClonePath(t)
	st, err := store.New(path)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions, err := NewSessions(st, testBackendID)
	if err != nil {
		t.Fatalf("NewSessions: %v", err)
	}
	c := &clock{at: time.UnixMilli(1_700_000_000_000)}
	sessions.now = c.now
	owner, err := st.EnsureOwnerUser("Owner")
	if err != nil {
		t.Fatalf("EnsureOwnerUser: %v", err)
	}
	device, err := st.CreateDevice(owner.ID, "This Desktop", string(DeviceDesktop), "linux")
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open the staging handle: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return sessions, st, raw, owner, device
}

// reviveSession un-revokes one session row behind the store's back.
func reviveSession(t *testing.T, raw *sql.DB, sessionID string) {
	t.Helper()
	if _, err := raw.Exec(`UPDATE sessions SET revoked_at = NULL WHERE id = ?`, sessionID); err != nil {
		t.Fatalf("stage the straggler: %v", err)
	}
}

// Revocation is absolute (docs/specs/remote-access.md §2): a session is
// live only while its own row AND its device's are both unrevoked, and
// that conjunction is what every consult reads.

// TestLiveRefusesASessionWhoseDeviceIsRevoked is the enforcement — the
// per-RPC path, the per-presentation path, and the second consult that
// would otherwise come off the fast path.
func TestLiveRefusesASessionWhoseDeviceIsRevoked(t *testing.T) {
	sessions, st, raw, owner, device := stragglerFixture(t)
	session, credential := mustMint(t, sessions, owner, device, time.Hour)

	// The straggler: revoke the device, then put an un-revoked session row
	// back for it. Staged at the store layer on purpose — the question is
	// what every CONSULT does about one, not how one got there.
	if _, err := st.RevokeDevice(device.ID, sessions.Now()); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	reviveSession(t, raw, session.ID)
	sessions.forget(session.ID)

	if _, reason := sessions.Live(session.ID); reason != ReasonRevokedDevice {
		t.Fatalf("Live = %s, want revoked_device", reason)
	}
	if _, reason := sessions.Verify(credential); reason != ReasonRevokedDevice {
		t.Fatalf("Verify = %s, want revoked_device", reason)
	}
	// The second consult would come off the fast path if the first had
	// installed the row. It must not have.
	if _, reason := sessions.Live(session.ID); reason != ReasonRevokedDevice {
		t.Fatalf("second Live = %s, want revoked_device", reason)
	}
	if _, hit := sessions.live[session.ID]; hit {
		t.Fatal("a refused session was installed in the fast path")
	}
}

// TestFastPathEntriesDoNotOutliveTheirDevice — the entry a hit reads
// carries the device stamp from install time, so what keeps it honest is
// that a device revocation sweeps and forgets every session it holds.
func TestFastPathEntriesDoNotOutliveTheirDevice(t *testing.T) {
	sessions, _, _, owner, device := newFixture(t)
	session, _ := mustMint(t, sessions, owner, device, time.Hour)
	if _, reason := sessions.Live(session.ID); reason.Refused() {
		t.Fatalf("Live refused a fresh session: %s", reason)
	}
	if _, hit := sessions.live[session.ID]; !hit {
		t.Fatal("the fast path did not install a live session")
	}
	if _, err := sessions.RevokeDevice(device.ID); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	if _, hit := sessions.live[session.ID]; hit {
		t.Fatal("a device revocation left its session in the fast path")
	}
	if _, reason := sessions.Live(session.ID); !reason.Refused() {
		t.Fatal("the fast path still admits a revoked device's session")
	}
}

// TestRevokeDeviceMovesTheGenerationEvenWhenItSweepsNothing — the guard
// for the straggler that appears AFTER a sweep. A Live() slow path already
// in flight may hold a joined row whose device stamp is now stale; moving
// the generation is what makes it decline to install one. Bumping only per
// swept session would leave exactly the zero-session case unguarded.
func TestRevokeDeviceMovesTheGenerationEvenWhenItSweepsNothing(t *testing.T) {
	sessions, _, _, owner, device := newFixture(t)
	_, _ = mustMint(t, sessions, owner, device, time.Hour)
	if _, err := sessions.RevokeDevice(device.ID); err != nil {
		t.Fatalf("first RevokeDevice: %v", err)
	}
	before := sessions.generationNow()

	revoked, err := sessions.RevokeDevice(device.ID)
	if err != nil {
		t.Fatalf("second RevokeDevice: %v", err)
	}
	if revoked.DeviceMoved || revoked.SessionsEnded != 0 {
		t.Fatalf("second RevokeDevice = %+v, want nothing moved", revoked)
	}
	if sessions.generationNow() == before {
		t.Fatal("a device revocation that swept nothing left the generation where it was")
	}
}

// TestRevokeDeviceReportsWhatItDid — "revoked, N sessions ended, M
// connections closed" and "already revoked, nothing was live" are
// different answers, and the surface has to be able to tell them apart.
func TestRevokeDeviceReportsWhatItDid(t *testing.T) {
	sessions, _, _, owner, device := newFixture(t)
	first, _ := mustMint(t, sessions, owner, device, time.Hour)
	second, _ := mustMint(t, sessions, owner, device, time.Hour)
	closer := &recordingConns{}
	sessions.AttachConns(closer)

	revoked, err := sessions.RevokeDevice(device.ID)
	if err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	if !revoked.DeviceMoved || revoked.SessionsEnded != 2 || revoked.ConnectionsClosed != 2 {
		t.Fatalf("RevokeDevice = %+v, want the device, 2 sessions, 2 connections", revoked)
	}
	closer.mustHaveClosed(t, first.ID, second.ID)

	again, err := sessions.RevokeDevice(device.ID)
	if err != nil {
		t.Fatalf("second RevokeDevice: %v", err)
	}
	if again.DeviceMoved || again.SessionsEnded != 0 || again.ConnectionsClosed != 0 {
		t.Fatalf("second RevokeDevice = %+v, want nothing", again)
	}
}

// TestNoMintPathAdmitsARevokedDevice walks every entry point in this
// package that can produce or renew a credential. The gate is inside the
// chokepoints each of them goes through (store.CreateSession's INSERT
// predicate, ActivateSession's and ExtendSession's UPDATE predicates,
// RotateRefreshSecret's transaction and accessTokensFor's device argument), never in these callers — which is what
// TestEveryCredentialProducingCallGoesThroughAChokepoint pins.
func TestNoMintPathAdmitsARevokedDevice(t *testing.T) {
	t.Run("Mint", func(t *testing.T) {
		sessions, st, _, owner, device := newFixture(t)
		if _, err := st.RevokeDevice(device.ID, sessions.Now()); err != nil {
			t.Fatalf("RevokeDevice: %v", err)
		}
		if _, _, err := sessions.Mint(MintRequest{
			UserID: owner.ID, DeviceID: device.ID,
			BindingClass: BindingDeviceBound, Scopes: []Scope{ScopeThreadsRead},
			TTL: time.Hour,
		}); err == nil {
			t.Fatal("Mint issued a session for a revoked device")
		}
	})

	t.Run("RedeemPairing", func(t *testing.T) {
		sessions, st, _, owner, _ := newFixture(t)
		link := mustMintLink(t, sessions, owner)
		redemption := mustRedeem(t, sessions, link.Token, "thumb-revoked")
		if _, err := sessions.ConfirmPairing(redemption.PairingID); err != nil {
			t.Fatalf("ConfirmPairing: %v", err)
		}
		if _, err := st.RevokeDevice(redemption.DeviceID, sessions.Now()); err != nil {
			t.Fatalf("RevokeDevice: %v", err)
		}
		second := mustMintLink(t, sessions, owner)
		if _, reason := sessions.RedeemPairing(RedemptionRequest{
			Token: second.Token, Proof: bearerProof("thumb-revoked"),
		}); reason != ReasonRevokedDevice {
			t.Fatalf("re-redemption for a revoked device = %s, want revoked_device", reason)
		}
	})

	t.Run("ConfirmPairing", func(t *testing.T) {
		sessions, st, _, owner, _ := newFixture(t)
		link := mustMintLink(t, sessions, owner)
		redemption := mustRedeem(t, sessions, link.Token, "thumb-pending")
		if _, err := st.RevokeDevice(redemption.DeviceID, sessions.Now()); err != nil {
			t.Fatalf("RevokeDevice: %v", err)
		}
		if _, err := sessions.ConfirmPairing(redemption.PairingID); err == nil {
			t.Fatal("ConfirmPairing activated a session for a revoked device")
		}
	})

	t.Run("Refresh", func(t *testing.T) {
		sessions, st, raw, owner, _ := stragglerFixture(t)
		link := mustMintLink(t, sessions, owner)
		redemption := mustRedeem(t, sessions, link.Token, "thumb-refresh")
		if _, err := sessions.ConfirmPairing(redemption.PairingID); err != nil {
			t.Fatalf("ConfirmPairing: %v", err)
		}
		secret := redemption.Tokens.RefreshSecret
		if secret == "" {
			t.Fatal("no refresh secret to rotate")
		}
		if _, err := st.RevokeDevice(redemption.DeviceID, sessions.Now()); err != nil {
			t.Fatalf("RevokeDevice: %v", err)
		}
		// Stage the straggler: the session row survived the sweep.
		reviveSession(t, raw, redemption.Tokens.SessionID)
		sessions.forget(redemption.Tokens.SessionID)
		if _, reason := sessions.Refresh(RefreshRequest{
			Secret: secret, Proof: bearerProof("thumb-refresh"),
		}); !reason.Refused() {
			t.Fatal("rotation renewed a credential for a revoked device")
		}
	})

	t.Run("EnsureLocalChannelSession", func(t *testing.T) {
		sessions, st, _, owner, _ := newFixture(t)
		session, _, err := sessions.EnsureLocalChannelSession(owner.ID)
		if err != nil {
			t.Fatalf("EnsureLocalChannelSession: %v", err)
		}
		if _, err := st.RevokeDevice(session.DeviceID, sessions.Now()); err != nil {
			t.Fatalf("RevokeDevice: %v", err)
		}
		if _, _, err := sessions.EnsureLocalChannelSession(owner.ID); err == nil {
			t.Fatal("the local channel re-minted around a revoked device")
		}
	})
}

// TestRedemptionRacingARevokeLeavesNothingLive is the straggler path the
// repro found: RedeemPairing reads the device row as live, the revoke
// transaction commits and sweeps what it can see, and the redemption's
// CreateSession lands afterwards — a session on a revoked device that no
// later revoke could reach. The insert predicate closes it at the row, and
// the conjunction refuses whatever any other path might still produce.
func TestRedemptionRacingARevokeLeavesNothingLive(t *testing.T) {
	sessions, st, _, owner, _ := newFixture(t)
	// Pair once so the device row exists and can be revoked, then race a
	// second redemption of the SAME key against the revocation.
	first := mustMintLink(t, sessions, owner)
	paired := mustRedeem(t, sessions, first.Token, "thumb-race")
	if _, err := sessions.ConfirmPairing(paired.PairingID); err != nil {
		t.Fatalf("ConfirmPairing: %v", err)
	}

	links := make([]PairingLink, 8)
	for i := range links {
		links[i] = mustMintLink(t, sessions, owner)
	}
	var wg sync.WaitGroup
	wg.Add(len(links) + 1)
	go func() {
		defer wg.Done()
		if _, err := st.RevokeDevice(paired.DeviceID, sessions.Now()); err != nil {
			t.Errorf("RevokeDevice: %v", err)
		}
	}()
	for _, link := range links {
		go func(link PairingLink) {
			defer wg.Done()
			redemption, reason := sessions.RedeemPairing(RedemptionRequest{
				Token: link.Token, Proof: bearerProof("thumb-race"),
			})
			if reason.Refused() {
				return
			}
			_, _ = sessions.ConfirmPairing(redemption.PairingID)
		}(link)
	}
	wg.Wait()

	assertNothingLiveForDevice(t, sessions, st, paired.DeviceID)
}

func assertNothingLiveForDevice(t *testing.T, sessions *Sessions, st *store.Store, deviceID string) {
	t.Helper()
	rows, err := st.ListSessionsForDevice(deviceID)
	if err != nil {
		t.Fatalf("ListSessionsForDevice: %v", err)
	}
	for _, row := range rows {
		sessions.forget(row.ID)
		if _, reason := sessions.Live(row.ID); !reason.Refused() {
			t.Fatalf("session %s of revoked device %s is still live", row.ID, deviceID)
		}
	}
}
