package adf

import (
	"context"
	"maps"
	"slices"
	"strings"
)

// editor carries one Edit through the tree: containers whose markers
// survive keep their original node and have their children edited the
// same way the top level is.
type editor struct {
	c *converter
	// used are the fresh nodes taken from md, which still need their
	// pending references resolved and their markers stripped.
	used []*Node
	// pool holds unpaired original containers by shape, for a container
	// moved where no gap pairs it; claimed marks those a fresh container
	// took, which are not lost.
	pool    map[string][]*Node
	kids    map[*Node]map[string]int // pooled originals' child keys
	claimed map[*Node]bool
	// want is a pairing across order: the fresh container that should take
	// this original, which stays pooled until it does.
	want   map[*Node]*Node
	taken  []*Node // fresh containers and markers placed with no original
	losses []loss
	// gapOrigs are the originals in the gap each fresh block took the
	// place of, where restoreInline may find what it shows.
	gapOrigs map[*Node][]*Node
	// pairedOrig is the original a fresh block (or cell) replaced, where
	// the edit knows it, for restoring that original's inline nodes.
	pairedOrig map[*Node]*Node
	// leaves are the fresh blocks other than containers placed with no
	// original, by key, for a moved original to reclaim (see
	// reclaimLeaves); normKey caches what an original reads back as.
	leaves  []leaf
	normKey map[*Node]string
	origs   []leaf // each original an edited level held, by normKey
	// carry pairs a fresh block with the original it replaced for
	// certain, whose settings Markdown can't show it takes back (see
	// restoreBlock) unless that original moved.
	carry []carried
	// reuse pairs a fresh child md left unchanged with its original, which
	// takes its place once the fresh nodes have settled (see useOriginals).
	reuse [][2]*Node
	// patched counts the blocks md's text change was applied to in place.
	patched int
}

// useOriginals puts each reused original in place of the fresh child it
// matched, after the edit settled the fresh nodes, so none of that work
// touches an original.
func (e *editor) useOriginals(root *Node) {
	if len(e.reuse) == 0 {
		return
	}
	with := map[*Node]*Node{}
	for _, r := range e.reuse {
		with[r[0]] = r[1]
	}
	walk(root, func(n, _ *Node) {
		for i, c := range n.Content {
			if o, ok := with[c]; ok {
				n.Content[i] = o
			}
		}
	})
}

// carried is a fresh block, the original it replaced, and that original
// as md reads it back.
type carried struct{ fresh, orig, norm *Node }

type leaf struct {
	n   *Node
	key string
}

// loss is an original block an edit left out: replaced by fresh blocks
// (line is the first one's) or removed.
type loss struct {
	orig     *Node
	line     int
	replaced bool
	unclosed int // line of a leaf marker left by a macro's lost close
	// flattens is a replaced block that held a container whose content
	// is still in md without its markers.
	flattens bool
	repl     []*Node // the fresh blocks that replaced it
	// part names a node that stays, with md's content in place of its
	// own (a merged table cell, a table rebuilt from md): what it held
	// that Markdown can't carry, even what no one would notice, is lost.
	part string
	why  string // why, for a part
}

// region is the line range of one fresh block and whether its warnings
// count: a block taken from md does, a paired container's own lines
// don't, but its children's do (kids).
type region struct {
	lo, hi int
	used   bool
	kids   []region
}

