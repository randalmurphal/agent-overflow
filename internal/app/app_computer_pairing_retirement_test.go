package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/pairbootstrap"
	"agent-overflow/internal/transport"
)

// heldMintWindow opens a window on the host's book whose mint waits on the
// returned gate, starts a requester's reveal on it, and returns once the
// mint is held. The minted link ID arrives on the returned channel, and the
// reveal's answer on the other. The gate's release is registered after the
// window's close, so cleanup releases the mint before the close joins it.
func heldMintWindow(t *testing.T, a *App) (pairbootstrap.Snapshot, *testGate, <-chan string, <-chan error) {
	t.Helper()
	book := a.computerPairingBook()
	g := newTestGate()
	minted := make(chan string, 1)
	view, err := book.Open(func(string) (pairbootstrap.Invitation, error) {
		g.wait()
		invite, err := a.mintDevicePairingPurpose("desktop", "full", "", "", true)
		minted <- invite.LinkID
		return pairbootstrap.Invitation{LinkID: invite.LinkID, URL: invite.URL}, err
	}, admitTestWindow(a))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close(view.WindowID) })
	t.Cleanup(g.open)
	c, ch := beginOnBook(t, book)
	r, err := c.Reveal(ch)
	if err != nil {
		t.Fatal(err)
	}
	revealed := make(chan error, 1)
	go func() { _, err := book.Reveal(ch.ID, r, "lan"); revealed <- err }()
	<-g.reached
	return view, g, minted, revealed
}

// requireRefusedStanding checks what the owner is shown for an invitation
// whose row is still open while its cancellation is unfinished: canceled,
// with no number to compare, and absent from the pending pairings.
func requireRefusedStanding(t *testing.T, a *App, linkID string) {
	t.Helper()
	if link, err := a.store.GetPairingLink(linkID); err != nil || link.Settled() {
		t.Fatalf("invitation %s: want its row still unsettled (%+v, %v)", linkID, link, err)
	}
	status, err := a.DevicePairingStatus(linkID)
	if err != nil || status.State != pairingStateCanceled || status.VerificationNumber != "" {
		t.Fatalf("status of %s = %+v, %v; want canceled with no number", linkID, status, err)
	}
	overview, err := a.GetAccessOverview()
	if err != nil {
		t.Fatal(err)
	}
	for _, pending := range overview.PendingPairings {
		if pending.LinkID == linkID {
			t.Fatalf("the owner is offered %s as pending: %+v", linkID, pending)
		}
	}
}

// invitationMint mints nothing a test reveals.
func invitationMint(string) (pairbootstrap.Invitation, error) {
	return pairbootstrap.Invitation{}, errors.New("not minted")
}

func refusedConfirmation(t *testing.T, a *App, linkID, when string) {
	t.Helper()
	if err := a.ConfirmDevicePairing(linkID); !errors.Is(err, identity.ErrPairingRefused) {
		t.Fatalf("%s: confirming %s = %v, want identity.ErrPairingRefused", when, linkID, err)
	}
}

// While the store refuses cancellations and confirmations still write, no
// order of replacement, late mint, redemption, close and recovery makes a
// closed window's invitation confirmable, and each unfinished cancellation
// is kept until its own statement succeeds.
func TestComputerPairingClosedInvitationIsNeverConfirmableWhileCancellationsFail(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, path := computerPairingHost(t)
	ledger := countCancellations(a)
	book := a.computerPairingBook()
	first, g, mintedFirst, revealedFirst := heldMintWindow(t, a)
	restore := rejectCancellations(t, path)
	// Replace the first window while its mint is held.
	replaced := make(chan error, 1)
	go func() {
		_, err := a.OpenComputerPairing(connCtx(t), "lan", "full")
		replaced <- err
	}()
	waitFor(t, "the replacement to remove the first window", func() bool { return !book.Snapshot(first.WindowID).Open })
	g.open()
	if err := <-replaced; err == nil {
		t.Fatal("a window opened while the replaced window's invitation was uncancelled")
	}
	if err := <-revealedFirst; !errors.Is(err, pairbootstrap.ErrClosed) {
		t.Fatalf("the replaced window's late reveal = %v, want ErrClosed", err)
	}
	late := <-mintedFirst
	if book.Snapshot("").Open {
		t.Fatal("a refused replacement left a window open")
	}
	// Nothing can redeem the late invitation, whose token never left the
	// book, so only what the owner is shown is checked here: the redeemed
	// invitation below is the one the book's refusal guards.
	requireRefusedStanding(t, a, late)

	restore()
	second, err := a.OpenComputerPairing(connCtx(t), "lan", "full")
	if err != nil {
		t.Fatalf("the replacement after storage recovered: %v", err)
	}
	requireCanceled(t, a, late)
	invite := revealOnBook(t, book)
	redeemOnHost(t, a, invite, "second-window-device")

	restore = rejectCancellations(t, path)
	if err := a.CloseComputerPairing(second.ID); err == nil {
		t.Fatal("a close whose cancellation the store refused reported success")
	}
	refusedConfirmation(t, a, invite.LinkID, "after its close failed")
	requireRefusedStanding(t, a, invite.LinkID)
	if _, err := a.OpenComputerPairing(connCtx(t), "lan", "full"); err == nil {
		t.Fatal("a third window opened while the second's cancellation failed")
	}
	refusedConfirmation(t, a, invite.LinkID, "after a refused open retried it")

	restore()
	if err := a.CloseComputerPairing(second.ID); err != nil {
		t.Fatalf("closing again after storage recovered: %v", err)
	}
	requireCanceled(t, a, invite.LinkID)
	refusedConfirmation(t, a, invite.LinkID, "after storage recovered")
	// The late invitation: refused in the replacement, durable in the next
	// open. The second window's: its close, the refused open, its close.
	if got, want := [2]int{ledger.count(late), ledger.count(invite.LinkID)}, [2]int{2, 3}; got != want {
		t.Fatalf("cancellation statements (late, second) = %v, want %v", got, want)
	}
}

