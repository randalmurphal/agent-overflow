package pairbootstrap

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var errStorage = errors.New("test cancellation failure")

// ledger is a book's cancel callback that counts the cancellation
// statements issued for each invitation and fails them while failing is set.
type ledger struct {
	mu      sync.Mutex
	calls   map[string]int
	failing bool
}

func newLedger() *ledger { return &ledger{calls: map[string]int{}} }

func (l *ledger) cancel(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls[id]++
	if l.failing {
		return errStorage
	}
	return nil
}

func (l *ledger) fail(on bool) {
	l.mu.Lock()
	l.failing = on
	l.mu.Unlock()
}

func (l *ledger) count(id string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls[id]
}

// gate holds whatever waits on it until released.
type gate struct {
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGate() *gate { return &gate{reached: make(chan struct{}, 1), release: make(chan struct{})} }

func (g *gate) wait() {
	select {
	case g.reached <- struct{}{}:
	default:
	}
	<-g.release
}

func (g *gate) open() { g.once.Do(func() { close(g.release) }) }

// admitted admits every window.
func admitted(Snapshot) error { return nil }

func mintOf(id string) func(string) (Invitation, error) {
	return func(string) (Invitation, error) { return Invitation{LinkID: id, URL: "https://host/#" + id}, nil }
}

// heldMintWindow opens a window whose mint of id waits on the returned gate,
// starts a requester's reveal on it, and returns once the mint is held. The
// gate's release is registered after the window's close, so cleanup
// releases the mint before the close joins it.
func heldMintWindow(t *testing.T, b *Book, id string) (Snapshot, *gate, <-chan error) {
	t.Helper()
	g := newGate()
	view := openWindow(t, b, func(string) (Invitation, error) {
		g.wait()
		return Invitation{LinkID: id, URL: "https://host/#" + id}, nil
	})
	t.Cleanup(g.open)
	c := newClient(t)
	ch, err := b.Begin(c.Commitment())
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Reveal(ch)
	if err != nil {
		t.Fatal(err)
	}
	revealed := make(chan error, 1)
	go func() { _, err := b.Reveal(ch.ID, r, "lan"); revealed <- err }()
	<-g.reached
	return view, g, revealed
}

// waitFor polls until cond holds. The bound only ends a test whose
// condition never comes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func (b *Book) pendingLink() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pending == nil {
		return ""
	}
	return b.pending.linkID
}

// confirmed reports whether Confirm ran the caller's confirmation for id.
func confirmed(t *testing.T, b *Book, id string) bool {
	t.Helper()
	ran := false
	err := b.Confirm(id, func() error { ran = true; return nil })
	if ran != (err == nil) {
		t.Fatalf("Confirm(%s) = %v with confirmation run %v", id, err, ran)
	}
	if err != nil && !errors.Is(err, ErrClosed) {
		t.Fatalf("Confirm(%s) = %v, want ErrClosed", id, err)
	}
	return ran
}

// A window closed while its mint is in flight waits for that mint, and the
// close answers for the late invitation's cancellation: one statement, its
// error returned, the invitation refused until a later close makes it
// durable.
func TestCloseJoinsAnInFlightMintAndAnswersForItsInvitation(t *testing.T) {
	l := newLedger()
	b := NewBook(l.cancel, time.Hour)
	old, g, revealed := heldMintWindow(t, b, "late")
	l.fail(true)
	closed := make(chan error, 1)
	go func() { closed <- b.Close(old.WindowID) }()
	// The window is removed before its mint finishes.
	waitFor(t, "the close to remove the window", func() bool { return !b.Snapshot(old.WindowID).Open })
	g.open()
	if err := <-closed; !errors.Is(err, errStorage) {
		t.Fatalf("close during the mint = %v, want the late invitation's cancellation error", err)
	}
	if err := <-revealed; !errors.Is(err, ErrClosed) {
		t.Fatalf("late reveal = %v, want ErrClosed", err)
	}
	if n := l.count("late"); n != 1 {
		t.Fatalf("the close issued %d cancellation statements, want 1", n)
	}
	if confirmed(t, b, "late") {
		t.Fatal("a closed window's late invitation was confirmable")
	}
	l.fail(false)
	if err := b.Close(old.WindowID); err != nil {
		t.Fatalf("closing again after storage recovered = %v", err)
	}
	if n := l.count("late"); n != 2 {
		t.Fatalf("closing again issued %d statements in all, want 2", n)
	}
	if err := b.Close(old.WindowID); err != nil || l.count("late") != 2 {
		t.Fatalf("a durable cancellation was issued again (%v, %d statements)", err, l.count("late"))
	}
}

