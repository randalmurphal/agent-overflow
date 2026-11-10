package openai

import (
	"strings"
	"unicode"
)

// IsReading reports whether transcript, the model's account of what it
// spoke, is a reading of text rather than an answer to it, a repeat, or
// silence. Words are lowercase runs of letters and digits (words). With lcs the
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

// words splits text into lowercase runs of letters and digits. A script
// written without spaces between words gives one word per character, so a
// sentence in it is compared piece by piece like any other.
func words(text string) []string {
	var out []string
	start := -1
	flush := func(end int) {
		if start >= 0 {
			out = append(out, text[start:end])
			start = -1
		}
	}
	text = strings.ToLower(text)
	for i, r := range text {
		switch {
		case unicode.In(r, unspaced...):
			flush(i)
			out = append(out, string(r))
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if start < 0 {
				start = i
			}
		default:
			flush(i)
		}
	}
	flush(len(text))
	return out
}

// unspaced are the scripts written without spaces between words.
var unspaced = []*unicode.RangeTable{
	unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Thai, unicode.Lao, unicode.Khmer, unicode.Myanmar,
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
