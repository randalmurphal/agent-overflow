package pairbootstrap

import (
	"errors"
	"slices"
	"sync"
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
	b.Expire("another-window", EndedAddressChanged)
	if !b.Snapshot(w.WindowID).Open {
		t.Fatal("Expire of another id closed the window")
	}
	b.Expire(w.WindowID, EndedAddressChanged)
	if confirmed(t, b, "expiring") {
		t.Fatal("an expired window's invitation was admitted")
	}
	if b.Snapshot(w.WindowID).Open {
		t.Fatal("an expired window reads open")
	}
	if _, err := b.Begin(newClient(t).Commitment()); !errors.Is(err, ErrClosed) {
		t.Fatalf("a request to an expired window = %v, want ErrClosed", err)
	}
	if err := b.Close(w.WindowID, EndedClosed); err != nil || l.count("expiring") != 1 {
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
	if err := b.Close(w.WindowID, EndedClosed); err != nil {
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

// scripted is a cancel that answers its calls with answers in order, the
// last one repeating, and records the links it was asked for.
type scripted struct {
	mu      sync.Mutex
	answers []error
	asked   []string
}

func (s *scripted) cancel(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, id)
	return s.answers[min(len(s.asked), len(s.answers))-1]
}

func (s *scripted) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.asked)
}

// CancelLink retires the open window that minted the link, joining a mint in
// flight first so a link it is about to publish is found in its window. Any
// other link is canceled directly; every answer is the book's one cancel.
func TestCancelLinkRetiresTheWindowThatMintedIt(t *testing.T) {
	l := newLedger()
	b := NewBook(l.cancel, time.Hour)
	w, g, revealed := heldMintWindow(t, b, "in-flight")
	type canceled struct {
		window string
		err    error
	}
	done := make(chan canceled, 1)
	// The mint is released only once CancelLink waits for it.
	b.joining = g.open
	go func() {
		id, err := b.CancelLink("in-flight")
		done <- canceled{id, err}
	}()
	got := <-done
	if got.err != nil || got.window != w.WindowID {
		t.Fatalf("CancelLink = %+v, want the minting window retired", got)
	}
	if err := <-revealed; err != nil {
		t.Fatal(err)
	}
	if b.Snapshot(w.WindowID).Open || l.count("in-flight") != 1 {
		t.Fatalf("after CancelLink the window is open (%v) or has %d statements, want one", b.Snapshot(w.WindowID).Open, l.count("in-flight"))
	}
	l.fail(true)
	id, err := b.CancelLink("ordinary")
	if id != "" || !errors.Is(err, errStorage) || l.count("ordinary") != 1 {
		t.Fatalf("CancelLink of another link = (%q, %v) with %d statements, want its one cancel's answer", id, err, l.count("ordinary"))
	}
}

// An empty link id matches no window: a window still waiting for a
// requester, whose link is empty, stays open, and the cancel answers for the
// id.
func TestCancelLinkOfAnEmptyIDLeavesAWaitingWindowOpen(t *testing.T) {
	refused := Settled(errors.New("unknown link"))
	s := &scripted{answers: []error{refused}}
	b := NewBook(s.cancel, time.Hour)
	w := openWindow(t, b, mintOf("unrevealed"))
	id, err := b.CancelLink("")
	if asked := s.calls(); id != "" || !errors.Is(err, refused) || len(asked) != 1 || asked[0] != "" {
		t.Fatalf("CancelLink(\"\") = (%q, %v), asked %q; want the refusal of the empty id", id, err, asked)
	}
	if !b.Snapshot(w.WindowID).Open {
		t.Fatal("CancelLink(\"\") closed a window waiting for a requester")
	}
}

