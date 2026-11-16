package adf

import (
	"regexp"
	"slices"
)

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
			out = append(out, Warning{Line: l.line, Loses: nodeLoss(l.orig),
				Msg: l.part + " had " + joinWords(feats) + ", which the edit would lose" + l.why})
		case l.unclosed > 0:
			out = append(out, Warning{Line: l.unclosed, Loses: lossMarkers,
				Msg: "<!-- " + macroName(l.orig) + " --> is never closed with <!-- /" +
					macroName(l.orig) + " -->; its macro is not kept"})
		case len(feats) == 0:
		case l.replaced && slices.Contains(feats, "a status lozenge") &&
			lozengeText(l.orig, l.repl) != "":
			out = append(out, Warning{Line: l.line, Loses: lossMarkers,
				Msg: lozengeText(l.orig, l.repl) + " is plain text now: a lozenge whose " +
					"text changes " +
					"along with other formatting in its block can't be kept; change the " +
					"lozenge's text by itself"})
		case l.replaced:
			out = append(out, Warning{Line: l.line,
				Msg: "replaced a block that had " + joinWords(feats) +
					"; those are now plain", Loses: flattenLoss(l)})
		default:
			out = append(out, Warning{
				Msg: "removed a block that had " + joinWords(feats)})
		}
	}
	return slices.Compact(out)
}

// flattenLoss is the loss of l, a replaced block, when it flattened a
// container (see loss.flattens): every such block is or holds a layout, a
// macro, a panel, a table or an expand, so the set is never empty.
func flattenLoss(l loss) LossSet {
	if !l.flattens {
		return 0
	}
	return nodeLoss(l.orig)
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
