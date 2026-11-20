package adf

import (
	"context"
	"slices"
	"strings"
	"unicode/utf8"
)

// Word marks (a text color, a highlight, an inline comment) belong to
// words: an edit keeps one on each original word that survives it, and
// never puts one on a word the edit added. A word survives as itself, or
// edited a little (see closeWords): "blocked" edited to "blocking" keeps
// its color, a word typed beside it takes none. Between two words that
// keep a mark, the space or punctuation keeps it too where it had it.

// wordSpan is one word of a token run: tokens [lo, hi), letters and
// digits of text.
type wordSpan struct {
	lo, hi int
	text   string
}

func isTextTok(t runeTok) bool { return t.node != nil && t.node.Type == "text" }

func isWordTok(t runeTok) bool {
	if !isTextTok(t) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(t.s)
	return isAlnum(r)
}

func wordSpans(ts []runeTok) []wordSpan {
	var out []wordSpan
	for i := 0; i < len(ts); {
		if !isWordTok(ts[i]) {
			i++
			continue
		}
		j := i
		var b strings.Builder
		for j < len(ts) && isWordTok(ts[j]) {
			b.WriteString(ts[j].s)
			j++
		}
		out = append(out, wordSpan{i, j, b.String()})
		i = j
	}
	return out
}

// hiddenOf is the word marks among ms.
func hiddenOf(ms []Mark) []Mark {
	var out []Mark
	for _, m := range ms {
		if wordMarks.get()[m.Type] {
			out = append(out, m)
		}
	}
	return out
}

func hasMarkEq(ms []Mark, m Mark) bool {
	return slices.ContainsFunc(ms, func(x Mark) bool { return markEqual(x, m) })
}

// closeWords reports whether b is a's word edited a little: a typo (up to
// two characters, a third of the word, keeping its start or length) or a
// new ending on a kept
// stem of at least four letters and half the word ("blocked" to
// "blocking"). "Tot" is no edit of "Total".
func closeWords(a, b string) bool {
	if a == b {
		return true
	}
	ra, rb := []rune(strings.ToLower(a)), []rune(strings.ToLower(b))
	longer := max(len(ra), len(rb))
	// A typo keeps the word's start or its length: "rewritten" is no
	// typo of "written".
	if d := levenshtein(ra, rb); d <= 2 && d*3 <= longer &&
		(len(ra) == len(rb) || len(ra) > 0 && len(rb) > 0 && ra[0] == rb[0]) {
		return true
	}
	pre := 0
	for pre < len(ra) && pre < len(rb) && ra[pre] == rb[pre] {
		pre++
	}
	return pre >= 4 && pre*2 >= longer
}

