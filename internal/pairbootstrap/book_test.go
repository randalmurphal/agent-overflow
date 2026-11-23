package pairbootstrap

import (
	"errors"
	"runtime"
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
	issued  chan string
}

func newLedger() *ledger { return &ledger{calls: map[string]int{}, issued: make(chan string, 16)} }

func (l *ledger) cancel(id string) error {
	l.mu.Lock()
	l.calls[id]++
	failing := l.failing
	l.mu.Unlock()
	l.issued <- id
	if failing {
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

// gate holds a mint until released. Release runs on every exit of the test,
// so a failure cannot leave a goroutine blocked until the package timeout.
type gate struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGate(t *testing.T) *gate {
	g := &gate{started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(g.open)
	return g
}

func (g *gate) open() { g.once.Do(func() { close(g.release) }) }

func (g *gate) mint(id string) func() (Invitation, error) {
	return func() (Invitation, error) {
		close(g.started)
		<-g.release
		return Invitation{LinkID: id, URL: "https://host/#" + id}, nil
	}
}

func mintOf(id string) func() (Invitation, error) {
	return func() (Invitation, error) { return Invitation{LinkID: id, URL: "https://host/#" + id}, nil }
}

// waitFor yields until cond holds; the test's own timeout bounds it.
func waitFor(cond func() bool) {
	for !cond() {
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
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
	b := NewBook(l.cancel)
	g := newGate(t)
	old := openWindow(t, b, g.mint("late"))
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
	go func() { _, err := b.Reveal(ch.ID, r); revealed <- err }()
	<-g.started
	l.fail(true)
	closed := make(chan error, 1)
	go func() { closed <- b.Close(old.WindowID) }()
	// The window is removed before its mint finishes.
	waitFor(func() bool { return !b.Snapshot(old.WindowID).Open })
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
// open, and holds every later window back until it is durable, so the set of
// unfinished cancellations holds one window's invitation at most.
func TestFailedCancellationStaysRefusedAndBlocksTheNextWindow(t *testing.T) {
	l := newLedger()
	b := NewBook(l.cancel)
	first := openWindow(t, b, mintOf("first"))
	handshake(t, b, newClient(t))
	l.fail(true)
	if err := b.Close(first.WindowID); !errors.Is(err, errStorage) {
		t.Fatalf("close = %v, want the storage error", err)
	}
	if n := l.count("first"); n != 1 {
		t.Fatalf("the close issued %d cancellation statements, want 1", n)
	}
	if confirmed(t, b, "first") {
		t.Fatal("a closed window's invitation was confirmable while its cancellation failed")
	}
	if _, err := b.Open(mintOf("second")); !errors.Is(err, errStorage) {
		t.Fatalf("open with an unfinished cancellation = %v, want the storage error", err)
	}
	if b.Snapshot("").Open {
		t.Fatal("a window opened while an earlier cancellation was unfinished")
	}
	if n := l.count("first"); n != 2 {
		t.Fatalf("the open retried %d times in all, want 2 statements", n)
	}
	if len(b.retiring) != 1 {
		t.Fatalf("%d unfinished cancellations, want 1", len(b.retiring))
	}
	if confirmed(t, b, "first") {
		t.Fatal("a failed open made the earlier invitation confirmable")
	}
	l.fail(false)
	second := openWindow(t, b, mintOf("second"))
	if n := l.count("first"); n != 3 || len(b.retiring) != 0 {
		t.Fatalf("recovered open: %d statements, %d unfinished", n, len(b.retiring))
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

// Expiry retires the window's invitation, and the invitation is refused from
// the expiry instant, before the timer has run.
func TestExpiryRetiresTheInvitation(t *testing.T) {
	l := newLedger()
	b := NewBook(l.cancel)
	w, err := b.open(mintOf("expiring"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close(w.WindowID) })
	handshake(t, b, newClient(t))
	b.mu.Lock()
	b.window.view.ExpiresAtMs = time.Now().UnixMilli() - 1
	b.mu.Unlock()
	if confirmed(t, b, "expiring") {
		t.Fatal("an expired window's invitation was confirmable before its timer ran")
	}
	b.mu.Lock()
	b.window.timer.Reset(0)
	b.mu.Unlock()
	waitFor(func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.window == nil
	})
	// The timer's close holds the settling order until its cancellation
	// returns; taking it here waits for that close to finish.
	b.settle.Lock()
	n := l.count("expiring")
	b.settle.Unlock()
	if n != 1 {
		t.Fatalf("expiry issued %d cancellation statements, want 1", n)
	}
	if _, err := b.Begin(newClient(t).Commitment()); !errors.Is(err, ErrClosed) {
		t.Fatal("expired window accepted request")
	}
}

// A confirmation holds the order that removals and cancellations take, so a
// window cannot be removed between a confirmation's check and its write.
func TestConfirmationIsOrderedAgainstRemoval(t *testing.T) {
	b := NewBook(newLedger().cancel)
	openWindow(t, b, mintOf("open"))
	handshake(t, b, newClient(t))
	err := b.Confirm("open", func() error {
		if b.settle.TryLock() {
			b.settle.Unlock()
			return errors.New("the confirmation ran outside the settling order")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A malformed invitation for the open window is retired at once, and the
// window mints no other.
func TestUnpublishableInvitationIsRetiredAndNotReminted(t *testing.T) {
	l := newLedger()
	b := NewBook(l.cancel)
	var minted atomic.Int32
	openWindow(t, b, func() (Invitation, error) { minted.Add(1); return Invitation{LinkID: "bad"}, nil })
	c := newClient(t)
	ch, err := b.Begin(c.Commitment())
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Reveal(ch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reveal(ch.ID, r); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reveal = %v, want ErrInvalid", err)
	}
	if _, err := b.Reveal(ch.ID, r); !errors.Is(err, ErrInvalid) || minted.Load() != 1 {
		t.Fatalf("retry = %v after %d mints, want ErrInvalid after 1", err, minted.Load())
	}
	if n := l.count("bad"); n != 1 || len(b.retiring) != 0 {
		t.Fatalf("%d statements, %d unfinished; want 1 and 0", n, len(b.retiring))
	}
}
