// Package jstexttest holds test helpers for text a JavaScript client
// receives: what JSON delivers for a string, how packed text decodes, and
// text with every shape a patch carries. Only tests import it. It does not
// import jstext, so jstext's own tests can use it.
package jstexttest

import (
	"encoding/json"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"
)

// JSONDelivered is the string a JSON client receives for raw: each invalid
// byte is one U+FFFD.
func JSONDelivered(t testing.TB, raw string) string {
	t.Helper()
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got string
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return got
}

// Unpack decodes packed text: each character's code point is one byte of
// UTF-8, decoded as a JSON client's TextDecoder would.
func Unpack(t testing.TB, packed string) string {
	t.Helper()
	out := make([]byte, 0, len(packed))
	for _, r := range packed {
		if r > 0xFF {
			t.Fatalf("packed text holds %U, past U+00FF", r)
		}
		out = append(out, byte(r))
	}
	if !utf8.Valid(out) {
		t.Fatalf("packed text unpacks to invalid UTF-8 %q", out)
	}
	return string(out)
}

// ArbitraryText is about size bytes drawn from characters of every UTF-8
// length and the invalid shapes a non-UTF-8 file carries, with lines from
// empty to longer than longLine bytes. It holds no NUL, so git diffs it as
// text.
func ArbitraryText(rng *rand.Rand, size, longLine int) string {
	inLine := []string{"a", "plain ascii ", "é", "—", "\U0001F600", "\xff", "\xc3", "\xe2\x82", "\xf0\x9f\x98", "\xed\xa0\x80", "\x80", "\r"}
	var b strings.Builder
	for b.Len() < size {
		switch rng.IntN(512) {
		case 0:
			for end := b.Len() + longLine + rng.IntN(longLine); b.Len() < end; {
				b.WriteString(inLine[rng.IntN(len(inLine))])
			}
		case 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16:
			b.WriteByte('\n')
		default:
			b.WriteString(inLine[rng.IntN(len(inLine))])
		}
	}
	return b.String()
}
