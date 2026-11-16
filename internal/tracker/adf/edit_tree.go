package adf

import (
	"context"
	"maps"
	"regexp"
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

// restoreMoved gives the changed paragraphs and headings of doc the word
// marks their words take from the changed ones of orig they share text
// with, read in order as one text (see restoreWordMarks): every part of a
// split paragraph, and the whole of a merged one, keeps its colors, at
// any level (half of a bullet split out below its list). Blocks connect
// where they share half the words of the smaller; text that shares less
// is new and takes nothing. A part takes a block mark (alignment,
// indentation) its sources agree on; a merge whose sources differ keeps
// none, and the invariant refuses the loss.
func restoreMoved(orig, doc *Node) {
	inOrig, inDoc := treeSet(orig), treeSet(doc)
	var os, fs []*Node
	under := map[*Node]string{} // the type of the node holding each
	// A code block holds no word marks or block marks to restore.
	movable := func(n *Node) bool { return isInlineParent(n.Type) && n.Type != "codeBlock" }
	walk(orig, func(n, p *Node) {
		if movable(n) && !inDoc[n] && p != nil {
			os, under[n] = append(os, n), p.Type
		}
	})
	walk(doc, func(n, p *Node) {
		if movable(n) && !inOrig[n] && p != nil {
			fs, under[n] = append(fs, n), p.Type
		}
	})
	if len(os) == 0 || len(fs) == 0 {
		return
	}
	// An original that reads as another with other state could be either:
	// identity is never guessed, so neither gives its state. Only those
	// with the text of a changed one can read as it.
	texts := map[string]bool{}
	for _, n := range os {
		texts[plainText(n)] = true
	}
	states := map[string]map[string]bool{}
	walk(orig, func(n, _ *Node) {
		if movable(n) && texts[plainText(n)] {
			k := renderInline(n.Content, ctxPara)
			if states[k] == nil {
				states[k] = map[string]bool{}
			}
			states[k][hiddenState(n)] = true
		}
	})
	os = slices.DeleteFunc(os, func(n *Node) bool {
		return len(states[renderInline(n.Content, ctxPara)]) != 1
	})
	if len(os) == 0 || len(fs) == 0 || len(os) > alignmentCells/len(fs) {
		return
	}
	bags := func(ns []*Node) ([]map[string]int, []int) {
		out, size := make([]map[string]int, len(ns)), make([]int, len(ns))
		for i, n := range ns {
			out[i] = map[string]int{}
			for _, w := range wordSpans(runeToks(n.Content, false)) {
				out[i][w.text]++
				size[i]++
			}
		}
		return out, size
	}
	ob, osz := bags(os)
	fb, fsz := bags(fs)
	// Components of the graph of blocks that share text.
	parent := make([]int, len(os)+len(fs))
	for i := range parent {
		parent[i] = i
	}
	find := func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	for i := range os {
		for j := range fs {
			if c := common(ob[i], fb[j]); c > 0 && c*2 >= min(osz[i], fsz[j]) {
				parent[find(i)] = find(len(os) + j)
			}
		}
	}
	groups := map[int][2][]int{}
	for i := range os {
		g := groups[find(i)]
		g[0] = append(g[0], i)
		groups[find(i)] = g
	}
	for j := range fs {
		g := groups[find(len(os)+j)]
		g[1] = append(g[1], j)
		groups[find(len(os)+j)] = g
	}
	for _, g := range groups {
		if len(g[0]) == 0 || len(g[1]) == 0 {
			continue
		}
		var src, dst [][]*Node
		for _, i := range g[0] {
			src = append(src, os[i].Content)
		}
		for _, j := range g[1] {
			dst = append(dst, fs[j].Content)
		}
		res, from := restoreWordMarks(src, dst)
		for k, j := range g[1] {
			f := fs[j]
			f.Content = res[k]
			if len(from[k]) == 0 {
				continue
			}
			first := os[g[0][from[k][0]]]
			if under[first] != under[f] {
				continue // a block mark belongs where its block was
			}
			want := blockMarks(first.Marks)
			for _, s := range from[k][1:] {
				if !marksEqual(blockMarks(os[g[0][s]].Marks), want) {
					want = nil
				}
			}
			f.Marks = append(slices.DeleteFunc(slices.Clone(f.Marks), func(m Mark) bool {
				return blockMarkTypes[m.Type]
			}), want...)
			sortMarks(f.Marks)
			if len(f.Marks) == 0 {
				f.Marks = nil
			}
		}
	}
}

// hiddenState is what of inline parent n md doesn't show: its block marks
// and its text's word marks, character by character.
func hiddenState(n *Node) string {
	var b strings.Builder
	b.WriteString(jsonText(blockMarks(n.Marks)))
	for _, t := range runeToks(n.Content, false) {
		b.WriteString(jsonText(hiddenOf(t.marks)))
	}
	return b.String()
}

// blockMarkTypes are the marks of a paragraph or heading itself.
var blockMarkTypes = map[string]bool{"alignment": true, "indentation": true}

func blockMarks(ms []Mark) []Mark {
	var out []Mark
	for _, m := range ms {
		if blockMarkTypes[m.Type] {
			out = append(out, m)
		}
	}
	return out
}

// contentPairs pairs originals (by index into norm) with fresh blocks of
// their type in one gap by what they share, as assign does, never on a
// tie: a pair must share an inline node or child block, or half the
// original's words. A reworded paragraph beside a new one pairs with its
// original; so does the part of a split paragraph that kept most of it.
// Words count as letters and digits, in any case.
var wordsRe = regexp.MustCompile(`[\p{L}\p{N}]+`)

func contentPairs(norm normalized, is []int, fs []*Node) map[*Node]int {
	if len(is) == 0 || len(fs) == 0 || len(is) > alignmentCells/len(fs) {
		return nil
	}
	shape := func(at int, n *Node) shaped {
		words := map[string]int{}
		walk(n, func(t, _ *Node) {
			for _, w := range wordsRe.FindAllString(strings.ToLower(t.Text), -1) {
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

// takeMoved returns the original pairing chose for f, a fresh block no
// gap paired, across order. Any other move is left to reclaim, which sees
// the whole tree.
func (e *editor) takeMoved(f *Node) (*Node, bool) {
	if o := e.want[f]; o != nil && !e.claimed[o] {
		e.claimed[o] = true
		return o, true
	}
	return nil, false
}

// takeNew places f, a fresh block with no original, and returns what
// goes in the output. A container or a macro marker stays as it is, for
// a moved original to reclaim once the whole tree is edited (see
// reclaim); anything else settles now.
func (e *editor) takeNew(f *Node) []*Node {
	e.used = append(e.used, f)
	if e.moveKey(f, true) != "" {
		e.taken = append(e.taken, f)
		return []*Node{f}
	}
	if f.Type == tMarker {
		return nil
	}
	k := key(f)
	out := e.c.settle(f)
	if len(out) == 1 && out[0] == f {
		e.leaves = append(e.leaves, leaf{f, k})
	}
	return out
}

// reclaim pairs the original containers and macros the edit lost with
// the containers (or macro markers) md placed elsewhere in the tree, which
// a moved block's level saw before the original's level was edited. It
// pairs as assign does: by shared children, then shared words, and never
// when a tie leaves the choice a guess. What stays unclaimed then settles:
// a new container is built or flattened, a leftover marker goes.
func (e *editor) reclaim(ctx context.Context, root *Node) error {
	for again := len(e.taken) > 0; again; {
		again = false
		inTree := treeSet(root)
		var lost []*Node
		var a, b []shaped
		for _, l := range e.losses {
			if sh := e.moveKey(l.orig, false); sh != "" && !e.claimed[l.orig] && l.part == "" {
				a = append(a, shaped{len(lost), sh, e.kidsOf(ctx, l.orig), 0,
					key(l.orig), wordBag(l.orig)})
				lost = append(lost, l.orig)
			}
		}
		for k, t := range e.taken {
			if sh := e.moveKey(t, true); sh != "" && inTree[t] {
				b = append(b, newShaped(k, sh, t, t, 0))
			}
		}
		if len(a) == 0 || len(b) == 0 || len(a) > alignmentCells/len(b) {
			break
		}
		pairs, _ := assign(a, b)
		for _, pr := range pairs {
			o, f := lost[a[pr[0]].at], e.taken[b[pr[1]].at]
			n := o
			if o.Type != "extension" {
				var err error
				if n, _, err = e.container(ctx, o, f, 1<<30); err != nil {
					return err
				}
			}
			e.claimed[o] = true
			replaceNode(root, f, []*Node{n})
			again = true
		}
	}
	if len(e.taken) > 0 {
		inTree := treeSet(root)
		for _, t := range e.taken {
			if inTree[t] {
				replaceNode(root, t, e.c.settle(t))
			}
		}
	}
	return e.reclaimLeaves(ctx, root)
}

// reclaimLeaves gives a block md moved (a paragraph, a list, an image)
// its original back: a fresh block placed with no original takes the
// original the edit lost that reads back exactly as it does, so a moved
// centered paragraph stays centered and a moved image keeps its size.
// It never guesses: when another original on an edited level reads the
// same but differs underneath, alignment may have kept the wrong one in
// place, and none is reclaimed.
func (e *editor) reclaimLeaves(ctx context.Context, root *Node) error {
	if len(e.leaves) == 0 {
		return nil
	}
	inTree := treeSet(root)
	fresh := map[string][]*Node{}
	var keys []string
	for _, l := range e.leaves {
		if inTree[l.n] {
			if len(fresh[l.key]) == 0 {
				keys = append(keys, l.key)
			}
			fresh[l.key] = append(fresh[l.key], l.n)
		}
	}
	lost := map[string][]*Node{}
	for _, l := range e.losses {
		if e.claimed[l.orig] || l.part != "" || e.moveKey(l.orig, false) != "" {
			continue
		}
		k, err := e.normalKey(ctx, l.orig)
		if err != nil {
			return err
		}
		if k != "" && len(fresh[k]) > 0 {
			lost[k] = append(lost[k], l.orig)
		}
	}
	under := map[string]map[string]bool{} // what originals reading k are
	for _, o := range e.origs {
		if len(lost[o.key]) > 0 {
			if under[o.key] == nil {
				under[o.key] = map[string]bool{}
			}
			under[o.key][key(o.n)] = true
		}
	}
	gone := map[*Node]bool{}
	for _, k := range keys {
		os, fs := lost[k], fresh[k]
		alike := true
		for _, o := range os {
			alike = alike && len(under[k]) <= 1 && key(o) == key(os[0])
		}
		if len(os) == 0 || !alike {
			continue
		}
		for x := range min(len(os), len(fs)) {
			replaceNode(root, fs[x], []*Node{os[x]})
			e.claimed[os[x]], gone[fs[x]] = true, true
		}
	}
	if len(gone) > 0 {
		e.used = slices.DeleteFunc(e.used, func(n *Node) bool { return gone[n] })
	}
	return nil
}

// normalKey is the key of what o reads back as, or "" when that is not
// one block.
func (e *editor) normalKey(ctx context.Context, o *Node) (string, error) {
	if k, ok := e.normKey[o]; ok {
		return k, nil
	}
	norm, err := normalize(ctx, []*Node{o}, e.c.st)
	if err != nil {
		return "", err
	}
	k := ""
	if len(norm.blocks[0]) == 1 {
		k = key(norm.blocks[0][0])
	}
	if e.normKey == nil {
		e.normKey = map[*Node]string{}
	}
	e.normKey[o] = k
	return k, nil
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

// kidsOf is o's normalized children's keys, from the pool or worked out.
func (e *editor) kidsOf(ctx context.Context, o *Node) map[string]int {
	if k, ok := e.kids[o]; ok && k != nil {
		return k
	}
	var k map[string]int
	norm, err := normalize(ctx, []*Node{o}, e.c.st)
	if err == nil && len(norm.blocks[0]) == 1 {
		k = childKeys(norm.blocks[0][0])
	}
	if e.kids == nil {
		e.kids = map[*Node]map[string]int{}
	}
	e.kids[o] = k
	return k
}

// treeSet is every node in root's tree.
func treeSet(root *Node) map[*Node]bool {
	set := map[*Node]bool{}
	walk(root, func(m, _ *Node) { set[m] = true })
	return set
}

// replaceNode puts with in place of old wherever root's tree holds it.
func replaceNode(root, old *Node, with []*Node) {
	walk(root, func(m, _ *Node) {
		for i := 0; i < len(m.Content); i++ {
			if m.Content[i] == old {
				m.Content = slices.Concat(m.Content[:i:i], with, m.Content[i+1:])
				return
			}
		}
	})
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

// lossWarnings names, for each original block the edit replaced or
// removed, the noticeable features Markdown could not carry over. A
// replaced block that held a container flattened it.
func (e *editor) lossWarnings() []Warning {
	var out []Warning
	for _, l := range e.losses {
		if e.claimed[l.orig] {
			continue
		}
		feats := warnFeatures(l.orig)
		if l.part != "" {
			feats = lossPhrases(l.orig)
		}
		if l.replaced {
			feats = notIn(l.orig, feats, l.repl)
		}
		switch {
		case l.part != "" && len(feats) > 0 && cosmetic(feats...):
			out = append(out, Warning{Line: l.line,
				Msg: l.part + " had " + joinWords(feats) + ", which the edit dropped" + l.why})
		case l.part != "" && len(feats) > 0:
			out = append(out, Warning{Line: l.line, Flattens: true,
				Msg: l.part + " had " + joinWords(feats) + ", which the edit would lose" + l.why})
		case l.unclosed > 0:
			out = append(out, Warning{Line: l.unclosed, Flattens: true,
				Msg: "<!-- " + macroName(l.orig) + " --> is never closed with <!-- /" +
					macroName(l.orig) + " -->; its macro is not kept"})
		case len(feats) == 0:
		case l.replaced && slices.Contains(feats, "a status lozenge") &&
			lozengeText(l.orig, l.repl) != "":
			out = append(out, Warning{Line: l.line, Flattens: true,
				Msg: lozengeText(l.orig, l.repl) + " is plain text now: a lozenge whose " +
					"text changes " +
					"along with other formatting in its block can't be kept; change the " +
					"lozenge's text by itself"})
		case l.replaced:
			out = append(out, Warning{Line: l.line,
				Msg: "replaced a block that had " + joinWords(feats) +
					"; those are now plain", Flattens: l.flattens})
		default:
			out = append(out, Warning{
				Msg: "removed a block that had " + joinWords(feats)})
		}
	}
	return slices.Compact(out)
}

// lozengeText is the first text in repl written as a lozenge is,
// "[READY]", that none of orig's lozenges reads as: a lozenge whose text
// the user changed, which md reads as plain text. "" when there is none.
func lozengeText(orig *Node, repl []*Node) string {
	was := map[string]bool{}
	walk(orig, func(n, _ *Node) {
		if n.Type == "status" {
			was["["+n.attrStr("text")+"]"] = true
		}
	})
	found := ""
	for _, n := range repl {
		walk(n, func(t, _ *Node) {
			if found != "" || t.Type != "text" || hasMark(t, "code") {
				return
			}
			for _, m := range lozengeRe.FindAllString(t.Text, -1) {
				if !was[m] {
					found = m
					return
				}
			}
		})
	}
	return found
}

var lozengeRe = regexp.MustCompile(`\[[A-Z][A-Z0-9 ]*\]`)

// notIn drops from feats those the replacement still has: restored
// lozenges and macros are not lost.
func notIn(orig *Node, feats []string, repl []*Node) []string {
	had, still := featureCounts(orig), map[string]int{}
	for _, n := range repl {
		for f, k := range featureCounts(n) {
			still[f] += k
		}
	}
	var out []string
	for _, f := range feats {
		if still[f] < had[f] {
			out = append(out, f)
		}
	}
	return out
}

// warned is every original node under a loss that already has a warning
// of its own (Flattens for content, plain for dress), which the edit
// invariant leaves to it.
func (e *editor) warned() map[*Node]bool {
	out := map[*Node]bool{}
	for _, l := range e.losses {
		if (l.flattens || l.unclosed > 0) && !e.claimed[l.orig] {
			walk(l.orig, func(n, _ *Node) { out[n] = true })
		}
	}
	return out
}

// holdsContainer reports whether n is or holds a container Markdown can't
// write back: a column layout, a macro with a body, or a custom panel.
func holdsContainer(n *Node) bool {
	found := false
	walk(n, func(m, _ *Node) {
		switch m.Type {
		case "layoutSection", "bodiedExtension":
			found = true
		case "panel":
			found = found || customPanel(m)
		}
	})
	return found
}

// editWarnings keeps the parse warnings that fall inside blocks taken
// from md, at any depth.
func (c *converter) editWarnings(top []region) []Warning {
	var out []Warning
	for _, w := range c.st.warns {
		if w.Line == 0 || inUsed(top, w.Line) {
			out = append(out, w)
		}
	}
	return out
}

func inUsed(rs []region, line int) bool {
	for _, r := range rs {
		if line >= r.lo && (line < r.hi || r.lo == r.hi) {
			if r.used {
				return true
			}
			return inUsed(r.kids, line)
		}
	}
	return false
}

// macroMiss refuses a macro marker that matches no macro while the
// original has macros of its key: its ID was dropped or mistyped, and the
// macro it meant would go. The warning names the IDs that exist.
func (c *converter) macroMiss(line int, key, id string) bool {
	ids := c.st.macros[key]
	if len(ids) == 0 || slices.Contains(ids, id) {
		return false
	}
	written := "<!-- macro: " + key
	if id != "" {
		written += " " + id
	}
	c.st.warns = append(c.st.warns, Warning{Line: line, Flattens: true,
		Msg: written + " --> matches none of the " + key + " macros here (" +
			strings.Join(ids, ", ") + "); write the marker with the ID it had"})
	return true
}

// settle finishes a fresh block taken without an original: a macro made
// from markers alone has no type to send, so its children stand in its
// place, with a warning; containers inside it settle the same way.
func (c *converter) settle(n *Node) []*Node {
	switch {
	case n.Type == tMarker:
		// A leftover marker at block level: nothing to send.
		if m, ok := parseMarker(n); ok && m.kind == "macro" && !m.close &&
			!c.macroMiss(c.lineFor(n), m.arg, m.id) {
			c.warn(c.lineFor(n), "<!-- %s --> matches no macro here, and a new one "+
				"can't be made from Markdown; it is left out", n.attrStr("text"))
		}
		return nil
	case isInlineParent(n.Type):
		return []*Node{n}
	}
	var kids []*Node
	for _, ch := range n.Content {
		kids = append(kids, c.settle(ch)...)
	}
	n.Content = kids
	if n.Type == "bodiedExtension" && c.st.built[n] {
		id, _ := n.Attrs[attrMacroID].(string)
		if c.macroMiss(c.lineFor(n), commentSafe(n.attrStr("extensionKey")), id) {
			return kids
		}
		c.warn(c.lineFor(n), "<!-- %s --> matches no macro here, and a new one "+
			"can't be made from Markdown; its content is kept as plain blocks",
			macroName(n))
		return kids
	}
	if n.Type == "layoutSection" && c.st.built[n] && len(n.Content) < 2 {
		c.warn(c.lineFor(n), "a new <!-- layout --> needs two or three columns; "+
			"its content is kept as plain blocks")
		return unwrapColumns(kids)
	}
	if n.Type == "panel" && c.st.built[n] && !validPanelType(n.attrStr("panelType")) {
		c.warn(c.lineFor(n), "<!-- panel: %s --> is not a panel type (info, note, "+
			"tip, warning, error, success or custom); its content is kept as plain blocks",
			commentSafe(n.attrStr("panelType")))
		return kids
	}
	return []*Node{n}
}

func validPanelType(t string) bool {
	switch t {
	case "info", "note", "tip", "warning", "error", "success", "custom":
		return true
	}
	return false
}

// plainText is the text of inline parent n, with each other node as it
// renders alone: a cheap key that two parents rendering alike share.
func plainText(n *Node) string {
	var b strings.Builder
	for _, c := range n.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		} else {
			b.WriteString(renderInline([]*Node{c}, ctxPara))
		}
	}
	return b.String()
}
