package trackerapi

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"

	"agent-overflow/internal/secret"
)

// sentAuth is the Authorization value a request carried and its scheme.
// The value is the request's own header string; a Response holds it
// behind a pointer, which formatters print as an address.
type sentAuth struct {
	scheme authScheme
	value  string
}

// Scrub returns text with the credential this answer's request carried
// replaced by secret.Redacted, in every form the transport removes from
// its own errors. A caller passes it any text taken from the answer that
// it places in an error or returns for display, such as an error list
// inside a 200 body; the transport's own errors are already scrubbed. A
// request off the base origin carried no credential, and text comes back
// unchanged.
func (r *Response) Scrub(text string) string {
	if r == nil || r.auth == nil {
		return text
	}
	s := newScrubber(r.auth.scheme, r.auth.value)
	defer s.clear()
	return s.text(text)
}

// scrubber replaces, in text an answer supplied, the credential its
// request carried, in each form a tracker can repeat it: the
// Authorization value, the value without its scheme, and the secret
// alone. Its forms are copies made for one use and cleared after it.
type scrubber struct{ forms [][]byte }

// newScrubber derives the forms from the Authorization value a hop sent;
// a hop off the base origin carried none and has nothing to scrub. They
// are added longest first, so a value goes whole before the secret inside
// it.
func newScrubber(scheme authScheme, auth string) *scrubber {
	s := &scrubber{}
	if auth == "" {
		return s
	}
	s.add([]byte(auth))
	switch scheme {
	case schemeBasic:
		encoded := strings.TrimPrefix(auth, "Basic ")
		s.add([]byte(encoded))
		if pair, err := base64.StdEncoding.DecodeString(encoded); err == nil {
			if _, password, ok := bytes.Cut(pair, []byte(":")); ok {
				s.add(bytes.Clone(password))
			}
			clear(pair)
		}
	case schemeBearer:
		s.add([]byte(strings.TrimPrefix(auth, "Bearer ")))
	}
	return s
}

func (s *scrubber) add(form []byte) {
	if len(form) > 0 {
		s.forms = append(s.forms, form)
	}
}

// clear zeroes the forms.
func (s *scrubber) clear() {
	for _, form := range s.forms {
		clear(form)
	}
}

// text is v with every form replaced by secret.Redacted.
func (s *scrubber) text(v string) string {
	b := []byte(v)
	replaced := false
	for _, form := range s.forms {
		if bytes.Contains(b, form) {
			b = bytes.ReplaceAll(b, form, []byte(secret.Redacted))
			replaced = true
		}
	}
	if !replaced {
		return v
	}
	return string(b)
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

// error is err, or when its text holds a form, an error of the scrubbed
// text alone: the original's chain would still reach the secret.
func (s *scrubber) error(err error) error {
	if err == nil || len(s.forms) == 0 {
		return err
	}
	defer s.clear()
	if text := s.text(err.Error()); text != err.Error() {
		return errors.New(text)
	}
	return err
}

// answerText is the text of an error answer that an error carries: the
// dialect's problem and the body, at most maxErrorBody bytes, scrubbed of
// the credential the request carried. Every error built from an answer's
// text takes it from here.
func (c *Client) answerText(a *answer) (Problem, string) {
	s := newScrubber(a.sent.scheme, a.sent.auth)
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
