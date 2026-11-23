package adf

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// table keeps orig, a table the edit paired with a table in md: its
// attributes stay, a row or cell whose Markdown is unchanged stays as it
// was, and a changed cell takes the new content with the original cell's
// type, attributes and paragraph alignment, so header cells, widths,
// colors and lozenges in untouched cells survive. Rows pair by content
// (see pairRows); cells within a row in order; a row or cell md adds is
// new. With merged cells, rows stay in place: a moved row, or a write into
// a merged-over position, rebuilds the whole table from md, and the loss
// is refused.
func (e *editor) table(ctx context.Context, orig, fresh *Node, hi int) (
	*Node, []region, error) {
	n := *orig
	n.Attrs = maps.Clone(orig.Attrs)
	norm, err := normalize(ctx, []*Node{orig}, e.c.st)
	if err != nil {
		return nil, nil, err
	}
	reg := region{lo: e.c.lineFor(fresh), hi: hi}
	starts, spans := gridStarts(orig.Content)
	nb := norm.blocks[0]
	fallback := func() (*Node, []region, error) {
		// The Markdown grid doesn't map onto orig's cells: md's rows stand.
		n.Content = fresh.Content
		e.used = append(e.used, fresh.Content...)
		reg.used = true
		e.losses = append(e.losses, loss{orig: orig, line: reg.lo, replaced: true,
			flattens: true, part: "the table", repl: []*Node{&n}})
		return &n, []region{reg}, nil
	}
	if len(nb) != 1 || nb[0].Type != "table" || len(nb[0].Content) != len(orig.Content) {
		return fallback()
	}
	nt := nb[0]
	for i, r := range nt.Content {
		if len(r.Content) != len(starts[i]) {
			return fallback()
		}
	}
	rows, unsure, err := pairRows(ctx, nt.Content, orig.Content, fresh.Content)
	if err != nil {
		return nil, nil, err
	}
	if spans && !inPlace(orig.Content, nt.Content, fresh.Content, rows, starts) {
		return fallback()
	}
	n.Content = make([]*Node, len(fresh.Content))
	for j, fr := range fresh.Content {
		i := rows[j]
		switch {
		case i < 0:
			headerColumns(orig, fr)
			n.Content[j] = fr
			e.used = append(e.used, fr)
			reg.used = true
		case key(nt.Content[i]) == key(fr):
			n.Content[j] = orig.Content[i]
		default:
			row, err := e.mergeRow(ctx, orig.Content[i], nt.Content[i], fr, starts[i], spans)
			if err != nil {
				return nil, nil, err
			}
			n.Content[j] = row
			reg.used = true
		}
	}
	for _, i := range unsure {
		e.losses = append(e.losses, loss{orig: orig.Content[i], line: reg.lo, flattens: true,
			part: "the table row " + strconv.Quote(strings.Join(rowText(nt.Content[i]), " | ")),
			why:  "; more than one row in the Markdown could be it"})
	}
	return &n, []region{reg}, nil
}

func rowText(row *Node) []string {
	var out []string
	for _, c := range row.Content {
		out = append(out, strings.TrimSpace(renderCell(c)))
	}
	return out
}

// headerColumns makes header cells of fr's cells, a row md added, in
// the columns where every body row of orig, a table without merged cells
// in that column, starts a header cell: a header column stays one.
func headerColumns(orig, fr *Node) {
	if len(orig.Content) < 2 {
		return
	}
	for k, c := range fr.Content {
		if c.Type != "tableCell" {
			continue
		}
		header := true
		for _, row := range orig.Content[1:] {
			if k >= len(row.Content) || row.Content[k].Type != "tableHeader" ||
				span(row.Content[k], "colspan") != 1 {
				header = false
				break
			}
		}
		if header {
			c.Type = "tableHeader"
		}
	}
}

// inPlace reports whether a table with merged cells can take md's rows
// in place: every original row pairs by content with a row md writes, in
// order, writing nothing into a merged-over position, and a row md adds
// goes between rows no merged cell spans.
func inPlace(orig, norm, fresh []*Node, rows []int, starts [][]int) bool {
	joined := make([]bool, len(orig)) // row r is spanned into row r+1
	for r, row := range orig {
		for _, c := range row.Content {
			for d := 0; d < span(c, "rowspan")-1 && r+d < len(orig); d++ {
				joined[r+d] = true
			}
		}
	}
	next := 0 // the original row the next paired fresh row must be
	for j, fr := range fresh {
		i := rows[j]
		if i < 0 {
			if next > 0 && joined[next-1] {
				return false // a new row inside a merged cell
			}
			continue
		}
		nr := norm[i]
		if i != next || len(fr.Content) != len(nr.Content) {
			return false
		}
		for k, fc := range fr.Content {
			if starts[i][k] < 0 && key(nr.Content[k]) != key(fc) {
				return false
			}
		}
		next++
	}
	return next == len(norm)
}

