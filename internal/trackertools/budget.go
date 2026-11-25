package trackertools

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// answerBudget is the most bytes one answer of either tool holds, as the
// JSON text the transport sends. Codex keeps a tool result of at most
// 64 KiB of its serialized CallToolResult; serializing the answer's text
// into that result at most doubles it (each quote or backslash escaped),
// so 24 KiB stays under the limit with room for the envelope.
const answerBudget = 24 << 10

// cut is the mark at a text's cut: at the end of a text shown in part,
// and at the start of the rest when it is shown.
const cut = "…"

// textClip is the part of a text from byte offset from that fits allow
// bytes as a JSON string, cut at a rune and marked with cut where the
// text goes on, and the offset the rest starts at (len(text) when none is
// left). A text that has nothing to show within allow shows nothing. from
// is 0 or an offset the text honors (atText): a continuation checks every
// offset a client sends before it gets here.
func textClip(text string, from, allow int) (string, int) {
	lead := ""
	if from > 0 {
		lead = cut
	}
	room := allow - 2 - len(lead) // the quotes
	if jsonLen(text[from:]) <= room {
		return lead + text[from:], len(text)
	}
	room -= len(cut)
	end := from
	for end < len(text) {
		r, size := utf8.DecodeRuneInString(text[end:])
		n := runeJSONLen(r, size)
		if n > room {
			break
		}
		room -= n
		end += size
	}
	if end == from {
		return "", from
	}
	return lead + text[from:end] + cut, end
}

// jsonLen is the bytes text takes as a JSON string's contents, as
// encoding/json writes it.
func jsonLen(text string) int {
	n := 0
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		n += runeJSONLen(r, size)
		i += size
	}
	return n
}

// runeJSONLen is the bytes one rune of a string takes as encoding/json
// writes it: escapes for quotes, backslashes, control characters, the HTML
// characters, the line separators and invalid UTF-8; itself otherwise.
func runeJSONLen(r rune, size int) int {
	switch {
	case r == utf8.RuneError && size == 1:
		return 6 // �
	case r == '"' || r == '\\' || r == '\n' || r == '\r' || r == '\t':
		return 2
	case r < 0x20 || r == '<' || r == '>' || r == '&' || r == ' ' || r == ' ':
		return 6
	}
	return size
}

// fit is the answer build makes within answerBudget, for what (named in
// the error when none fits): build shares pool bytes among the parts it
// cuts, and gets less, by what its answer was over, while the answer is
// over the budget.
func fit[T any](what string, pool int, build func(pool int) (T, error)) (T, error) {
	for range 8 {
		out, err := build(pool)
		if err != nil {
			return out, err
		}
		over := sizeOf(out) - answerBudget
		if over <= 0 {
			return out, nil
		}
		pool -= over
	}
	var none T
	return none, fmt.Errorf("trackertools: the answer for %s did not fit %d bytes", what, answerBudget)
}

// sizeOf is the bytes v takes as JSON.
func sizeOf(v any) int {
	raw, err := json.Marshal(v)
	if err != nil {
		// Every answer part is strings, numbers and lists of them.
		panic("trackertools: encode an answer part: " + err.Error())
	}
	return len(raw)
}

// allot shares total bytes among parts that want demands, in proportion
// to their weights, none getting more than it wants; what a part leaves
// goes to the parts that want more, in their order. A part whose share is
// below what it wants is cut to its share.
func allot(demands, weights []int, total int) []int {
	out := make([]int, len(demands))
	left := total
	sum := 0
	for i, d := range demands {
		if d > 0 {
			sum += weights[i]
		}
	}
	if sum == 0 {
		return out
	}
	for i, d := range demands {
		if d > 0 {
			out[i] = min(d, total*weights[i]/sum)
			left -= out[i]
		}
	}
	for i, d := range demands {
		if left <= 0 {
			break
		}
		extra := min(d-out[i], left)
		out[i] += extra
		left -= extra
	}
	return out
}

// sizeLabel is n bytes as a person reads a size.
func sizeLabel(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d bytes", n)
	}
	return fmt.Sprintf("%d KB", (n+1023)/1024)
}
