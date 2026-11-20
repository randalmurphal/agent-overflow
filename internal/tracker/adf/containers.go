package adf

import (
	"strconv"
	"strings"
)

// Containers Markdown has no syntax for are written as their children
// between comment markers, each marker its own HTML block:
//
//	<!-- layout -->                 a column layout, around its columns
//	<!-- column 50% --> ... <!-- /column -->
//	<!-- /layout -->
//	<!-- macro: KEY --> ... <!-- /macro: KEY -->   a macro with a body
//	<!-- panel: TYPE --> ... <!-- /panel -->        a custom panel
//
// A macro marker with no close is a macro without a body. The parser
// folds a balanced pair into the container (with what the markers say:
// column widths, the macro key, the panel type), and Edit keeps the
// original container, editing only its children, while both markers
// survive. A marker left open or closed out of turn is unbalanced: its
// container is not built, and a Flattens warning names it.

// containerKind is the kind of an open or close marker, "" for any other
// comment.
type marker struct {
	kind  string // "layout", "column", "macro", "panel", "details"
	close bool
	arg   string // column width ("50%"), macro key, panel type
	id    string // a macro's ID (see macroName), "" when written without
}

// macroIDRe is a macro marker's ID: four hex digits after the key.
var macroIDRe = lazyRegexp{pattern: `^[0-9a-f]{4}$`}

func parseMarker(n *Node) (marker, bool) {
	switch n.Type {
	case tDetailsOpen:
		return marker{kind: "details"}, true
	case tDetailsClose:
		return marker{kind: "details", close: true}, true
	case tMarker:
	default:
		return marker{}, false
	}
	t := n.attrStr("text")
	m := marker{close: strings.HasPrefix(t, "/")}
	t = strings.TrimPrefix(t, "/")
	name, arg, _ := strings.Cut(t, " ")
	switch name = strings.TrimSuffix(name, ":"); name {
	case "layout":
		if arg != "" {
			return marker{}, false
		}
	case "column":
		if m.close && arg != "" {
			return marker{}, false
		}
	case "macro", "panel":
		if !m.close && arg == "" {
			return marker{}, false
		}
		if !strings.HasPrefix(t, name+":") && arg != "" {
			return marker{}, false
		}
	default:
		return marker{}, false
	}
	m.kind, m.arg = name, strings.TrimSpace(arg)
	if name == "macro" {
		if f := strings.Fields(m.arg); len(f) >= 2 && macroIDRe.get().MatchString(f[len(f)-1]) {
			m.id = f[len(f)-1]
			m.arg = strings.Join(f[:len(f)-1], " ")
		}
	}
	return m, true
}

// markerText is how a marker is written, for messages.
func markerText(m marker) string {
	if m.kind == "details" {
		if m.close {
			return "</details>"
		}
		return "<details>"
	}
	s := m.kind
	if (m.kind == "macro" || m.kind == "panel") && m.arg != "" {
		s += ":"
	}
	if m.arg != "" {
		s += " " + m.arg
	}
	if m.id != "" {
		s += " " + m.id
	}
	if m.close {
		s = "/" + s
	}
	return "<!-- " + s + " -->"
}

// bodiedMacros finds which macro open markers in ns have a close: a
// close binds to the nearest open with its key (a bare /macro to the
// nearest open), and opens it passes over are macros without a body.
func bodiedMacros(ns []*Node) map[*Node]bool {
	bodied := map[*Node]bool{}
	type open struct {
		n       *Node
		key, id string
	}
	var stack []open
	for _, n := range ns {
		m, ok := parseMarker(n)
		if !ok || m.kind != "macro" {
			continue
		}
		if !m.close {
			stack = append(stack, open{n, m.arg, m.id})
			continue
		}
		for k := len(stack) - 1; k >= 0; k-- {
			if m.arg == "" || stack[k].key == m.arg && (m.id == "" || stack[k].id == m.id) {
				bodied[stack[k].n] = true
				bodied[n] = true
				stack = stack[:k]
				break
			}
		}
	}
	return bodied
}

// groupDetails folds <details> ... </details> runs into expand nodes and
// balanced container markers into their containers. A <details> left
// open or closed alone is an error, as before; an unbalanced container
// marker stays a plain marker, its content stays where it is, and a
// Flattens warning names it.
func (c *converter) groupDetails(ns []*Node) ([]*Node, error) {
	type frame struct {
		open *Node
		m    marker
		kids []*Node
	}
	bodied := bodiedMacros(ns)
	stack := []frame{{}}
	// unwind closes frames above depth as unbalanced: their open marker
	// and children go to the frame below.
	unwind := func(depth int) error {
		for len(stack) > depth {
			f := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if f.m.kind == "details" {
				return errAt(c.lineFor(f.open),
					"<details> is never closed with </details>")
			}
			c.unbalanced(f.open, f.m, "is never closed")
			parent := &stack[len(stack)-1]
			parent.kids = append(append(parent.kids, f.open), unwrapColumns(f.kids)...)
		}
		return nil
	}
	for _, n := range ns {
		top := &stack[len(stack)-1]
		m, ok := parseMarker(n)
		if ok && m.kind == "macro" && !bodied[n] {
			ok = false // a macro without a body
		}
		switch {
		case !ok:
			top.kids = append(top.kids, n)
		case !m.close:
			stack = append(stack, frame{open: n, m: m})
		default:
			at := -1
			for k := len(stack) - 1; k > 0; k-- {
				if stack[k].m.kind == m.kind {
					at = k
					break
				}
			}
			if at < 0 {
				if m.kind == "details" {
					return nil, errAt(c.lineFor(n),
						"</details> without a matching <details>")
				}
				c.unbalanced(n, m, "closes nothing")
				top.kids = append(top.kids, n)
				continue
			}
			if err := unwind(at + 1); err != nil {
				return nil, err
			}
			f := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			parent := &stack[len(stack)-1]
			parent.kids = append(parent.kids,
				c.buildContainer(f.open, f.m, n, f.kids, parent.m.kind)...)
		}
	}
	if err := unwind(1); err != nil {
		return nil, err
	}
	return stack[0].kids, nil
}