// mergeRow edits orig, a table row, to fresh: norm is orig as md reads
// back, one cell per grid position, and starts maps each position to the
// orig cell that starts there (-1 where a merged cell covers it). Without
// merged cells, cells pair by content in order; with them in place (see
// inPlace), skipping covered positions.
func (e *editor) mergeRow(ctx context.Context, orig, norm, fresh *Node, starts []int,
	spans bool) (*Node, error) {
	row := *orig
	row.Content = nil
	cells := make([]int, len(fresh.Content))
	if spans {
		for k := range cells {
			cells[k] = k
		}
	} else {
		var err error
		if cells, err = pairInOrder(ctx, keysOf(norm.Content), keysOf(fresh.Content)); err != nil {
			return nil, err
		}
	}
	for k, fc := range fresh.Content {
		c := cells[k]
		switch {
		case c < 0:
			row.Content = append(row.Content, fc)
			e.used = append(e.used, fc)
		case starts[c] < 0:
		case key(norm.Content[c]) == key(fc):
			row.Content = append(row.Content, orig.Content[starts[c]])
		default:
			cell := e.mergeCell(orig.Content[starts[c]], norm.Content[c], fc, e.c.lineFor(fresh))
			row.Content = append(row.Content, cell)
		}
	}
	return &row, nil
}

// pairRows gives each fresh row the original row it pairs with, or -1,
// by content and never by guess. norm is orig as md reads back. Rows md
// shows unchanged pair first, in order (their longest common subsequence),
// then across order; where rows that read the same differ underneath
// (colors, widths), they pair only when every one of them stays, in order.
// The rest pair as containers do (see assign): by the cells they share,
// then the words, within a gap between unchanged rows or, sharing a cell,
// across gaps. unsure lists the originals left unpaired because more than
// one fresh row could be theirs.
func pairRows(ctx context.Context, norm, orig, fresh []*Node) (out, unsure []int, err error) {
	a, b := keysOf(norm), keysOf(fresh)
	match, err := lcs(ctx, a, b, alignmentCells)
	if err != nil {
		return nil, nil, err
	}
	under := map[string]map[string]bool{} // what rows reading k are underneath
	countA, countB, matched := map[string]int{}, map[string]int{}, map[string]int{}
	for i, k := range a {
		if under[k] == nil {
			under[k] = map[string]bool{}
		}
		under[k][key(orig[i])] = true
		countA[k]++
		if match[i] >= 0 {
			matched[k]++
		}
	}
	for _, k := range b {
		countB[k]++
	}
	alike := func(k string) bool { return len(under[k]) <= 1 }
	out = make([]int, len(b))
	for j := range out {
		out[j] = -1
	}
	for i, j := range match {
		k := a[i]
		if j >= 0 && !alike(k) && (countA[k] != countB[k] || matched[k] != countA[k]) {
			match[i] = -1
		}
		if match[i] >= 0 {
			out[match[i]] = i
		}
	}
	// Moved rows: an unchanged row elsewhere, when the originals reading
	// the same are alike, or there is one of each.
	freeA, freeB := map[string][]int{}, map[string][]int{}
	for i, k := range a {
		if match[i] < 0 {
			freeA[k] = append(freeA[k], i)
		}
	}
	for j, k := range b {
		if out[j] < 0 {
			freeB[k] = append(freeB[k], j)
		}
	}
	movedA := map[int]bool{}
	for k, js := range freeB {
		is := freeA[k]
		if len(is) == 0 {
			continue
		}
		if !alike(k) && (len(is) != 1 || len(js) != 1) {
			for _, i := range is {
				movedA[i] = true // not changed rows either: they read the same as md's
				unsure = append(unsure, i)
			}
			continue
		}
		for x := range min(len(is), len(js)) {
			out[js[x]], movedA[is[x]] = is[x], true
		}
	}
	// Changed rows, numbered by the gap between unchanged rows in place.
	var sa, sb []shaped
	g := 0
	for i := range a {
		if match[i] >= 0 {
			g++
		} else if !movedA[i] {
			sa = append(sa, shaped{i, "row", cellKeys(norm[i]), g, key(orig[i]), wordBag(norm[i])})
		}
	}
	g = 0
	for j := range b {
		switch {
		case out[j] >= 0 && match[out[j]] == j:
			g++
		case out[j] < 0:
			f := fresh[j]
			sb = append(sb, shaped{j, "row", cellKeys(f), g, key(f), wordBag(f)})
		}
	}
	if len(sa) == 0 || len(sb) == 0 || len(sa) > alignmentCells/len(sb) {
		slices.Sort(unsure)
		return out, unsure, nil
	}
	pairs, tied := assign(sa, sb)
	for _, pr := range pairs {
		out[sb[pr[1]].at] = sa[pr[0]].at
	}
	for _, x := range tied {
		unsure = append(unsure, sa[x].at)
	}
	slices.Sort(unsure)
	return out, unsure, nil
}

