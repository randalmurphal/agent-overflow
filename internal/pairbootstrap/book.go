package pairbootstrap

import (
	"crypto/ecdh"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

const WindowTTL = 2 * time.Minute

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
// window's invitation is refused confirmation from the moment the window is
// removed (closed, replaced or expired) until cancel reports that its
// cancellation is durable; until then it stays in retiring, keyed by link ID
// with the window that minted it, and every Open and every Close naming that
// window tries the cancellation again.
//
// mu guards the fields and is never held across a callback. settle orders
// every decision that settles a window's invitation: a removal (with the join
// of the window's mint in flight), each cancellation statement, and a
// confirmation's check and write. Removal joins the window's mint under
// settle, so no removed window has a mint in flight once settle is free.
//
// retiring holds the invitation of at most one window: a window mints at most
// one invitation, and Open installs a window only after every earlier
// window's cancellation is durable.
type Book struct {
	mu       sync.Mutex
	settle   sync.Mutex
	window   *window
	retiring map[string]string // link ID -> window ID
	cancel   func(linkID string) error
}

type window struct {
	view    Snapshot
	mint    func() (Invitation, error)
	timer   *time.Timer
	request *request
	// minting is closed when a mint in flight has published its invitation
	// or recorded it for retirement; nil when no mint is in flight.
	minting chan struct{}
	// spent is set once the window minted an invitation, so it mints no other.
	spent bool
}

type request struct {
	commitment Commitment
	challenge  Challenge
	key        *ecdh.PrivateKey
	sealed     *SealedInvitation
}

// NewBook returns a book whose cancel makes one invitation's cancellation
// durable. cancel answers nil when the invitation is already settled.
func NewBook(cancel func(linkID string) error) *Book {
	return &Book{cancel: cancel, retiring: map[string]string{}}
}

// Open replaces any window with a new one. It first retires every invitation
// whose cancellation is still unfinished and opens nothing while one of those
// cancellations fails; the replaced window is closed either way.
func (b *Book) Open(mint func() (Invitation, error)) (Snapshot, error) {
	return b.open(mint, WindowTTL)
}

func (b *Book) open(mint func() (Invitation, error), ttl time.Duration) (Snapshot, error) {
	b.settle.Lock()
	defer b.settle.Unlock()
	b.mu.Lock()
	old := b.window
	b.mu.Unlock()
	if old != nil {
		b.removeSettling(old.view.WindowID)
	}
	if err := b.retireSettling("", true); err != nil {
		return Snapshot{}, err
	}
	w := &window{mint: mint, view: Snapshot{Open: true, WindowID: encode(random(16)), ExpiresAtMs: time.Now().Add(ttl).UnixMilli()}}
	b.mu.Lock()
	b.window = w
	// Expiry's cancellation failure stays in retiring; cancel reports it.
	w.timer = time.AfterFunc(ttl, func() { _ = b.Close(w.view.WindowID) })
	view := w.view
	b.mu.Unlock()
	return view, nil
}

// Close retires the named window, never a replacement, and answers whether
// that window's invitation is durably cancelled. A window already gone whose
// cancellation failed earlier is tried again; any other id is a no-op.
func (b *Book) Close(windowID string) error {
	b.settle.Lock()
	defer b.settle.Unlock()
	b.removeSettling(windowID)
	return b.retireSettling(windowID, false)
}

// removeSettling removes the window if it is the open one, refusing its
// invitation from that moment, and waits for its mint in flight to record
// the invitation it made. The caller holds settle.
func (b *Book) removeSettling(windowID string) {
	b.mu.Lock()
	w := b.window
	if w == nil || w.view.WindowID != windowID {
		b.mu.Unlock()
		return
	}
	b.window = nil
	w.timer.Stop()
	if w.view.LinkID != "" {
		b.retiring[w.view.LinkID] = windowID
	}
	minting := w.minting
	b.mu.Unlock()
	if minting != nil {
		<-minting
	}
}

// retireSettling issues one cancellation statement for each unfinished
// invitation of windowID, or of every window when every is set, and forgets
// each whose cancellation is durable. The caller holds settle.
func (b *Book) retireSettling(windowID string, every bool) error {
	b.mu.Lock()
	var links []string
	for link, owner := range b.retiring {
		if every || owner == windowID {
			links = append(links, link)
		}
	}
	b.mu.Unlock()
	var errs []error
	for _, link := range links {
		if err := b.cancelLink(link); err != nil {
			errs = append(errs, err)
			continue
		}
		b.mu.Lock()
		delete(b.retiring, link)
		b.mu.Unlock()
	}
	return errors.Join(errs...)
}

func (b *Book) cancelLink(linkID string) error {
	if b.cancel == nil {
		return nil
	}
	return b.cancel(linkID)
}

// Confirm runs confirm unless linkID belongs to a window that is no longer
// open: one removed and not yet durably cancelled, or the open window past
// its expiry. A removal waits for a confirmation already running, and a
// confirmation never starts between a removal and its cancellation. A link
// no window minted, or one durably cancelled, is the caller's to judge.
func (b *Book) Confirm(linkID string, confirm func() error) error {
	b.settle.Lock()
	defer b.settle.Unlock()
	b.mu.Lock()
	_, retiring := b.retiring[linkID]
	w := b.window
	expired := w != nil && w.view.LinkID == linkID && time.Now().UnixMilli() >= w.view.ExpiresAtMs
	b.mu.Unlock()
	if retiring || expired {
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

func (b *Book) Reveal(id string, r Reveal) (SealedInvitation, error) {
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
	invite, err := w.mint()
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
		b.retiring[invite.LinkID] = w.view.WindowID
	}
	w.minting = nil
	close(minting)
	b.mu.Unlock()
	if current && err == nil {
		return sealed, nil
	}
	if current && invite.LinkID != "" {
		b.settle.Lock()
		err = errors.Join(err, b.retireSettling(w.view.WindowID, false))
		b.settle.Unlock()
	}
	if err != nil {
		return SealedInvitation{}, err
	}
	return SealedInvitation{}, ErrClosed
}
