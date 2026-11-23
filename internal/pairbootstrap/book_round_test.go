package pairbootstrap

import (
	"errors"
	"testing"
	"time"
)

// A second Stop touches nothing: it issues no cancellation statement and
// returns the first Stop's answer.
func TestALaterStopReturnsTheFirstAnswerAndTouchesNothing(t *testing.T) {
	l := newLedger()
	b := NewBook(l.cancel, time.Hour)
	openWindow(t, b, mintOf("stopping"))
	handshake(t, b, newClient(t))
	l.fail(true)
	first := b.Stop()
	if !errors.Is(first, errStorage) {
		t.Fatalf("stop with a failing retirement = %v, want the storage error", first)
	}
	n := l.count("stopping")
	l.fail(false)
	if second := b.Stop(); second != first {
		t.Fatalf("a second stop = %v, want the first answer %v", second, first)
	}
	if got := l.count("stopping"); got != n {
		t.Fatalf("a second stop issued %d more cancellation statements", got-n)
	}
}

// Expire refuses a window at once, as its expiry would: its published
// invitation is refused, it reads closed, and it admits no request. The
// window's close still retires the invitation.
func TestExpireRefusesTheWindowAtOnce(t *testing.T) {
	l := newLedger()
	b := NewBook(l.cancel, time.Hour)
	w := openWindow(t, b, mintOf("expiring"))
	handshake(t, b, newClient(t))
	if !confirmed(t, b, "expiring") {
		t.Fatal("the open window's invitation was refused before Expire")
	}
	b.Expire("another-window")
	if !b.Snapshot(w.WindowID).Open {
		t.Fatal("Expire of another id closed the window")
	}
	b.Expire(w.WindowID)
	if confirmed(t, b, "expiring") {
		t.Fatal("an expired window's invitation was admitted")
	}
	if b.Snapshot(w.WindowID).Open {
		t.Fatal("an expired window reads open")
	}
	if _, err := b.Begin(newClient(t).Commitment()); !errors.Is(err, ErrClosed) {
		t.Fatalf("a request to an expired window = %v, want ErrClosed", err)
	}
	if err := b.Close(w.WindowID); err != nil || l.count("expiring") != 1 {
		t.Fatalf("the close after Expire = %v with %d statements, want one durable cancellation", err, l.count("expiring"))
	}
}

