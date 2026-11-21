package adf

import (
	"context"
	"slices"
)

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