// An owner's cancel of a window's link is one cancellation, whose outcome is
// both the owner's answer and the book's record: one that commits answers
// success with nothing left pending, whatever a second statement would have
// answered; one that fails answers the failure and keeps the record for the
// retry, whatever a second statement would have answered.
func TestCancelLinkIsOneCancellationForTheAnswerAndTheRecord(t *testing.T) {
	for name, tc := range map[string]struct {
		answers []error
		want    error
		pending bool
	}{
		"the cancellation commits":   {answers: []error{nil, errStorage}, want: nil, pending: false},
		"the cancellation fails":     {answers: []error{errStorage, nil}, want: errStorage, pending: true},
		"the link was settled first": {answers: []error{Settled(errSettledFirst), errStorage}, want: errSettledFirst, pending: false},
	} {
		t.Run(name, func(t *testing.T) {
			s := &scripted{answers: tc.answers}
			b := NewBook(s.cancel, time.Hour)
			w := openWindow(t, b, mintOf("owned"))
			handshake(t, b, newClient(t))
			id, err := b.CancelLink("owned")
			if id != w.WindowID || !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("CancelLink = (%q, %v), want the window and %v", id, err, tc.want)
			}
			if asked := s.calls(); len(asked) != 1 {
				t.Fatalf("the cancel asked the store %d times (%q), want once", len(asked), asked)
			}
			if b.Snapshot(w.WindowID).Open || b.Refuses("owned") != tc.pending {
				t.Fatalf("after the cancel the window is open (%v) or its record pending is %v, want %v", b.Snapshot(w.WindowID).Open, b.Refuses("owned"), tc.pending)
			}
		})
	}
}

var errSettledFirst = errors.New("already approved")

// A cancel of a link the book still has pending is that pending record's
// one cancellation: its answer is the owner's, and it settles the record.
func TestCancelLinkOfAPendingLinkAnswersItsCancellation(t *testing.T) {
	s := &scripted{answers: []error{errStorage, Settled(errSettledFirst), errStorage}}
	b := NewBook(s.cancel, time.Hour)
	w := openWindow(t, b, mintOf("pending"))
	handshake(t, b, newClient(t))
	if err := b.Close(w.WindowID, EndedClosed); !errors.Is(err, errStorage) {
		t.Fatalf("the close with a failing retirement = %v, want the storage error", err)
	}
	id, err := b.CancelLink("pending")
	if id != "" || !errors.Is(err, errSettledFirst) || len(s.calls()) != 2 {
		t.Fatalf("CancelLink of a pending link = (%q, %v) after %d statements, want the settled refusal from one more", id, err, len(s.calls()))
	}
	if b.Refuses("pending") {
		t.Fatal("the pending record survived a settled answer")
	}
}

// A settled refusal is durable for every retirement: a close answers it as
// success, and an open after it opens.
func TestASettledRefusalIsADurableRetirement(t *testing.T) {
	s := &scripted{answers: []error{Settled(errSettledFirst)}}
	b := NewBook(s.cancel, time.Hour)
	w := openWindow(t, b, mintOf("settled"))
	handshake(t, b, newClient(t))
	if err := b.Close(w.WindowID, EndedClosed); err != nil {
		t.Fatalf("closing a window whose link was settled first = %v, want nil", err)
	}
	if b.Refuses("settled") {
		t.Fatal("a settled link stayed pending")
	}
	openWindow(t, b, mintOf("next"))
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
	b.Expire(w.WindowID, EndedAddressChanged)
	ran = false
	if err := b.Admit("held", true, func() error { ran = true; return nil }); !errors.Is(err, ErrClosed) || ran {
		t.Fatalf("Admit of an expired window's link = %v (ran %v), want ErrClosed", err, ran)
	}
}

// endingOf is the book's Standing.EndingOf for windowID.
func endingOf(t *testing.T, b *Book, windowID string) Ending {
	t.Helper()
	var ending Ending
	if err := b.Read(func(s Standing) error { ending = s.EndingOf(windowID); return nil }); err != nil {
		t.Fatal(err)
	}
	return ending
}