func levenshtein(a, b []rune) int {
	if len(a) > 64 || len(b) > 64 {
		if string(a) == string(b) {
			return 0
		}
		return max(len(a), len(b))
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			c := prev[j-1]
			if a[i-1] != b[j-1] {
				c++
			}
			cur[j] = min(c, prev[j]+1, cur[j-1]+1)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// alignWords pairs the words of f with those of o: equal words by their
// longest common subsequence, then, between two pairs, each unpaired word
// of f with the next unpaired word of o it is close to, in order. The
// result maps an f word index to an o word index, or -1.
func alignWords(ow, fw []wordSpan) []int {
	a, b := make([]string, len(ow)), make([]string, len(fw))
	for i, w := range ow {
		a[i] = w.text
	}
	for j, w := range fw {
		b[j] = w.text
	}
	out := make([]int, len(fw))
	for j := range out {
		out[j] = -1
	}
	m, err := lcs(context.Background(), a, b, alignmentCells)
	if err != nil {
		return out
	}
	for i, j := range m {
		if j >= 0 {
			out[j] = i
		}
	}
	// The unpaired words between two pairs, in order.
	nextO := 0
	for j := 0; j < len(fw); j++ {
		if out[j] >= 0 {
			nextO = out[j] + 1
			continue
		}
		limit := len(ow) // the next paired o word
		for k := j + 1; k < len(fw); k++ {
			if out[k] >= 0 {
				limit = out[k]
				break
			}
		}
		for i := nextO; i < limit; i++ {
			if m[i] < 0 && closeWords(ow[i].text, fw[j].text) {
				out[j], nextO = i, i+1
				break
			}
		}
	}
	return out
}

// wordMarksFor gives each token of f the word marks it takes from o (see
// the rule above), and srcOf, for each f word, the o word it survives
// from, or -1.
func wordMarksFor(o, f []runeTok) (add [][]Mark, srcOf []int, fw, ow []wordSpan) {
	ow, fw = wordSpans(o), wordSpans(f)
	srcOf = alignWords(ow, fw)
	add = make([][]Mark, len(f))
	for j, i := range srcOf {
		if i >= 0 {
			markWord(o[ow[i].lo:ow[i].hi], f[fw[j].lo:fw[j].hi], add[fw[j].lo:fw[j].hi])
		}
	}
	markSeparators(o, f, ow, fw, srcOf, add)
	dropMovedComments(o, f, fw, srcOf, add)
	return add, srcOf, fw, ow
}

// markWord marks the characters of fresh word fs from original word os:
// a kept character as it was; a changed one inside the word where the
// kept ones beside it agree, or the whole word had the mark.
func markWord(os, fs []runeTok, add [][]Mark) {
	a, b := make([]string, len(os)), make([]string, len(fs))
	for i, t := range os {
		a[i] = t.s
	}
	for j, t := range fs {
		b[j] = t.s
	}
	m, err := lcs(context.Background(), a, b, alignmentCells)
	if err != nil {
		return
	}
	from := make([]int, len(fs))
	for j := range from {
		from[j] = -1
	}
	for i, j := range m {
		if j >= 0 {
			from[j] = i
		}
	}
	var whole []Mark // marks every character of the original word has
	for _, mk := range hiddenOf(os[0].marks) {
		all := true
		for _, t := range os {
			all = all && hasMarkEq(t.marks, mk)
		}
		if all {
			whole = append(whole, mk)
		}
	}
	for j := range fs {
		if i := from[j]; i >= 0 {
			add[j] = hiddenOf(os[i].marks)
			continue
		}
		add[j] = whole
	}
}

// markSeparators marks the spaces and punctuation of f between or beside
// words that kept a mark, or inline nodes (a date, a lozenge) that
// stayed, where the original text there had it. A run cut where a
// paragraph was split keeps the marks of the part of the original run
// it still holds.
func markSeparators(o, f []runeTok, ow, fw []wordSpan, srcOf []int, add [][]Mark) {
	nodeOf := alignNodes(o, f) // f non-text token -> o token, or -1
	wordEnd, wordStart := map[int]int{}, map[int]int{}
	for k, w := range fw {
		wordEnd[w.hi], wordStart[w.lo] = k, k
	}
	isSep := func(t runeTok) bool { return isTextTok(t) && !isWordTok(t) }
	for j := 0; j < len(f); {
		if !isSep(f[j]) {
			j++
			continue
		}
		lo := j
		for j < len(f) && isSep(f[j]) {
			j++
		}
		hi := j
		// Where the run's sides were in o: a, the end of what is left of
		// it, b, the start of what is right of it; -1 when not known.
		a, b := -1, -1
		var leftMarks, rightMarks []Mark
		if k, ok := wordEnd[lo]; ok && srcOf[k] >= 0 {
			a, leftMarks = ow[srcOf[k]].hi, add[lo-1]
		} else if lo > 0 && !isTextTok(f[lo-1]) && nodeOf[lo-1] >= 0 {
			a = nodeOf[lo-1] + 1
		}
		if k, ok := wordStart[hi]; ok && srcOf[k] >= 0 {
			b, rightMarks = ow[srcOf[k]].lo, add[hi]
		} else if hi < len(f) && !isTextTok(f[hi]) && nodeOf[hi] >= 0 {
			b = nodeOf[hi]
		}
		run := f[lo:hi]
		_, wordRight := wordStart[hi]
		_, wordLeft := wordEnd[lo]
		switch {
		case a >= 0 && b >= 0 && a <= b:
			markRun(o[a:b], run, leftMarks, rightMarks, add[lo:hi])
		case a >= 0 && b < 0 && !wordRight: // a run cut at its end
			end := a
			for end < len(o) && isSep(o[end]) {
				end++
			}
			if prefixOf(run, o[a:end]) {
				markRun(o[a:a+len(run)], run, nil, nil, add[lo:hi])
			}
		case b >= 0 && a < 0 && !wordLeft: // a run cut at its start
			start := b
			for start > 0 && isSep(o[start-1]) {
				start--
			}
			if prefixOf(reversed(run), reversed(o[start:b])) {
				markRun(o[b-len(run):b], run, nil, nil, add[lo:hi])
			}
		}
	}
}

// alignNodes pairs the inline nodes (and run ends) of f with those of o
// by type, in order.
func alignNodes(o, f []runeTok) []int {
	var oi, fi []int
	var a, b []string
	for i, t := range o {
		if !isTextTok(t) {
			oi, a = append(oi, i), append(a, t.s)
		}
	}
	for j, t := range f {
		if !isTextTok(t) {
			fi, b = append(fi, j), append(b, t.s)
		}
	}
	out := make([]int, len(f))
	for j := range out {
		out[j] = -1
	}
	m, err := lcs(context.Background(), a, b, alignmentCells)
	if err != nil {
		return out
	}
	for x, y := range m {
		if y >= 0 {
			out[fi[y]] = oi[x]
		}
	}
	return out
}

func prefixOf(a, b []runeTok) bool {
	if len(a) > len(b) {
		return false
	}
	for i := range a {
		if a[i].s != b[i].s {
			return false
		}
	}
	return true
}

func reversed(ts []runeTok) []runeTok {
	out := slices.Clone(ts)
	slices.Reverse(out)
	return out
}

// markRun marks the separator run fs from region, the original text
// between the words beside it. Unchanged, each character keeps its marks,
// as an original character that survived; changed, the run takes a mark
// only where all of region and the words on both sides have it.
func markRun(region, fs []runeTok, leftMarks, rightMarks []Mark, add [][]Mark) {
	if len(region) == 0 {
		return
	}
	for _, t := range region {
		if !isTextTok(t) {
			return
		}
	}
	same := len(region) == len(fs)
	for x := range fs {
		same = same && region[x].s == fs[x].s
	}
	var cands []Mark
	for _, t := range region {
		for _, m := range hiddenOf(t.marks) {
			if !hasMarkEq(cands, m) {
				cands = append(cands, m)
			}
		}
	}
	for _, mk := range cands {
		has := func(x int) bool { return hasMarkEq(region[x].marks, mk) }
		if !same {
			all := hasMarkEq(leftMarks, mk) || leftMarks == nil
			all = all && (hasMarkEq(rightMarks, mk) || rightMarks == nil)
			for x := range region {
				all = all && has(x)
			}
			if all && (leftMarks != nil || rightMarks != nil) {
				for x := range fs {
					add[x] = append(add[x], mk)
				}
			}
			continue
		}
		for x := range fs {
			if has(x) {
				add[x] = append(add[x], mk)
			}
		}
	}
}

// dropMovedComments settles each inline comment on f: it covers the text
// from the first to the last character that kept it when that text is a
// typo away at most from the text it covered and every word in it
// survives from an original word, and nothing otherwise: a word typed
// into the span ("total Awas") never joins the comment. A comment
// belongs to exactly its words.
func dropMovedComments(o, f []runeTok, fw []wordSpan, srcOf []int, add [][]Mark) {
	type span struct {
		mark   Mark
		was    strings.Builder
		lo, hi int
	}
	spans := map[string]*span{}
	var ids []string
	for _, t := range o {
		for _, m := range t.marks {
			if m.Type != "annotation" {
				continue
			}
			id := strFrom(m.Attrs["id"])
			if spans[id] == nil {
				spans[id] = &span{mark: m, lo: -1}
				ids = append(ids, id)
			}
			spans[id].was.WriteString(t.s)
		}
	}
	for j := range f {
		for _, m := range add[j] {
			if s := spans[strFrom(m.Attrs["id"])]; m.Type == "annotation" && s != nil {
				if s.lo < 0 {
					s.lo = j
				}
				s.hi = j + 1
			}
		}
	}
	const typo = 2
	for _, id := range ids {
		s := spans[id]
		if s.lo < 0 {
			continue
		}
		var now strings.Builder
		whole := true
		for _, t := range f[s.lo:s.hi] {
			whole = whole && isTextTok(t)
			now.WriteString(t.s)
		}
		for k, w := range fw {
			whole = whole && (w.hi <= s.lo || w.lo >= s.hi || srcOf[k] >= 0)
		}
		keep := whole && levenshtein([]rune(s.was.String()), []rune(now.String())) <= typo
		for j := range add {
			in := j >= s.lo && j < s.hi
			add[j] = slices.DeleteFunc(slices.Clone(add[j]), func(m Mark) bool {
				return markEqual(m, s.mark)
			})
			if keep && in {
				add[j] = append(add[j], s.mark)
			}
		}
	}
}

// withWordMarks is run ns with each text character's word marks replaced
// by add (indexed as runeToks(ns) is).
func withWordMarks(ns []*Node, add [][]Mark) []*Node {
	var out []*Node
	j := 0
	changed := false
	for _, n := range ns {
		if n.Type != "text" {
			out = append(out, n)
			j++
			continue
		}
		rs := []rune(n.Text)
		base := withoutWordMarks(n.Marks)
		marksAt := func(x int) []Mark {
			ms := append(slices.Clone(base), add[j+x]...)
			sortMarks(ms)
			return ms
		}
		start := 0
		for x := 1; x <= len(rs); x++ {
			if x < len(rs) && marksEqual(marksAt(x), marksAt(start)) {
				continue
			}
			t := *n
			t.Text = string(rs[start:x])
			if ms := marksAt(start); len(ms) > 0 {
				t.Marks = ms
			} else {
				t.Marks = nil
			}
			changed = changed || !marksEqual(t.Marks, n.Marks) || x-start != len(rs)
			out = append(out, &t)
			start = x
		}
		j += len(rs)
	}
	if !changed {
		return ns
	}
	return mergeText(out)
}

// restoreWordMarks gives the inline runs outs the word marks their words
// take from the inline runs origs, read as one text each in order (the
// parts of a split paragraph, the sources of a merged one). from lists,
// for each out run, the orig runs its surviving words come from.
func restoreWordMarks(origs, outs [][]*Node) (res [][]*Node, from [][]int) {
	var o, f []runeTok
	runOfO := []int{}
	for i, r := range origs {
		ts := runeToks(r, false)
		o = append(o, ts...)
		for range ts {
			runOfO = append(runOfO, i)
		}
		o = append(o, runeTok{s: "\x00|"})
		runOfO = append(runOfO, i)
	}
	starts := make([]int, len(outs))
	for k, r := range outs {
		starts[k] = len(f)
		f = append(f, runeToks(r, false)...)
		f = append(f, runeTok{s: "\x00|"})
	}
	add, srcOf, fw, ow := wordMarksFor(o, f)
	res = make([][]*Node, len(outs))
	from = make([][]int, len(outs))
	for k, r := range outs {
		end := len(f) - 1
		if k+1 < len(outs) {
			end = starts[k+1] - 1
		}
		res[k] = withWordMarks(r, add[starts[k]:end])
		for x, w := range fw {
			if w.lo >= starts[k] && w.hi <= end && srcOf[x] >= 0 {
				if src := runOfO[ow[srcOf[x]].lo]; !slices.Contains(from[k], src) {
					from[k] = append(from[k], src)
				}
			}
		}
	}
	return res, from
}
