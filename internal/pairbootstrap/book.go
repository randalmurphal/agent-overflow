package pairbootstrap

import (
	"crypto/ecdh"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

const (
	WindowTTL = 2 * time.Minute
	// CancelRetry is how long a book waits before it retries an unfinished
	// cancellation itself. Each failure doubles the wait, to at most
	// maxCancelRetry; the retries end when the cancellation is durable.
	CancelRetry    = 2 * time.Second
	maxCancelRetry = time.Minute
)

type Snapshot struct {
	Open               bool   `json:"open"`
	WindowID           string `json:"windowId"`
	ExpiresAtMs        int64  `json:"expiresAtMs"`
	RequestID          string `json:"requestId"`
	Label              string `json:"label"`
	Platform           string `json:"platform"`
	VerificationNumber string `json:"verificationNumber"`
	LinkID             string `json:"linkId"`
}

// Book retains at most one window and one requester. The owner must reopen to
// replace an occupied window, limiting guesses and avoiding invisible requester
// replacement while the owner compares screens. No session state lives here.
//
// The book also owns the retirement of every invitation a window minted. A
// window's invitation is refused from the moment the window is removed
// (closed, replaced or expired) until cancel reports that its cancellation is
// durable. Until then it is the book's pending retirement, retried by every
// Open, by every Close naming its window, and by the book's retry timer on a
// doubling schedule. Stop ends all of it.
//
// pending is one record, not a set: a window mints at most one invitation,
// every removal joins its window's mint before the next act, and Open
// installs a window only once nothing is pending, so a second record can
// never be needed while one exists.
//
// mu guards the fields and is never held across a callback; it is a leaf,
// which a caller may take under its own locks (Expire). turn runs Open,
// Close, CancelLink, Stop and a retirement one at a time, with the removal's
// join of the window's mint in flight inside it. settle orders a removal
// against an admitted write's check and commit (Admit), against a Read, and
// against a reveal's publication of its link and digits, and covers nothing
// else, so an approval waits at most for another admitted write, a Read's
// row read, a publication or a removal's bookkeeping. Lock order: the
// caller's identity lock (identity.Sessions' own-device lock, held around
// Admit), turn, settle, mu. The callbacks run under settle (Admit, Read) may
// use the caller's store and must not take that identity lock; cancel runs
// under turn alone. A caller must not hold a store transaction while it
// calls into the book.
type Book struct {
	mu      sync.Mutex
	turn    orderLock
	settle  orderLock
	window  *window
	pending *retirement
	// retryTimer retries pending once its window is gone; nil until first
	// needed.
	retryTimer *time.Timer
	stopped    bool
	// stopErr is the first Stop's answer, which every later Stop returns.
	stopErr error
	// last is the window opened last and the link it published, kept after
	// the window is removed so a decision's result stays readable for it
	// (Standing.LinkOf) until the next Open replaces it.
	last   lastWindow
	cancel func(linkID string) error
	retry  time.Duration
	// joining, when a test sets it before the CancelLink it observes, runs
	// when that CancelLink finds a mint in flight, before it waits for it.
	joining func()
}

// lastWindow is Book.last.
type lastWindow struct {
	windowID, linkID string
}

type window struct {
	view Snapshot
	mint func(route string) (Invitation, error)
	// timer is the window's expiry.
	timer   *time.Timer
	request *request
	// minting is closed when a mint in flight has published its invitation
	// or recorded it for retirement; nil when no mint is in flight.
	minting chan struct{}
	// spent is set once the window minted an invitation, so it mints no other.
	spent bool
}

// retirement is an invitation whose cancellation is not yet durable.
type retirement struct {
	// windowID minted it.
	windowID string
	linkID   string
	// wait is the delay before the next retry the retry timer runs.
	wait time.Duration
}

// ErrStopped answers an Open or Close after Stop.
var ErrStopped = errors.New("pairbootstrap: pairing is shutting down")

type request struct {
	commitment Commitment
	challenge  Challenge
	key        *ecdh.PrivateKey
	sealed     *SealedInvitation
}

// orderLock is a mutex whose contended acquisitions a test can observe.
type orderLock struct {
	mu sync.Mutex
	// waiting, when set before the book is used, runs when Lock finds the
	// lock held, before it blocks.
	waiting func()
}

func (l *orderLock) Lock() {
	if l.mu.TryLock() {
		return
	}
	if l.waiting != nil {
		l.waiting()
	}
	l.mu.Lock()
}

func (l *orderLock) Unlock() { l.mu.Unlock() }

// NewBook returns a book whose cancel makes one invitation's cancellation
// durable, answering nil when the invitation is already settled. retry is the
// first wait before the book retries a failed cancellation itself.
func NewBook(cancel func(linkID string) error, retry time.Duration) *Book {
	if cancel == nil {
		panic("pairbootstrap: a book needs a cancel")
	}
	if retry <= 0 {
		panic("pairbootstrap: a book needs a positive cancellation retry")
	}
	return &Book{cancel: cancel, retry: retry}
}

// Open replaces any window with a new one whose mint makes its invitation for
// the route a reveal names. It first retires the pending invitation and opens
// nothing while that cancellation fails; the replaced window is closed either
// way. admit runs before the new window is visible, with the book's turn held,
// and an error from it opens nothing: the caller admits the window and records
// it as its own there, so a Close or a retirement that follows waits for this
// Open and then finds the window. admit must not call the book.
func (b *Book) Open(mint func(route string) (Invitation, error), admit func(Snapshot) error) (Snapshot, error) {
	return b.open(mint, admit, WindowTTL)
}

func (b *Book) open(mint func(route string) (Invitation, error), admit func(Snapshot) error, ttl time.Duration) (Snapshot, error) {
	if mint == nil {
		return Snapshot{}, errors.New("pairbootstrap: a window needs a mint")
	}
	if admit == nil {
		return Snapshot{}, errors.New("pairbootstrap: a window needs an admission")
	}
	b.turn.Lock()
	defer b.turn.Unlock()
	b.mu.Lock()
	old, stopped := b.window, b.stopped
	b.mu.Unlock()
	if stopped {
		return Snapshot{}, ErrStopped
	}
	if old != nil {
		b.removeInTurn(old.view.WindowID)
	}
	if err := b.retireInTurn("", true); err != nil {
		return Snapshot{}, err
	}
	w := &window{mint: mint, view: Snapshot{Open: true, WindowID: encode(random(16)), ExpiresAtMs: time.Now().Add(ttl).UnixMilli()}}
	if err := admit(w.view); err != nil {
		return Snapshot{}, err
	}
	b.mu.Lock()
	b.window = w
	b.last = lastWindow{windowID: w.view.WindowID}
	w.timer = time.AfterFunc(ttl, func() { _ = b.Close(w.view.WindowID) })
	view := w.view
	b.mu.Unlock()
	return view, nil
}

// Close retires the named window, never a replacement, and answers whether
// that window's invitation is durably cancelled. A window already gone whose
// cancellation failed earlier is tried again; any other id is a no-op.
func (b *Book) Close(windowID string) error {
	b.turn.Lock()
	defer b.turn.Unlock()
	if b.isStopped() {
		return ErrStopped
	}
	b.removeInTurn(windowID)
	return b.retireInTurn(windowID, false)
}

// Stop retires the open window and tries its invitation's cancellation once,
// then refuses every Open, Close and CancelLink and disarms every timer. A
// timer callback already in a Close or a retry holds turn, so Stop waits for
// it; one that takes turn after Stop finds the book stopped and does
// nothing. Stop answers for a cancellation still unfinished, which the
// caller's restart recovery must retire. A later Stop touches nothing and
// returns the first one's answer.
func (b *Book) Stop() error {
	b.turn.Lock()
	defer b.turn.Unlock()
	b.mu.Lock()
	w, stopped, stopErr := b.window, b.stopped, b.stopErr
	b.mu.Unlock()
	if stopped {
		return stopErr
	}
	if w != nil {
		b.removeInTurn(w.view.WindowID)
	}
	err := b.retireInTurn("", true)
	b.mu.Lock()
	b.stopped, b.stopErr = true, err
	if b.retryTimer != nil {
		b.retryTimer.Stop()
	}
	b.mu.Unlock()
	return err
}

// Expire refuses the named window from this moment, as its expiry would:
// its invitation is refused confirmation (Refuses), it admits no request,
// and it reads closed. It does nothing else; the caller closes the window
// (Close) to retire its invitation. Expire takes mu alone, so a caller may
// hold its own locks around it. Any other id is a no-op.
func (b *Book) Expire(windowID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.window != nil && b.window.view.WindowID == windowID {
		b.window.view.ExpiresAtMs = 0
	}
}

// CancelLink cancels linkID for its owner. When the open window minted it,
// the window is removed and retired, as Close would, and its id returned so
// the caller can end what it attached to that window. A mint in flight is
// joined first, so a link it is about to publish is found in its window.
// A link the book still has pending is retired as the book would. Any other
// link is canceled by other, with the book's turn held, so its answer (an
// already settled link included) is the caller's. The wait is bounded by
// that one mint.
func (b *Book) CancelLink(linkID string, other func(linkID string) error) (windowID string, err error) {
	b.turn.Lock()
	defer b.turn.Unlock()
	b.mu.Lock()
	stopped := b.stopped
	var minting chan struct{}
	if b.window != nil {
		minting = b.window.minting
	}
	b.mu.Unlock()
	if stopped {
		return "", ErrStopped
	}
	if minting != nil {
		if b.joining != nil {
			b.joining()
		}
		<-minting
	}
	b.mu.Lock()
	if b.window != nil && b.window.view.LinkID == linkID {
		windowID = b.window.view.WindowID
	}
	var pending string
	if b.pending != nil && b.pending.linkID == linkID {
		pending = b.pending.windowID
	}
	b.mu.Unlock()
	switch {
	case windowID != "":
		b.removeInTurn(windowID)
		return windowID, b.retireInTurn(windowID, false)
	case pending != "":
		return "", b.retireInTurn(pending, false)
	default:
		return "", other(linkID)
	}
}

func (b *Book) isStopped() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stopped
}