// A failed cancellation keeps its invitation refused, is retried by the next
// open, and holds every later window back until it is durable.
func TestFailedCancellationStaysRefusedAndBlocksTheNextWindow(t *testing.T) {
	l := newLedger()
	b := NewBook(l.cancel, time.Hour)
	first := openWindow(t, b, mintOf("first"))
	handshake(t, b, newClient(t))
	l.fail(true)
	if err := b.Close(first.WindowID); !errors.Is(err, errStorage) {
		t.Fatalf("close = %v, want the storage error", err)
	}
	if n := l.count("first"); n != 1 {
		t.Fatalf("the close issued %d cancellation statements, want 1", n)
	}
	if confirmed(t, b, "first") || !b.Refuses("first") {
		t.Fatal("a closed window's invitation was confirmable while its cancellation failed")
	}
	if _, err := b.Open(mintOf("second"), admitted); !errors.Is(err, errStorage) {
		t.Fatalf("open with an unfinished cancellation = %v, want the storage error", err)
	}
	if b.Snapshot("").Open {
		t.Fatal("a window opened while an earlier cancellation was unfinished")
	}
	if n := l.count("first"); n != 2 {
		t.Fatalf("the open retried %d times in all, want 2 statements", n)
	}
	if confirmed(t, b, "first") {
		t.Fatal("a failed open made the earlier invitation confirmable")
	}
	l.fail(false)
	second := openWindow(t, b, mintOf("second"))
	if n := l.count("first"); n != 3 || b.pendingLink() != "" {
		t.Fatalf("recovered open: %d statements, %q pending", n, b.pendingLink())
	}
	// Durable now: the store, not the book, refuses it from here.
	if !confirmed(t, b, "first") {
		t.Fatal("a durably cancelled invitation is still held by the book")
	}
	handshake(t, b, newClient(t))
	if !confirmed(t, b, "second") {
		t.Fatal("the open window's invitation was refused")
	}
	// A stale close of the first window touches nothing of the second.
	if err := b.Close(first.WindowID); err != nil || l.count("second") != 0 || !b.Snapshot(second.WindowID).Open {
		t.Fatalf("stale close: %v, %d statements for the replacement", err, l.count("second"))
	}
}

// A cancellation that failed after its window was removed is retried by the
// book's retry timer, the wait doubling each time, until it is durable, and
// the retries end there.
func TestAFailedCancellationRetriesItselfUntilDurable(t *testing.T) {
	l := newLedger()
	b := NewBook(l.cancel, 5*time.Millisecond)
	w := openWindow(t, b, mintOf("retried"))
	handshake(t, b, newClient(t))
	l.fail(true)
	if err := b.Close(w.WindowID); !errors.Is(err, errStorage) {
		t.Fatalf("close = %v, want the storage error", err)
	}
	waitFor(t, "two retries", func() bool { return l.count("retried") >= 3 })
	b.mu.Lock()
	wait := b.pending.wait
	b.mu.Unlock()
	if wait < 20*time.Millisecond {
		t.Fatalf("after two retries the next waits %v, want it doubled from 5ms at least twice", wait)
	}
	if confirmed(t, b, "retried") {
		t.Fatal("an invitation was confirmable while its retries failed")
	}
	l.fail(false)
	waitFor(t, "a durable retry", func() bool { return b.pendingLink() == "" })
}

