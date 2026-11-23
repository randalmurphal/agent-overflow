package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/nativenetwork"
	"agent-overflow/internal/pairbootstrap"
	"agent-overflow/internal/store"
)

// openAddressedWindow opens a LAN pairing window on a and checks the address
// it shows.
func openAddressedWindow(t *testing.T, a *App, want string) ComputerPairingWindow {
	t.Helper()
	t.Cleanup(a.stopComputerPairing)
	window, err := a.OpenComputerPairing(connCtx(t), "lan", "full")
	if err != nil {
		t.Fatal(err)
	}
	if window.Address != want {
		t.Fatalf("the window shows %s, want %s", window.Address, want)
	}
	return window
}

// requireWindowRetired checks, once the path that moved the address has
// returned, that the window it opened is gone from the owner and from
// discovery.
func requireWindowRetired(t *testing.T, a *App, window ComputerPairingWindow) {
	t.Helper()
	if status, err := a.ComputerPairingStatus(window.ID); err != nil || status.State != "expired" {
		t.Fatalf("the window after its address moved = %+v (%v), want expired", status, err)
	}
	requireNoPublishedWindow(t, a)
}

// A settings save that moves the listener's port retires the window showing
// the old port before the save returns.
func TestComputerPairingARebindRetiresTheOpenWindow(t *testing.T) {
	remotetest.Require(t)
	b := newNativeNetworkBackend(t)
	window := openAddressedWindow(t, b.app, fmt.Sprintf("https://172.20.0.2:%d", portFromAddr(b.srv.Addr())))
	if err := moveListener(t, b.app); err != nil {
		t.Fatal(err)
	}
	requireWindowRetired(t, b.app, window)
}

// A launcher report that changes the forwarded LAN address retires the
// window showing the old one before the report returns.
func TestComputerPairingANativeAddressChangeRetiresTheOpenWindow(t *testing.T) {
	remotetest.Require(t)
	b, ctx, _, cfg := nativeNetworkBackend(t)
	if err := b.app.ReportNativeNetworkState(ctx, nativeReport(b, cfg)); err != nil {
		t.Fatal(err)
	}
	port := portFromAddr(b.srv.Addr())
	window := openAddressedWindow(t, b.app, fmt.Sprintf("https://192.168.1.55:%d", port))
	moved := nativenetwork.State{Generation: cfg.Generation, Addresses: []string{fmt.Sprintf("https://192.168.1.56:%d", port)}, ScanID: cfg.ScanID}
	if err := b.app.ReportNativeNetworkState(ctx, moved); err != nil {
		t.Fatal(err)
	}
	requireWindowRetired(t, b.app, window)
}

// Losing the launcher's connection, which carried the forwarded LAN
// address, retires the window showing that address before the release
// returns.
func TestComputerPairingLauncherLossRetiresTheOpenWindow(t *testing.T) {
	remotetest.Require(t)
	b, ctx, conn, cfg := nativeNetworkBackend(t)
	if err := b.app.ReportNativeNetworkState(ctx, nativeReport(b, cfg)); err != nil {
		t.Fatal(err)
	}
	window := openAddressedWindow(t, b.app, fmt.Sprintf("https://192.168.1.55:%d", portFromAddr(b.srv.Addr())))
	conn.RunCleanups()
	requireWindowRetired(t, b.app, window)
}

// requireCanceledStanding checks that the owner is shown linkID as
// canceled, with no number, and is not offered it as pending.
func requireCanceledStanding(t *testing.T, a *App, linkID, when string) {
	t.Helper()
	status, err := a.DevicePairingStatus(linkID)
	if err != nil || status.State != pairingStateCanceled || status.VerificationNumber != "" {
		t.Fatalf("%s: status = %+v (%v), want canceled with no number", when, status, err)
	}
	overview, err := a.GetAccessOverview()
	if err != nil {
		t.Fatal(err)
	}
	for _, pending := range overview.PendingPairings {
		if pending.LinkID == linkID {
			t.Fatalf("%s: the owner is offered %s as pending: %+v", when, linkID, pending)
		}
	}
}