// A close while its window's mint is in flight answers for the invitation
// that mint makes: the close reports its refused cancellation, and that
// cancellation is kept and retried by every later open until it is durable,
// whatever arrives for the old window meanwhile.
func TestComputerPairingCloseDuringAMintAnswersForTheLateInvitation(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, path := computerPairingHost(t)
	ledger := countCancellations(a)
	book := a.computerPairingBook()
	window, g, minted, revealed := heldMintWindow(t, a)
	restore := rejectCancellations(t, path)
	closed := make(chan error, 1)
	go func() { closed <- a.CloseComputerPairing(window.WindowID) }()
	waitFor(t, "the close to remove the window", func() bool { return !book.Snapshot(window.WindowID).Open })
	g.open()
	if err := <-closed; err == nil || !strings.Contains(err.Error(), "test cancellation failure") {
		t.Fatalf("close during the mint = %v, want the late invitation's storage error", err)
	}
	late := <-minted
	if err := <-revealed; !errors.Is(err, pairbootstrap.ErrClosed) {
		t.Fatalf("late reveal = %v, want ErrClosed", err)
	}
	if n := ledger.count(late); n != 1 {
		t.Fatalf("the close issued %d cancellation statements, want 1", n)
	}
	// A late packet for the old window changes nothing.
	_, _ = (authEndpoints{app: a}).NearbyPair(context.Background(), "", pairbootstrap.Request{Op: "reveal", ID: "late"})
	if _, err := a.OpenComputerPairing(connCtx(t), "lan", "full"); err == nil {
		t.Fatal("a window opened while the late invitation was uncancelled")
	}
	if n := ledger.count(late); n != 2 {
		t.Fatalf("the refused open retried %d statements in all, want 2", n)
	}
	requireRefusedStanding(t, a, late)
	restore()
	next, err := a.OpenComputerPairing(connCtx(t), "lan", "full")
	if err != nil {
		t.Fatalf("open after storage recovered: %v", err)
	}
	requireCanceled(t, a, late)
	if n := ledger.count(late); n != 3 {
		t.Fatalf("%d cancellation statements in all, want 3", n)
	}
	// The next window's invitation is its own and untouched.
	invite := revealOnBook(t, book)
	if status, err := a.ComputerPairingStatus(next.ID); err != nil || status.LinkID != invite.LinkID || status.State == "expired" {
		t.Fatalf("next window = %+v, %v", status, err)
	}
	if n := ledger.count(invite.LinkID); n != 0 {
		t.Fatalf("the next window's invitation had %d cancellation statements", n)
	}
}

// A close answers for the invitation the book published for its window
// even when nothing outside the book has seen that invitation: the store's
// refusal is the close's error, from one statement, and closing again
// retries it.
func TestComputerPairingCloseReportsItsInvitationsRefusedCancellation(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, path := computerPairingHost(t)
	ledger := countCancellations(a)
	window, err := a.OpenComputerPairing(connCtx(t), "lan", "full")
	if err != nil {
		t.Fatal(err)
	}
	invite := revealOnBook(t, a.computerPairingBook())
	restore := rejectCancellations(t, path)
	if err := a.CloseComputerPairing(window.ID); err == nil || !strings.Contains(err.Error(), "test cancellation failure") {
		t.Fatalf("close = %v, want the store's refusal", err)
	}
	if n := ledger.count(invite.LinkID); n != 1 {
		t.Fatalf("the close issued %d cancellation statements, want 1", n)
	}
	restore()
	if err := a.CloseComputerPairing(window.ID); err != nil {
		t.Fatalf("closing again after storage recovered: %v", err)
	}
	if err := a.CloseComputerPairing(window.ID); err != nil || ledger.count(invite.LinkID) != 2 {
		t.Fatalf("a durable cancellation was issued again (%v, %d statements)", err, ledger.count(invite.LinkID))
	}
	requireCanceled(t, a, invite.LinkID)
}