// A cancellation made durable by a close while a retry is scheduled ends
// the schedule.
func TestADurableCloseEndsTheRetrySchedule(t *testing.T) {
	l := newLedger()
	b := NewBook(l.cancel, time.Hour)
	w := openWindow(t, b, mintOf("retried"))
	handshake(t, b, newClient(t))
	l.fail(true)
	if err := b.Close(w.WindowID); !errors.Is(err, errStorage) {
		t.Fatalf("close = %v, want the storage error", err)
	}
	b.mu.Lock()
	timer := b.retryTimer
	b.mu.Unlock()
	l.fail(false)
	if err := b.Close(w.WindowID); err != nil {
		t.Fatal(err)
	}
	if timer.Stop() {
		t.Fatal("a retry stayed scheduled after the cancellation was durable")
	}
}

// Expiry retires the invitation through the window's real timer.
func TestExpiryRetiresTheInvitation(t *testing.T) {
	l := newLedger()
	b := NewBook(l.cancel, time.Hour)
	w, err := b.open(mintOf("expiring"), admitted, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close(w.WindowID) })
	handshake(t, b, newClient(t))
	waitFor(t, "expiry to cancel the invitation", func() bool { return l.count("expiring") == 1 })
	if b.Snapshot(w.WindowID).Open {
		t.Fatal("expired window remained open")
	}
	if _, err := b.Begin(newClient(t).Commitment()); !errors.Is(err, ErrClosed) {
		t.Fatal("expired window accepted request")
	}
}

// The invitation of a window past its expiry is refused from that instant,
// before the timer has run.
func TestAnExpiredWindowsInvitationIsRefusedBeforeItsTimerRuns(t *testing.T) {
	b := NewBook(newLedger().cancel, time.Hour)
	openWindow(t, b, mintOf("expiring"))
	handshake(t, b, newClient(t))
	b.mu.Lock()
	b.window.view.ExpiresAtMs = time.Now().UnixMilli() - 1
	b.mu.Unlock()
	if confirmed(t, b, "expiring") || !b.Refuses("expiring") {
		t.Fatal("an expired window's invitation was confirmable before its timer ran")
	}
}

// A removal waits for a confirmation already writing: the window stays open
// until the confirmation returns.
func TestARemovalWaitsForARunningConfirmation(t *testing.T) {
	b := NewBook(newLedger().cancel, time.Hour)
	w := openWindow(t, b, mintOf("open"))
	handshake(t, b, newClient(t))
	removalWaits := make(chan struct{}, 1)
	b.settle.waiting = func() {
		select {
		case removalWaits <- struct{}{}:
		default:
		}
	}
	writing, release := newGate(), newGate()
	t.Cleanup(release.open)
	confirmedErr := make(chan error, 1)
	go func() {
		confirmedErr <- b.Confirm("open", func() error { writing.open(); release.wait(); return nil })
	}()
	<-writing.release
	closed := make(chan error, 1)
	go func() { closed <- b.Close(w.WindowID) }()
	select {
	case <-removalWaits:
	case err := <-closed:
		t.Fatalf("the window was removed while a confirmation was writing (close = %v)", err)
	}
	if !b.Snapshot(w.WindowID).Open {
		t.Fatal("the window closed while a confirmation was writing")
	}
	release.open()
	if err := <-confirmedErr; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

// The deadline is checked when an approval is admitted. An approval admitted
// before it completes though its write runs past it: the window's real
// expiry waits for the write, and only then issues its one cancellation,
// which the settled link answers as done.
func TestAnApprovalAdmittedBeforeTheDeadlineCompletesAfterIt(t *testing.T) {
	l := newLedger()
	b := NewBook(l.cancel, time.Hour)
	expiryWaits := make(chan struct{}, 1)
	b.settle.waiting = func() {
		select {
		case expiryWaits <- struct{}{}:
		default:
		}
	}
	w, err := b.open(mintOf("approved"), admitted, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close(w.WindowID) })
	handshake(t, b, newClient(t))
	var wroteAfterDeadline bool
	err = b.Confirm("approved", func() error {
		// The write is held until the window's own timer has fired and is
		// waiting to remove the window.
		select {
		case <-expiryWaits:
		case <-time.After(30 * time.Second):
			return errors.New("the window's expiry never came")
		}
		if l.count("approved") != 0 {
			return errors.New("the expiry cancelled the invitation while its approval was writing")
		}
		wroteAfterDeadline = time.Now().UnixMilli() >= w.ExpiresAtMs
		return nil
	})
	if err != nil {
		t.Fatalf("an approval admitted before the deadline = %v, want it to complete", err)
	}
	if !wroteAfterDeadline {
		t.Fatal("the approval's write did not run past the deadline")
	}
	waitFor(t, "the expiry's cancellation", func() bool { return l.count("approved") == 1 })
	waitFor(t, "the expiry to finish", func() bool { return b.pendingLink() == "" })
	if b.Snapshot(w.WindowID).Open {
		t.Fatal("the window stayed open past its expiry")
	}
}

