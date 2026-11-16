package adf

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// Edit converts md as FromMarkdown does, then keeps orig's own ADF for
// every top-level block the edit left unchanged, so what Markdown can't
// express (status lozenges, column widths, macros, layouts, localIds)
// survives edits elsewhere in the document. A block counts as unchanged
// when the Markdown ToMarkdown wrote for it still parses to the same
// thing. Changed blocks come from md; a Warning names anything they held
// that is now lost. Mentions in changed blocks resolve first by the
// names already in orig, so unchanged mentions need no Resolver call.
func Edit(ctx context.Context, orig *Node, md string, r Resolver) (*Node,
	[]Warning, error) {
	doc, ws, err := edit(ctx, orig, md, r)
	return doc, publicWarnings(ws), publicErr(err)
}

func edit(ctx context.Context, orig *Node, md string, r Resolver) (*Node,
	[]Warning, error) {
	if orig == nil {
		return FromMarkdown(ctx, md, r)
	}
	orig = dropNil(orig)
	md = lineFeeds(md)
	c := newConverter(ctx, md, r, true)
	c.st.macros = map[string][]string{}
	walk(orig, func(n, _ *Node) {
		switch n.Type {
		case "decisionList":
			c.st.decisions = true
		case "extension", "bodiedExtension", "inlineExtension":
			k := commentSafe(n.attrStr("extensionKey"))
			if id := macroID(n); !slices.Contains(c.st.macros[k], id) {
				c.st.macros[k] = append(c.st.macros[k], id)
			}
		}
	})
	fresh, err := c.document()
	if err != nil {
		return nil, nil, err
	}
	e := &editor{c: c, claimed: map[*Node]bool{}, want: map[*Node]*Node{},
		pairedOrig: map[*Node]*Node{}, gapOrigs: map[*Node][]*Node{}}
	out, top, err := e.blocks(ctx, orig.Content, fresh, 1<<30)
	if err != nil {
		return nil, nil, err
	}
	doc := &Node{Type: "doc", Version: 1, Content: out}
	if err := e.reclaim(ctx, doc); err != nil {
		return nil, nil, err
	}
	if err := e.carryAttrs(ctx); err != nil {
		return nil, nil, err
	}
	out = doc.Content
	newAt := make([]bool, len(e.used))
	for i := range newAt {
		newAt[i] = true
	}
	if err := c.resolvePending(newAt, e.used, orig); err != nil {
		return nil, nil, err
	}
	// With nothing taken from md and nothing lost, the result is the
	// original's own nodes: there is nothing to restore or check.
	changed := len(e.used) > 0 || len(e.losses) > 0 || e.patched > 0
	if changed {
		e.restoreInline(orig, out)
		restoreMoved(orig, &Node{Type: "doc", Content: out})
	}
	for _, b := range e.used {
		finish(b)
	}
	doc.Content = out
	e.useOriginals(doc)
	warns := append(c.editWarnings(top), e.lossWarnings()...)
	if err := Validate(doc); err != nil {
		return nil, nil, fmt.Errorf("adf: edit built invalid ADF: %w", err)
	}
	if changed {
		warns = append(warns, checkKept(orig, doc, md, e.warned())...)
	}
	return doc, warns, nil
}

// normalized is each original block as its Markdown reads back now:
// the blocks it parses to, or broken when it doesn't parse.
type normalized struct {
	blocks [][]*Node
	broken []bool
}

func normalize(ctx context.Context, old []*Node, opts *convState) (normalized, error) {
	n := normalized{blocks: make([][]*Node, len(old)),
		broken: make([]bool, len(old))}
	for i, b := range old {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		c := newConverter(ctx, ToMarkdown(Doc(b)), nil, true)
		c.st.coll, c.st.decisions = opts.coll, opts.decisions
		ns, err := c.document()
		n.blocks[i], n.broken[i] = ns, err != nil
	}
	return n, ctx.Err()
}

// empty reports whether original block i renders to nothing (an empty
// paragraph, a placeholder) yet parses.
func (n normalized) empty(i int) bool {
	return len(n.blocks[i]) == 0 && !n.broken[i]
}

type entry struct{ orig, fresh int }

type editPlan struct {
	order []entry
	kept  []bool   // original i is output as it was
	start []int    // matched originals: first fresh index, or -1
	runs  [][2]int // per original: the fresh range [lo, hi) it became
	// pairOf maps a fresh container to the original container it pairs
	// with by shape: the original is kept and its children edited.
	pairOf map[int]int
}