// blocks edits old into fresh, both sibling lists, and returns the
// output siblings and the regions of fresh up to line hi.
func (e *editor) blocks(ctx context.Context, old, fresh []*Node, hi int) (
	[]*Node, []region, error) {
	norm, err := normalize(ctx, old, e.c.st)
	if err != nil {
		return nil, nil, err
	}
	p, err := e.align(ctx, old, norm, fresh)
	if err != nil {
		return nil, nil, err
	}
	if e.normKey == nil {
		e.normKey = map[*Node]string{}
	}
	for i, b := range old {
		if len(norm.blocks[i]) == 1 {
			k := key(norm.blocks[i][0])
			e.normKey[b] = k
			e.origs = append(e.origs, leaf{b, k})
		}
	}
	paired := map[int]bool{}
	for _, i := range p.pairOf {
		paired[i] = true
	}
	for i, b := range old {
		if !p.kept[i] && !paired[i] {
			var kids map[string]int
			if len(norm.blocks[i]) == 1 {
				kids = childKeys(norm.blocks[i][0])
			}
			e.toPool(b, kids)
		}
	}
	regions := make([]region, len(fresh))
	for j := range fresh {
		regions[j].lo, regions[j].hi = e.c.lineFor(fresh[j]), hi
		if j+1 < len(fresh) {
			regions[j].hi = e.c.lineFor(fresh[j+1])
		}
	}
	var out []*Node
	for _, en := range p.order {
		if en.orig >= 0 {
			out = append(out, old[en.orig])
			continue
		}
		j, f := en.fresh, fresh[en.fresh]
		orig, ok := (*Node)(nil), false
		if i, paired := p.pairOf[j]; paired {
			orig, ok = old[i], true
		} else {
			orig, ok = e.takeMoved(f)
		}
		switch {
		case ok && orig.Type == "extension":
			out = append(out, orig) // a moved macro without a body
		case ok:
			n, kids, err := e.container(ctx, orig, f, regions[j].hi)
			if err != nil {
				return nil, nil, err
			}
			regions[j].kids = kids
			out = append(out, n)
		default:
			regions[j].used = true
			out = append(out, e.takeNew(f)...)
		}
	}
	solo, typed, gaps := gapPairs(p, norm, old, fresh)
	for f, os := range gaps {
		e.gapOrigs[f] = os
	}
	patched, err := e.patchPairs(ctx, p, norm, old, fresh, solo, typed, out)
	if err != nil {
		return nil, nil, err
	}
	carry := func(f *Node, i int) {
		if f.Type != tMarker && len(norm.blocks[i]) == 1 {
			e.carry = append(e.carry, carried{f, old[i], norm.blocks[i][0]})
		}
	}
	for _, f := range fresh {
		if i, ok := typed[f]; ok && !solo[i] && !patched[i] {
			e.pairedOrig[f] = old[i]
			carry(f, i)
		}
	}
	for i, b := range old {
		if p.kept[i] || paired[i] || patched[i] {
			continue
		}
		l := loss{orig: b}
		if j := firstBlock(fresh, p.runs[i]); j >= 0 {
			l.line, l.replaced = e.c.lineFor(fresh[j]), true
			l.repl = fresh[p.runs[i][0]:p.runs[i][1]]
			if solo[i] {
				for _, f := range l.repl {
					e.pairedOrig[f] = b
					carry(f, i)
				}
			}
			l.unclosed = e.unclosedMacro(b, fresh, p.runs[i])
			l.flattens = holdsContainer(b)
			if e.c.shape(b, false) != "" && len(norm.blocks[i]) == 1 {
				// A container deleted with its content, next to new
				// blocks, was removed, not flattened.
				l.flattens = keptContent(norm.blocks[i][0], fresh[p.runs[i][0]:p.runs[i][1]])
			}
		}
		e.losses = append(e.losses, l)
	}
	return out, regions, nil
}

// patchPairs applies md's text change to each original paired for
// certain with the one block that replaced it (see patchText), in place
// of that block in out; patched marks those originals, which are kept.
func (e *editor) patchPairs(ctx context.Context, p editPlan, norm normalized, old,
	fresh []*Node, solo map[int]bool, typed map[*Node]int, out []*Node) (map[int]bool, error) {
	pairOf := map[int]*Node{}
	for i := range old {
		if !solo[i] {
			continue
		}
		for _, f := range fresh[p.runs[i][0]:p.runs[i][1]] {
			if f.Type != tMarker {
				pairOf[i] = f
			}
		}
	}
	for f, i := range typed {
		if !solo[i] {
			pairOf[i] = f
		}
	}
	patched := map[int]bool{}
	gone := map[*Node]bool{}
	for i, f := range pairOf {
		if len(norm.blocks[i]) != 1 || e.c.shape(old[i], false) != "" || e.c.shape(f, true) != "" {
			continue
		}
		n := patchText(ctx, old[i], norm.blocks[i][0], f, e.c.st)
		if n == nil {
			continue
		}
		if k := slices.Index(out, f); k >= 0 {
			out[k] = n
			patched[i], gone[f] = true, true
			e.patched++
		}
	}
	if len(gone) > 0 {
		e.used = slices.DeleteFunc(e.used, func(n *Node) bool { return gone[n] })
	}
	return patched, ctx.Err()
}

// moveKey is what lets a moved block reclaim its original: a container's
// shape, or a macro without a body by its marker. "" for anything else.
func (e *editor) moveKey(n *Node, fresh bool) string {
	if n.Type == "extension" && !fresh {
		return "ext|" + macroName(n)
	}
	if n.Type == tMarker {
		if m, ok := parseMarker(n); ok && m.kind == "macro" && !m.close {
			return "ext|" + strings.TrimSuffix(strings.TrimPrefix(markerText(m), "<!-- "), " -->")
		}
		return ""
	}
	return e.c.shape(n, fresh)
}

// toPool offers b, an original the edit didn't keep or pair in place, to
// a block moved elsewhere; kids are its normalized children's keys.
func (e *editor) toPool(b *Node, kids map[string]int) {
	sh := e.moveKey(b, false)
	if sh == "" {
		return
	}
	if e.pool == nil {
		e.pool = map[string][]*Node{}
		e.kids = map[*Node]map[string]int{}
	}
	e.pool[sh] = append(e.pool[sh], b)
	e.kids[b] = kids
}

