// Everything a workflow run records — a goal, an envelope's question, an
// output value, a narrative excerpt — is *data*. Composing it raw into the
// seed of a triage thread or the wake message of a bound thread would let that
// data read as instructions to the receiving agent. Quote returns a rendering
// that is unambiguously one value: escaped to ASCII (so an embedded newline,
// quote, or bidi control character cannot forge structure), rune-truncated to
// a caller-chosen budget, and with the three markup-significant bytes escaped
// so the same string is also safe inside HTML-ish surfaces.
//
// Stdlib-only and pure, so every prompt-composing surface can share exactly one
// definition of "quoted as data".
package untrustedtext

import (
	"strconv"
	"strings"
)

// DefaultFieldRunes is the budget for a single record field — an id, a status,
// a branch name, a one-line summary. Callers with a genuinely long value (a
// narrative excerpt) pass their own.
const DefaultFieldRunes = 2_048

// TruncationSuffix marks a value the budget cut short. It is appended outside
// the quoting so a reader can tell truncation from content.
const TruncationSuffix = "…[truncated]"

// Field quotes one record field at DefaultFieldRunes.
func Field(value string) string { return Quote(value, DefaultFieldRunes) }

// Quote renders value as a single quoted, ASCII-escaped token of at most
// maxRunes runes of content. Invalid UTF-8 is replaced rather than dropped, so
// a byte sequence that is not text still shows up as something.
func Quote(value string, maxRunes int) string {
	quoted := strconv.QuoteToASCII(Truncate(strings.ToValidUTF8(value, "�"), maxRunes))
	return markupEscaper.Replace(quoted)
}

// markupEscaper rewrites the three markup-significant bytes into their \u
// escapes. The result is still a valid Go/JSON string literal that unquotes to
// the original characters — the escaping hides them from a surface that scans
// for markup, it does not change the value.
var markupEscaper = strings.NewReplacer("<", `\u003c`, ">", `\u003e`, "&", `\u0026`)

// Truncate caps value at maxRunes runes, appending TruncationSuffix when it
// cuts. A non-positive maxRunes returns value unchanged — the caller declined
// to set a budget rather than asking for an empty string.
func Truncate(value string, maxRunes int) string {
	if maxRunes <= 0 {
		return value
	}
	count := 0
	for index := range value {
		if count == maxRunes {
			return value[:index] + TruncationSuffix
		}
		count++
	}
	return value
}