// removeInTurn removes the window if it is the open one, refusing its
// invitation from that moment, and waits for its mint in flight to record the
// invitation it made. The caller holds turn.
func (b *Book) removeInTurn(windowID string) {
	b.settle.Lock()
	b.mu.Lock()
	w := b.window
	if w == nil || w.view.WindowID != windowID {
		b.mu.Unlock()
		b.settle.Unlock()
		return
	}
	b.window = nil
	w.timer.Stop()
	if w.view.LinkID != "" {
		b.pendLocked(w.view.WindowID, w.view.LinkID)
	}
	minting := w.minting
	b.mu.Unlock()
	b.settle.Unlock()
	if minting != nil {
		<-minting
	}
}

// pendLocked makes linkID, which windowID minted, the pending retirement.
// The caller holds mu.
func (b *Book) pendLocked(windowID, linkID string) {
	if b.pending != nil && b.pending.linkID != linkID {
		// The type comment's argument says this cannot happen. Dropping
		// either invitation would leave it confirmable, so stop instead.
		panic("pairbootstrap: a second unfinished cancellation")
	}
	b.pending = &retirement{windowID: windowID, linkID: linkID, wait: b.retry}
}

// retireInTurn issues one cancellation statement for the pending invitation
// when it is windowID's, or whosever it is when every is set. A durable
// cancellation clears it; a failure keeps it and, once its window is
// removed, schedules the retry timer. The caller holds turn.
func (b *Book) retireInTurn(windowID string, every bool) error {
	b.mu.Lock()
	p := b.pending
	b.mu.Unlock()
	if p == nil || (!every && p.windowID != windowID) {
		return nil
	}
	err := b.cancel(p.linkID)
	b.mu.Lock()
	defer b.mu.Unlock()
	removed := b.window == nil || b.window.view.WindowID != p.windowID
	switch {
	case err == nil:
		b.pending = nil
		if b.retryTimer != nil {
			b.retryTimer.Stop()
		}
	case removed && !b.stopped:
		if b.retryTimer == nil {
			b.retryTimer = time.AfterFunc(p.wait, b.retryPending)
		} else {
			b.retryTimer.Reset(p.wait)
		}
		p.wait = min(2*p.wait, max(b.retry, maxCancelRetry))
	}
	return err
}

