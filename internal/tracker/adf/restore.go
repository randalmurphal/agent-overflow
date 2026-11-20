package adf

import (
	"slices"
	"strings"
)

// restorable are the inline rich nodes Markdown writes as plain text (or
// a comment) that the parser can't rebuild: a status lozenge reads back
// as "[DONE]", a date as 2026-10-22, an emoji as its character, an inline
// macro as its marker.
var restorable = lazy[map[string]bool]{build: func() map[string]bool {
	return map[string]bool{"status": true, "date": true, "emoji": true,
		"inlineExtension": true}
}}

// restoreInline puts back, in the blocks taken from md, the original
// inline rich nodes that the edit would otherwise turn into plain text:
// where a fresh block's text (or an inline marker) shows exactly what
// ToMarkdown wrote for an original node the result no longer holds, that
// text becomes the original node again. Editing a paragraph around a
// lozenge keeps the lozenge.
//
// Identity is never guessed. A node comes back only from the original
// block the fresh one replaced for certain, or failing that from the
// originals in the gap the fresh block took the place of (a paragraph
// split in two, or reworded beside a new one), and only when every
// candidate there is alike (same attributes); never from another block,
// deleted or edited elsewhere, whose literal text happens to match. What
// can't come back stays text, and the edit invariant refuses the loss.
func (e *editor) restoreInline(texts *editTexts, out []*Node) {
	lost, _ := unkept(texts.orig, Doc(out...), texts.gone)
	var cands []rich
	for _, r := range lost {
		if restorable.get()[r.n.Type] {
			cands = append(cands, r)
		}
	}
	if len(cands) == 0 {
		return
	}
	pool := map[string][]*Node{} // rep -> unclaimed originals, for splitText
	var reps []string
	inside := map[*Node]*Node{} // lost node -> the paired original holding it
	sigOf := map[*Node]string{}
	for _, r := range cands {
		if len(pool[r.rep]) == 0 {
			reps = append(reps, r.rep)
		}
		pool[r.rep] = append(pool[r.rep], r.n)
		sigOf[r.n] = r.sig
	}
	size := map[*Node]int{}
	for _, o := range e.pairedOrig {
		if _, ok := size[o]; !ok {
			walk(o, func(*Node, *Node) { size[o]++ })
		}
	}
	for _, o := range e.pairedOrig {
		walk(o, func(n, _ *Node) {
			// The innermost paired original holding n is its own.
			if _, ok := sigOf[n]; ok && (inside[n] == nil || size[o] < size[inside[n]]) {
				inside[n] = o
			}
		})
	}
	// pick is the one original rep can stand for among ns, or nil when
	// they differ.
	pick := func(ns []*Node) *Node {
		if len(ns) == 0 {
			return nil
		}
		for _, n := range ns[1:] {
			if sigOf[n] != sigOf[ns[0]] {
				return nil
			}
		}
		return ns[0]
	}
	claim := func(rep string, m *Node) {
		pool[rep] = slices.DeleteFunc(pool[rep], func(n *Node) bool { return n == m })
	}
	var rec func(n, from *Node, gap map[*Node]bool)
	rec = func(n, from *Node, gap map[*Node]bool) {
		if n.Type == "codeBlock" {
			return
		}
		if isInlineParent(n.Type) {
			take := func(rep string) *Node {
				var mine, near []*Node
				for _, c := range pool[rep] {
					if from != nil && inside[c] == from {
						mine = append(mine, c)
					}
					if gap[c] {
						near = append(near, c)
					}
				}
				if len(mine) == 0 {
					mine = near
				}
				m := pick(mine)
				// Only where the parent may hold it, so the result stays valid.
				if m == nil || !allowed(n.Type, m.Type) {
					return nil
				}
				claim(rep, m)
				return cloneNode(m)
			}
			n.Content = restoreRun(n.Content, reps, pool, take)
			return
		}
		for _, c := range n.Content {
			rec(c, from, gap)
		}
	}
	for _, b := range e.used {
		var gap map[*Node]bool
		for _, o := range e.gapOrigs[b] {
			walk(o, func(n, _ *Node) {
				if _, ok := sigOf[n]; ok {
					if gap == nil {
						gap = map[*Node]bool{}
					}
					gap[n] = true
				}
			})
		}
		rec(b, e.pairedOrig[b], gap)
	}
}

// restoreRun rebuilds one inline run, splitting text at each rep that
// still has an original to give back.
func restoreRun(ns []*Node, reps []string, pool map[string][]*Node,
	take func(string) *Node) []*Node {
	var out []*Node
	for _, n := range ns {
		switch {
		case n.Type == tMarker:
			rep := "<!-- " + strings.TrimSpace(n.attrStr("text")) + " -->"
			if m := take(rep); m != nil {
				out = append(out, m)
				continue
			}
			out = append(out, n)
		case n.Type == "text" && !hasMark(n, "code"):
			out = append(out, splitText(n, reps, pool, take)...)
		default:
			out = append(out, n)
		}
	}
	return out
}

func splitText(n *Node, reps []string, pool map[string][]*Node,
	take func(string) *Node) []*Node {
	var out []*Node
	text := n.Text
	for {
		at, rep := -1, ""
		for _, r := range reps {
			if len(pool[r]) == 0 || strings.HasPrefix(r, "<!--") {
				continue
			}
			if i := strings.Index(text, r); i >= 0 && (at < 0 || i < at) {
				at, rep = i, r
			}
		}
		if at < 0 {
			break
		}
		m := take(rep)
		if m == nil {
			break
		}
		if at > 0 {
			out = append(out, textNode(text[:at], n.Marks...))
		}
		out = append(out, m)
		text = text[at+len(rep):]
	}
	if len(out) == 0 {
		return []*Node{n}
	}
	if text != "" {
		out = append(out, textNode(text, n.Marks...))
	}
	return out
}
