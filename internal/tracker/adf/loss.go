package adf

import (
	"slices"
	"strings"
)

// LossKind is a way the Markdown writes content it cannot express. It
// decides how an edit keeps that content.
type LossKind string

const (
	// LossMarkers is every lost content but an expand: what is written
	// with comment markers or as [TEXT] (a macro, a custom panel, a column
	// layout, a status lozenge) and the other content nodes Markdown shows.
	LossMarkers LossKind = "markers"
	// LossDetails is an expand, written as <details><summary>TITLE</summary>
	// and </details>.
	LossDetails LossKind = "details"
)

// Keep says how to edit around content of kind k so the edit keeps it,
// and how to remove that content on purpose.
func (k LossKind) Keep() string {
	switch k {
	case LossMarkers:
		return "keep each container's <!-- open --> and <!-- /close --> markers " +
			"and edit between them; to drop a macro or lozenge, delete its marker or [TEXT]"
	case LossDetails:
		return "keep each <details><summary>TITLE</summary> and its </details>, " +
			"each on a line of its own, and edit between them; to drop an expand, " +
			"delete both tags"
	}
	return ""
}

// LossSet is the set of LossKinds a warning's lost content is written
// with, empty when the warning loses no content. A block can lose both.
type LossSet uint8

const (
	lossMarkers LossSet = 1 << iota
	lossDetails
)

// Kinds lists the kinds in s, LossMarkers before LossDetails.
func (s LossSet) Kinds() []LossKind {
	var out []LossKind
	if s&lossMarkers != 0 {
		out = append(out, LossMarkers)
	}
	if s&lossDetails != 0 {
		out = append(out, LossDetails)
	}
	return out
}

// LossError is an edit that would lose content the user did not remove:
// a container flattened because its markers were removed or left
// unbalanced, or a content node the Markdown still shows. Lost dress (a
// color, an alignment, a width, an inline comment) never makes one.
type LossError struct {
	// Lost are the edit's Flattens warnings, in the order Edit returned them.
	Lost []Warning
	// Kinds are the ways the lost content is written, LossMarkers before
	// LossDetails; each one's Keep says how an edit keeps it. A clear's
	// loss has none: the clear removes that content on purpose.
	Kinds []LossKind
}

func (e *LossError) Error() string {
	lost := make([]string, len(e.Lost))
	for i, w := range e.Lost {
		lost[i] = w.String()
	}
	return "the edit would lose what Markdown can't show: " + strings.Join(lost, "; ")
}

// CheckLoss reports whether an edit may be written. ws are the warnings
// Edit returned and accepted is the user's explicit acceptance of the
// loss. It returns a *LossError when a warning flattens content and the
// user has not accepted, and nil otherwise. A caller that writes an
// edited document runs it and writes nothing when it fails.
func CheckLoss(ws []Warning, accepted bool) error {
	var lost []Warning
	var kinds LossSet
	for _, w := range ws {
		if w.Flattens() {
			lost = append(lost, w)
			if !w.clear {
				kinds |= w.Loses
			}
		}
	}
	if len(lost) == 0 || accepted {
		return nil
	}
	return &LossError{Lost: lost, Kinds: kinds.Kinds()}
}

// markerLoss is the loss of the container marker m stands for: details
// for <details>, markers for a comment marker.
func markerLoss(m marker) LossSet {
	if m.kind == "details" {
		return lossDetails
	}
	return lossMarkers
}

// nodeLoss is the loss of n's subtree, a block or cell an edit flattens
// or rebuilds: details for an expand in it, and markers for any other
// container (a layout, a macro, a panel, a table) or any node or mark
// whose loss is more than dress (see cosmeticPhrases).
func nodeLoss(n *Node) LossSet {
	var s LossSet
	content := func(p string, _ bool) {
		if !cosmeticPhrases.get()[p] && p != "expand nesting" {
			s |= lossMarkers
		}
	}
	walk(n, func(m, parent *Node) {
		switch m.Type {
		case "expand", "nestedExpand":
			s |= lossDetails
		case "layoutSection", "bodiedExtension", "extension", "panel", "table":
			s |= lossMarkers
		}
		nodeFeatures(m, parent, content, true)
		for _, mk := range m.Marks {
			markFeatures(mk, m, content)
		}
	})
	return s
}

// ClearLoss is what a clear of doc loses that Markdown shows only
// simplified: one warning naming the rich nodes doc holds, which loses
// their content (see nodeLoss), so CheckLoss refuses the clear until the
// loss is accepted, and names no way to keep it (no Kinds): the clear
// removes that content on purpose. A clear removes every node's Markdown,
// which explains no loss here: the person or agent that empties a field
// sees only what Markdown shows of it. It is nil when doc holds nothing
// Markdown shows only simplified, or only dress.
func ClearLoss(doc *Node) []Warning {
	loses := nodeLoss(doc)
	if loses == 0 {
		return nil
	}
	var names []string
	for _, r := range inventory(doc) {
		if !r.cosmetic && !slices.Contains(names, r.label) {
			names = append(names, r.label)
		}
	}
	msg := "the clear removes content Markdown shows only simplified"
	if len(names) > 0 {
		msg += ": " + strings.Join(names, ", ")
	}
	return []Warning{{Msg: msg, Loses: loses, clear: true}}
}

// richLoss is the loss of r, a rich node an edit drops: none for dress,
// details for an expand, markers for anything else.
func richLoss(r rich) LossSet {
	switch {
	case r.cosmetic:
		return 0
	case r.n.Type == "expand" || r.n.Type == "nestedExpand":
		return lossDetails
	}
	return lossMarkers
}
