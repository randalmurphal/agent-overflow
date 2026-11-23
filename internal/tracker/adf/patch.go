package adf

import (
	"context"
	"strings"
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

func strFrom(v any) string {
	s, _ := v.(string)
	return s
}
