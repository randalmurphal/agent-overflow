package trackerapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

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
// escape that could spell part of one (mayHold). Once the credential was
// dropped its bytes are zeroed and no body can be shown free of it, so
// every body but an empty one fails with ErrCredentialChanged.
func (s *scrubber) body(body []byte) ([]byte, error) {
	switch {
	case len(body) == 0:
		return body, nil
	case s.lost:
		return nil, ErrCredentialChanged
	case s.n == 0 || !s.mayHold(body):
		return body, nil
	}
	values, ok := jsonValues(body)
	if !ok {
		return s.bytes(body), nil
	}
	var texts []string
	for _, v := range values {
		collectStrings(v, &texts)
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
// decode. A form in the decoded text is a run of bytes, each written in
// body as itself or produced by decoding: an escape, or an invalid UTF-8
// sequence, which a decoder replaces by U+FFFD. When no escape and no
// replacement produces a byte any form holds, every run of decoded bytes
// that could be a form is written in body as itself, and the byte search
// finds it. So body may hold a form when:
//   - it holds a form as bytes;
//   - a form holds a byte of U+FFFD's encoding and body is not valid
//     UTF-8;
//   - an escape decodes to a byte a form holds: a one-letter escape to
//     its byte, a \u escape below U+0080 to that byte, and any other \u
//     escape (a surrogate half, paired or not, included) to bytes of
//     0x80 and above, which count when a form holds any such byte;
//   - an escape is malformed (a \u without four hex digits, an unknown
//     letter, a backslash that ends the body), which no decoder accepts
//     but which is not shown to be clean.
func (s *scrubber) mayHold(body []byte) bool {
	for i := range s.n {
		form := s.form(i)
		if bytes.Contains(body, form) {
			return true
		}
	}
	held := s.heldBytes()
	if (held.has(0xEF) || held.has(0xBF) || held.has(0xBD)) && !utf8.Valid(body) {
		return true
	}
	for i := bytes.IndexByte(body, '\\'); i >= 0; {
		n, decoded, ok := jsonEscape(body[i:])
		switch {
		case !ok:
			return true
		case decoded < utf8.RuneSelf && held.has(byte(decoded)):
			return true
		case decoded >= utf8.RuneSelf && held.high:
			return true
		}
		next := bytes.IndexByte(body[i+n:], '\\')
		if next < 0 {
			return false
		}
		i += n + next
	}
	return false
}

// byteSet is the bytes the forms hold.
type byteSet struct {
	in   [256]bool
	high bool // a form holds a byte of 0x80 or above
}

func (b *byteSet) has(c byte) bool { return b.in[c] }

func (s *scrubber) heldBytes() byteSet {
	var set byteSet
	for i := range s.n {
		form := s.form(i)
		for _, c := range form {
			set.in[c] = true
			set.high = set.high || c >= utf8.RuneSelf
		}
	}
	return set
}

// jsonEscape reads the escape at the start of e, which begins with a
// backslash: its length and the code point it spells, the code unit for
// a \u escape. ok is false when it is malformed.
func jsonEscape(e []byte) (n int, decoded rune, ok bool) {
	if len(e) < 2 {
		return len(e), 0, false
	}
	switch e[1] {
	case '"', '\\', '/':
		return 2, rune(e[1]), true
	case 'b':
		return 2, '\b', true
	case 'f':
		return 2, '\f', true
	case 'n':
		return 2, '\n', true
	case 'r':
		return 2, '\r', true
	case 't':
		return 2, '\t', true
	case 'u':
		if len(e) < 6 {
			return len(e), 0, false
		}
		var cp rune
		for _, c := range e[2:6] {
			var d byte
			switch {
			case '0' <= c && c <= '9':
				d = c - '0'
			case 'a' <= c && c <= 'f':
				d = c - 'a' + 10
			case 'A' <= c && c <= 'F':
				d = c - 'A' + 10
			default:
				return 2, 0, false
			}
			cp = cp<<4 | rune(d)
		}
		return 6, cp, true
	}
	return 2, 0, false
}

// bytes is body with every form replaced as bytes; body itself when it
// holds none. A copy it replaces in is cleared.
func (s *scrubber) bytes(body []byte) []byte {
	out := body
	for i := range s.n {
		form := s.form(i)
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
