package adf

import (
	"context"
	"slices"
	"strings"
	"unicode"
)

// inlineCtx is where a run of inline content is rendered; it decides how
// hard breaks look and which characters are syntax there.
type inlineCtx int

const (
	ctxPara    inlineCtx = iota // a paragraph line start is block syntax
	ctxHeading                  // after "# ", on one line
	ctxCell                     // inside a GFM table cell, on one line
	ctxTask                     // after "- [ ] "
)

// renderInline renders inline nodes with the lightest escaping that
// still parses back to what fully escaped text parses to. Most text
// needs no backslashes at all; when a guess is wrong, the parse check
// catches it and escaping grows one category at a time.
func renderInline(ns []*Node, ctx inlineCtx) string {
	return renderItems(inlineItems(ns, ctx), ctx, true)
}

// renderItems is renderInline on built items. With skipInert, runs whose
// unescaped text Markdown can't misread (see inert) skip the parse
// check; tests turn that off to confirm the shortcut changes nothing.
func renderItems(items []item, ctx inlineCtx, skipInert bool) string {
	s, _ := renderVerified(items, ctx, skipInert)
	return s
}

// renderVerified renders items and checks the result reads back with the
// same text and marks. Where no escaping makes a mark parse (emphasis
// on punctuation between letters: a*\_*b), that mark is dropped from the
// offending text instead of writing literal delimiters; dropped reports
// it, and lossy lists it as formatting Markdown can't show.
func renderVerified(items []item, ctx inlineCtx, skipInert bool) (
	s string, dropped bool) {
	for {
		out, full, toks, checked := renderOnce(items, ctx, skipInert)
		if !checked {
			return out, dropped
		}
		d, ok := sigDiff(full, toks, ctx)
		if !ok || d < 0 {
			return out, dropped
		}
		next, fixed := dropOffending(items, ctx, d)
		if !fixed {
			return out, dropped
		}
		items, dropped = next, true
	}
}

// renderOnce is one rendering of items: the chosen string, the fully
// escaped string and the tokens it came from, and whether it was parse
// checked (inert text is not).
func renderOnce(items []item, ctx inlineCtx, skipInert bool) (
	out, full string, linked []tok, checked bool) {
	if len(items) == 0 {
		return "", "", nil, false
	}
	// Tokens depend on the level only through escLinks.
	bare := markTokens(items, 0)
	minimal := emitTokens(bare, ctx, 0)
	isInert := skipInert && inert(bare, ctx)
	if isInert && !hasDelims(bare) {
		return minimal, "", nil, false
	}
	linked = markTokens(items, escLinks)
	emit := func(l escLevel) string {
		if l&escLinks != 0 {
			return emitTokens(linked, ctx, l)
		}
		return emitTokens(bare, ctx, l)
	}
	full = emit(escAll | escFull)
	if isInert {
		// Inert text reads the same escaped or not, but whether the
		// delimiters around it flank still needs the check.
		return minimal, full, linked, true
	}
	if minimal == full {
		return minimal, full, linked, true
	}
	want, ok := parseKey(full, ctx)
	if !ok {
		return full, full, linked, false
	}
	if got, ok := parseKey(minimal, ctx); ok && got == want {
		return minimal, full, linked, true
	}
	for _, l := range escLadder {
		s := emit(l)
		if got, ok := parseKey(s, ctx); ok && got == want {
			return s, full, linked, true
		}
	}
	return full, full, linked, true
}

// hasDelims reports emphasis or strike delimiters, which may fail to
// flank; links and HTML-tag marks always parse.
func hasDelims(toks []tok) bool {
	for _, t := range toks {
		if t.kind == tkOpen && flanking(t.mark) {
			return true
		}
	}
	return false
}

// sigDiff compares what md parses to with what toks meant, as one entry
// per non-space character or inline node (see sigEntry). It returns the
// first differing entry, -1 when they agree, and false when either side
// can't be read. The token side is listed once and the parsed side
// compared with it as it is walked.
func sigDiff(md string, toks []tok, ctx inlineCtx) (int, bool) {
	var names sigNames
	want, ok := tokenSig(toks, ctx, &names)
	if !ok {
		return 0, false
	}
	i, diff := 0, -1
	ok = parseSig(md, ctx, &names, func(e sigEntry) bool {
		if i >= len(want) || want[i] != e {
			diff = i
			return false
		}
		i++
		return true
	})
	switch {
	case !ok:
		return 0, false
	case diff >= 0:
		return diff, true
	case i != len(want):
		return i, true
	}
	return -1, true
}

// parseSig parses md in ctx and hands visit its inline content in order
// (see sigDiff) until visit returns false. It reports false when md
// cannot be parsed. Entries name their marks through names.
func parseSig(md string, ctx inlineCtx, names *sigNames, visit func(sigEntry) bool) bool {
	switch ctx {
	case ctxHeading:
		md = "# " + md
	case ctxCell:
		md = "| |\n| --- |\n| " + md + " |"
	case ctxTask:
		md = "- [ ] " + strings.ReplaceAll(md, "\n", "\n  ")
	}
	c := newConverter(context.Background(), md, nil, true)
	blocks, err := c.document()
	if err != nil {
		return false
	}
	var walk func(ns []*Node) bool
	walk = func(ns []*Node) bool {
		for _, n := range ns {
			switch {
			case n.Type == "text":
				sig := names.id(markSig(n.Marks, ""))
				for _, r := range n.Text {
					if !unicode.IsSpace(r) && !visit(sigEntry{r: r, marks: sig}) {
						return false
					}
				}
			case isInline(n.Type):
				if !visit(sigEntry{r: nodeSig, marks: names.id(n.Type)}) {
					return false
				}
			default:
				if !walk(n.Content) {
					return false
				}
			}
		}
		return true
	}
	walk(blocks)
	return true
}

