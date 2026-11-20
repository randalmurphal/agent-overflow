package adf

import (
	"cmp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

type tok struct {
	kind   tokKind
	s      string
	mark   Mark
	pair   int
	inLink bool
}

func findMark(ms []Mark, m Mark) bool {
	for _, x := range ms {
		if markEqual(x, m) {
			return true
		}
	}
	return false
}

var openRank = lazy[map[string]int]{build: func() map[string]int {
	return map[string]int{
		"link": 0, "strong": 1, "em": 2, "strike": 3, "underline": 4,
		"subsup": 5,
	}
}}

// tagMark reports marks written as HTML tags (<u>, <sub>, <sup>).
func tagMark(m Mark) bool { return m.Type == "underline" || m.Type == "subsup" }

// tagsDiffer reports whether item marks ms hold a different set of tag
// marks than stack.
func tagsDiffer(ms, stack []Mark) bool {
	n := 0
	for _, m := range stack {
		if tagMark(m) {
			if !findMark(ms, m) {
				return true
			}
			n++
		}
	}
	for _, m := range ms {
		if tagMark(m) {
			n--
		}
	}
	return n != 0
}

// markTokens turns items into tokens with mark delimiters. Marks that
// run longest open first, so they close last and are not reopened.
// HTML-tag marks are the exception: they always open outside emphasis,
// and emphasis closes and reopens around each tag. Inside, a tag's < or >
// would sit against an emphasis delimiter and stop it flanking:
// **a <u>b</u>**c reads back as literal asterisks, **a** <u>**b**</u>c
// does not.
func markTokens(items []item, lvl escLevel) []tok {
	items = bareLinks(items, lvl)
	var toks []tok
	var stack []Mark
	closeTo := func(k int) {
		for len(stack) > k {
			toks = append(toks, tok{kind: tkClose, mark: stack[len(stack)-1]})
			stack = stack[:len(stack)-1]
		}
	}
	for i, it := range items {
		k := 0
		for k < len(stack) && findMark(it.marks, stack[k]) {
			k++
		}
		if tagsDiffer(it.marks, stack) {
			for j, m := range stack[:k] {
				if flanking(m) {
					k = j
					break
				}
			}
		}
		closeTo(k)
		var add []Mark
		for _, m := range it.marks {
			if !findMark(stack, m) {
				add = append(add, m)
			}
		}
		slices.SortStableFunc(add, func(a, b Mark) int {
			if ta, tb := tagMark(a), tagMark(b); ta != tb {
				if ta {
					return -1
				}
				return 1
			}
			return cmp.Or(
				cmp.Compare(runLength(items, i, b), runLength(items, i, a)),
				cmp.Compare(openRank.get()[a.Type], openRank.get()[b.Type]))
		})
		for _, m := range add {
			toks = append(toks, tok{kind: tkOpen, mark: m})
			stack = append(stack, m)
		}
		toks = append(toks, tok{kind: it.kind, s: it.s})
	}
	closeTo(0)
	// Moving a space out of one mark can leave it at the edge of the next
	// ([strong "a", strong+em " "] becomes **a **), so repeat until the
	// edges settle.
	for range 16 {
		before := slices.Clone(toks)
		toks = dropEmptyPairs(moveSpaces(movePunct(moveSpaces(toks))))
		if slices.EqualFunc(before, toks, sameTok) {
			break
		}
	}
	return pairUp(toks)
}

func sameTok(a, b tok) bool {
	return a.kind == b.kind && a.s == b.s && markEqual(a.mark, b.mark)
}

func runLength(items []item, i int, m Mark) int {
	n := 0
	for ; i < len(items) && findMark(items[i].marks, m); i++ {
		n++
	}
	return n
}

// bareLinks writes a link whose text is its own URL as the bare URL,
// which GFM autolinks back to the same link.
func bareLinks(items []item, lvl escLevel) []item {
	if lvl&escLinks != 0 {
		return items
	}
	out := make([]item, len(items))
	for i, it := range items {
		out[i] = it
		if it.kind != tkText {
			continue
		}
		for j, m := range it.marks {
			if m.Type != "link" || len(m.Attrs) != 1 {
				continue
			}
			href, _ := m.Attrs["href"].(string)
			if !bareURL(it.s, href) {
				continue
			}
			ms := append(append([]Mark(nil), it.marks[:j]...),
				it.marks[j+1:]...)
			out[i] = item{kind: tkRaw, s: it.s, marks: ms}
			break
		}
	}
	return out
}

var (
	bareURLRe   = lazyRegexp{pattern: `^(https?|ftp)://[^\s<>|\\` + "`" + `]+$`}
	bareMailRe  = lazyRegexp{pattern: `^[A-Za-z0-9._+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+$`}
	bareWWWRe   = lazyRegexp{pattern: `^www\.[^\s<>|\\` + "`" + `]+$`}
	urlTailPunc = "?!.,:*_~'\";)"
)

func bareURL(text, href string) bool {
	if text == "" || strings.ContainsAny(text[len(text)-1:], urlTailPunc) {
		return false
	}
	switch {
	case text == href:
		return bareURLRe.get().MatchString(text)
	case "mailto:"+text == href:
		return bareMailRe.get().MatchString(text)
	case "http://"+text == href:
		return bareWWWRe.get().MatchString(text)
	}
	return false
}

func flanking(m Mark) bool {
	return m.Type == "strong" || m.Type == "em" || m.Type == "strike"
}

// moveSpaces moves whitespace at the inside edge of emphasis outside
// it: "** bold **" is not emphasis in Markdown, " **bold** " is. Any
// Unicode space counts, NBSP included, as it does for CommonMark.
func moveSpaces(toks []tok) []tok {
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.kind != tkText {
			continue
		}
		if lead := t.s[:len(t.s)-len(strings.TrimLeftFunc(t.s, unicode.IsSpace))]; lead != "" {
			j := i
			for j > 0 && toks[j-1].kind == tkOpen && flanking(toks[j-1].mark) {
				j--
			}
			if j < i {
				toks[i].s = t.s[len(lead):]
				toks = insertTok(toks, j, tok{kind: tkText, s: lead})
				i++
				t = toks[i]
			}
		}
		if trail := t.s[len(strings.TrimRightFunc(t.s, unicode.IsSpace)):]; trail != "" {
			j := i
			for j+1 < len(toks) && toks[j+1].kind == tkClose &&
				flanking(toks[j+1].mark) {
				j++
			}
			if j > i {
				toks[i].s = t.s[:len(t.s)-len(trail)]
				toks = insertTok(toks, j+1, tok{kind: tkText, s: trail})
			}
		}
	}
	out := toks[:0]
	for _, t := range toks {
		if t.kind == tkText && t.s == "" {
			continue
		}
		if k := len(out) - 1; k >= 0 && t.kind == tkText &&
			out[k].kind == tkText {
			out[k].s += t.s
			continue
		}
		out = append(out, t)
	}
	return out
}

