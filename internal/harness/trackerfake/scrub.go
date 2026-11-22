package trackerfake

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/lexer"
)

// scrubText is s with every secret replaced by the redacted marker,
// whether it is written plainly or with any of its characters escaped as
// the JSON and GraphQL string grammars allow (\uXXXX, a surrogate pair,
// \u{...}, \/ and the other one-character escapes), at any depth of
// escaping: an escape's backslash may itself be escaped, as quoting an
// escaped text again does. So no decoding of the text yields a secret.
// secrets come longest first, so one that holds another is replaced
// whole.
func scrubText(s string, secrets []string) string {
	if len(secrets) == 0 {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		end := -1
		for _, sec := range secrets {
			if n, ok := matchSecret(s, i, sec); ok && sec != "" {
				end = n
				break
			}
		}
		if end < 0 {
			b.WriteByte(s[i])
			i++
			continue
		}
		b.WriteString(redacted)
		i = end
	}
	return b.String()
}

// matchSecret reports whether s from i reads as sec, each character
// plain or escaped, and where that reading ends.
func matchSecret(s string, i int, sec string) (int, bool) {
	if sec == "" {
		return i, true
	}
	if i >= len(s) {
		return 0, false
	}
	want, w := utf8.DecodeRuneInString(sec)
	if r, n := utf8.DecodeRuneInString(s[i:]); r == want && (r != utf8.RuneError || n > 1) {
		if end, ok := matchSecret(s, i+n, sec[w:]); ok {
			return end, true
		}
	}
	if r, n, ok := escapedRune(s[i:]); ok && r == want {
		return matchSecret(s, i+n, sec[w:])
	}
	return 0, false
}

// escapedRune decodes the escape s starts with: a run of backslashes
// (one, or more where the escape was itself escaped) and then a JSON or
// GraphQL string escape, or, for two or more, a backslash.
func escapedRune(s string) (rune, int, bool) {
	run := backslashes(s)
	if run == 0 {
		return 0, 0, false
	}
	if run == len(s) {
		return '\\', run, run >= 2
	}
	switch c := s[run]; c {
	case '"', '/':
		return rune(c), run + 1, true
	case 'b':
		return '\b', run + 1, true
	case 'f':
		return '\f', run + 1, true
	case 'n':
		return '\n', run + 1, true
	case 'r':
		return '\r', run + 1, true
	case 't':
		return '\t', run + 1, true
	case 'u':
	default:
		return '\\', run, run >= 2
	}
	at := run + 1
	if at < len(s) && s[at] == '{' {
		end := strings.IndexByte(s[at:min(len(s), at+10)], '}')
		if end < 2 {
			return 0, 0, false
		}
		v, err := strconv.ParseUint(s[at+1:at+end], 16, 32)
		if err != nil || v > utf8.MaxRune {
			return 0, 0, false
		}
		return rune(v), at + end + 1, true
	}
	hi, ok := hex4(s[at:])
	if !ok {
		return 0, 0, false
	}
	at += 4
	if utf16.IsSurrogate(hi) {
		if next := backslashes(s[at:]); next > 0 && at+next < len(s) && s[at+next] == 'u' {
			if lo, ok := hex4(s[at+next+1:]); ok {
				if r := utf16.DecodeRune(hi, lo); r != utf8.RuneError {
					return r, at + next + 5, true
				}
			}
		}
	}
	return hi, at, true
}

// backslashes counts the backslashes s starts with.
func backslashes(s string) int {
	n := 0
	for n < len(s) && s[n] == '\\' {
		n++
	}
	return n
}

func hex4(s string) (rune, bool) {
	if len(s) < 4 {
		return 0, false
	}
	v, err := strconv.ParseUint(s[:4], 16, 16)
	return rune(v), err == nil
}

// scrubDocument is a GraphQL document with every secret replaced. A
// string literal, block strings included, is judged by its value as the
// lexer reads it, and one whose value holds a secret is written again,
// as a plain string literal, with the secret replaced. The text between
// literals (names, numbers, punctuation, comments, white space) is
// scrubbed as text, and the whole once more for a secret written plainly
// across a literal's edge. ok is false when the document does not lex or
// a literal's span does not lex back to that literal: the caller then
// leaves the document out.
func scrubDocument(doc string, secrets []string) (string, bool) {
	runes := []rune(doc)
	lex := lexer.New(&ast.Source{Input: doc})
	var b strings.Builder
	text, last := 0, 0
	for {
		tok, err := lex.ReadToken()
		if err != nil {
			return "", false
		}
		start, end := tok.Pos.Start, tok.Pos.End
		if tok.Kind == lexer.EOF {
			start, end = len(runes), len(runes)
		}
		if start < last || end < start || end > len(runes) {
			return "", false
		}
		last = end
		if tok.Kind != lexer.EOF && tok.Kind != lexer.String && tok.Kind != lexer.BlockString {
			continue
		}
		b.WriteString(scrubText(string(runes[text:start]), secrets))
		text = end
		if tok.Kind == lexer.EOF {
			return plainScrub(b.String(), secrets), true
		}
		span := string(runes[start:end])
		if !lexesTo(span, tok) {
			return "", false
		}
		clean := plainScrub(tok.Value, secrets)
		if clean == tok.Value {
			b.WriteString(span)
			continue
		}
		var literal bytes.Buffer
		enc := json.NewEncoder(&literal)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(clean); err != nil {
			return "", false
		}
		b.Write(bytes.TrimSuffix(literal.Bytes(), []byte("\n")))
	}
}

// lexesTo reports whether span is exactly one token equal to tok.
func lexesTo(span string, tok lexer.Token) bool {
	lex := lexer.New(&ast.Source{Input: span})
	got, err := lex.ReadToken()
	if err != nil || got.Kind != tok.Kind || got.Value != tok.Value {
		return false
	}
	next, err := lex.ReadToken()
	return err == nil && next.Kind == lexer.EOF
}

// plainScrub replaces each secret where it is written plainly.
func plainScrub(s string, secrets []string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, redacted)
		}
	}
	return s
}
