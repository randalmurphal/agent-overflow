package adf

import (
	"strings"
	"unicode/utf8"
)

// delimSlot indexes writer.runs by emphasis character: * _ ~.
func delimSlot(r rune) int {
	switch r {
	case '*':
		return 0
	case '_':
		return 1
	}
	return 2
}

// canDelim reports whether a run of c between before and after could
// open or close emphasis under CommonMark's flanking rules.
func canDelim(c, before, after rune) bool {
	left := !isSpaceRune(after) &&
		(!isPunctRune(after) || isSpaceRune(before) || isPunctRune(before))
	right := !isSpaceRune(before) &&
		(!isPunctRune(before) || isSpaceRune(after) || isPunctRune(after))
	if c == '_' {
		return left && (!right || isPunctRune(before)) ||
			right && (!left || isPunctRune(after))
	}
	return left || right
}

// rawEdge approximates token i's first (or last) character before it is
// written, for counting delimiter runs.
func (w *writer) rawEdge(i int, last bool) rune {
	if i < 0 || i >= len(w.toks) {
		return '\n'
	}
	t := w.toks[i]
	switch t.kind {
	case tkText, tkRaw:
		if t.s == "" {
			return '\n'
		}
		if last {
			r, _ := utf8.DecodeLastRuneInString(t.s)
			return r
		}
		r, _ := utf8.DecodeRuneInString(t.s)
		return r
	case tkCode:
		return '`'
	case tkBreak:
		return '\n'
	}
	return '*'
}

// countRuns counts, per emphasis character, the runs in text that could
// act as delimiters, plus our own delimiters using that character. A
// lone run can't pair with anything, so it needs no escape.
func (w *writer) countRuns() {
	w.runs = [3]int{}
	for i, t := range w.toks {
		switch t.kind {
		case tkText:
			if !strings.ContainsAny(t.s, "*_~`") {
				continue
			}
			rs := []rune(t.s)
			for j := 0; j < len(rs); j++ {
				r := rs[j]
				if r == '`' {
					w.tick++
				}
				if r != '*' && r != '_' && r != '~' {
					continue
				}
				k := j
				for k < len(rs) && rs[k] == r {
					k++
				}
				before, after := w.rawEdge(i-1, true), w.rawEdge(i+1, false)
				if j > 0 {
					before = rs[j-1]
				}
				if k < len(rs) {
					after = rs[k]
				}
				if canDelim(r, before, after) {
					w.runs[delimSlot(r)]++
				}
				j = k - 1
			}
		case tkCode:
			w.tick += 2
		case tkOpen:
			switch t.mark.Type {
			case "strong":
				w.runs[0] += 2
			case "em":
				w.runs[0] += 2
				w.runs[1] += 2
			case "strike":
				w.runs[2] += 2
			}
		}
	}
}

// escapeRun reports whether a run of n emphasis characters r between
// before and after needs escaping: when it could pair with another run
// or with our own delimiters, or when it would fence code.
func (w *writer) escapeRun(r rune, n int, before, after rune) bool {
	switch {
	case w.lvl&escEmph != 0:
		return true
	case canDelim(r, before, after) &&
		(w.runs[delimSlot(r)] >= 2 || before == r || after == r):
		return true
	}
	return r == '~' && n >= 3
}
