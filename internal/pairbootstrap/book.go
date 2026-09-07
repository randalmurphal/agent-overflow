package pairbootstrap

import (
	"crypto/ecdh"
	"encoding/json"
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
type Book struct {
	mu     sync.Mutex
	window *window
	cancel func(string)
}

type window struct {
	view    Snapshot
	mint    func() (Invitation, error)
	timer   *time.Timer
	request *request
}

type request struct {
	commitment Commitment
	challenge  Challenge
	key        *ecdh.PrivateKey
	sealed     *SealedInvitation
	minting    bool
}

func NewBook(cancel func(string)) *Book { return &Book{cancel: cancel} }

func (b *Book) Open(mint func() (Invitation, error)) Snapshot {
	return b.open(mint, WindowTTL)
}

func (b *Book) open(mint func() (Invitation, error), ttl time.Duration) Snapshot {
	b.mu.Lock()
	old := b.window
	if old != nil {
		old.timer.Stop()
	}
	w := &window{mint: mint, view: Snapshot{Open: true, WindowID: encode(random(16)), ExpiresAtMs: time.Now().Add(ttl).UnixMilli()}}
	b.window = w
	w.timer = time.AfterFunc(ttl, func() { b.Close(w.view.WindowID) })
	view := w.view
	b.mu.Unlock()
	b.cancelWindow(old)
	return view
}

// Close cannot retire a replacement window opened after an old UI disappeared.
func (b *Book) Close(windowID string) {
	b.mu.Lock()
	w := b.window
	if w == nil || w.view.WindowID != windowID {
		b.mu.Unlock()
		return
	}
	b.window = nil
	w.timer.Stop()
	b.mu.Unlock()
	b.cancelWindow(w)
}

func (b *Book) cancelWindow(w *window) {
	if w != nil && w.view.LinkID != "" && b.cancel != nil {
		b.cancel(w.view.LinkID)
	}
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
	if req.minting {
		b.mu.Unlock()
		return SealedInvitation{}, ErrBusy
	}
	aead, transcript, sas, err := derive(req.key, r.PublicKey, req.commitment, req.challenge, r)
	if err != nil {
		b.mu.Unlock()
		return SealedInvitation{}, err
	}
	req.minting = true
	b.mu.Unlock()

	// Minting touches the caller's ordinary invitation store, never under the
	// book lock. A concurrent close cancels any late result before publishing it.
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
	if current && err == nil {
		req.sealed = &sealed
		req.key = nil
		w.view.Label, w.view.Platform, w.view.VerificationNumber, w.view.LinkID = r.Label, r.Platform, sas, invite.LinkID
	}
	req.minting = false
	b.mu.Unlock()
	if !current || err != nil {
		if invite.LinkID != "" && b.cancel != nil {
			b.cancel(invite.LinkID)
		}
		if err != nil {
			return SealedInvitation{}, err
		}
		return SealedInvitation{}, ErrClosed
	}
	return sealed, nil
}
