package adf

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"
)

// patchText edits orig, the one original fresh replaced, by applying the
// text change md made instead of rebuilding the block from md, so the
// block keeps every node, mark and attribute it had: a list inside a
// table cell, a line break inside a text node, a lozenge whose text
// changed (it keeps its node and color), dates and mentions. norm is orig
// as md reads it back. It applies only when norm and fresh differ in the
// text of their text nodes alone (no syntax added, removed or moved), and
// every changed character maps onto orig; otherwise it returns nil and
// the block is rebuilt. What it returns reads back as fresh: a patch
// whose Markdown would read otherwise (a word typed beside a mark landing
// inside it) is discarded.
func patchText(ctx context.Context, orig, norm, fresh *Node, opts *convState) *Node {
	p := patchEdits(ctx, orig, norm, fresh)
	if p == nil || !readsBack(ctx, p, fresh, opts) {
		return nil
	}
	return p
}

// readsBack reports whether p, a patched block or table cell, reads back
// from its Markdown as fresh, the block md wrote, does.
func readsBack(ctx context.Context, p, fresh *Node, opts *convState) bool {
	cell := p.Type == "tableCell" || p.Type == "tableHeader"
	b := p
	if cell {
		b = &Node{Type: "table", Content: []*Node{{Type: "tableRow", Content: []*Node{p}}}}
	}
	norm, err := normalize(ctx, []*Node{b}, opts)
	if err != nil || norm.broken[0] || len(norm.blocks[0]) != 1 {
		return false
	}
	got := norm.blocks[0][0]
	if !cell {
		return key(got) == key(fresh)
	}
	if len(got.Content) != 1 || len(got.Content[0].Content) != 1 {
		return false
	}
	return key(&Node{Content: got.Content[0].Content[0].Content}) ==
		key(&Node{Content: fresh.Content})
}

// patchEdits maps the text change from norm to fresh onto a copy of orig,
// or returns nil when some change doesn't map.
func patchEdits(ctx context.Context, orig, norm, fresh *Node) *Node {
	if skeleton(norm) != skeleton(fresh) || reordered(norm, fresh) {
		return nil
	}
	var nt, ft []*Node
	walk(norm, func(n, _ *Node) {
		if n.Type == "text" {
			nt = append(nt, n)
		}
	})
	walk(fresh, func(n, _ *Node) {
		if n.Type == "text" {
			ft = append(ft, n)
		}
	})
	if len(nt) != len(ft) {
		return nil
	}
	ot, normAt := patchTokens(orig, false), map[*Node]int{}
	nk := patchTokens(norm, true)
	for i, t := range nk {
		if t.off == 0 && t.node.Type == "text" {
			normAt[t.node] = i
		}
	}
	match, err := lcs(ctx, patchStrings(ot), patchStrings(nk), alignmentCells)
	if err != nil {
		return nil
	}
	origOf := make([]int, len(nk)) // norm token -> orig token, or -1
	for i := range origOf {
		origOf[i] = -1
	}
	for i, k := range match {
		if k >= 0 {
			origOf[k] = i
		}
	}
	ed := newEdits()
	for x, n := range nt {
		f := ft[x]
		if n.Text == f.Text {
			continue
		}
		base, ok := normAt[n]
		if !ok {
			return nil
		}
		if !ed.apply(ctx, ot, origOf, base, n.Marks, []rune(n.Text), []rune(f.Text)) {
			return nil
		}
	}
	if len(ed.del) == 0 && len(ed.ins) == 0 && len(ed.insEnd) == 0 {
		return nil
	}
	p := ed.build(orig, ot)
	if p == nil {
		return nil
	}
	reWordMark(orig, p)
	return p
}

// reWordMark gives each inline run of p, orig patched, the word marks its
// words take from orig's (see wordMarksFor): text typed beside a colored
// word lands in that word's node, but takes no color.
func reWordMark(orig, p *Node) {
	var os, ps []*Node
	walk(orig, func(n, _ *Node) {
		if isInlineParent(n.Type) {
			os = append(os, n)
		}
	})
	walk(p, func(n, _ *Node) {
		if isInlineParent(n.Type) {
			ps = append(ps, n)
		}
	})
	if len(os) != len(ps) {
		return
	}
	for i, n := range ps {
		add, _, _, _ := wordMarksFor(runeToks(os[i].Content, false), runeToks(n.Content, false))
		n.Content = withWordMarks(n.Content, add)
	}
}

