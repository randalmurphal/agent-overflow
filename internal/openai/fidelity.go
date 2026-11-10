package openai

import (
	"strings"
	"unicode"
)

// IsReading reports whether transcript, the model's account of what it
// spoke, is a reading of text rather than an answer to it, a repeat, or
// silence. Both are split into words (words), each compared as written
// except the few short forms in spelledOut. It is not a reading when:
//
//   - nothing was spoken;
//   - the text has three or more words, two or more of them without
//     digits, and the transcript keeps half or fewer of those, in order (an
//     answer, or a reading cut short). Words with digits are left out
//     because a number is spoken as other words. More than half, so that
//     one word a question and its answer share ("is") is not a reading of
//     two;
//   - the spoken words that are not the text's exceed max(3, half the
//     text's words), plus two per digit and two more for each word with
//     digits, the room a number needs when spelled out (an answer after the
//     reading, or a second reading).
//
// A text of one or two words, or with one word beside its numbers, is
// judged only by the last rule: one word spoken for another there is as
// likely a spelling ("OK", "Okay") as an answer.
func IsReading(text, transcript string) bool {
	want, spoken := words(text), words(transcript)
	if len(spoken) == 0 {
		return false
	}
	var plain []string
	numberRoom := 0
	for _, word := range want {
		if digits := strings.IndexFunc(word, unicode.IsDigit); digits < 0 {
			plain = append(plain, word)
			continue
		}
		for _, r := range word {
			if unicode.IsDigit(r) {
				numberRoom += 2
			}
		}
		numberRoom += 2
	}
	if len(want) >= 3 && len(plain) >= 2 && 2*lcs(plain, spoken) <= len(plain) {
		return false
	}
	return len(spoken)-lcs(want, spoken) <= max(3, len(want)/2)+numberRoom
}

// spelledOut gives the short forms a transcript may write in full the
// spelling they are compared under. Only whole words listed here are
// treated as one another: a word that merely begins another ("up",
// "update") is a different word.
var spelledOut = map[string]string{
	"ok":     "okay",
	"vs":     "versus",
	"approx": "approximately",
}

// words splits text into lowercase runs of letters and digits, each under
// its spelledOut form when it has one. A script written without spaces
// between words gives one word per character, so a sentence in it is
// compared piece by piece like any other.
func words(text string) []string {
	var out []string
	start := -1
	flush := func(end int) {
		if start >= 0 {
			word := text[start:end]
			if full, ok := spelledOut[word]; ok {
				word = full
			}
			out = append(out, word)
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
