package adf

import (
	"context"
	"slices"
	"strings"
)

// restoreBlock gives fresh, a block that replaced orig for certain, what
// orig had that Markdown can't show: norm is orig as md reads it back, so
// whatever norm lacks of orig is what Markdown dropped, and whatever fresh
// shares with norm the user left alone. An attribute or mark Markdown
// dropped comes back where the user left alone what Markdown shows of
// it; in text, a mark comes back on each character that survives the
// edit (see restoreRuns). Children pair by content as table rows do (see
// pairRows), never by guess; a child md left unchanged is reused as it
// was, through reuse (fresh's child, the original), once the edit has
// settled every fresh node.
func restoreBlock(ctx context.Context, orig, norm, fresh *Node,
	reuse func(f, o *Node)) error {
	if !sameKind(orig.Type, norm.Type) || !sameKind(norm.Type, fresh.Type) {
		return nil
	}
	restoreAttrs(orig, norm, fresh)
	fresh.Marks = mergeMarks(orig.Marks, norm.Marks, fresh.Marks)
	if isInlineParent(fresh.Type) {
		fresh.Content = restoreRuns(orig.Content, norm.Content, fresh.Content)
		return nil
	}
	if len(orig.Content) != len(norm.Content) || len(fresh.Content) == 0 {
		return nil
	}
	for i, o := range orig.Content {
		if !sameKind(o.Type, norm.Content[i].Type) {
			return nil
		}
	}
	pairs, _, err := pairRows(ctx, norm.Content, orig.Content, fresh.Content)
	if err != nil {
		return err
	}
	for j, i := range pairs {
		switch {
		case i < 0:
		case key(norm.Content[i]) == key(fresh.Content[j]):
			reuse(fresh.Content[j], orig.Content[i])
		default:
			err := restoreBlock(ctx, orig.Content[i], norm.Content[i], fresh.Content[j], reuse)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// sameKind reports whether two node types stand for one another: a cell
// and a header cell do, as an edited cell keeps its original's type.
func sameKind(a, b string) bool {
	cell := func(t string) bool { return t == "tableCell" || t == "tableHeader" }
	return a == b || cell(a) && cell(b)
}

// restoreAttrs sets each attribute of orig that norm doesn't have as it
// is (Markdown dropped or changed it) on fresh, where fresh has it as norm
// does (the user didn't change what Markdown shows of it). A node still
// to be resolved is left to resolve.
func restoreAttrs(orig, norm, fresh *Node) {
	for k := range fresh.Attrs {
		if strings.HasPrefix(k, "\x00") {
			return // still to resolve (a mention by name): it settles later
		}
	}
	for k, v := range orig.Attrs {
		shown, kept := norm.attr(k), fresh.attr(k)
		if k == "localId" || valueEqual(shown, v) || !valueEqual(kept, shown) {
			continue
		}
		fresh.setAttr(k, cloneValue(v))
	}
}

// mergeMarks is fresh's marks plus each of orig's that norm lacks: added
// when md shows nothing of it, or in place of the form md shows (a link
// without its extra attributes) where fresh still has that form. Nothing
// joins inline code, which takes no other marks.
func mergeMarks(orig, norm, fresh []Mark) []Mark {
	out := fresh
	cloned := false
	has := func(ms []Mark, m Mark) bool {
		return slices.ContainsFunc(ms, func(x Mark) bool { return markEqual(x, m) })
	}
	code := slices.ContainsFunc(fresh, func(m Mark) bool { return m.Type == "code" })
	for _, m := range orig {
		if has(norm, m) || code && m.Type != "link" {
			continue
		}
		at := -1
		if k := slices.IndexFunc(norm, func(x Mark) bool { return x.Type == m.Type }); k >= 0 {
			at = slices.IndexFunc(out, func(x Mark) bool { return markEqual(x, norm[k]) })
			if at < 0 {
				continue // the user changed it
			}
		} else if slices.ContainsFunc(out, func(x Mark) bool { return x.Type == m.Type }) {
			continue
		}
		if !cloned {
			out, cloned = slices.Clone(out), true
		}
		c := cloneNode(&Node{Marks: []Mark{m}}).Marks[0]
		if at >= 0 {
			out[at] = c
		} else {
			out = append(out, c)
		}
	}
	if cloned {
		sortMarks(out)
	}
	return out
}

// runeTok is one character of an inline run, or one inline node other than
// text: its mark set and node, for restoring marks.
type runeTok struct {
	s     string // the character, or the node's type (orig) or key (md)
	marks []Mark
	node  *Node
}

func runeToks(ns []*Node, byKey bool) []runeTok {
	var out []runeTok
	for _, n := range ns {
		if n.Type != "text" {
			s := "\x00" + n.Type
			if byKey {
				s = "\x00" + key(n)
			}
			out = append(out, runeTok{s, n.Marks, n})
			continue
		}
		for _, r := range n.Text {
			out = append(out, runeTok{string(r), n.Marks, n})
		}
	}
	return out
}

func tokStrings(ts []runeTok) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.s
	}
	return out
}

// restoreRuns gives fresh, an edited inline run, the marks and inline
// attributes orig had that Markdown can't show (a text color, a link's
// details, an image's size). Characters pair orig to norm and norm to
// fresh by their longest common subsequence, so a mark comes back on
// each character that survives the edit; text typed inside a run whose
// characters on both sides take the same marks takes them too. Word
// marks (colors, comments) go by word instead (see wordMarksFor). Inline
// nodes pair by type with norm and by content with fresh.
func restoreRuns(orig, norm, fresh []*Node) []*Node {
	o, n, f := runeToks(orig, false), runeToks(norm, false), runeToks(fresh, true)
	nk := runeToks(norm, true)
	on, err := lcs(context.Background(), tokStrings(o), tokStrings(n), alignmentCells)
	if err != nil {
		return fresh
	}
	nf, err := lcs(context.Background(), tokStrings(nk), tokStrings(f), alignmentCells)
	if err != nil {
		return fresh
	}
	origOf := make([]int, len(f)) // fresh token -> orig token, or -1
	for j := range origOf {
		origOf[j] = -1
	}
	normOf := make([]int, len(n))
	for k := range normOf {
		normOf[k] = -1
	}
	for i, k := range on {
		if k >= 0 {
			normOf[k] = i
		}
	}
	for k, j := range nf {
		if j >= 0 && normOf[k] >= 0 {
			origOf[j] = normOf[k]
		}
	}
	marks := make([][]Mark, len(f))
	changed := false
	for j, t := range f {
		marks[j] = t.marks
		i := origOf[j]
		if i < 0 {
			continue
		}
		k := on[i]
		if t.node.Type != "text" {
			restoreAttrs(o[i].node, n[k].node, t.node)
		}
		om := o[i].marks
		if t.node.Type == "text" {
			om = withoutWordMarks(om) // they go by word, below
		}
		if m := mergeMarks(om, n[k].marks, t.marks); !marksEqual(m, t.marks) {
			marks[j], changed = m, true
		}
	}
	// Typed text inside a run takes the run's marks.
	for j := 0; j < len(f); j++ {
		if origOf[j] >= 0 || j == 0 || origOf[j-1] < 0 || f[j].node.Type != "text" {
			continue
		}
		end := j
		for end < len(f) && origOf[end] < 0 && f[end].node.Type == "text" {
			end++
		}
		l, r := j-1, end
		if r < len(f) && origOf[r] >= 0 && marksEqual(marks[l], marks[r]) &&
			marksEqual(f[l].marks, f[r].marks) && !marksEqual(marks[l], f[l].marks) {
			for x := j; x < end; x++ {
				if marksEqual(f[x].marks, f[l].marks) {
					marks[x] = marks[l]
				}
			}
		}
		j = end
	}
	add, _, _, _ := wordMarksFor(o, f)
	if !changed {
		return withWordMarks(fresh, add)
	}
	var out []*Node
	j := 0
	for _, fn := range fresh {
		if fn.Type != "text" {
			fn.Marks = marks[j]
			out = append(out, fn)
			j++
			continue
		}
		rs := []rune(fn.Text)
		start := 0
		for x := 1; x <= len(rs); x++ {
			if x < len(rs) && marksEqual(marks[j+x], marks[j+start]) {
				continue
			}
			t := *fn
			t.Text = string(rs[start:x])
			t.Marks = marks[j+start]
			out = append(out, &t)
			start = x
		}
		j += len(rs)
	}
	return withWordMarks(mergeText(out), add)
}

// wordMarks are the marks md can't show that belong to a text's words: a
// color, a highlight, an inline comment.
var wordMarks = map[string]bool{"textColor": true, "backgroundColor": true, "annotation": true}

// withoutWordMarks is ms without the marks of wordMarks.
func withoutWordMarks(ms []Mark) []Mark {
	return slices.DeleteFunc(slices.Clone(ms), func(m Mark) bool { return wordMarks[m.Type] })
}
