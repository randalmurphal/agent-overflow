package trackerapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"unsafe"

	"agent-overflow/internal/secret"
)

// scrubRef is what a Response, a *StatusError and an *AuthError keep of
// the credential their request carried, for Scrub and CredentialMark: the
// client, the credential and the generation of the read that stored it.
// The client and credential are pointers, which every fmt path prints as
// an address, and the secret stays in the Credential's secret.Bytes: none
// of them holds a copy. A nil scrubRef, and the zero one, is a request
// that carried no credential.
type scrubRef struct {
	c    *Client
	cred *Credential
	read uint64
}

// refOf is what an answer to a hop that sent sent keeps; the zero
// scrubRef when the request carried no credential.
func (c *Client) refOf(sent sentWith) scrubRef {
	if !sent.carried {
		return scrubRef{}
	}
	return scrubRef{c: c, cred: sent.cred, read: sent.read}
}

// scrubRef is refOf for an error, nil when the request carried no
// credential.
func (c *Client) scrubRef(sent sentWith) *scrubRef {
	if !sent.carried {
		return nil
	}
	r := c.refOf(sent)
	return &r
}

// mark is the CredentialMark of every type that keeps r; zero when the
// request carried no credential.
func (r *scrubRef) mark() CredentialMark {
	if r == nil || r.c == nil {
		return CredentialMark{}
	}
	return CredentialMark{client: r.c.tag, read: r.read}
}

// CredentialMark tells which read of the client's credential source this
// answer's request carried; zero when it carried none. Compare two marks
// to learn whether two answers were drawn under the same credential.
func (r *Response) CredentialMark() CredentialMark {
	if r == nil {
		return CredentialMark{}
	}
	return r.scrub.mark()
}

// CredentialMark is Response.CredentialMark for the answer e came from.
func (e *StatusError) CredentialMark() CredentialMark {
	if e == nil {
		return CredentialMark{}
	}
	return e.scrub.mark()
}

// CredentialMark is Response.CredentialMark for the answer e came from.
func (e *AuthError) CredentialMark() CredentialMark {
	if e == nil {
		return CredentialMark{}
	}
	return e.scrub.mark()
}

// text is every Scrub method: text with the credential replaced, the
// text unchanged when the request carried none (r is nil), and
// secret.Redacted for any non-empty text once the credential is zeroed.
func (r *scrubRef) text(text string) string {
	if r == nil || r.c == nil {
		return text
	}
	var s scrubber
	r.c.loadScrubber(&s, sentWith{carried: true, cred: r.cred})
	defer s.clear()
	return s.text(text)
}

// Scrub returns text with the credential this answer's request carried
// replaced by secret.Redacted, in every form the transport removes from
// its own errors. Body and the transport's own errors are already
// scrubbed; a caller passes Scrub other text about the answer it places
// in an error, such as a decode error's.
//
// A request off the base origin carried no credential, and text comes
// back unchanged. Once the client no longer holds that credential (a
// re-read replaced it, ForgetCredential, Close), its bytes are zeroed and
// Scrub cannot find them: it returns secret.Redacted in place of any
// non-empty text.
func (r *Response) Scrub(text string) string {
	if r == nil {
		return text
	}
	return r.scrub.text(text)
}

// scrubber replaces, in text an answer supplied, the credential its
// request carried, in each form Credential.forms names. Its forms are
// copies made for one use and cleared after it.
//
// The forms lie back to back in buf, or in big when they do not fit, and
// form i ends at ends[i]. A scrubber declared as a local variable stays
// on the stack, since it holds no pointer into itself, so the common
// credential is copied without allocating.
type scrubber struct {
	// lost is a credential zeroed since it was sent: it can no longer be
	// found, so every text is withheld.
	lost bool
	n    int
	ends [maxForms]int
	buf  [1 << 10]byte
	big  []byte
}

// loadScrubber sets s to the forms of the credential a hop carried,
// copied under the credential lock that guards its zeroing, and to no
// forms for a hop that carried none. Whatever s held before is cleared.
// The caller clears s after use.
func (c *Client) loadScrubber(s *scrubber, sent sentWith) {
	if !sent.carried {
		s.reset()
		return
	}
	c.credMu.RLock()
	defer c.credMu.RUnlock()
	if sent.cred.secret.Len() == 0 {
		s.reset()
		s.lost = true
		return
	}
	s.load(sent.cred)
}

// reset clears the forms' copy and returns s to the zero scrubber.
func (s *scrubber) reset() {
	s.clear()
	s.lost, s.n, s.big = false, 0, nil
}

// load sets s to cr's forms, clearing whatever it held. The caller holds
// the credential lock, or owns cr.
func (s *scrubber) load(cr *Credential) {
	s.reset()
	wire := cr.wire.Reveal()
	if len(wire) > len(s.buf) {
		s.big = bytes.Clone(wire)
	} else {
		copy(s.buf[:], wire)
	}
	s.n, s.ends = cr.n, cr.ends
}

// form is form i, a view into s.
func (s *scrubber) form(i int) []byte {
	start := 0
	if i > 0 {
		start = s.ends[i-1]
	}
	backing := s.buf[:]
	if s.big != nil {
		backing = s.big
	}
	return backing[start:s.ends[i]:s.ends[i]]
}