// retryPending is the retry timer's run; a failure stays pending and cancel
// reports it.
func (b *Book) retryPending() {
	b.turn.Lock()
	defer b.turn.Unlock()
	if !b.isStopped() {
		_ = b.retireInTurn("", true)
	}
}

// Standing is what the book says about its invitations at one instant.
type Standing struct {
	pending string
	open    Snapshot
	last    lastWindow
	now     int64
}

// LinkOf is the link the named window published, while it is the window
// opened last, open or not; "" for any other window or one that published
// none.
func (s Standing) LinkOf(windowID string) string {
	if windowID == "" || s.last.windowID != windowID {
		return ""
	}
	return s.last.linkID
}

// Refuses reports whether linkID belongs to a window that is no longer open:
// one removed and not yet durably cancelled, or the open window past its
// expiry. A link no window minted, or one durably cancelled, is the
// caller's to judge.
func (s Standing) Refuses(linkID string) bool {
	if linkID == "" {
		return false
	}
	return s.pending == linkID || (s.open.LinkID == linkID && s.now >= s.open.ExpiresAtMs)
}

// Window is the open window, closed (Open false) when none is open or it is
// past its expiry.
func (s Standing) Window() Snapshot {
	if !s.open.Open || s.now >= s.open.ExpiresAtMs {
		return Snapshot{}
	}
	return s.open
}