// A close of the first window is admitted before the replacement opens and
// finishes while the replacement is opening: it cancels only the first
// window's invitation, and the replacement opens, reveals, redeems and
// confirms its own.
func TestComputerPairingACloseAdmittedBeforeAReplacementKeepsTheReplacementsInvitation(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, _ := computerPairingHost(t)
	ledger := countCancellations(a)
	book := a.computerPairingBook()
	first, err := a.OpenComputerPairing(connCtx(t), "lan", "full")
	if err != nil {
		t.Fatal(err)
	}
	old := revealOnBook(t, book)
	held := newTestGate()
	t.Cleanup(held.open)
	ledger.mu.Lock()
	ledger.hold = func(id string) {
		if id == old.LinkID {
			held.wait()
		}
	}
	ledger.mu.Unlock()
	closed := make(chan error, 1)
	go func() { closed <- a.CloseComputerPairing(first.ID) }()
	<-held.reached
	type opened struct {
		window ComputerPairingWindow
		err    error
	}
	next := make(chan opened, 1)
	go func() {
		w, err := a.OpenComputerPairing(connCtx(t), "lan", "full")
		next <- opened{w, err}
	}()
	// The replacement is inside its open, behind the admitted close.
	waitFor(t, "the replacement to start opening", func() bool {
		if a.computerPairing.ops.TryLock() {
			a.computerPairing.ops.Unlock()
			return false
		}
		return true
	})
	held.open()
	if err := <-closed; err != nil {
		t.Fatalf("the first window's close: %v", err)
	}
	replacement := <-next
	if replacement.err != nil {
		t.Fatalf("the replacement: %v", replacement.err)
	}
	// The replacement's invitation goes through the HTTP adapter's path.
	endpoints := authEndpoints{app: a}
	c, err := pairbootstrap.Start("desktop", "windows")
	if err != nil {
		t.Fatal(err)
	}
	began, err := endpoints.NearbyPair(context.Background(), "", pairbootstrap.Request{Op: "begin", Commitment: c.Commitment()})
	if err != nil {
		t.Fatal(err)
	}
	ch := began.(pairbootstrap.Challenge)
	r, err := c.Reveal(ch)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := endpoints.NearbyPair(context.Background(), "", pairbootstrap.Request{Op: "reveal", ID: ch.ID, Reveal: r})
	if err != nil {
		t.Fatal(err)
	}
	invite, _, err := c.Open(sealed.(pairbootstrap.SealedInvitation))
	if err != nil {
		t.Fatal(err)
	}
	redeemOnHost(t, a, invite, "replacement-device")
	status, err := a.ComputerPairingStatus(replacement.window.ID)
	if err != nil || status.State != "ready" || status.LinkID != invite.LinkID {
		t.Fatalf("replacement window = %+v, %v; want ready with its invitation", status, err)
	}
	if n := ledger.count(invite.LinkID); n != 0 {
		t.Fatalf("the replacement's invitation had %d cancellation statements", n)
	}
	requireCanceled(t, a, old.LinkID)
	if err := a.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatalf("confirming the replacement's invitation: %v", err)
	}
}

// A cancellation that ends the invitation's row but not its pending session
// is not durable: the whole cancellation is retried until both end, and the
// invitation stays refused meanwhile.
func TestComputerPairingCancellationEndsThePendingSessionWithItsInvitation(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, path := computerPairingHost(t)
	ledger := countCancellations(a)
	window, err := a.OpenComputerPairing(connCtx(t), "lan", "full")
	if err != nil {
		t.Fatal(err)
	}
	invite := revealOnBook(t, a.computerPairingBook())
	redeemOnHost(t, a, invite, "revocation-device")
	execOnStore(t, path, `CREATE TRIGGER reject_session_revoke BEFORE UPDATE OF revoked_at ON sessions BEGIN SELECT RAISE(ABORT, 'test revocation failure'); END`)
	if err := a.CloseComputerPairing(window.ID); err == nil || !strings.Contains(err.Error(), "test revocation failure") {
		t.Fatalf("close = %v, want the refused session revocation", err)
	}
	requireRefusedStanding(t, a, invite.LinkID)
	refusedConfirmation(t, a, invite.LinkID, "while its session revocation fails")
	execOnStore(t, path, `DROP TRIGGER reject_session_revoke`)
	if err := a.CloseComputerPairing(window.ID); err != nil {
		t.Fatalf("closing again after storage recovered: %v", err)
	}
	requireCanceled(t, a, invite.LinkID)
	if n := ledger.count(invite.LinkID); n != 2 {
		t.Fatalf("%d cancellation statements, want 2", n)
	}
}