// gapPairs pairs the unmatched originals with the fresh blocks that
// replaced them where that is not a guess: an original that is the only
// one with content in its gap, beside exactly one new block (solo); in
// any gap a block type with one original and one new block of it; and
// failing that, an original and the block of its type in its gap that
// alone reads most like it (see contentPairs). typed maps fresh block to
// original, and gaps each fresh block to the originals of its gap.
func gapPairs(p editPlan, norm normalized, old, fresh []*Node) (solo map[int]bool,
	typed map[*Node]int, gaps map[*Node][]*Node) {
	solo, typed, gaps = map[int]bool{}, map[*Node]int{}, map[*Node][]*Node{}
	var gap []int
	prevEnd := 0
	flush := func(end int) {
		var blocks []*Node
		for j := prevEnd; j < end; j++ {
			if fresh[j].Type != tMarker {
				blocks = append(blocks, fresh[j])
			}
		}
		if len(gap) == 1 && len(blocks) == 1 {
			solo[gap[0]] = true
		}
		ofType := map[string][]int{}
		for _, i := range gap {
			ofType[old[i].Type] = append(ofType[old[i].Type], i)
		}
		count := map[string]int{}
		for _, f := range blocks {
			count[f.Type]++
		}
		var left []int
		var leftF []*Node
		for _, f := range blocks {
			if is := ofType[f.Type]; len(is) == 1 && count[f.Type] == 1 {
				typed[f] = is[0]
			} else {
				leftF = append(leftF, f)
			}
			for _, i := range gap {
				gaps[f] = append(gaps[f], old[i])
			}
		}
		for _, i := range gap {
			if len(ofType[old[i].Type]) > 1 || count[old[i].Type] > 1 {
				left = append(left, i)
			}
		}
		if len(gap) > 1 || len(blocks) > 1 {
			maps.Copy(typed, contentPairs(norm, left, leftF))
		}
		gap = gap[:0]
	}
	for i := range norm.blocks {
		switch {
		case p.start[i] >= 0:
			flush(p.start[i])
			prevEnd = p.runs[i][1]
		case !norm.empty(i):
			gap = append(gap, i)
		}
	}
	flush(len(fresh))
	return solo, typed, gaps
}

// wordsRe is a word for contentPairs: letters and digits, in any case.
var wordsRe = lazyRegexp{pattern: `[\p{L}\p{N}]+`}

// contentPairs pairs originals (by index into norm) with fresh blocks of
// their type in one gap by what they share, as assign does, never on a
// tie: a pair must share an inline node or child block, or half the
// original's words. A reworded paragraph beside a new one pairs with its
// original; so does the part of a split paragraph that kept most of it.
func contentPairs(norm normalized, is []int, fs []*Node) map[*Node]int {
	if len(is) == 0 || len(fs) == 0 || len(is) > alignmentCells/len(fs) {
		return nil
	}
	shape := func(at int, n *Node) shaped {
		words := map[string]int{}
		walk(n, func(t, _ *Node) {
			for _, w := range wordsRe.get().FindAllString(strings.ToLower(t.Text), -1) {
				words[w]++
			}
		})
		return shaped{at, n.Type, childKeys(n), 0, key(n), words}
	}
	var a, b []shaped
	for _, i := range is {
		if len(norm.blocks[i]) == 1 {
			a = append(a, shape(i, norm.blocks[i][0]))
		}
	}
	for j, f := range fs {
		b = append(b, shape(j, f))
	}
	pairs, _ := assign(a, b)
	out := map[*Node]int{}
	for _, pr := range pairs {
		x, y := a[pr[0]], b[pr[1]]
		words := 0
		for _, c := range x.words {
			words += c
		}
		if common(x.kids, y.kids) > 0 || words > 0 && common(x.words, y.words)*2 >= words {
			out[fs[y.at]] = x.at
		}
	}
	return out
}

// carryAttrs gives each fresh block that replaced an original for
// certain what that original had that Markdown can't show, unless the
// original moved elsewhere.
func (e *editor) carryAttrs(ctx context.Context) error {
	for _, c := range e.carry {
		if e.claimed[c.orig] {
			continue
		}
		if err := restoreBlock(ctx, c.orig, c.norm, c.fresh, func(f, o *Node) {
			e.reuse = append(e.reuse, [2]*Node{f, o})
		}); err != nil {
			return err
		}
	}
	return nil
}