// movePunct moves punctuation at the inside edge of emphasis outside it
// when a letter or digit sits on the outside: CommonMark can't open
// emphasis in "3**.x**", so the text would show literal asterisks.
func movePunct(toks []tok) []tok {
	isP := isPunctRune
	for i := 0; i < len(toks); i++ {
		if toks[i].kind != tkText {
			continue
		}
		rs := []rune(toks[i].s)
		j := i
		for j > 0 && toks[j-1].kind == tkOpen && flanking(toks[j-1].mark) {
			j--
		}
		if j < i && j > 0 && endsAlnum(toks[j-1]) {
			k := 0
			for k < len(rs) && isP(rs[k]) {
				k++
			}
			if k > 0 && k < len(rs) {
				toks[i].s = string(rs[k:])
				toks = insertTok(toks, j, tok{kind: tkText, s: string(rs[:k])})
				i++
				rs = rs[k:]
			}
		}
		j = i
		for j+1 < len(toks) && toks[j+1].kind == tkClose &&
			flanking(toks[j+1].mark) {
			j++
		}
		if j > i && j+1 < len(toks) && startsAlnum(toks[j+1]) {
			k := len(rs)
			for k > 0 && isP(rs[k-1]) {
				k--
			}
			if k < len(rs) && k > 0 {
				toks[i].s = string(rs[:k])
				toks = insertTok(toks, j+1, tok{kind: tkText,
					s: string(rs[k:])})
			}
		}
	}
	return mergeTextToks(toks)
}

func endsAlnum(t tok) bool {
	if t.kind != tkText && t.kind != tkRaw {
		return false
	}
	r, _ := utf8.DecodeLastRuneInString(t.s)
	return isAlnum(r)
}

func startsAlnum(t tok) bool {
	if t.kind != tkText && t.kind != tkRaw {
		return false
	}
	r, _ := utf8.DecodeRuneInString(t.s)
	return isAlnum(r)
}

func insertTok(toks []tok, at int, t tok) []tok {
	toks = append(toks, tok{})
	copy(toks[at+1:], toks[at:])
	toks[at] = t
	return toks
}

// dropEmptyPairs removes delimiters with nothing between them.
func dropEmptyPairs(toks []tok) []tok {
	for {
		changed := false
		out := toks[:0]
		for i := 0; i < len(toks); i++ {
			if i+1 < len(toks) && toks[i].kind == tkOpen &&
				toks[i+1].kind == tkClose &&
				markEqual(toks[i].mark, toks[i+1].mark) {
				i++
				changed = true
				continue
			}
			out = append(out, toks[i])
		}
		toks = out
		if !changed {
			return mergeTextToks(toks)
		}
	}
}

func mergeTextToks(toks []tok) []tok {
	out := toks[:0]
	for _, t := range toks {
		if k := len(out) - 1; k >= 0 && t.kind == tkText &&
			out[k].kind == tkText {
			out[k].s += t.s
			continue
		}
		out = append(out, t)
	}
	return out
}

// pairUp links each open to its close and marks text inside links.
func pairUp(toks []tok) []tok {
	var stack []int
	links := 0
	for i := range toks {
		switch toks[i].kind {
		case tkOpen:
			stack = append(stack, i)
			if toks[i].mark.Type == "link" {
				links++
			}
		case tkClose:
			if k := len(stack) - 1; k >= 0 {
				toks[i].pair, toks[stack[k]].pair = stack[k], i
				stack = stack[:k]
			}
			if toks[i].mark.Type == "link" {
				links--
			}
		default:
			toks[i].inLink = links > 0
		}
	}
	return toks
}