// A listener move while a window opens changes the address the window
// shows, not whether it opens: the open checked the old address, the move
// published the new one while the open prepared, and the window is admitted
// under the move's publication, shows its address, and stays open after it.
func TestComputerPairingAnOpenAcrossARebindShowsTheMovedAddress(t *testing.T) {
	remotetest.Require(t)
	a := newNativeNetworkBackend(t).app
	t.Cleanup(a.stopComputerPairing)
	conn, err := transport.CallerConnection(connCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	old, err := a.publishComputerRoutes().pairing.on("lan")
	if err != nil {
		t.Fatal(err)
	}
	g := newTestGate()
	t.Cleanup(g.open)
	type opened struct {
		window ComputerPairingWindow
		err    error
	}
	next := make(chan opened, 1)
	go func() {
		w, err := a.openComputerPairing(conn, "lan", "full", "", func() error { g.wait(); return nil })
		next <- opened{w, err}
	}()
	<-g.reached
	if err := moveListener(t, a); err != nil {
		t.Fatal(err)
	}
	g.open()
	result := <-next
	if result.err != nil {
		t.Fatalf("open across a rebind: %v", result.err)
	}
	want, err := a.publishComputerRoutes().pairing.on("lan")
	if err != nil {
		t.Fatal(err)
	}
	if want == old || result.window.Address != want {
		t.Fatalf("the window shows %q (checked %q), want the moved address %q", result.window.Address, old, want)
	}
	if !a.computerPairingBook().Snapshot(result.window.ID).Open {
		t.Fatal("the window admitted under the move's publication was retired")
	}
}

// publishedSerial is the address publication's current serial.
func publishedSerial(a *App) uint64 {
	state := &a.computerRoutesPublished
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.serial
}

// moveListener saves a new listen port, which rebinds the listener.
func moveListener(t *testing.T, a *App) error {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	port := portFromAddr(l.Addr().String())
	if err := l.Close(); err != nil {
		return err
	}
	s := a.persistedNetworkSettings()
	s.ListenPort = port
	_, err = a.SetNetworkSettings(context.Background(), s)
	return err
}

// requireNoPublishedWindow checks that discovery answers no open window and
// that the book holds none.
func requireNoPublishedWindow(t *testing.T, a *App) {
	t.Helper()
	info, err := (authEndpoints{app: a}).NearbyPair(context.Background(), "", pairbootstrap.Request{Op: "info"})
	if err != nil {
		t.Fatal(err)
	}
	if encoded := fmt.Sprintf("%+v", info); !strings.Contains(encoded, "Open:false") {
		t.Fatalf("discovery answers %s, want no open window", encoded)
	}
	if a.computerPairingOpen() {
		t.Fatal("a window with a stale address stayed open")
	}
}

// An unfinished cancellation lives in memory only. A restart retires it
// with every other unconfirmed invitation before sessions are published,
// and a boot whose retirement the store refuses publishes none, so the
// invitation is never confirmable across the restart.
func TestComputerPairingUnfinishedCancellationIsRetiredAcrossRestart(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, path := computerPairingHost(t)
	window, err := a.OpenComputerPairing(connCtx(t), "lan", "full")
	if err != nil {
		t.Fatal(err)
	}
	invite := revealOnBook(t, a.computerPairingBook())
	redeemOnHost(t, a, invite, "restart-device")
	restore := rejectCancellations(t, path)
	if err := a.CloseComputerPairing(window.ID); err == nil {
		t.Fatal("a refused cancellation was reported as success")
	}
	// The first process shuts down: its pairing stops with it, so no retry
	// of its outlives it and nothing but the restart retires the invitation.
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.computerPairingBook().Open(invitationMint, func(pairbootstrap.Snapshot) error { return nil }); !errors.Is(err, pairbootstrap.ErrStopped) {
		t.Fatalf("an open after shutdown = %v, want ErrStopped", err)
	}
	rebooted := newTestAppAtStorePath(t, path)
	if err := rebooted.initIdentity("backend-under-test"); err == nil {
		t.Fatal("boot published sessions without retiring the unfinished invitation")
	}
	if err := rebooted.ConfirmDevicePairing(invite.LinkID); err == nil {
		t.Fatal("an invitation was confirmed on a boot that could not retire it")
	}
	restore()
	if err := rebooted.initIdentity("backend-under-test"); err != nil {
		t.Fatal(err)
	}
	requireCanceled(t, rebooted, invite.LinkID)
	refusedConfirmation(t, rebooted, invite.LinkID, "after restart")
}