// A replacement waits for an earlier close that is still joining its
// window's mint, so it sees the late invitation and opens nothing while
// that cancellation fails.
func TestAReplacementWaitsForAnEarlierCloseToFinish(t *testing.T) {
	l := newLedger()
	b := NewBook(l.cancel, time.Hour)
	old, g, revealed := heldMintWindow(t, b, "late")
	l.fail(true)
	closed := make(chan error, 1)
	go func() { closed <- b.Close(old.WindowID) }()
	waitFor(t, "the close to remove the window", func() bool { return !b.Snapshot(old.WindowID).Open })
	replacementWaits := make(chan struct{}, 1)
	// Set before the replacement starts; the close holds turn already.
	b.turn.waiting = func() {
		select {
		case replacementWaits <- struct{}{}:
		default:
		}
	}
	opened := make(chan error, 1)
	go func() {
		w, err := b.Open(mintOf("next"), admitted)
		if err == nil {
			_ = b.Close(w.WindowID)
		}
		opened <- err
	}()
	select {
	case <-replacementWaits:
	case err := <-opened:
		t.Fatalf("the replacement ran while the earlier close was joining its mint (open = %v)", err)
	}
	g.open()
	if err := <-closed; !errors.Is(err, errStorage) {
		t.Fatalf("close = %v, want the late invitation's storage error", err)
	}
	<-revealed
	if err := <-opened; !errors.Is(err, errStorage) {
		t.Fatalf("replacement = %v, want refused while the late invitation is uncancelled", err)
	}
	if b.pendingLink() != "late" {
		t.Fatalf("pending = %q, want the late invitation", b.pendingLink())
	}
}

// A malformed invitation for the open window is retired at once, and the
// window mints no other.
func TestUnpublishableInvitationIsRetiredAndNotReminted(t *testing.T) {
	l := newLedger()
	b := NewBook(l.cancel, time.Hour)
	var minted atomic.Int32
	openWindow(t, b, func(string) (Invitation, error) { minted.Add(1); return Invitation{LinkID: "bad"}, nil })
	c := newClient(t)
	ch, err := b.Begin(c.Commitment())
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Reveal(ch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reveal(ch.ID, r, "lan"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reveal = %v, want ErrInvalid", err)
	}
	if _, err := b.Reveal(ch.ID, r, "lan"); !errors.Is(err, ErrInvalid) || minted.Load() != 1 {
		t.Fatalf("retry = %v after %d mints, want ErrInvalid after 1", err, minted.Load())
	}
	if n := l.count("bad"); n != 1 || b.pendingLink() != "" {
		t.Fatalf("%d statements, %q pending; want 1 and none", n, b.pendingLink())
	}
}

// The admitted reveal's route reaches its mint; a refused reveal on another
// route changes nothing.
func TestTheAdmittedRevealsRouteReachesTheMint(t *testing.T) {
	b := settledBook()
	var routes []string
	openWindow(t, b, func(route string) (Invitation, error) {
		routes = append(routes, route)
		return Invitation{LinkID: "link", URL: "https://host/#link"}, nil
	})
	c := newClient(t)
	ch, err := b.Begin(c.Commitment())
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Reveal(ch)
	if err != nil {
		t.Fatal(err)
	}
	forged := r
	forged.Label = "someone else"
	if _, err := b.Reveal(ch.ID, forged, "lan"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("forged reveal = %v, want ErrInvalid", err)
	}
	if _, err := b.Reveal(ch.ID, r, "tailnet"); err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0] != "tailnet" {
		t.Fatalf("mint routes = %v, want [tailnet]", routes)
	}
}