// cellKeys counts a row's cells with content by key: empty cells, alike
// in every row, say nothing about which row is which.
// A list item is first its own text: its first block counts twice, so
// two items that trade their text (or one's sublist goes to the other)
// pair by that text, not by the sublist.
func cellKeys(row *Node) map[string]int {
	keys := map[string]int{}
	for i, c := range row.Content {
		if !emptyCell(c) {
			keys[key(c)]++
			if i == 0 && row.Type == "listItem" {
				keys[key(c)]++
			}
		}
	}
	return keys
}

// emptyCell reports a cell with no text and no inline nodes.
func emptyCell(c *Node) bool {
	empty := true
	walk(c, func(n, _ *Node) {
		switch n.Type {
		case "tableCell", "tableHeader", "paragraph":
		default:
			empty = false
		}
	})
	return empty
}

// gridStarts maps each row's grid positions, as renderTable lays cells
// out, to the index of the cell starting there, or -1 where a merged cell
// covers it; spans reports any merged cell.
func gridStarts(rows []*Node) (starts [][]int, spans bool) {
	taken := make([][]bool, len(rows))
	starts = make([][]int, len(rows))
	cols := 0
	type at struct{ r, c, i int }
	var placed []at
	for r, row := range rows {
		c := 0
		for i, cell := range row.Content {
			for c < len(taken[r]) && taken[r][c] {
				c++
			}
			cs := span(cell, "colspan")
			rsp := min(span(cell, "rowspan"), len(rows)-r)
			spans = spans || cs > 1 || rsp > 1
			for dr := 0; dr < rsp; dr++ {
				for len(taken[r+dr]) < c+cs {
					taken[r+dr] = append(taken[r+dr], false)
				}
				for dc := 0; dc < cs; dc++ {
					taken[r+dr][c+dc] = true
				}
			}
			placed = append(placed, at{r, c, i})
			c += cs
		}
		cols = max(cols, c)
	}
	for _, t := range taken {
		cols = max(cols, len(t))
	}
	for r := range starts {
		starts[r] = make([]int, cols)
		for c := range starts[r] {
			starts[r][c] = -1
		}
	}
	for _, p := range placed {
		starts[p.r][p.c] = p.i
	}
	return starts, spans
}

// mergeCell is fresh's content in orig's cell: its type (a header stays a
// header), attributes, and a lone paragraph's marks (alignment).
func (e *editor) mergeCell(orig, norm, fresh *Node, line int) *Node {
	if n := patchText(e.c.ctx, orig, norm, fresh, e.c.st); n != nil {
		e.patched++
		return n
	}
	m := *fresh
	m.Type = orig.Type
	m.Attrs = maps.Clone(orig.Attrs)
	if len(orig.Content) == 1 && len(fresh.Content) == 1 &&
		orig.Content[0].Type == "paragraph" && fresh.Content[0].Type == "paragraph" &&
		len(orig.Content[0].Marks) > 0 {
		p := *fresh.Content[0]
		p.Marks = append([]Mark(nil), orig.Content[0].Marks...)
		m.Content = []*Node{&p}
	}
	e.used = append(e.used, &m)
	e.pairedOrig[&m] = orig
	e.carry = append(e.carry, carried{&m, orig, norm})
	// What else the cell held (text color, lists, a second paragraph)
	// Markdown can't carry: the edit loses it, and lossWarnings names it.
	text := strconv.Quote(strings.TrimSpace(renderCell(orig)))
	e.losses = append(e.losses, loss{orig: orig, line: line, replaced: true,
		flattens: true, part: "the table cell " + text, repl: []*Node{&m}})
	return &m
}

func keysOf(ns []*Node) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = key(n)
	}
	return out
}

// pairInOrder gives each b index the a index it pairs with, or -1: equal
// keys first (their longest common subsequence), then, between two such
// matches, the rest in order.
func pairInOrder(ctx context.Context, a, b []string) ([]int, error) {
	match, err := lcs(ctx, a, b, alignmentCells)
	if err != nil {
		return nil, err
	}
	out := make([]int, len(b))
	for j := range out {
		out[j] = -1
	}
	for i, j := range match {
		if j >= 0 {
			out[j] = i
		}
	}
	ai, bj := 0, 0
	for ai <= len(a) && bj <= len(b) {
		// the next match, or the ends
		na := ai
		for na < len(a) && match[na] < 0 {
			na++
		}
		nb := len(b)
		if na < len(a) {
			nb = match[na]
		}
		for x, y := ai, bj; x < na && y < nb; x, y = x+1, y+1 {
			out[y] = x
		}
		ai, bj = na+1, nb+1
	}
	return out, nil
}
