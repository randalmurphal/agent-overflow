package adf

import "strings"

// fit reshapes n's subtree to ADF's content rules, top down. Each child a
// parent can't hold is lowered to something it can (a heading becomes a
// bold paragraph, a nested quote is unwrapped) with a warning, or is an
// error when nothing sensible fits (a table in a list item).
func (c *converter) fit(n *Node) error {
	if specs[n.Type].content == nil || isInlineParent(n.Type) {
		return nil
	}
	var out []*Node
	for _, ch := range n.Content {
		ns, err := c.fitChild(n, ch)
		if err != nil {
			return err
		}
		out = append(out, ns...)
	}
	n.Content = out
	for _, ch := range n.Content {
		if err := c.fit(ch); err != nil {
			return err
		}
	}
	return nil
}

func isInlineParent(t string) bool {
	switch t {
	case "paragraph", "heading", "taskItem", "decisionItem", "caption",
		"codeBlock":
		return true
	}
	return false
}

func (c *converter) fitChild(parent, ch *Node) ([]*Node, error) {
	p := parent.Type
	if ch.Type == tMarker || allowed(p, ch.Type) {
		return []*Node{ch}, nil
	}
	line := c.lineFor(ch)
	if line == 0 {
		line = c.lineFor(parent)
	}
	where := humanType(p)
	refit := func(ns []*Node) ([]*Node, error) {
		var out []*Node
		for _, x := range ns {
			got, err := c.fitChild(parent, x)
			if err != nil {
				return nil, err
			}
			out = append(out, got...)
		}
		return out, nil
	}
	switch ch.Type {
	case "heading":
		c.warn(line, "a heading can't go in %s; made it bold text", where)
		para := c.at(&Node{Type: "paragraph", Content: embolden(ch.Content)},
			line)
		return refit([]*Node{para})
	case "expand", "nestedExpand":
		if allowed(p, "nestedExpand") {
			if ch.Type == "expand" {
				c.warn(line, "a <details> section inside another became "+
					"a nested expand")
			}
			ch.Type = "nestedExpand"
			if ch.Attrs == nil {
				ch.Attrs = map[string]any{"title": ""}
			}
			return []*Node{ch}, nil
		}
		if allowed(p, "expand") {
			ch.Type = "expand"
			return []*Node{ch}, nil
		}
		c.warn(line, "a <details> section can't go in %s; kept its "+
			"content without the collapsible", where)
		var ns []*Node
		if title := ch.attrStr("title"); title != "" {
			ns = append(ns, c.at(&Node{Type: "paragraph",
				Content: []*Node{textNode(title, Mark{Type: "strong"})}},
				line))
		}
		return refit(append(ns, ch.Content...))
	case "blockquote", "panel":
		c.warn(line, "%s can't go in %s; kept its content without it",
			article(humanType(ch.Type)), where)
		return refit(ch.Content)
	case "rule":
		c.warn(line, "a horizontal rule can't go in %s; dropped it", where)
		return nil, nil
	case "taskList":
		if allowed(p, "bulletList") {
			c.warn(line, "a checklist can't go in %s; made it a bullet "+
				"list", where)
			return refit([]*Node{c.taskToBullets(ch)})
		}
	case "table":
		return nil, errAt(line, "a table can't go in %s", where)
	}
	return nil, errAt(line, "%s can't go in %s", article(ch.Type), where)
}

func article(s string) string {
	if strings.HasPrefix(s, "a ") || strings.HasPrefix(s, "an ") {
		return s
	}
	return "a " + s
}

// embolden adds strong to every text node that can take it.
func embolden(ns []*Node) []*Node {
	out := make([]*Node, len(ns))
	for i, n := range ns {
		if n.Type == "text" && !hasMark(n, "code") && !hasMark(n, "strong") {
			cp := *n
			cp.Marks = append(append([]Mark(nil), n.Marks...),
				Mark{Type: "strong"})
			sortMarks(cp.Marks)
			n = &cp
		}
		out[i] = n
	}
	return out
}

func (c *converter) taskToBullets(tl *Node) *Node {
	bl := &Node{Type: "bulletList"}
	for _, it := range tl.Content {
		if it.Type == "taskList" {
			if k := len(bl.Content); k > 0 {
				prev := bl.Content[k-1]
				prev.Content = append(prev.Content, c.taskToBullets(it))
			}
			continue
		}
		para := &Node{Type: "paragraph", Content: mergeText(append(
			[]*Node{textNode(checkbox(it) + " ")}, it.Content...))}
		bl.Content = append(bl.Content, &Node{Type: "listItem",
			Content: []*Node{para}})
	}
	return bl
}
