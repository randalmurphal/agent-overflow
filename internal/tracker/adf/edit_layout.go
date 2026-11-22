package adf

import (
	"context"
	"maps"
)

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