// unwrapColumns replaces the columns among a flattened layout's children
// with their blocks.
func unwrapColumns(kids []*Node) []*Node {
	var out []*Node
	for _, k := range kids {
		if k.Type == "layoutColumn" {
			out = append(out, k.Content...)
			continue
		}
		out = append(out, k)
	}
	return out
}

// unbalanced warns that marker n has no partner.
func (c *converter) unbalanced(n *Node, m marker, what string) {
	partner := m
	partner.close = !m.close
	if partner.close {
		partner.arg = ""
		if m.kind == "macro" {
			partner.arg, partner.id = m.arg, m.id
		}
	}
	msg := markerText(m) + " " + what
	if what == "is never closed" {
		msg += " with " + markerText(partner)
	}
	c.st.warns = append(c.st.warns, Warning{Line: c.lineFor(n),
		Msg: msg + "; its container is not kept", Loses: markerLoss(m)})
}

// buildContainer makes the container a balanced marker pair holds. Its
// attributes are only what the markers say; Edit takes the rest from the
// original. Content that can't form the container (a layout holding more
// than columns) leaves the markers plain, with a warning.
func (c *converter) buildContainer(open *Node, m marker, closer *Node,
	kids []*Node, parentKind string) []*Node {
	line := c.lineFor(open)
	flat := func(why string) []*Node {
		c.st.warns = append(c.st.warns, Warning{Line: line,
			Msg: markerText(m) + " " + why + "; its container is not kept", Loses: markerLoss(m)})
		return append(append([]*Node{open}, unwrapColumns(kids)...), closer)
	}
	var n *Node
	switch m.kind {
	case "details":
		n = &Node{Type: "expand", Attrs: open.Attrs, Content: kids}
	case "column":
		if parentKind != "layout" {
			return flat("is outside a <!-- layout -->")
		}
		n = &Node{Type: "layoutColumn", Content: kids}
		if w, ok := parseWidth(m.arg); ok {
			n.Attrs = map[string]any{"width": w}
		} else if m.arg != "" {
			return flat("has a width that isn't a percentage like 50%")
		}
	case "layout":
		var cols []*Node
		for _, k := range kids {
			switch {
			case k.Type == "layoutColumn":
				cols = append(cols, k)
			case k.Type != tMarker:
				return flat("holds content outside its columns")
			}
		}
		if len(cols) == 0 || len(cols) > 3 {
			return flat("needs one to three columns")
		}
		for _, col := range cols {
			if col.attr("width") == nil {
				col.Attrs = map[string]any{"width": 100 / float64(len(cols))}
			}
		}
		n = &Node{Type: "layoutSection", Content: cols}
		c.st.built[n] = true
	case "macro":
		n = &Node{Type: "bodiedExtension", Attrs: map[string]any{"extensionKey": m.arg,
			attrMacroID: m.id},
			Content: kids}
		c.st.built[n] = true
	case "panel":
		n = &Node{Type: "panel", Attrs: map[string]any{"panelType": m.arg},
			Content: kids}
		c.st.built[n] = true
	}
	return []*Node{c.at(n, line)}
}

// parseWidth reads a column width marker argument like "50%".
func parseWidth(s string) (float64, bool) {
	v, ok := strings.CutSuffix(s, "%")
	if !ok {
		return 0, false
	}
	w, err := strconv.ParseFloat(v, 64)
	return w, err == nil && w > 0 && w <= 100
}

// shape is what pairs an original container with the one its markers
// parse back to when its children changed: the kind and what the markers
// say about it besides the children. "" for anything else.
func (c *converter) shape(n *Node, fresh bool) string {
	switch n.Type {
	case "layoutSection":
		return "layout"
	case "bodiedExtension":
		return "macro|" + commentSafe(n.attrStr("extensionKey")) + "|" + macroID(n)
	case "panel":
		if fresh && c.st.built[n] || !fresh && customPanel(n) {
			return "panel*|" + commentSafe(n.attrStr("panelType"))
		}
		return "panel|" + n.attrStr("panelType")
	case "expand", "nestedExpand":
		return "expand"
	case "table":
		return "table"
	}
	return ""
}