// A book refuses to be built without a cancel, and a window without a mint
// or an admission replaces nothing.
func TestTheBookRequiresItsCallbacks(t *testing.T) {
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("NewBook accepted a nil cancel")
			}
		}()
		NewBook(nil, time.Hour)
	}()
	b := settledBook()
	w := openWindow(t, b, mintOf("kept"))
	if _, err := b.Open(nil, admitted); err == nil {
		t.Fatal("Open accepted a nil mint")
	}
	if _, err := b.Open(mintOf("unadmitted"), nil); err == nil {
		t.Fatal("Open accepted a nil admission")
	}
	if !b.Snapshot(w.WindowID).Open {
		t.Fatal("a refused open replaced the open window")
	}
}

// A window is invisible until its admission returns, and one whose admission
// refuses never becomes visible; the window it replaces is closed either way.
func TestAWindowIsVisibleOnlyOnceAdmitted(t *testing.T) {
	l := newLedger()
	b := NewBook(l.cancel, time.Hour)
	old := openWindow(t, b, mintOf("old"))
	handshake(t, b, newClient(t))
	refused := errors.New("not admitted")
	var seen Snapshot
	_, err := b.Open(mintOf("new"), func(w Snapshot) error {
		seen = w
		if b.Snapshot("").Open {
			return errors.New("a window was visible during an admission")
		}
		if _, err := b.Begin(newClient(t).Commitment()); !errors.Is(err, ErrClosed) {
			return errors.New("a requester reached a window not yet admitted")
		}
		return refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("an open whose admission refused = %v, want the refusal", err)
	}
	if seen.WindowID == "" || b.Snapshot("").Open {
		t.Fatalf("an unadmitted window became visible (admitted %+v)", seen)
	}
	if b.Snapshot(old.WindowID).Open || l.count("old") != 1 {
		t.Fatal("the replaced window was not retired")
	}
	w, err := b.Open(mintOf("next"), func(w Snapshot) error { seen = w; return nil })
	if err != nil || w != seen || !b.Snapshot(w.WindowID).Open {
		t.Fatalf("an admitted open = %+v %v, admitted %+v", w, err, seen)
	}
	t.Cleanup(func() { _ = b.Close(w.WindowID) })
}

// Stop retires the open window and its invitation, then refuses opens and
// closes and leaves no retry armed; an unfinished cancellation is answered.
func TestStopRetiresTheWindowAndEndsEveryRetry(t *testing.T) {
	l := newLedger()
	b := NewBook(l.cancel, 5*time.Millisecond)
	w := openWindow(t, b, mintOf("stopped"))
	handshake(t, b, newClient(t))
	l.fail(true)
	if err := b.Close(w.WindowID); !errors.Is(err, errStorage) {
		t.Fatalf("close = %v, want the storage error", err)
	}
	waitFor(t, "a retry", func() bool { return l.count("stopped") >= 2 })
	if err := b.Stop(); !errors.Is(err, errStorage) {
		t.Fatalf("stop with an unfinished cancellation = %v, want the storage error", err)
	}
	if !b.Refuses("stopped") {
		t.Fatal("stop released an invitation whose cancellation is unfinished")
	}
	b.mu.Lock()
	timer := b.retryTimer
	b.mu.Unlock()
	if timer.Stop() {
		t.Fatal("a retry stayed armed after stop")
	}
	n := l.count("stopped")
	if _, err := b.Open(mintOf("after"), admitted); !errors.Is(err, ErrStopped) {
		t.Fatalf("open after stop = %v, want ErrStopped", err)
	}
	if err := b.Close(w.WindowID); !errors.Is(err, ErrStopped) {
		t.Fatalf("close after stop = %v, want ErrStopped", err)
	}
	// A retry that fired before stop and took turn after it issues nothing.
	b.retryPending()
	if got := l.count("stopped"); got != n {
		t.Fatalf("a stopped book issued %d more cancellation statements", got-n)
	}
}

// Stop retires an open window's invitation durably when it can.
func TestStopRetiresTheOpenWindow(t *testing.T) {
	l := newLedger()
	b := NewBook(l.cancel, time.Hour)
	w := openWindow(t, b, mintOf("open"))
	handshake(t, b, newClient(t))
	if err := b.Stop(); err != nil {
		t.Fatal(err)
	}
	if b.Snapshot(w.WindowID).Open || l.count("open") != 1 || b.pendingLink() != "" {
		t.Fatalf("stop left the window open or its invitation unretired (%d statements)", l.count("open"))
	}
	b.mu.Lock()
	kept := b.window
	b.mu.Unlock()
	if kept != nil {
		t.Fatal("stop kept a window")
	}
}

// A removal waits for a Read, so the standing a Read hands out holds for
// everything read inside it.
func TestARemovalWaitsForARead(t *testing.T) {
	b := NewBook(newLedger().cancel, time.Hour)
	w := openWindow(t, b, mintOf("read"))
	handshake(t, b, newClient(t))
	removalWaits := make(chan struct{}, 1)
	b.settle.waiting = func() {
		select {
		case removalWaits <- struct{}{}:
		default:
		}
	}
	reading, release := newGate(), newGate()
	t.Cleanup(release.open)
	var standing Standing
	read := make(chan error, 1)
	go func() {
		read <- b.Read(func(s Standing) error { standing = s; reading.open(); release.wait(); return nil })
	}()
	<-reading.release
	closed := make(chan error, 1)
	go func() { closed <- b.Close(w.WindowID) }()
	select {
	case <-removalWaits:
	case err := <-closed:
		t.Fatalf("the window was removed during a read (close = %v)", err)
	}
	release.open()
	if err := <-read; err != nil {
		t.Fatal(err)
	}
	if standing.Refuses("read") || standing.Window().LinkID != "read" {
		t.Fatalf("the read's standing = %+v, want the open window's live invitation", standing)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

// A Read during an unfinished retirement refuses that invitation, and one
// after it finished no longer names it.
func TestAReadRefusesAnUnfinishedRetirement(t *testing.T) {
	l := newLedger()
	b := NewBook(l.cancel, time.Hour)
	w := openWindow(t, b, mintOf("retiring"))
	handshake(t, b, newClient(t))
	l.fail(true)
	if err := b.Close(w.WindowID); !errors.Is(err, errStorage) {
		t.Fatalf("close = %v, want the storage error", err)
	}
	var during, after Standing
	_ = b.Read(func(s Standing) error { during = s; return nil })
	if !during.Refuses("retiring") || during.Window().Open {
		t.Fatalf("a read during the retirement = %+v, want it refused and no window", during)
	}
	l.fail(false)
	if err := b.Close(w.WindowID); err != nil {
		t.Fatal(err)
	}
	_ = b.Read(func(s Standing) error { after = s; return nil })
	if after.Refuses("retiring") {
		t.Fatal("a durable retirement is still refused by the book")
	}
}