func (b *Book) standing() Standing {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := Standing{now: time.Now().UnixMilli(), last: b.last}
	if b.pending != nil {
		s.pending = b.pending.linkID
	}
	if b.window != nil {
		s.open = b.window.view
	}
	return s
}

// Refuses reports Standing.Refuses for this instant.
func (b *Book) Refuses(linkID string) bool { return b.standing().Refuses(linkID) }

// Admit runs write unless the book refuses linkID, answering ErrClosed for
// a refusal. A removal waits for a write already admitted, and no write is
// admitted between a removal and its cancellation: the caller's redemption
// of a window's invitation and its owner's confirmation both run here.
func (b *Book) Admit(linkID string, write func() error) error {
	b.settle.Lock()
	defer b.settle.Unlock()
	if b.Refuses(linkID) {
		return ErrClosed
	}
	return write()
}

// Read hands read the book's standing and lets it read the caller's records
// of the same invitations before anything changes the answer: no removal
// starts until read returns, and a retirement that finished earlier is
// already absent from the standing, so a record read inside read agrees with
// it. read should do only that reading.
func (b *Book) Read(read func(Standing) error) error {
	b.settle.Lock()
	defer b.settle.Unlock()
	return read(b.standing())
}

func (b *Book) Snapshot(windowID string) Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.window == nil || (windowID != "" && b.window.view.WindowID != windowID) || time.Now().UnixMilli() >= b.window.view.ExpiresAtMs {
		return Snapshot{}
	}
	return b.window.view
}