// reordered reports whether, anywhere in norm and fresh (one skeleton),
// a fresh block reads as a sibling of the norm block in its place, or
// shares more words with one: items swapped, which the patch, pairing by
// place, would give each other's hidden state (a decision's state, a
// color). The rebuild pairs them by content.
func reordered(norm, fresh *Node) bool {
	if isInlineParent(norm.Type) || len(norm.Content) != len(fresh.Content) {
		return false
	}
	if len(norm.Content) > 1 {
		keys := keysOf(norm.Content)
		bags := make([]map[string]int, len(norm.Content))
		for i, n := range norm.Content {
			bags[i] = wordBag(n)
		}
		for j, f := range fresh.Content {
			k := key(f)
			if k == keys[j] {
				continue
			}
			fb := wordBag(f)
			own := common(fb, bags[j])
			for i := range norm.Content {
				if i != j && (k == keys[i] || common(fb, bags[i]) > own) {
					return true
				}
			}
		}
	}
	for i, n := range norm.Content {
		if reordered(n, fresh.Content[i]) {
			return true
		}
	}
	return false
}

// skeleton is n's key with every text node's text left out: two trees
// with one skeleton differ in text alone.
func skeleton(n *Node) string {
	var b strings.Builder
	var rec func(n *Node)
	rec = func(n *Node) {
		c := *n
		c.Text, c.Content = "", nil
		k := key(&c)
		b.WriteString(k[:len(k)-1])
		for _, ch := range n.Content {
			rec(ch)
		}
		b.WriteString("}")
	}
	rec(n)
	return b.String()
}

// patchTok is one character of text, or of what md shows for a lozenge,
// date or emoji, or one other inline node or block end: node and offset
// say where it comes from.
type patchTok struct {
	s    string
	node *Node
	off  int
}

// shownAs is the text md writes for an inline node it shows as text.
func shownAs(n *Node) (string, bool) {
	switch n.Type {
	case "status":
		return "[" + n.attrStr("text") + "]", true
	case "date":
		return formatDate(n.attrStr("timestamp")), true
	case "emoji":
		return emojiText(n), true
	}
	return "", false
}

func patchTokens(root *Node, md bool) []patchTok {
	var out []patchTok
	var rec func(n *Node)
	rec = func(n *Node) {
		if n.Type == "text" {
			for i, r := range []rune(n.Text) {
				out = append(out, patchTok{string(r), n, i})
			}
			return
		}
		if s, ok := shownAs(n); ok && !md {
			for i, r := range []rune(s) {
				out = append(out, patchTok{string(r), n, i})
			}
			return
		}
		if len(n.Content) == 0 && n != root {
			out = append(out, patchTok{"\x00" + n.Type, n, -1})
			return
		}
		for _, c := range n.Content {
			rec(c)
		}
		if isInlineParent(n.Type) {
			out = append(out, patchTok{"\x00|", n, -1})
		}
	}
	rec(root)
	return out
}

func patchStrings(ts []patchTok) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.s
	}
	return out
}

// edits are the character changes to orig's tokens: deleted ones, and
// text inserted before (at) or after a token.
type edits struct {
	del         map[int]bool
	ins, insEnd map[int]string
}

func newEdits() *edits {
	return &edits{del: map[int]bool{}, ins: map[int]string{}, insEnd: map[int]string{}}
}

// apply maps the change from text a (a norm text node starting at norm
// token base) to b onto orig tokens. Every deleted character, and the
// neighbor each insertion goes beside, must map onto orig.
//
// Between two original nodes, a run goes into the one that shows in md
// with the marks the md text node has (vis): a word typed after a space md
// moved out of a bold run goes beside the bold, not inside it. Whatever
// still reads back otherwise, patchText's check discards.
func (e *edits) apply(ctx context.Context, ot []patchTok, origOf []int, base int, vis []Mark,
	a, b []rune) bool {
	fits := func(o int) bool {
		n := ot[o].node
		return n.Type != "text" || visibleEqual(n.Marks, vis)
	}
	as, bs := make([]string, len(a)), make([]string, len(b))
	for i, r := range a {
		as[i] = string(r)
	}
	for i, r := range b {
		bs[i] = string(r)
	}
	m, err := lcs(ctx, as, bs, alignmentCells)
	if err != nil {
		return false
	}
	kept := make([]bool, len(b))
	for i, j := range m {
		if j < 0 {
			if origOf[base+i] < 0 {
				return false
			}
			e.del[origOf[base+i]] = true
			continue
		}
		kept[j] = true
	}
	// Each run of new characters goes beside the original character
	// before it, or for a run at the start, the one after it.
	prev := -1 // the a index of the last kept character
	aOf := make([]int, len(b))
	for i, j := range m {
		if j >= 0 {
			aOf[j] = i
		}
	}
	for j := 0; j < len(b); {
		if kept[j] {
			prev = aOf[j]
			j++
			continue
		}
		end := j
		for end < len(b) && !kept[end] {
			end++
		}
		run := string(b[j:end])
		// The character before the run in a, deleted or not, else after.
		before := len(a) - 1
		if end < len(b) {
			before = aOf[end] - 1
		}
		at := max(before, prev)
		after, before := -1, -1 // the original tokens the run may follow or lead
		if at >= 0 {
			after = origOf[base+at]
		}
		if at+1 < len(a) {
			before = origOf[base+at+1]
		}
		switch {
		case after >= 0 && (fits(after) || before < 0 || !fits(before)):
			e.insEnd[after] += run
		case before >= 0:
			e.ins[before] = run + e.ins[before]
		default:
			return false
		}
		j = end
	}
	return true
}

