package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/pairbootstrap"
)

// heldMintWindow opens a window on the host's book whose mint waits on g,
// starts a requester's reveal on it, and returns once the mint is held. The
// minted link ID arrives on the returned channel, and the reveal's answer
// on the other.
func heldMintWindow(t *testing.T, a *App, g *testGate) (pairbootstrap.Snapshot, <-chan string, <-chan error) {
	t.Helper()
	book := a.computerPairingBook()
	minted := make(chan string, 1)
	view, err := book.Open(func() (pairbootstrap.Invitation, error) {
		g.wait()
		invite, err := a.mintDevicePairing("desktop", "full", "")
		minted <- invite.LinkID
		return pairbootstrap.Invitation{LinkID: invite.LinkID, URL: invite.URL}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close(view.WindowID) })
	s := &a.computerPairing
	s.mu.Lock()
	s.windowID = view.WindowID
	s.mu.Unlock()
	c, ch := beginOnBook(t, book)
	r, err := c.Reveal(ch)
	if err != nil {
		t.Fatal(err)
	}
	revealed := make(chan error, 1)
	go func() { _, err := book.Reveal(ch.ID, r); revealed <- err }()
	<-g.reached
	return view, minted, revealed
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
	g := newTestGate(t)
	first, mintedFirst, revealedFirst := heldMintWindow(t, a, g)
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
	refusedConfirmation(t, a, late, "late invitation while its cancellation fails")

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
	g := newTestGate(t)
	window, minted, revealed := heldMintWindow(t, a, g)
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
	refusedConfirmation(t, a, late, "late invitation while its cancellation fails")
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

// Try again closes the old window and opens a new one at the same time.
// The old window's close, held inside its cancellation while the
// replacement opens, cancels only the old window's invitation: the
// replacement's invitation, revealed afterwards, stays ready and
// confirmable, and the old close arriving again leaves it alone.
func TestComputerPairingTryAgainKeepsTheReplacementsInvitation(t *testing.T) {
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
	g := newTestGate(t)
	ledger.mu.Lock()
	ledger.hold = func(id string) {
		if id == old.LinkID {
			g.wait()
		}
	}
	ledger.mu.Unlock()
	closed := make(chan error, 1)
	go func() { closed <- a.CloseComputerPairing(first.ID) }()
	<-g.reached
	type opened struct {
		window ComputerPairingWindow
		err    error
	}
	replaced := make(chan opened, 1)
	go func() {
		w, err := a.OpenComputerPairing(connCtx(t), "lan", "full")
		replaced <- opened{w, err}
	}()
	g.open()
	if err := <-closed; err != nil {
		t.Fatalf("closing the old window: %v", err)
	}
	next := <-replaced
	if next.err != nil {
		t.Fatalf("opening the replacement: %v", next.err)
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
	if err := a.CloseComputerPairing(first.ID); err != nil {
		t.Fatalf("the old close arriving again: %v", err)
	}
	status, err := a.ComputerPairingStatus(next.window.ID)
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
