package trackerapi

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"agent-overflow/internal/secret"
)

type authScheme uint8

const (
	schemeBasic authScheme = iota + 1
	schemeBearer
	schemeKey
)

// Credential is one login: a secret and how a request presents it. Every
// formatter, encoder and slog prints it as secret.Redacted; reached
// through an unexported field, reflection prints an address in place of
// the secret. A client owns the Credential its source returns and zeroes
// it when it is replaced, forgotten or closed.
type Credential struct {
	scheme authScheme
	user   string
	secret *secret.Bytes
}

// Basic presents user and token as "Authorization: Basic". The Credential
// takes ownership of token.
func Basic(user string, token *secret.Bytes) *Credential {
	return &Credential{scheme: schemeBasic, user: user, secret: token}
}

// Bearer presents token as "Authorization: Bearer <token>". The Credential
// takes ownership of token.
func Bearer(token *secret.Bytes) *Credential {
	return &Credential{scheme: schemeBearer, secret: token}
}

// Key presents key alone as "Authorization: <key>". The Credential takes
// ownership of key.
func Key(key *secret.Bytes) *Credential {
	return &Credential{scheme: schemeKey, secret: key}
}

// Zero overwrites the secret. A zeroed Credential authorizes nothing.
// Safe on nil.
func (cr *Credential) Zero() {
	if cr != nil {
		cr.secret.Zero()
	}
}

func (Credential) String() string   { return secret.Redacted }
func (Credential) GoString() string { return secret.Redacted }

// Format prints secret.Redacted for every verb and flag.
func (Credential) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(secret.Redacted)) }

func (Credential) MarshalJSON() ([]byte, error) { return []byte(`"` + secret.Redacted + `"`), nil }
func (Credential) MarshalText() ([]byte, error) { return []byte(secret.Redacted), nil }
func (Credential) LogValue() slog.Value         { return slog.StringValue(secret.Redacted) }

// check reports a credential no request can carry. The reason never
// quotes the secret.
func (cr *Credential) check() error {
	switch {
	case cr == nil:
		return errors.New("no credential")
	case cr.secret.Len() == 0:
		return errors.New("an empty secret")
	}
	switch cr.scheme {
	case schemeBasic:
		if cr.user == "" || strings.Contains(cr.user, ":") || hasControl(cr.user) {
			return errors.New("a user name Basic authentication cannot carry")
		}
	case schemeBearer, schemeKey:
		if hasControl(string(cr.secret.Reveal())) {
			return errors.New("a secret with a line break or control character")
		}
	default:
		return errors.New("a credential not made by Basic, Bearer or Key")
	}
	return nil
}

func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// authorize sets h's Authorization. A nil or zeroed credential sets
// nothing, so the tracker answers 401 and the client reads again.
func (cr *Credential) authorize(h http.Header) {
	if cr == nil || cr.secret.Len() == 0 {
		return
	}
	raw := cr.secret.Reveal()
	switch cr.scheme {
	case schemeBasic:
		plain := make([]byte, 0, len(cr.user)+1+len(raw))
		plain = append(append(append(plain, cr.user...), ':'), raw...)
		h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString(plain))
		clear(plain)
	case schemeBearer:
		h.Set("Authorization", "Bearer "+string(raw))
	case schemeKey:
		h.Set("Authorization", string(raw))
	}
}

func (cr *Credential) equal(other *Credential) bool {
	return cr.scheme == other.scheme && cr.user == other.user &&
		subtle.ConstantTimeCompare(cr.secret.Reveal(), other.secret.Reveal()) == 1
}

// CredentialSource reads a client's credential. Credential returns a new
// Credential on each call, which the client owns from then on, and
// returns when ctx ends. The client calls it from one goroutine at a time.
type CredentialSource interface {
	Credential(ctx context.Context) (*Credential, error)
}

// StaticCredentials is a CredentialSource that hands out a copy of cred on
// each read. The caller keeps ownership of cred and zeroes it once no
// client reads the source, and does not zero it while one may.
func StaticCredentials(cred *Credential) CredentialSource { return staticCredentials{cred: cred} }

type staticCredentials struct{ cred *Credential }

func (s staticCredentials) Credential(context.Context) (*Credential, error) {
	if s.cred == nil {
		return nil, errors.New("trackerapi: StaticCredentials holds no credential")
	}
	return &Credential{scheme: s.cred.scheme, user: s.cred.user, secret: secret.New(s.cred.secret.Reveal())}, nil
}