// align matches the normalized original blocks against the fresh ones
// and decides which originals to keep. Each original with content becomes
// one run of fresh blocks: the blocks it matched, the container its
// markers still form (paired by shape, so edits inside it and deletions
// around it never pair it with a neighbor), the blocks positionally
// paired with it among the new ones in its gap (edited), or none
// (deleted).
func (e *editor) align(ctx context.Context, old []*Node, norm normalized,
	fresh []*Node) (editPlan, error) {
	kept, start, err := matchBlocks(ctx, norm, fresh)
	if err != nil {
		return editPlan{}, err
	}
	width := make([]int, len(norm.blocks))
	for i := range width {
		width[i] = len(norm.blocks[i])
	}
	pairOf, err := e.pairShapes(ctx, old, norm, fresh, start, width)
	if err != nil {
		return editPlan{}, err
	}
	anchor, hasContent := anchorEmpty(norm)
	p := editPlan{kept: kept, start: start, pairOf: pairOf,
		runs: pairRuns(norm, fresh, start, width)}
	// Blocks that render to nothing follow the nearest block that does:
	// they stay while it stays or was edited, and go when it was deleted.
	for i := range kept {
		if norm.empty(i) {
			if a := anchor[i]; a >= 0 {
				kept[i] = kept[a] || p.replaced(fresh, a)
			} else {
				kept[i] = !hasContent && len(fresh) == 0
			}
		}
	}
	p.order = order(norm, fresh, p, anchor)
	return p, nil
}

// pairShapes pairs the unmatched original containers with fresh
// containers of the same shape by the children they share, across order
// and across gaps between exactly matched originals: two like macros that
// swapped places and were both edited each keep their own attributes, and
// a table moved past other blocks keeps its settings. Pairs inside one gap
// that keep document order get start and a width of one fresh block, so
// pairRuns treats them as matched; the rest go to takeMoved through want.
// Across gaps only containers with children in common pair.
func (e *editor) pairShapes(ctx context.Context, old []*Node, norm normalized,
	fresh []*Node, start, width []int) (map[int]int, error) {
	oldGap, freshGap := gaps(start, width, len(fresh))
	var a, b []shaped
	for i, g := range oldGap {
		if sh := e.c.shape(old[i], false); g >= 0 && sh != "" && len(norm.blocks[i]) == 1 {
			a = append(a, newShaped(i, sh, old[i], norm.blocks[i][0], g))
		}
	}
	for j, g := range freshGap {
		if sh := e.c.shape(fresh[j], true); g >= 0 && sh != "" {
			b = append(b, newShaped(j, sh, fresh[j], fresh[j], g))
		}
	}
	pairOf := map[int]int{}
	if len(a) == 0 || len(b) == 0 {
		return pairOf, nil
	}
	if len(a) > alignmentCells/len(b) {
		return pairOf, nil // too many to weigh: moved containers stay unpaired
	}
	pairs, _ := assign(a, b)
	// Per gap, the longest run of pairs whose fresh order follows theirs.
	keep := make([]bool, len(pairs))
	best, prev := make([]int, len(pairs)), make([]int, len(pairs))
	ends := map[int]int{}
	for i, pr := range pairs {
		best[i], prev[i] = 0, -1
		if a[pr[0]].gap != b[pr[1]].gap {
			continue
		}
		best[i] = 1
		for k := range i {
			if best[k] > 0 && a[pairs[k][0]].gap == a[pr[0]].gap &&
				b[pairs[k][1]].at < b[pr[1]].at && best[k]+1 > best[i] {
				best[i], prev[i] = best[k]+1, k
			}
		}
		g := a[pr[0]].gap
		if e, ok := ends[g]; !ok || best[i] > best[e] {
			ends[g] = i
		}
	}
	for _, end := range ends {
		for i := end; i >= 0; i = prev[i] {
			keep[i] = true
		}
	}
	for i, pr := range pairs {
		oi, fj := a[pr[0]].at, b[pr[1]].at
		if keep[i] {
			start[oi], width[oi] = fj, 1
			pairOf[fj] = oi
		} else {
			e.want[fresh[fj]] = old[oi]
		}
	}
	return pairOf, ctx.Err()
}

// gaps numbers the gaps between exactly matched originals: each
// unmatched original's gap and each fresh block's, -1 for a matched one.
func gaps(start, width []int, nFresh int) (oldGap, freshGap []int) {
	oldGap, freshGap = make([]int, len(start)), make([]int, nFresh)
	g, prevEnd := 0, 0
	for i := range start {
		if start[i] < 0 {
			oldGap[i] = g
			continue
		}
		oldGap[i] = -1
		for j := prevEnd; j < start[i]; j++ {
			freshGap[j] = g
		}
		for j := start[i]; j < start[i]+width[i] && j < nFresh; j++ {
			freshGap[j] = -1
		}
		prevEnd, g = start[i]+width[i], g+1
	}
	for j := prevEnd; j < nFresh; j++ {
		freshGap[j] = g
	}
	return oldGap, freshGap
}

// replaced reports whether unmatched original i has new blocks in its run.
func (p editPlan) replaced(fresh []*Node, i int) bool {
	return !p.kept[i] && firstBlock(fresh, p.runs[i]) >= 0
}

// firstBlock is the first fresh index in run that is a block, not a
// marker, or -1.
func firstBlock(fresh []*Node, run [2]int) int {
	for j := run[0]; j < run[1]; j++ {
		if fresh[j].Type != tMarker {
			return j
		}
	}
	return -1
}

