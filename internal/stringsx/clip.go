package stringsx

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