// A status read during a retirement shows the invitation canceled, never
// pending, both before the cancellation commits and after it commits while
// the book still holds the retirement.
func TestComputerPairingStatusDuringARetirementShowsItCanceled(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, _ := computerPairingHost(t)
	ledger := countCancellations(a)
	window, err := a.OpenComputerPairing(connCtx(t), "lan", "full")
	if err != nil {
		t.Fatal(err)
	}
	invite := revealOnBook(t, a.computerPairingBook())
	redeemOnHost(t, a, invite, "retiring-device")
	if status, err := a.DevicePairingStatus(invite.LinkID); err != nil || status.State != pairingStateRedeemed || status.VerificationNumber == "" {
		t.Fatalf("before the close: %+v (%v), want redeemed with its number", status, err)
	}
	before, committed := newTestGate(), newTestGate()
	t.Cleanup(before.open)
	t.Cleanup(committed.open)
	ledger.mu.Lock()
	ledger.hold = func(string) { before.wait() }
	ledger.after = func(string) { committed.wait() }
	ledger.mu.Unlock()
	closed := make(chan error, 1)
	go func() { closed <- a.CloseComputerPairing(window.ID) }()
	<-before.reached
	requireRefusedStanding(t, a, invite.LinkID)
	before.open()
	<-committed.reached
	requireCanceledStanding(t, a, invite.LinkID, "after the cancellation committed, before the book released it")
	committed.open()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	requireCanceledStanding(t, a, invite.LinkID, "after the retirement")
}

// A link spent by a redemption that has not yet written its session is not
// ready to approve.
func TestAPairingSpentButNotYetNamingItsSessionIsPending(t *testing.T) {
	spent := store.PairingLink{ID: "spent", CreatedAt: 1, ExpiresAt: 10_000, RedeemedAt: 2}
	if got := pairingState(spent, 3, pairbootstrap.Standing{}); got != pairingStatePending {
		t.Fatalf("a spent, unattached link reads %s, want pending", got)
	}
	spent.SessionID = "session"
	if got := pairingState(spent, 3, pairbootstrap.Standing{}); got != pairingStateRedeemed {
		t.Fatalf("an attached link reads %s, want redeemed", got)
	}
}

// A confirmation that cannot admit its device ends the pairing: the owner is
// told it was not completed, and the pairing reads canceled.
func TestComputerPairingAConfirmationThatCannotAdmitEndsThePairing(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	b, window := computerPairingBackend(t)
	invite, _ := exchangeComputerPairing(t, b)
	redeemComputerInvitation(t, b, invite)
	link, err := b.app.store.GetPairingLink(invite.LinkID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.app.identityState().sessions.RevokeDevice(link.DeviceID); err != nil {
		t.Fatal(err)
	}
	err = b.app.ConfirmDevicePairing(invite.LinkID)
	if !errors.Is(err, identity.ErrPairingNotCompleted) || !strings.Contains(err.Error(), "the pairing was not completed") {
		t.Fatalf("confirming a pairing whose device was revoked = %v, want it reported not completed", err)
	}
	if status, err := b.app.DevicePairingStatus(invite.LinkID); err != nil || status.State != pairingStateCanceled {
		t.Fatalf("the pairing after an incomplete confirmation = %+v (%v), want canceled", status, err)
	}
	if status, err := b.app.ComputerPairingStatus(window.WindowID); err != nil || status.State != "expired" {
		t.Fatalf("the window after an incomplete confirmation = %+v (%v), want expired", status, err)
	}
}

// A confirmation whose write fails changes nothing: the pairing is still
// ready, and the owner's retry admits the device.
func TestComputerPairingAConfirmationWhoseWriteFailsCanBeRetried(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	b, window, path := computerPairingBackendAt(t)
	invite, _ := exchangeComputerPairing(t, b)
	client := redeemComputerInvitation(t, b, invite)
	execOnStore(t, path, `CREATE TRIGGER reject_activation BEFORE UPDATE OF activated_at ON sessions BEGIN SELECT RAISE(ABORT, 'test activation failure'); END`)
	err := b.app.ConfirmDevicePairing(invite.LinkID)
	if err == nil || errors.Is(err, identity.ErrPairingNotCompleted) {
		t.Fatalf("a confirmation whose write failed = %v, want the storage error", err)
	}
	if status, err := b.app.ComputerPairingStatus(window.WindowID); err != nil || status.State != "ready" {
		t.Fatalf("the window after a failed confirmation = %+v (%v), want ready", status, err)
	}
	execOnStore(t, path, `DROP TRIGGER reject_activation`)
	if err := b.app.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatalf("the retry after storage recovered: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.AwaitActivation(ctx); err != nil {
		t.Fatal(err)
	}
}