// pairRuns gives every original its run of fresh blocks. A matched
// original's run is its match. Between two matched originals, the
// unmatched originals with content pair in order with the new blocks
// there: the k-th takes the k-th new block, the last takes all the rest,
// and those past the new blocks get an empty run (deleted). Markers ride
// with the block before them. A gap with no unmatched originals is pure
// insertion; empty originals get empty runs.
func pairRuns(norm normalized, fresh []*Node, start, width []int) [][2]int {
	runs := make([][2]int, len(norm.blocks))
	prevEnd := 0
	var unmatched []int
	flush := func(gapEnd int) {
		var blocks []int
		for j := prevEnd; j < gapEnd; j++ {
			if fresh[j].Type != tMarker {
				blocks = append(blocks, j)
			}
		}
		cut := func(q int) int {
			switch {
			case q == 0:
				return prevEnd
			case q >= len(unmatched) || q >= len(blocks):
				return gapEnd
			}
			return blocks[q]
		}
		for q, i := range unmatched {
			runs[i] = [2]int{cut(q), cut(q + 1)}
			if q > 0 && q >= len(blocks) {
				runs[i] = [2]int{gapEnd, gapEnd}
			}
		}
		unmatched = unmatched[:0]
	}
	for i := range norm.blocks {
		switch {
		case start[i] >= 0:
			flush(start[i])
			prevEnd = start[i] + width[i]
			runs[i] = [2]int{start[i], prevEnd}
		case norm.empty(i):
			runs[i] = [2]int{prevEnd, prevEnd}
		default:
			unmatched = append(unmatched, i)
		}
	}
	flush(len(fresh))
	return runs
}

// matchBlocks keeps each original whose normalized blocks all match a
// consecutive run of fresh blocks, and records where that run starts. It
// aligns whole originals, not single nodes: sibling blocks that share
// nodes (two Section macros with the same markers) must not split one
// fresh run between them so that neither is kept. The objective is still
// the most normalized blocks matched.
func matchBlocks(ctx context.Context, norm normalized, fresh []*Node) (
	kept []bool, start []int, err error) {
	runs := make([][]string, len(norm.blocks))
	for i, ns := range norm.blocks {
		if len(ns) == 0 || norm.broken[i] {
			continue // empty and broken originals are placed, not matched
		}
		runs[i] = make([]string, len(ns))
		for k, n := range ns {
			runs[i][k] = key(n)
		}
	}
	bKeys := make([]string, len(fresh))
	for j, n := range fresh {
		bKeys[j] = key(n)
	}
	start, err = matchRuns(ctx, runs, bKeys, alignmentCells)
	if err != nil {
		return nil, nil, err
	}
	kept = make([]bool, len(norm.blocks))
	for i := range kept {
		kept[i] = start[i] >= 0
	}
	return kept, start, nil
}

// anchorEmpty gives each empty original the nearest original with
// content, preferring the one before it, or -1 when there is none.
func anchorEmpty(norm normalized) (anchor []int, hasContent bool) {
	anchor = make([]int, len(norm.blocks))
	previous := -1
	for i := range anchor {
		anchor[i] = -1
		if norm.empty(i) {
			anchor[i] = previous
		} else {
			previous, hasContent = i, true
		}
	}
	next := -1
	for i := len(anchor) - 1; i >= 0; i-- {
		if !norm.empty(i) {
			next = i
		} else if anchor[i] < 0 {
			anchor[i] = next
		}
	}
	return anchor, hasContent
}

// order lists the output in document order: for each original with
// content, the empty originals anchored before it, then the original
// itself (kept) or its run (edited), then the empty originals anchored
// after it. A gap where no original stood gets its new blocks in place.
func order(norm normalized, fresh []*Node, p editPlan,
	anchor []int) []entry {
	var out []entry
	anchored := make([][]int, len(norm.blocks)) // empties per anchor, in order
	for e := range norm.blocks {
		if norm.empty(e) && anchor[e] >= 0 && p.kept[e] {
			anchored[anchor[e]] = append(anchored[anchor[e]], e)
		}
	}
	spacers := func(c int, before bool) {
		for _, e := range anchored[c] {
			if (e < c) == before {
				out = append(out, entry{e, -1})
			}
		}
	}
	emitted := 0 // first fresh index not yet placed
	for c := range norm.blocks {
		if norm.empty(c) {
			continue
		}
		run := p.runs[c]
		if p.start[c] >= 0 || run[0] < run[1] {
			// New blocks in a gap no unmatched original claimed.
			for ; emitted < run[0]; emitted++ {
				out = append(out, entry{-1, emitted})
			}
		}
		spacers(c, true)
		if p.kept[c] {
			out = append(out, entry{c, -1})
		} else {
			for j := run[0]; j < run[1]; j++ {
				out = append(out, entry{-1, j})
			}
		}
		emitted = max(emitted, run[1])
		spacers(c, false)
	}
	for ; emitted < len(fresh); emitted++ {
		out = append(out, entry{-1, emitted})
	}
	for e := range norm.blocks {
		if norm.empty(e) && anchor[e] < 0 && p.kept[e] {
			out = append(out, entry{e, -1})
		}
	}
	return out
}

func joinWords(ws []string) string {
	switch len(ws) {
	case 0:
		return ""
	case 1:
		return ws[0]
	}
	return strings.Join(ws[:len(ws)-1], ", ") + " and " + ws[len(ws)-1]
}
