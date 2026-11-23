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
// Open, by every Close naming its window, and by the window's own timer on a
// doubling schedule.
//
// pending is one record, not a set: a window mints at most one invitation,
// every removal joins its window's mint before the next act, and Open
// installs a window only once nothing is pending, so a second record can
// never be needed while one exists.
//
// mu guards the fields and is never held across a callback. turn runs Open,
// Close and a retirement one at a time, with the removal's join of the
// window's mint in flight inside it. settle orders a removal against a
// confirmation's check and write, and covers nothing else, so an approval
// waits at most for another approval's write or a removal's bookkeeping.
// Lock order: turn, settle, mu.
type Book struct {
	mu      sync.Mutex
	turn    orderLock
	settle  orderLock
	window  *window
	pending *retirement
	cancel  func(linkID string) error
	retry   time.Duration
}

type window struct {
	view    Snapshot
	mint    func(route string) (Invitation, error)
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
	// window minted it; once removed, the window's timer runs the retries.
	window *window
	linkID string
	// wait is the delay before the next retry the timer runs.
	wait time.Duration
}

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
// way.
func (b *Book) Open(mint func(route string) (Invitation, error)) (Snapshot, error) {
	return b.open(mint, WindowTTL)
}

func (b *Book) open(mint func(route string) (Invitation, error), ttl time.Duration) (Snapshot, error) {
	if mint == nil {
		return Snapshot{}, errors.New("pairbootstrap: a window needs a mint")
	}
	b.turn.Lock()
	defer b.turn.Unlock()
	b.mu.Lock()
	old := b.window
	b.mu.Unlock()
	if old != nil {
		b.removeInTurn(old.view.WindowID)
	}
	if err := b.retireInTurn("", true); err != nil {
		return Snapshot{}, err
	}
	w := &window{mint: mint, view: Snapshot{Open: true, WindowID: encode(random(16)), ExpiresAtMs: time.Now().Add(ttl).UnixMilli()}}
	b.mu.Lock()
	b.window = w
	// Expiry, and later the retries of the window's invitation; a failure
	// stays pending and cancel reports it.
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
	b.removeInTurn(windowID)
	return b.retireInTurn(windowID, false)
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
		b.pendLocked(w, w.view.LinkID)
	}
	minting := w.minting
	b.mu.Unlock()
	b.settle.Unlock()
	if minting != nil {
		<-minting
	}
}

// pendLocked makes linkID the pending retirement. The caller holds mu.
func (b *Book) pendLocked(w *window, linkID string) {
	if b.pending != nil && b.pending.linkID != linkID {
		// The type comment's argument says this cannot happen. Dropping
		// either invitation would leave it confirmable, so stop instead.
		panic("pairbootstrap: a second unfinished cancellation")
	}
	b.pending = &retirement{window: w, linkID: linkID, wait: b.retry}
}

// retireInTurn issues one cancellation statement for the pending invitation
// when it is windowID's, or whosever it is when every is set. A durable
// cancellation clears it; a failure keeps it and, once its window is
// removed, schedules the window's timer to retry it. The caller holds turn.
func (b *Book) retireInTurn(windowID string, every bool) error {
	b.mu.Lock()
	p := b.pending
	b.mu.Unlock()
	if p == nil || (!every && p.window.view.WindowID != windowID) {
		return nil
	}
	err := b.cancel(p.linkID)
	b.mu.Lock()
	defer b.mu.Unlock()
	removed := b.window != p.window
	switch {
	case err == nil:
		b.pending = nil
		if removed {
			p.window.timer.Stop()
		}
	case removed:
		p.window.timer.Reset(p.wait)
		p.wait = min(2*p.wait, max(b.retry, maxCancelRetry))
	}
	return err
}

// Refuses reports whether linkID belongs to a window that is no longer open:
// one removed and not yet durably cancelled, or the open window past its
// expiry. A link no window minted, or one durably cancelled, is the
// caller's to judge.
func (b *Book) Refuses(linkID string) bool {
	if linkID == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pending != nil && b.pending.linkID == linkID {
		return true
	}
	w := b.window
	return w != nil && w.view.LinkID == linkID && time.Now().UnixMilli() >= w.view.ExpiresAtMs
}

// Confirm runs confirm unless the book refuses linkID. A removal waits for a
// confirmation already running, and a confirmation never starts between a
// removal and its cancellation.
func (b *Book) Confirm(linkID string, confirm func() error) error {
	b.settle.Lock()
	defer b.settle.Unlock()
	if b.Refuses(linkID) {
		return ErrClosed
	}
	return confirm()
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
	case invite.LinkID != "":
		// Never published, so refused from here until its cancellation is
		// durable. The window's removal retires it: a removal during the
		// mint is waiting on minting, and an expired window's timer has yet
		// to run. An open window's malformed invitation is retired below.
		b.pendLocked(w, invite.LinkID)
	}
	w.minting = nil
	close(minting)
	b.mu.Unlock()
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