func (b *Book) Begin(co Commitment) (Challenge, error) {
	if co.Version != Version {
		return Challenge{}, ErrInvalid
	}
	if _, err := decode(co.Commitment, 32); err != nil {
		return Challenge{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	w := b.window
	if w == nil || time.Now().UnixMilli() >= w.view.ExpiresAtMs {
		return Challenge{}, ErrClosed
	}
	if w.request != nil {
		if w.request.commitment == co {
			return w.request.challenge, nil
		}
		return Challenge{}, ErrBusy
	}
	key, nonce, err := ephemeral()
	if err != nil {
		return Challenge{}, err
	}
	ch := Challenge{ID: encode(random(16)), PublicKey: encode(key.PublicKey().Bytes()), Nonce: nonce, ExpiresAtMs: w.view.ExpiresAtMs}
	w.request = &request{commitment: co, challenge: ch, key: key}
	w.view.RequestID = ch.ID
	return ch, nil
}

// Reveal admits the requester's reveal and seals the window's invitation,
// minted for route: the listener the reveal arrived on, which the book passes
// to the mint unread.
func (b *Book) Reveal(id string, r Reveal, route string) (SealedInvitation, error) {
	b.mu.Lock()
	w := b.window
	if w == nil || time.Now().UnixMilli() >= w.view.ExpiresAtMs {
		b.mu.Unlock()
		return SealedInvitation{}, ErrClosed
	}
	req := w.request
	if req == nil || req.challenge.ID != id || commit(r) != req.commitment {
		b.mu.Unlock()
		return SealedInvitation{}, ErrInvalid
	}
	if req.sealed != nil {
		sealed := *req.sealed
		b.mu.Unlock()
		return sealed, nil
	}
	if w.minting != nil {
		b.mu.Unlock()
		return SealedInvitation{}, ErrBusy
	}
	if w.spent {
		b.mu.Unlock()
		return SealedInvitation{}, ErrInvalid
	}
	aead, transcript, sas, err := derive(req.key, r.PublicKey, req.commitment, req.challenge, r)
	if err != nil {
		b.mu.Unlock()
		return SealedInvitation{}, err
	}
	minting := make(chan struct{})
	w.minting = minting
	b.mu.Unlock()

	// Minting touches the caller's ordinary invitation store, never under the
	// book lock. A removal meanwhile waits for the result below and retires
	// the late invitation itself.
	invite, err := w.mint(route)
	var sealed SealedInvitation
	if err == nil {
		if invite.LinkID == "" || invite.URL == "" || len(invite.URL) > 16384 {
			err = ErrInvalid
		} else {
			plain, _ := json.Marshal(invite)
			nonce := random(aead.NonceSize())
			sealed = SealedInvitation{Nonce: encode(nonce), Ciphertext: encode(aead.Seal(nil, nonce, plain, transcript))}
		}
	}
	// The publication of the link and its digits takes settle, after the
	// mint and its store write are done and for the in-memory update alone,
	// so a Read never sees the link's store row without the window's digits
	// for it: a redemption needs the sealed invitation, which is returned
	// only after this.
	b.settle.Lock()
	b.mu.Lock()
	current := b.window == w && time.Now().UnixMilli() < w.view.ExpiresAtMs
	if invite.LinkID != "" {
		w.spent = true
	}
	switch {
	case current && err == nil:
		req.sealed = &sealed
		req.key = nil
		w.view.Label, w.view.Platform, w.view.VerificationNumber, w.view.LinkID = r.Label, r.Platform, sas, invite.LinkID
		if b.last.windowID == w.view.WindowID {
			b.last.linkID = invite.LinkID
		}
	case invite.LinkID != "":
		// Never published, so refused from here until its cancellation is
		// durable. The window's removal retires it: a removal during the
		// mint is waiting on minting, and an expired window's timer has yet
		// to run. An open window's malformed invitation is retired below.
		b.pendLocked(w.view.WindowID, invite.LinkID)
	}
	w.minting = nil
	close(minting)
	b.mu.Unlock()
	b.settle.Unlock()
	if current && err == nil {
		return sealed, nil
	}
	if current && invite.LinkID != "" {
		b.turn.Lock()
		err = errors.Join(err, b.retireInTurn(w.view.WindowID, false))
		b.turn.Unlock()
	}
	if err != nil {
		return SealedInvitation{}, err
	}
	return SealedInvitation{}, ErrClosed
}
