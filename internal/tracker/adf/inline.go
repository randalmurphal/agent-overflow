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
// per non-space character ("c:marks") or inline node ("{type}"). It
// returns the first differing entry, -1 when they agree, and false when
// either side can't be read.
func sigDiff(md string, toks []tok, ctx inlineCtx) (int, bool) {
	got, ok := parseSig(md, ctx)
	if !ok {
		return 0, false
	}
	want, ok := tokenSig(toks, ctx)
	if !ok {
		return 0, false
	}
	for i := range min(len(got), len(want)) {
		if got[i] != want[i] {
			return i, true
		}
	}
	if len(got) != len(want) {
		return min(len(got), len(want)), true
	}
	return -1, true
}

// parseSig parses md in ctx and lists its inline content (see sigDiff).
func parseSig(md string, ctx inlineCtx) ([]string, bool) {
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
		return nil, false
	}
	var out []string
	var walk func(ns []*Node)
	walk = func(ns []*Node) {
		for _, n := range ns {
			switch {
			case n.Type == "text":
				out = appendRunes(out, n.Text, n.Marks, "")
			case isInline(n.Type):
				out = append(out, "{"+n.Type+"}")
			default:
				walk(n.Content)
			}
		}
	}
	walk(blocks)
	return out, true
}

// tokenSig lists the inline content toks mean (see sigDiff); raw markup
// counts as what it parses to on its own.
func tokenSig(toks []tok, ctx inlineCtx) ([]string, bool) {
	var out []string
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
			out = appendRunes(out, t.s, stack, "")
		case tkCode:
			out = appendRunes(out, t.s, stack, "code")
		case tkBreak:
			out = append(out, "{hardBreak}")
		case tkRaw:
			sig, ok := parseSig(t.s, ctx)
			if !ok {
				return nil, false
			}
			out = append(out, sig...)
		}
	}
	return out, true
}

func appendRunes(out []string, s string, marks []Mark, extra string) []string {
	types := make([]string, 0, len(marks)+1)
	for _, m := range marks {
		types = append(types, m.Type)
	}
	if extra != "" {
		types = append(types, extra)
	}
	slices.Sort(types)
	sig := strings.Join(types, "+")
	for _, r := range s {
		if !unicode.IsSpace(r) {
			out = append(out, string(r)+":"+sig)
		}
	}
	return out
}

// dropOffending removes one mark near signature entry d from the items:
// from the item holding entry d or a neighbor, the first removal after
// which the rendering reads back correctly at least past d.
func dropOffending(items []item, ctx inlineCtx, d int) ([]item, bool) {
	at, n := -1, 0
	for i, it := range items {
		switch it.kind {
		case tkText, tkCode:
			n += len(appendRunes(nil, it.s, nil, ""))
		case tkBreak:
			n++
		case tkRaw:
			sig, _ := parseSig(it.s, ctx)
			n += len(sig)
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
