package openai

import (
	"strings"
	"unicode"
)

// IsReading reports whether transcript, the model's account of what it
// spoke, is a reading of text rather than an answer to it, a repeat, or
// silence. Words are lowercase runs of letters and digits. With lcs the
// longest common subsequence of the two word lists, it is not a reading
// when nothing was spoken, when the spoken words outside lcs exceed
// max(3, half the text's words), or when a text of six or more words kept
// under half of them. The slack admits a number read as words or a filler
// word; an answer or a second reading exceeds it.
func IsReading(text, transcript string) bool {
	want, spoken := words(text), words(transcript)
	if len(spoken) == 0 {
		return false
	}
	common := lcs(want, spoken)
	if extra := len(spoken) - common; extra > max(3, len(want)/2) {
		return false
	}
	if len(want) >= 6 && float64(common)/float64(len(want)) < 0.5 {
		return false
	}
	return true
}

func words(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// lcs is the length of the longest common subsequence of a and b, in two
// rows of memory.
func lcs(a, b []string) int {
	previous, current := make([]int, len(b)+1), make([]int, len(b)+1)
	for i := range a {
		for j := range b {
			switch {
			case a[i] == b[j]:
				current[j+1] = previous[j] + 1
			case previous[j+1] >= current[j]:
				current[j+1] = previous[j+1]
			default:
				current[j+1] = current[j]
			}
		}
		previous, current = current, previous
	}
	return previous[len(b)]
}
