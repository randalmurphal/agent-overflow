package engine

import "unicode/utf8"

// truncateWithNote bounds one persisted engine string and says so when it cut.
//
// Two of them share this rule and share it for the same two reasons: a park
// cause (`MaxParkCauseBytes`) and a redelivered feedback block
// (`MaxRedeliveredFeedbackBytes`). A reader who cannot tell a short value from a
// cut-off one will trust the wrong half, so a cut is always announced. And the
// cut lands on a RUNE boundary, because both values carry a person's or a
// model's own words: half a rune would be invalid UTF-8 in the store before any
// reader got the chance to quote it.
//
// The bound applies to the CONTENT; `note` is appended outside it, so the
// marker announcing the cut is never itself cut.
func truncateWithNote(text string, maxBytes int, note string) string {
	if len(text) <= maxBytes {
		return text
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + note
}