// container keeps orig, a container whose markers survive, with its
// attributes, and edits its children against fresh's. What the Markdown
// says about the container itself (a column's width, an expand's title)
// comes from fresh.
func (e *editor) container(ctx context.Context, orig, fresh *Node, hi int) (
	*Node, []region, error) {
	n := *orig
	if orig.Attrs != nil {
		n.Attrs = maps.Clone(orig.Attrs)
	}
	switch n.Type {
	case "layoutSection":
		return e.layout(ctx, &n, fresh, hi)
	case "table":
		return e.table(ctx, orig, fresh, hi)
	}
	if n.Type == "expand" || n.Type == "nestedExpand" {
		if t, ok := fresh.Attrs["title"]; ok {
			if n.Attrs == nil {
				n.Attrs = map[string]any{}
			}
			n.Attrs["title"] = t
		} else {
			delete(n.Attrs, "title")
		}
	}
	kids, regions, err := e.blocks(ctx, orig.Content, fresh.Content, hi)
	if err != nil {
		return nil, nil, err
	}
	n.Content = nonEmpty(kids)
	return &n, regions, nil
}

// layout pairs a layout's columns by the blocks they share, across
// order, so deleting or swapping a column leaves the others' macros,
// lozenges and panels as they were. A paired column keeps its attributes
// (taking a changed width) and has its blocks edited; a column md adds is
// new, and a column md drops is gone, its blocks free to reclaim where md
// moved them.
func (e *editor) layout(ctx context.Context, n, fresh *Node, hi int) (
	*Node, []region, error) {
	var a, b []shaped
	for i, oc := range n.Content {
		norm, err := normalize(ctx, oc.Content, e.c.st)
		if err != nil {
			return nil, nil, err
		}
		keys := map[string]int{}
		for _, ns := range norm.blocks {
			for _, nb := range ns {
				keys[key(nb)]++
			}
		}
		a = append(a, shaped{i, "column", keys, 0, key(oc), wordBag(oc)})
	}
	for k, fc := range fresh.Content {
		b = append(b, newShaped(k, "column", fc, fc, 0))
	}
	colOf := map[int]int{}
	paired := map[int]bool{}
	pairs, _ := assign(a, b)
	for _, pr := range pairs {
		colOf[pr[1]], paired[pr[0]] = pr[0], true
	}
	regions := make([]region, len(fresh.Content))
	cols := make([]*Node, 0, len(fresh.Content))
	for k, fc := range fresh.Content {
		regions[k].lo, regions[k].hi = e.c.lineFor(fc), hi
		if k+1 < len(fresh.Content) {
			regions[k].hi = e.c.lineFor(fresh.Content[k+1])
		}
		i, ok := colOf[k]
		if !ok {
			regions[k].used = true
			var kids []*Node
			for _, ch := range fc.Content {
				kids = append(kids, e.takeNew(ch)...)
			}
			fc.Content = nonEmpty(kids)
			cols = append(cols, fc)
			continue
		}
		oc := n.Content[i]
		col := *oc
		col.Attrs = maps.Clone(oc.Attrs)
		if w, ok := fc.attrNum("width"); ok {
			if ow, ok := oc.attrNum("width"); !ok || ow != w {
				if col.Attrs == nil {
					col.Attrs = map[string]any{}
				}
				col.Attrs["width"] = w
			}
		}
		kids, kr, err := e.blocks(ctx, oc.Content, fc.Content, regions[k].hi)
		if err != nil {
			return nil, nil, err
		}
		col.Content, regions[k].kids = nonEmpty(kids), kr
		cols = append(cols, &col)
	}
	for i, oc := range n.Content {
		if !paired[i] {
			for _, b := range oc.Content {
				e.losses = append(e.losses, loss{orig: b})
				e.toPool(b, nil)
			}
		}
	}
	n.Content = cols
	return n, regions, nil
}

// nonEmpty gives a container whose children were all deleted the empty
// paragraph ADF requires.
func nonEmpty(kids []*Node) []*Node {
	if len(kids) == 0 {
		return []*Node{{Type: "paragraph"}}
	}
	return kids
}

// keptContent reports whether any of container n's blocks is among run.
func keptContent(n *Node, run []*Node) bool {
	kids := childKeys(n)
	for _, f := range run {
		if kids[key(f)] > 0 {
			return true
		}
	}
	return false
}

// unclosedMacro returns the line of a leaf "macro: KEY" marker among the
// fresh blocks that replaced orig, a macro with a body: its close was
// deleted, so its open now reads as a macro without one.
func (e *editor) unclosedMacro(orig *Node, fresh []*Node, run [2]int) int {
	if orig.Type != "bodiedExtension" {
		return 0
	}
	want := macroName(orig)
	for j := run[0]; j < run[1]; j++ {
		if fresh[j].Type == tMarker && fresh[j].attrStr("text") == want {
			return e.c.lineFor(fresh[j])
		}
	}
	return 0
}
