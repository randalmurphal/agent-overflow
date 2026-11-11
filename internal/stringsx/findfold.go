package stringsx

import (
	"sort"
	"strings"
	"unicode/utf8"
)

// FoldForFind folds text for find matching. It is the one match rule of
// every find bar and row filter: the frontend's foldFindText
// (frontend/src/lib/utils/findMatch.ts), the CI log search and a pane's
// thread find (store find_fold). The cross-thread message search keeps
// SQLite's LOWER and LIKE. Each code point becomes its simple lower case
// by the checked-in table (findfold_table.go, Unicode
// FindFoldUnicodeVersion, generated from package unicode; the frontend
// reads the same table), one code point for one, and a byte that is not
// valid UTF-8 stays as it is. The fold keeps the text's code points, so a
// match's offset in it is the match's offset in the text. Text with
// nothing to fold is returned as it is, without a copy. The vectors in
// frontend/src/test/fixtures/findVectors.json hold both sides to it.
func FoldForFind(text string) string {
	for i := 0; i < len(text); {
		if c := text[i]; c < utf8.RuneSelf {
			if 'A' <= c && c <= 'Z' {
				return foldFrom(text, i)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		if size > 1 && foldRune(r) != r {
			return foldFrom(text, i)
		}
		i += size
	}
	return text
}

// foldFrom folds text from byte from on; the bytes before it fold to
// themselves.
func foldFrom(text string, from int) string {
	var b strings.Builder
	b.Grow(len(text))
	b.WriteString(text[:from])
	for i := from; i < len(text); {
		if c := text[i]; c < utf8.RuneSelf {
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			b.WriteByte(c)
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		if size == 1 {
			b.WriteByte(text[i])
		} else {
			b.WriteRune(foldRune(r))
		}
		i += size
	}
	return b.String()
}

// foldRange folds lo..hi at stride, each code point to itself plus delta.
type foldRange struct {
	lo, hi, delta, stride rune
}

// foldRune is r's simple lower case by the find fold table.
func foldRune(r rune) rune {
	if r < utf8.RuneSelf {
		if 'A' <= r && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}
	i := sort.Search(len(findFoldRanges), func(i int) bool { return findFoldRanges[i].hi >= r })
	if i < len(findFoldRanges) {
		if f := findFoldRanges[i]; f.lo <= r && (r-f.lo)%f.stride == 0 {
			return r + f.delta
		}
	}
	return r
}
