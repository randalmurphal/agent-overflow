package trackerapi

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

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

// MinSecretLen is the shortest secret a request may carry: one byte more
// than secret.Redacted. A shorter one cannot be told from ordinary answer
// text, so scrubbing it mangles answers, and one that is part of the
// marker would survive inside it. No tracker issues tokens that short. A
// caller that reads a token from a person checks it against this to
// refuse it in the person's terms.
const MinSecretLen = len(secret.Redacted) + 1

// check reports a credential no request can carry. The reason never
// quotes the secret.
func (cr *Credential) check() error {
	switch {
	case cr == nil:
		return errors.New("no credential")
	case cr.secret.Len() == 0:
		return errors.New("an empty secret")
	case cr.secret.Len() < MinSecretLen:
		return errors.New("a secret too short to be a token")
	}
	switch cr.scheme {
	case schemeBasic:
		if cr.user == "" || strings.Contains(cr.user, ":") || hasControl(cr.user) {
			return errors.New("a user name Basic authentication cannot carry")
		}
	case schemeBearer, schemeKey:
		if bytes.ContainsFunc(cr.secret.Reveal(), func(r rune) bool { return r < 0x20 || r == 0x7f }) {
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

// authorize sets h's Authorization and reports whether it did. A nil or
// zeroed credential sets nothing, so the tracker answers 401 and the
// client reads again. The header's string is a copy net/http requires;
// it lives as long as the request.
func (cr *Credential) authorize(h http.Header) bool {
	if cr == nil || cr.secret.Len() == 0 {
		return false
	}
	forms := cr.forms()
	h.Set("Authorization", string(forms[0]))
	clearForms(forms)
	return true
}

// forms is every form a request carries the credential in and a tracker
// can repeat it in, longest first: the Authorization value, the value
// without its scheme, and the secret alone. They are copies; the caller
// holds the credential lock while reading them out and clears them.
func (cr *Credential) forms() [][]byte {
	raw := cr.secret.Reveal()
	switch cr.scheme {
	case schemeBasic:
		plain := make([]byte, 0, len(cr.user)+1+len(raw))
		plain = append(append(append(plain, cr.user...), ':'), raw...)
		encoded := make([]byte, base64.StdEncoding.EncodedLen(len(plain)))
		base64.StdEncoding.Encode(encoded, plain)
		clear(plain)
		return [][]byte{append([]byte("Basic "), encoded...), encoded, bytes.Clone(raw)}
	case schemeBearer:
		return [][]byte{append([]byte("Bearer "), raw...), bytes.Clone(raw)}
	}
	return [][]byte{bytes.Clone(raw)}
}

func clearForms(forms [][]byte) {
	for _, form := range forms {
		clear(form)
	}
}

func (cr *Credential) equal(other *Credential) bool {
	return cr.scheme == other.scheme && cr.user == other.user &&
		subtle.ConstantTimeCompare(cr.secret.Reveal(), other.secret.Reveal()) == 1
}

// CredentialMark tells which read of a client's credential source a
// request carried. Two marks are equal exactly when their requests
// carried the credential stored by the same read on the same client; a
// re-read that returns an equal credential keeps the one held, and its
// mark. The zero mark is a request that carried no credential (off the
// base origin) and equals no other. A mark holds a generation number and a
// per-client tag, never secret bytes.
type CredentialMark struct {
	client *clientTag
	read   uint64
}

// clientTag identifies a client in its marks. It has a size, so two tags
// never share an address.
type clientTag struct{ _ byte }

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
// whose ctx ends stops waiting without ending the read. A refresh that
// fails drops the refused credential (see runRead).
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
		if !c.startRead(func() { c.runRead(f, epoch, refresh) }) {
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

// runRead is one shared read. A refresh that fails drops the credential
// held, as ForgetCredential does: it is the one the 401 refused, since
// reads run one at a time and none completed since the refusal (or a
// forget already dropped it). The tracker refused it and the source no
// longer returns it, so nothing may be sent with it, and the next call
// reads the source again. A call still in flight with it then gets
// ErrCredentialChanged for a 2xx answer, since its body can no longer be
// checked for the zeroed bytes.
func (c *Client) runRead(f *credentialRead, epoch uint64, refresh bool) {
	ctx, cancel := context.WithTimeout(c.life, credentialReadTimeout)
	defer cancel()
	f.err = c.storeCredential(ctx, epoch)
	if f.err != nil && refresh {
		c.ForgetCredential()
	}
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
		c.credRead = c.credGen + 1 // the generation this read completes
	}
	c.credGen++
	return nil
}