// A reveal's mint and its store write run while a Read holds the book, and
// only the publication of the link and its digits waits for the Read: the
// Read sees the window without them, and the next Read sees both.
func TestARevealPublishesOnlyBetweenReads(t *testing.T) {
	b := NewBook(newLedger().cancel, time.Hour)
	minted := make(chan struct{})
	openWindow(t, b, func(string) (Invitation, error) {
		close(minted)
		return Invitation{LinkID: "published", URL: "https://host/#published"}, nil
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
	publicationWaits := make(chan struct{}, 1)
	b.settle.waiting = func() {
		select {
		case publicationWaits <- struct{}{}:
		default:
		}
	}
	revealed := make(chan error, 1)
	var during Standing
	if err := b.Read(func(Standing) error {
		go func() { _, err := b.Reveal(ch.ID, r, "lan"); revealed <- err }()
		// The mint runs while this Read holds the book.
		<-minted
		// Its publication waits for this Read to return.
		select {
		case <-publicationWaits:
		case err := <-revealed:
			t.Errorf("the reveal published during a Read (reveal = %v)", err)
		}
		during = b.standing()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if during.Window().LinkID != "" || during.Window().VerificationNumber != "" {
		t.Fatalf("the window during the Read = %+v, want no link or digits yet", during.Window())
	}
	if err := <-revealed; err != nil {
		t.Fatal(err)
	}
	var after Standing
	_ = b.Read(func(s Standing) error { after = s; return nil })
	if after.Window().LinkID != "published" || after.Window().VerificationNumber == "" {
		t.Fatalf("the window after the Read = %+v, want its link and digits", after.Window())
	}
}

// The last window's published link stays readable after the window is
// removed, until the next Open replaces it.
func TestTheLastWindowsLinkOutlivesItsWindow(t *testing.T) {
	b := NewBook(newLedger().cancel, time.Hour)
	w := openWindow(t, b, mintOf("decided"))
	handshake(t, b, newClient(t))
	if err := b.Close(w.WindowID); err != nil {
		t.Fatal(err)
	}
	var st Standing
	_ = b.Read(func(s Standing) error { st = s; return nil })
	if st.LinkOf(w.WindowID) != "decided" || st.LinkOf("another-window") != "" {
		t.Fatalf("after the close, LinkOf = %q / %q, want the closed window's link only", st.LinkOf(w.WindowID), st.LinkOf("another-window"))
	}
	next := openWindow(t, b, mintOf("next"))
	_ = b.Read(func(s Standing) error { st = s; return nil })
	if st.LinkOf(w.WindowID) != "" || st.LinkOf(next.WindowID) != "" {
		t.Fatal("the next open kept the previous window's link, or named one before publishing")
	}
}

// CancelLink retires the open window that minted the link, joining a mint in
// flight first so a link it is about to publish is found in its window. The
// caller's cancel decides the answer for every link.
func TestCancelLinkRetiresTheWindowThatMintedIt(t *testing.T) {
	l := newLedger()
	b := NewBook(l.cancel, time.Hour)
	w, g, revealed := heldMintWindow(t, b, "in-flight")
	type canceled struct {
		window string
		err    error
	}
	done := make(chan canceled, 1)
	var decided []string
	other := func(id string) error { decided = append(decided, id); return nil }
	// The mint is released only once CancelLink waits for it.
	b.joining = g.open
	go func() {
		id, err := b.CancelLink("in-flight", other)
		done <- canceled{id, err}
	}()
	got := <-done
	if got.err != nil || got.window != w.WindowID {
		t.Fatalf("CancelLink = %+v, want the minting window retired", got)
	}
	if err := <-revealed; err != nil {
		t.Fatal(err)
	}
	if b.Snapshot(w.WindowID).Open || l.count("in-flight") != 1 || len(decided) != 1 {
		t.Fatalf("after CancelLink the window is open (%v), has %d statements, or the caller decided %v", b.Snapshot(w.WindowID).Open, l.count("in-flight"), decided)
	}
	var asked string
	id, err := b.CancelLink("ordinary", func(id string) error { asked = id; return errStorage })
	if id != "" || !errors.Is(err, errStorage) || asked != "ordinary" {
		t.Fatalf("CancelLink of another link = (%q, %v), asked %q; want the caller's cancel and its answer", id, err, asked)
	}
}

// An empty link id matches no window: a window still waiting for a
// requester, whose link is empty, stays open, and the caller's cancel
// answers for the id.
func TestCancelLinkOfAnEmptyIDLeavesAWaitingWindowOpen(t *testing.T) {
	l := newLedger()
	b := NewBook(l.cancel, time.Hour)
	w := openWindow(t, b, mintOf("unrevealed"))
	var asked []string
	id, err := b.CancelLink("", func(id string) error { asked = append(asked, id); return errStorage })
	if id != "" || !errors.Is(err, errStorage) || len(asked) != 1 || asked[0] != "" {
		t.Fatalf("CancelLink(\"\") = (%q, %v), asked %q; want the caller's refusal of the empty id", id, err, asked)
	}
	if !b.Snapshot(w.WindowID).Open {
		t.Fatal("CancelLink(\"\") closed a window waiting for a requester")
	}
}

// A held link is admitted only while the open, unexpired window holds it:
// a link no window published, or one an expired window holds, runs nothing.
func TestAdmitOfAHeldLinkNeedsTheWindowThatHoldsIt(t *testing.T) {
	b := NewBook(newLedger().cancel, time.Hour)
	w := openWindow(t, b, mintOf("held"))
	handshake(t, b, newClient(t))
	ran := false
	if err := b.Admit("stray", true, func() error { ran = true; return nil }); !errors.Is(err, ErrClosed) || ran {
		t.Fatalf("Admit of a held link no window published = %v (ran %v), want ErrClosed", err, ran)
	}
	if err := b.Admit("held", true, func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("Admit of the window's own link = %v (ran %v), want it run", err, ran)
	}
	b.Expire(w.WindowID)
	ran = false
	if err := b.Admit("held", true, func() error { ran = true; return nil }); !errors.Is(err, ErrClosed) || ran {
		t.Fatalf("Admit of an expired window's link = %v (ran %v), want ErrClosed", err, ran)
	}
}

// The caller's refusal of a window's link (one already approved) is the
// answer, and the window is retired all the same.
func TestCancelLinkAnswersTheCallersRefusalAndRetiresTheWindow(t *testing.T) {
	l := newLedger()
	b := NewBook(l.cancel, time.Hour)
	w := openWindow(t, b, mintOf("approved"))
	handshake(t, b, newClient(t))
	refused := errors.New("already approved")
	id, err := b.CancelLink("approved", func(string) error { return refused })
	if id != w.WindowID || !errors.Is(err, refused) {
		t.Fatalf("CancelLink of an approved link = (%q, %v), want its window and the refusal", id, err)
	}
	if b.Snapshot(w.WindowID).Open || b.Refuses("approved") {
		t.Fatal("the window stayed open, or its record pending, after the refused cancel")
	}
}

// A cancel of a link the book still has pending answers the caller's
// decision too, and settles the book's record once the retirement succeeds.
func TestCancelLinkOfAPendingLinkAnswersTheCallersDecision(t *testing.T) {
	l := newLedger()
	b := NewBook(l.cancel, time.Hour)
	w := openWindow(t, b, mintOf("pending"))
	handshake(t, b, newClient(t))
	l.fail(true)
	if err := b.Close(w.WindowID); !errors.Is(err, errStorage) {
		t.Fatalf("the close with a failing retirement = %v, want the storage error", err)
	}
	l.fail(false)
	refused := errors.New("already settled")
	id, err := b.CancelLink("pending", func(string) error { return refused })
	if id != "" || !errors.Is(err, refused) {
		t.Fatalf("CancelLink of a pending link = (%q, %v), want the caller's decision", id, err)
	}
	if b.Refuses("pending") {
		t.Fatal("the pending record survived a retirement that succeeded")
	}
}