// tokenSig lists the inline content toks mean (see sigDiff); raw markup
// counts as what it parses to on its own.
func tokenSig(toks []tok, ctx inlineCtx, names *sigNames) ([]sigEntry, bool) {
	size := 0
	for _, t := range toks {
		switch t.kind {
		case tkText, tkCode:
			size += countSig(t.s)
		case tkBreak:
			size++
		}
	}
	out := make([]sigEntry, 0, size)
	var stack []Mark
	for _, t := range toks {
		switch t.kind {
		case tkOpen:
			stack = append(stack, t.mark)
		case tkClose:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case tkText:
			out = appendRunes(out, t.s, names.id(markSig(stack, "")))
		case tkCode:
			out = appendRunes(out, t.s, names.id(markSig(stack, "code")))
		case tkBreak:
			out = append(out, sigEntry{r: nodeSig, marks: names.id("hardBreak")})
		case tkRaw:
			if !parseSig(t.s, ctx, names, func(e sigEntry) bool { out = append(out, e); return true }) {
				return nil, false
			}
		}
	}
	return out, true
}

// sigEntry is one entry of a signature: a non-space character with the
// sorted types of its marks joined by "+", or, with r set to nodeSig, an
// inline node with its type, each named in marks by its number in the
// comparison's sigNames. Two entries of one comparison are equal exactly
// when they name the same character under the same marks, or the same
// node.
type sigEntry struct {
	r     rune
	marks uint32
}

// nodeSig is the r of an inline node's entry: no character decodes to it.
const nodeSig rune = -1

// sigNames numbers the distinct mark and node names of one comparison,
// in the order first seen. A comparison names a handful, so a scan finds
// one.
type sigNames []string

func (n *sigNames) id(name string) uint32 {
	for i, have := range *n {
		if have == name {
			return uint32(i)
		}
	}
	*n = append(*n, name)
	return uint32(len(*n) - 1)
}

// appendRunes appends an entry for each non-space character of s, all
// under the marks numbered sig.
func appendRunes(out []sigEntry, s string, sig uint32) []sigEntry {
	for _, r := range s {
		if !unicode.IsSpace(r) {
			out = append(out, sigEntry{r: r, marks: sig})
		}
	}
	return out
}

// markSig is the sorted types of marks and extra joined by "+".
func markSig(marks []Mark, extra string) string {
	types := make([]string, 0, len(marks)+1)
	for _, m := range marks {
		types = append(types, m.Type)
	}
	if extra != "" {
		types = append(types, extra)
	}
	slices.Sort(types)
	return strings.Join(types, "+")
}

// countSig is the number of entries appendRunes adds for s.
func countSig(s string) int {
	n := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

// dropOffending removes one mark near signature entry d from the items:
// from the item holding entry d or a neighbor, the first removal after
// which the rendering reads back correctly at least past d.
func dropOffending(items []item, ctx inlineCtx, d int) ([]item, bool) {
	at, n := -1, 0
	for i, it := range items {
		switch it.kind {
		case tkText, tkCode:
			n += countSig(it.s)
		case tkBreak:
			n++
		case tkRaw:
			var names sigNames
			parseSig(it.s, ctx, &names, func(sigEntry) bool { n++; return true })
		}
		if n > d {
			at = i
			break
		}
	}
	if at < 0 {
		at = len(items) - 1
	}
	// A candidate is judged on its fully escaped rendering alone: one
	// parse, not the escape-level search renderOnce makes for output.
	progress := func(next []item) bool {
		if len(next) == 0 {
			return false
		}
		toks := markTokens(next, escLinks)
		nd, ok := sigDiff(emitTokens(toks, ctx, escAll|escFull), toks, ctx)
		return ok && (nd < 0 || nd > d)
	}
	near := []int{at, at - 1, at + 1}
	for _, k := range near {
		if k < 0 || k >= len(items) {
			continue
		}
		for m := range items[k].marks {
			next := slices.Clone(items)
			next[k].marks = slices.Delete(slices.Clone(next[k].marks), m, m+1)
			if progress(next) {
				return next, true
			}
		}
	}
	// No single mark: one item's text can't carry any of its marks here.
	for _, k := range near {
		if k >= 0 && k < len(items) && len(items[k].marks) > 1 {
			next := slices.Clone(items)
			next[k].marks = nil
			if progress(next) {
				return next, true
			}
		}
	}
	return nil, false
}

// parseKey parses rendered inline Markdown in its context and returns a
// canonical key for the result.
func parseKey(md string, ctx inlineCtx) (string, bool) {
	switch ctx {
	case ctxHeading:
		md = "# " + md
	case ctxCell:
		md = "| |\n| --- |\n| " + md + " |"
	case ctxTask:
		md = "- [ ] " + strings.ReplaceAll(md, "\n", "\n  ")
	}
	c := newConverter(context.Background(), md, nil, true)
	blocks, err := c.document()
	if err != nil {
		return "", false
	}
	return key(Doc(blocks...)), true
}