// A window keeps the first reason it stopped being open, recorded where it
// stopped, and the book answers it for the window opened last and the one
// before it.
func TestAWindowKeepsWhyItEnded(t *testing.T) {
	t.Run("deadline", func(t *testing.T) {
		b := NewBook(newLedger().cancel, time.Hour)
		w, err := b.open(mintOf("late"), admitted, 20*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		// The expiry's own close is held back, as a slow timer would be, so
		// another reason reaches the expired window first.
		b.mu.Lock()
		b.window.timer.Stop()
		b.mu.Unlock()
		for deadline := time.Now().Add(5 * time.Second); b.Snapshot(w.WindowID).Open; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("the window never reached its expiry")
			}
		}
		if got := endingOf(t, b, w.WindowID); got != EndedDeadline {
			t.Fatalf("a window past its expiry ended %q, want %q", got, EndedDeadline)
		}
		// A close for another reason after the expiry keeps it.
		if err := b.Close(w.WindowID, EndedClosed); err != nil {
			t.Fatal(err)
		}
		if got := endingOf(t, b, w.WindowID); got != EndedDeadline {
			t.Fatalf("a window closed after its expiry ended %q, want %q", got, EndedDeadline)
		}
	})
	t.Run("the first reason, then replaced", func(t *testing.T) {
		b := NewBook(newLedger().cancel, time.Hour)
		first := openWindow(t, b, mintOf("first"))
		if got := endingOf(t, b, first.WindowID); got != "" {
			t.Fatalf("an open window ended %q", got)
		}
		b.Expire(first.WindowID, EndedAddressChanged)
		if err := b.Close(first.WindowID, EndedClosed); err != nil {
			t.Fatal(err)
		}
		if got := endingOf(t, b, first.WindowID); got != EndedAddressChanged {
			t.Fatalf("a window expired then closed ended %q, want %q", got, EndedAddressChanged)
		}
		second := openWindow(t, b, mintOf("second"))
		if got := endingOf(t, b, first.WindowID); got != EndedAddressChanged {
			t.Fatalf("the window before the last ended %q, want %q", got, EndedAddressChanged)
		}
		third := openWindow(t, b, mintOf("third"))
		if got := endingOf(t, b, second.WindowID); got != EndedReplaced {
			t.Fatalf("a window a newer one replaced ended %q, want %q", got, EndedReplaced)
		}
		if got := endingOf(t, b, first.WindowID); got != "" {
			t.Fatalf("a window older than the one before the last ended %q, want none", got)
		}
		if got := endingOf(t, b, third.WindowID); got != "" {
			t.Fatalf("the open window ended %q", got)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		b := NewBook(newLedger().cancel, time.Hour)
		w := openWindow(t, b, mintOf("canceling"))
		handshake(t, b, newClient(t))
		if _, err := b.CancelLink("canceling"); err != nil {
			t.Fatal(err)
		}
		if got := endingOf(t, b, w.WindowID); got != EndedCanceled {
			t.Fatalf("a window whose invitation was canceled ended %q, want %q", got, EndedCanceled)
		}
	})
	t.Run("stopped", func(t *testing.T) {
		b := NewBook(newLedger().cancel, time.Hour)
		w := openWindow(t, b, mintOf("stopping"))
		if err := b.Stop(); err != nil {
			t.Fatal(err)
		}
		if got := endingOf(t, b, w.WindowID); got != EndedStopped {
			t.Fatalf("a window Stop removed ended %q, want %q", got, EndedStopped)
		}
	})
	t.Run("expired while admitting", func(t *testing.T) {
		b := NewBook(newLedger().cancel, time.Hour)
		w, err := b.Open(mintOf("admitting"), func(view Snapshot) error {
			b.Expire(view.WindowID, EndedDisconnected)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = b.Close(w.WindowID, EndedClosed) })
		if got := endingOf(t, b, w.WindowID); got != EndedDisconnected {
			t.Fatalf("a window Expire named while it was admitted ended %q, want %q", got, EndedDisconnected)
		}
	})
}
