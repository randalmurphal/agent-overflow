package trackerapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"agent-overflow/internal/secret"
)

// ErrCredentialChanged is an answer that arrived as the credential its
// request carried was dropped (a re-read replaced it, ForgetCredential,
// Close): its body can no longer be checked for that credential, so it is
// not returned. A write it answered may have been applied.
var ErrCredentialChanged = errors.New("trackerapi: the credential changed while the answer was read; read it again")

// body is an answer's body as Response.Body exposes it: every place a
// reader could find a form of the credential holds secret.Redacted
// instead. A JSON body has it replaced in every string, object keys
// included, as the strings decode, so a form written with JSON escapes is
// found; any other body has the forms replaced as bytes.
//
// A form outside every string, which only a credential that reads as a
// JSON number or literal could be, is replaced as bytes too: the body
// then no longer decodes, and the caller refuses it.
//
// The common answer holds no form and is returned as the same slice,
// with no copy: the walk runs only when the bytes hold a form, or an
// escape that could spell part of one (mayHold). A body with no string
// cannot hold the credential and is kept even once it was dropped; any
// other body then fails with ErrCredentialChanged.
func (s *scrubber) body(body []byte) ([]byte, error) {
	if len(body) == 0 || !s.lost && (len(s.forms) == 0 || !s.mayHold(body)) {
		return body, nil
	}
	values, ok := jsonValues(body)
	if !ok {
		if s.lost {
			return nil, ErrCredentialChanged
		}
		return s.bytes(body), nil
	}
	var texts []string
	for _, v := range values {
		collectStrings(v, &texts)
	}
	if len(texts) == 0 {
		return s.bytes(body), nil
	}
	if s.lost {
		return nil, ErrCredentialChanged
	}
	// Each string is a part of the joined text, so none holds a form when
	// the joined text holds none.
	if !s.holds(strings.Join(texts, "\x00")) {
		return s.bytes(body), nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, v := range values {
		if err := enc.Encode(scrubStrings(v, s.text)); err != nil {
			// Every value decoded from JSON encodes again.
			return nil, errors.New("trackerapi: the scrubbed answer did not encode")
		}
	}
	return s.bytes(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))), nil
}

// mayHold reports whether body could hold a form once its JSON strings
// decode: it holds one as bytes, or holds an escape that decodes to a
// byte some form holds. A \u escape can spell any byte, so it always
// counts; the others decode to one byte each.
func (s *scrubber) mayHold(body []byte) bool {
	for _, form := range s.forms {
		if bytes.Contains(body, form) {
			return true
		}
	}
	for i := bytes.IndexByte(body, '\\'); i >= 0 && i+1 < len(body); {
		c := body[i+1]
		if c == 'u' {
			return true
		}
		if d, ok := escapes[c]; ok && s.formHolds(d) {
			return true
		}
		next := bytes.IndexByte(body[i+2:], '\\')
		if next < 0 {
			break
		}
		i += 2 + next
	}
	return false
}

// escapes are JSON's one-byte escapes by the letter after the backslash.
var escapes = map[byte]byte{'"': '"', '\\': '\\', '/': '/', 'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t'}

// formHolds reports whether a form holds byte b.
func (s *scrubber) formHolds(b byte) bool {
	for _, form := range s.forms {
		if bytes.IndexByte(form, b) >= 0 {
			return true
		}
	}
	return false
}

// bytes is body with every form replaced as bytes; body itself when it
// holds none. A copy it replaces in is cleared.
func (s *scrubber) bytes(body []byte) []byte {
	out := body
	for _, form := range s.forms {
		if bytes.Contains(out, form) {
			next := bytes.ReplaceAll(out, form, []byte(secret.Redacted))
			if &out[0] != &body[0] {
				clear(out)
			}
			out = next
		}
	}
	return out
}

// jsonValues decodes body as one or more JSON values, numbers kept as
// written; ok is false when it is not JSON throughout.
func jsonValues(body []byte) (values []any, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	for {
		var v any
		err := dec.Decode(&v)
		if errors.Is(err, io.EOF) {
			return values, len(values) > 0
		}
		if err != nil {
			return nil, false
		}
		values = append(values, v)
	}
}

// collectStrings appends every string in a decoded JSON value, object
// keys included.
func collectStrings(v any, out *[]string) {
	switch v := v.(type) {
	case string:
		*out = append(*out, v)
	case map[string]any:
		for k, e := range v {
			*out = append(*out, k)
			collectStrings(e, out)
		}
	case []any:
		for _, e := range v {
			collectStrings(e, out)
		}
	}
}

// scrubStrings is v with scrub applied to every string, object keys
// included.
func scrubStrings(v any, scrub func(string) string) any {
	switch v := v.(type) {
	case string:
		return scrub(v)
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			out[scrub(k)] = scrubStrings(e, scrub)
		}
		return out
	case []any:
		for i, e := range v {
			v[i] = scrubStrings(e, scrub)
		}
		return v
	}
	return v
}
