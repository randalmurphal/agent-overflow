// Package stringsx holds small string helpers shared across the codebase.
// Keep it free of non-stdlib imports so every package can depend on it
// without introducing a cycle.
package stringsx

import "strings"

// FirstNonEmpty returns the first value that is not the empty string,
// without trimming whitespace. Use this when the inputs are already
// normalized (e.g. JSON-decoded string fields) and you only need to pick
// between candidate strings. Returns "" if every value is empty.
func FirstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// FirstNonEmptyTrimmed returns the first value that is not blank after
// TrimSpace, and returns that value trimmed. Use this for user- or
// provider-sourced strings where leading/trailing whitespace should be
// treated as empty and stripped from the output. Returns "" if every
// value is blank.
func FirstNonEmptyTrimmed(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
