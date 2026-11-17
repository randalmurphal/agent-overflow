package trackerapi

import (
	"bytes"
	"context"
	"errors"
	"io"

	"agent-overflow/internal/secret"
)

// scrubRef is what a Response, a *StatusError and an *AuthError keep of
// the credential their request carried, for Scrub and CredentialMark: the
// client, the credential and the generation of the read that stored it.
// The client and credential are pointers, which every fmt path prints as
// an address, and the secret stays in the Credential's secret.Bytes: none
// of them holds a copy.
type scrubRef struct {
	c    *Client
	cred *Credential
	read uint64
}

// scrubRef is what an answer to a hop that sent sent keeps; nil when the
// request carried no credential.
func (c *Client) scrubRef(sent sentWith) *scrubRef {
	if !sent.carried {
		return nil
	}
	return &scrubRef{c: c, cred: sent.cred, read: sent.read}
}

// mark is the CredentialMark of every type that keeps r; zero when the
// request carried no credential.
func (r *scrubRef) mark() CredentialMark {
	if r == nil {
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
	if r == nil {
		return text
	}
	s := r.c.scrubber(sentWith{carried: true, cred: r.cred})
	defer s.clear()
	return s.text(text)
}

// Scrub returns text with the credential this answer's request carried
// replaced by secret.Redacted, in every form the transport removes from
// its own errors. A caller passes it any text taken from the answer that
// it places in an error or returns for display, such as an error list
// inside a 200 body; the transport's own errors are already scrubbed.
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
type scrubber struct {
	forms [][]byte
	// lost is a credential zeroed since it was sent: it can no longer be
	// found, so every text is withheld.
	lost bool
}

// scrubber derives the forms of the credential a hop carried, under the
// credential lock that guards its zeroing.
func (c *Client) scrubber(sent sentWith) *scrubber {
	s := &scrubber{}
	if !sent.carried {
		return s
	}
	c.credMu.RLock()
	defer c.credMu.RUnlock()
	if sent.cred.secret.Len() == 0 {
		s.lost = true
		return s
	}
	s.forms = sent.cred.forms()
	return s
}

func (s *scrubber) clear() { clearForms(s.forms) }

// text is v with every form replaced by secret.Redacted.
func (s *scrubber) text(v string) string {
	if v == "" {
		return v
	}
	if s.lost {
		return secret.Redacted
	}
	b := []byte(v)
	for _, form := range s.forms {
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

// holds reports whether v holds a form.
func (s *scrubber) holds(v string) bool {
	b := []byte(v)
	defer clear(b)
	for _, form := range s.forms {
		if bytes.Contains(b, form) {
			return true
		}
	}
	return false
}

// trimCut drops the end of a body cut short when it begins a form, which
// the cut split so no whole form matches.
func (s *scrubber) trimCut(body []byte) []byte {
	keep := len(body)
	for _, form := range s.forms {
		for n := min(len(form)-1, len(body)); n > 0; n-- {
			if bytes.HasSuffix(body, form[:n]) {
				keep = min(keep, len(body)-n)
				break
			}
		}
	}
	return body[:keep]
}

// sentinels are the errors whose identity a scrubbed cause keeps. Their
// text is fixed, never the tracker's.
var sentinels = []error{context.Canceled, context.DeadlineExceeded, errClosed, errStalled, io.EOF, io.ErrUnexpectedEOF}

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
	s := c.scrubber(sent)
	defer s.clear()
	if !s.lost && !s.inChain(err) {
		return err
	}
	scrubbed := &scrubbedError{text: s.text(err.Error())}
	for _, sentinel := range sentinels {
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
// carried. Every error built from an answer's text takes it from here.
func (c *Client) answerText(a *answer) (Problem, string) {
	s := c.scrubber(a.sent)
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
	text := s.text(string(body))
	if len(text) > maxErrorBody {
		text = text[:maxErrorBody]
	}
	return problem, text
}