// visibleEqual reports whether marks a and b show the same in md: equal
// apart from marks Markdown can't show (colors, comments), with links
// compared by their target alone.
func visibleEqual(a, b []Mark) bool {
	vis := func(ms []Mark) []string {
		var out []string
		for _, m := range ms {
			if _, hidden := markPhrases.get()[m.Type]; hidden {
				continue
			}
			s := m.Type
			if m.Type == "link" {
				s += "|" + strFrom(m.Attrs["href"])
			} else {
				s += jsonText(m.Attrs)
			}
			out = append(out, s)
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(vis(a), vis(b))
}

func strFrom(v any) string {
	s, _ := v.(string)
	return s
}

// build is a copy of orig with the edits made: text nodes take their new
// text (one left empty goes), and a lozenge or date its new value.
func (e *edits) build(orig *Node, ot []patchTok) *Node {
	type change struct {
		del   []bool
		ins   []string
		after []string
	}
	changes := map[*Node]*change{}
	get := func(n *Node, size int) *change {
		c := changes[n]
		if c == nil {
			c = &change{del: make([]bool, size), ins: make([]string, size),
				after: make([]string, size)}
			changes[n] = c
		}
		return c
	}
	size := func(t patchTok) int {
		if t.node.Type == "text" {
			return len([]rune(t.node.Text))
		}
		s, _ := shownAs(t.node)
		return len([]rune(s))
	}
	for i, t := range ot {
		if t.off < 0 || !e.del[i] && e.ins[i] == "" && e.insEnd[i] == "" {
			continue
		}
		c := get(t.node, size(t))
		c.del[t.off] = c.del[t.off] || e.del[i]
		c.ins[t.off] += e.ins[i]
		c.after[t.off] += e.insEnd[i]
	}
	newText := map[*Node]string{}
	for n, c := range changes {
		src := []rune(n.Text)
		if n.Type != "text" {
			s, _ := shownAs(n)
			src = []rune(s)
		}
		var b strings.Builder
		for i, r := range src {
			b.WriteString(c.ins[i])
			if !c.del[i] {
				b.WriteRune(r)
			}
			b.WriteString(c.after[i])
		}
		newText[n] = b.String()
	}
	// A lozenge that now reads as another, itself changed, did is that
	// one moved (two swapped): the rebuild gives each its own back by its
	// text, where the patch would trade their colors.
	changed := map[string]bool{} // what changed lozenges read before
	for n := range newText {
		if n.Type == "status" {
			s, _ := shownAs(n)
			changed[s] = true
		}
	}
	for n, t := range newText {
		if n.Type == "status" && changed[t] {
			return nil
		}
	}
	ok := true
	var rec func(n *Node) *Node
	rec = func(n *Node) *Node {
		c := *n
		if n.Attrs != nil {
			c.Attrs = cloneValue(n.Attrs).(map[string]any)
		}
		if n.Marks != nil {
			c.Marks = cloneNode(&Node{Marks: n.Marks}).Marks
		}
		if t, changed := newText[n]; changed {
			switch n.Type {
			case "text":
				if t == "" {
					return nil
				}
				c.Text = t
			default:
				ok = ok && setShown(&c, n, t)
			}
		}
		if n.Content != nil {
			c.Content = make([]*Node, 0, len(n.Content))
			for _, ch := range n.Content {
				if x := rec(ch); x != nil {
					c.Content = append(c.Content, x)
				}
			}
		}
		return &c
	}
	out := rec(orig)
	if !ok {
		return nil
	}
	return out
}

// setShown gives c, a copy of the lozenge or date n, the value its new
// text t says; false when t can't be one (brackets gone, not a date).
func setShown(c, n *Node, t string) bool {
	switch n.Type {
	case "status":
		inner, found := strings.CutPrefix(t, "[")
		inner, closed := strings.CutSuffix(inner, "]")
		if !found || !closed || strings.TrimSpace(inner) == "" || strings.ContainsAny(inner, "[]") {
			return false
		}
		c.setAttr("text", inner) // as typed, which is how md shows it
		return true
	case "date":
		d, err := time.Parse("2006-01-02", t)
		if err != nil {
			return false
		}
		c.setAttr("timestamp", strconv.FormatInt(d.UnixMilli(), 10))
		return true
	}
	return false
}
