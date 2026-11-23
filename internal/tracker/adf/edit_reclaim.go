package adf

import (
	"context"
	"slices"
	"strings"
)

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
	c.st.warns = append(c.st.warns, Warning{Line: line, Loses: lossMarkers,
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
