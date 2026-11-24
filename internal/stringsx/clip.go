package stringsx

import "unicode/utf8"

// ClipRunes returns s if its byte length is <= maxBytes, otherwise the
// first maxBytes bytes backed off to the previous rune boundary, so the
// result never carries a torn UTF-8 sequence and never exceeds the
// budget. If maxBytes is non-positive, returns the empty string.
func ClipRunes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if maxBytes >= len(s) {
		return s
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}

// TailRunes is ClipRunes from the other end: s if its byte length is <=
// maxBytes, otherwise the LAST maxBytes bytes advanced to the next rune
// boundary. Same guarantee — valid UTF-8, never over budget — for the
// callers that keep a text's most recent end rather than its start. If
// maxBytes is non-positive, returns the empty string.
func TailRunes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if maxBytes >= len(s) {
		return s
	}
	start := len(s) - maxBytes
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

// CompleteRunePrefix returns the length of s without a trailing rune that has
// begun and not finished, for a caller that cuts a byte stream into pieces
// that must each decode. Bytes that are not part of an unfinished rune,
// invalid ones included, are kept.
func CompleteRunePrefix[T ~[]byte | ~string](s T) int {
	for back := 1; back < utf8.UTFMax && back <= len(s); back++ {
		start := len(s) - back
		if !utf8.RuneStart(s[start]) {
			continue
		}
		var tail [utf8.UTFMax]byte
		if !utf8.FullRune(tail[:copy(tail[:], s[start:])]) {
			return start
		}
		break
	}
	return len(s)
}