// credentialRead is one credential read in flight; err is set before done
// closes.
type credentialRead struct {
	done chan struct{}
	err  error
}

// loadCredential makes sure the client holds a credential. With refresh,
// a 401 answered the credential of generation sentGen, and the source is
// read again unless a read completed since. One read runs at a time, on
// the client's lifetime, shared by every caller waiting for it; a caller
// whose ctx ends stops waiting without ending the read.
func (c *Client) loadCredential(ctx context.Context, refresh bool, sentGen uint64) error {
	for {
		c.readMu.Lock()
		if f := c.reading; f != nil {
			c.readMu.Unlock()
			select {
			case <-f.done:
				if f.err != nil {
					return f.err
				}
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		c.credMu.RLock()
		held := c.cred != nil && (!refresh || c.credGen > sentGen)
		epoch := c.epoch
		c.credMu.RUnlock()
		if held {
			c.readMu.Unlock()
			return nil
		}
		f := &credentialRead{done: make(chan struct{})}
		if !c.startRead(func() { c.runRead(f, epoch) }) {
			c.readMu.Unlock()
			return errClosed
		}
		c.reading = f
		c.readMu.Unlock()
		select {
		case <-f.done:
			if f.err != nil {
				return f.err
			}
			// Read again when ForgetCredential dropped what this read
			// stored; otherwise the check above returns.
			refresh = false
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (c *Client) runRead(f *credentialRead, epoch uint64) {
	ctx, cancel := context.WithTimeout(c.life, credentialReadTimeout)
	defer cancel()
	f.err = c.storeCredential(ctx, epoch)
	c.readMu.Lock()
	c.reading = nil
	c.readMu.Unlock()
	close(f.done)
}

// storeCredential reads the source and keeps what it returned, zeroing
// the credential it replaces. A credential equal to the one held is
// zeroed instead and the held one kept, so a request can tell a re-read
// that changed nothing.
func (c *Client) storeCredential(ctx context.Context, epoch uint64) error {
	cred, err := c.source.Credential(ctx)
	if err != nil {
		cred.Zero()
		switch {
		case c.life.Err() != nil:
			return errClosed
		case ctx.Err() != nil:
			return fmt.Errorf("reading the credential took longer than %s: %w", credentialReadTimeout, err)
		}
		return fmt.Errorf("reading the credential: %w", err)
	}
	if err := cred.check(); err != nil {
		cred.Zero()
		return fmt.Errorf("the credential source returned %w", err)
	}
	c.credMu.Lock()
	defer c.credMu.Unlock()
	switch {
	case c.epoch != epoch:
		cred.Zero()
		return nil
	case cred == c.cred:
	case c.cred != nil && c.cred.equal(cred):
		cred.Zero()
	default:
		c.cred.Zero()
		c.cred = cred
	}
	c.credGen++
	return nil
}

// sentKey carries a request's *sent slot in its context.
type sentKey struct{}

// sent is what authTransport put on the wire for the hop that answered:
// whether it was on the base origin, and the credential and its
// generation when it was.
type sent struct {
	mu     sync.Mutex
	onBase bool
	cred   *Credential
	gen    uint64
}

func (s *sent) record(onBase bool, cred *Credential, gen uint64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onBase, s.cred, s.gen = onBase, cred, gen
}

func (s *sent) get() (onBase bool, cred *Credential, gen uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.onBase, s.cred, s.gen
}

// authTransport is where a credential meets a request. The request a
// Client builds carries none; each hop, redirects included, is cloned and
// authorized only when it is on the base origin, so no request a caller,
// an error or a redirect copy can see holds the secret.
type authTransport struct {
	c    *Client
	base http.RoundTripper
}

func (t authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	slot, _ := req.Context().Value(sentKey{}).(*sent)
	if originOf(req.URL) != t.c.origin {
		slot.record(false, nil, 0)
		return t.base.RoundTrip(req)
	}
	hop := req.Clone(req.Context())
	t.c.credMu.RLock()
	t.c.cred.authorize(hop.Header)
	cred, gen := t.c.cred, t.c.credGen
	t.c.credMu.RUnlock()
	slot.record(true, cred, gen)
	return t.base.RoundTrip(hop)
}
