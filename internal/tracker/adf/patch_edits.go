package adf

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"
)

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