// clear zeroes the forms' copy.
func (s *scrubber) clear() {
	if s.n > 0 && s.big == nil {
		clear(s.buf[:s.ends[s.n-1]])
	}
	clear(s.big)
}

// text is v with every form replaced by secret.Redacted.
func (s *scrubber) text(v string) string {
	if v == "" {
		return v
	}
	if s.lost {
		return secret.Redacted
	}
	if !s.holds(v) {
		return v
	}
	b := []byte(v)
	for i := range s.n {
		form := s.form(i)
		if bytes.Contains(b, form) {
			next := bytes.ReplaceAll(b, form, []byte(secret.Redacted))
			clear(b)
			b = next
		}
	}
	out := string(b)
	clear(b)
	return out
}

// holds reports whether v holds a form. It copies neither: each form is
// searched for through a string view of the scrubber's own copy, which
// nothing changes during the search.
func (s *scrubber) holds(v string) bool {
	for i := range s.n {
		form := s.form(i)
		if strings.Contains(v, unsafe.String(unsafe.SliceData(form), len(form))) {
			return true
		}
	}
	return false
}

// maxDecodes bounds how many layers of percent-encoding holdsEncoded
// undoes.
const maxDecodes = 4

// holdsEncoded reports whether v holds a form as written or under
// percent-encoding, which a caller that parses v as a URL undoes.
func (s *scrubber) holdsEncoded(v string) bool {
	for range maxDecodes {
		if s.holds(v) {
			return true
		}
		decoded, err := url.PathUnescape(v)
		if err != nil || decoded == v {
			return false
		}
		v = decoded
	}
	return s.holds(v)
}

// trimCut drops the end of a body cut short when it begins a form, which
// the cut split so no whole form matches.
func (s *scrubber) trimCut(body []byte) []byte {
	keep := len(body)
	for i := range s.n {
		form := s.form(i)
		for n := min(len(form)-1, len(body)); n > 0; n-- {
			if bytes.HasSuffix(body, form[:n]) {
				keep = min(keep, len(body)-n)
				break
			}
		}
	}
	return body[:keep]
}

// cause is how an error from another package (net/http, net, crypto/tls,
// encoding/json, io) enters an error this package returns, and the only
// way its text does. It is err itself when no error in err's chain
// repeats the credential the request carried. Otherwise it is an error of
// err's scrubbed text that unwraps only to the sentinels err matched, so
// nothing a caller can unwrap holds the secret.
func (c *Client) cause(sent sentWith, err error) error {
	if err == nil {
		return nil
	}
	var s scrubber
	c.loadScrubber(&s, sent)
	defer s.clear()
	if !s.lost && !s.inChain(err) {
		return err
	}
	scrubbed := &scrubbedError{text: s.text(err.Error())}
	// The errors whose identity a scrubbed cause keeps: their text is
	// fixed, never the tracker's.
	for _, sentinel := range [...]error{context.Canceled, context.DeadlineExceeded, errClosed, errStalled, io.EOF, io.ErrUnexpectedEOF} {
		if errors.Is(err, sentinel) {
			scrubbed.matches = append(scrubbed.matches, sentinel)
		}
	}
	return scrubbed
}

// inChain reports whether err, or any error a caller can unwrap from it,
// holds a form in its text.
func (s *scrubber) inChain(err error) bool {
	for stack := []error{err}; len(stack) > 0; {
		e := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if e == nil {
			continue
		}
		if s.holds(e.Error()) {
			return true
		}
		switch u := e.(type) {
		case interface{ Unwrap() error }:
			stack = append(stack, u.Unwrap())
		case interface{ Unwrap() []error }:
			stack = append(stack, u.Unwrap()...)
		}
	}
	return false
}

type scrubbedError struct {
	text    string
	matches []error
}

func (e *scrubbedError) Error() string   { return e.text }
func (e *scrubbedError) Unwrap() []error { return e.matches }

// answerText is the text of an error answer that an error carries: the
// dialect's problem, scrubbed after the dialect returns it, and the body,
// at most maxErrorBody bytes, scrubbed of the credential the request
// carried as Response.Body is, so decoding it finds none either. Every
// error built from an answer's text takes it from here.
func (c *Client) answerText(a *answer) (Problem, string) {
	var s scrubber
	c.loadScrubber(&s, a.sent)
	defer s.clear()
	body := a.body
	if a.cut {
		body = s.trimCut(body)
	}
	problem := c.dialect.Problem(a.status, body)
	problem.Message = s.text(problem.Message)
	if len(problem.Fields) > 0 {
		fields := make(map[string]string, len(problem.Fields))
		for name, message := range problem.Fields {
			fields[s.text(name)] = s.text(message)
		}
		problem.Fields = fields
	}
	// The body as a caller may decode it, then as text: a dropped
	// credential leaves the error for s.text to withhold.
	if scrubbed, err := s.body(body); err == nil {
		body = scrubbed
	}
	text := s.text(string(body))
	if len(text) > maxErrorBody {
		text = text[:maxErrorBody]
	}
	return problem, text
}
