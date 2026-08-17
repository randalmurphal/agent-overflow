package stringsx

import "unicode/utf8"

// Clip returns s if its byte length is <= max, otherwise the first max
// bytes. Operates on bytes, not runes — callers using it to cap UI fields
// (diagnostics, log payloads) want a hard byte ceiling, not a grapheme
// boundary. If max is negative, returns the empty string.
func Clip(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// ClipRunes returns s if its byte length is <= maxBytes, otherwise the
// first maxBytes bytes backed off to the previous rune boundary, so the
// result never carries a torn UTF-8 sequence and never exceeds the
// budget. Clip is the non-rune-safe sibling: use it when a hard byte
// ceiling is the whole point, and this one when the text still has to
// decode (prompt sections, anything a model or a terminal reads). If
// maxBytes is non-positive, returns the empty string.
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
