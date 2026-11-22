package adf

import (
	"slices"
	"strings"

	gast "github.com/yuin/goldmark/ast"
	east "github.com/yuin/goldmark/extension/ast"
)

// blocks converts the block children of n, grouping <details> sections.
func (c *converter) blocks(n gast.Node) ([]*Node, error) {
	var out []*Node
	for ch := n.FirstChild(); ch != nil; ch = ch.NextSibling() {
		ns, err := c.block(ch)
		if err != nil {
			return nil, err
		}
		out = append(out, ns...)
	}
	return c.groupDetails(out)
}

func (c *converter) block(n gast.Node) ([]*Node, error) {
	line := c.lineOf(n)
	switch n := n.(type) {
	case *gast.Paragraph, *gast.TextBlock:
		return c.paragraph(n, line)
	case *gast.Heading:
		inl, err := c.inlines(n, 0)
		if err != nil {
			return nil, err
		}
		if hasImage(inl) {
			return nil, errAt(line, "images can't go in a heading")
		}
		h := &Node{Type: "heading", Attrs: map[string]any{
			"level": n.Level}, Content: inl}
		return []*Node{c.at(h, line)}, nil
	case *gast.ThematicBreak:
		return []*Node{c.at(&Node{Type: "rule"}, line)}, nil
	case *gast.FencedCodeBlock:
		cb := c.codeBlock(n)
		if lang := string(n.Language(c.src)); lang != "" {
			cb.Attrs = map[string]any{"language": unescape([]byte(lang))}
		}
		return []*Node{c.at(cb, line)}, nil
	case *gast.CodeBlock:
		return []*Node{c.at(c.codeBlock(n), line)}, nil
	case *gast.Blockquote:
		return c.quote(n, line)
	case *gast.List:
		return c.list(n)
	case *gast.HTMLBlock:
		return c.htmlBlock(n)
	case *east.Table:
		return c.table(n, line)
	case *east.FootnoteList:
		return c.footnotes(n, line)
	case *gast.LinkReferenceDefinition:
		return nil, nil // goldmark already resolved the references
	}
	return nil, errAt(line, "unsupported Markdown (%s)", n.Kind())
}

func hasImage(inl []*Node) bool {
	for _, n := range inl {
		if n.Type == tImage {
			return true
		}
	}
	return false
}

// paragraph converts a paragraph, splitting it around images: ADF images
// are blocks of their own.
func (c *converter) paragraph(n gast.Node, line int) ([]*Node, error) {
	inl, err := c.inlines(n, 0)
	if err != nil {
		return nil, err
	}
	return c.splitImages(inl, line), nil
}

func (c *converter) splitImages(inl []*Node, line int) []*Node {
	var out, cur []*Node
	afterImage := false
	flush := func(beforeImage bool) {
		// A line break beside an image only separated the two: it goes,
		// or every edit of a cell holding both would add a blank line.
		cur = trimBreaks(cur)
		for trimmed := false; !trimmed; {
			cur, trimmed = trimInline(cur), true
			for afterImage && len(cur) > 0 && cur[0].Type == "hardBreak" {
				cur, trimmed = cur[1:], false
			}
			for beforeImage && len(cur) > 0 && cur[len(cur)-1].Type == "hardBreak" {
				cur, trimmed = cur[:len(cur)-1], false
			}
		}
		if len(cur) > 0 {
			out = append(out, c.at(&Node{Type: "paragraph", Content: cur},
				line))
		}
		cur = nil
	}
	for _, x := range inl {
		if x.Type == tImage {
			flush(true)
			out = append(out, c.at(x.Content[0], line))
			afterImage = true
			continue
		}
		cur = append(cur, x)
	}
	flush(false)
	return out
}

// cellItems gives each line of a cell's inline run that starts as a list
// item does ("- ", "1. ") one space after its marker: a cell shows a list
// as such lines, and the spaces after a marker, which a rendered page
// collapses, are no part of the item's text.
func cellItems(ns []*Node) []*Node {
	out, cloned := ns, false
	for i, n := range ns {
		if n.Type != "text" || hasMark(n, "code") || i > 0 && ns[i-1].Type != "hardBreak" {
			continue
		}
		m := cellItemRe.get().FindStringSubmatchIndex(n.Text)
		if m == nil {
			continue
		}
		if !cloned {
			out, cloned = slices.Clone(ns), true
		}
		c := *n
		c.Text = n.Text[:m[3]] + " " + n.Text[m[1]:]
		out[i] = &c
	}
	return out
}

var cellItemRe = lazyRegexp{pattern: `^([-*+]|\d{1,9}[.)])  +`}

// trimBreaks drops the spaces beside each line break, which a rendered
// page hides: "a <br> b" is "a", a break, "b".
func trimBreaks(ns []*Node) []*Node {
	var out []*Node
	for i, n := range ns {
		if n.Type == "text" && !hasMark(n, "code") {
			t := n.Text
			if i > 0 && ns[i-1].Type == "hardBreak" {
				t = strings.TrimLeft(t, " \t")
			}
			if i+1 < len(ns) && ns[i+1].Type == "hardBreak" {
				t = strings.TrimRight(t, " \t")
			}
			if t == "" {
				continue
			}
			if t != n.Text {
				c := *n
				c.Text = t
				n = &c
			}
		}
		out = append(out, n)
	}
	return out
}

// trimInline drops whitespace at the edges of an inline run, except
// inside inline code, where it is content.
func trimInline(ns []*Node) []*Node {
	for len(ns) > 0 && ns[0].Type == "text" && !hasMark(ns[0], "code") {
		t := strings.TrimLeft(ns[0].Text, " \t\n")
		if t != "" {
			c := *ns[0]
			c.Text = t
			ns[0] = &c
			break
		}
		ns = ns[1:]
	}
	for len(ns) > 0 && ns[len(ns)-1].Type == "text" &&
		!hasMark(ns[len(ns)-1], "code") {
		last := ns[len(ns)-1]
		t := strings.TrimRight(last.Text, " \t\n")
		if t != "" {
			c := *last
			c.Text = t
			ns[len(ns)-1] = &c
			break
		}
		ns = ns[:len(ns)-1]
	}
	return ns
}

func (c *converter) codeBlock(n gast.Node) *Node {
	var b strings.Builder
	lines := n.Lines()
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		b.Write(seg.Value(c.src))
	}
	s := strings.TrimSuffix(b.String(), "\n")
	cb := &Node{Type: "codeBlock"}
	if s != "" {
		cb.Content = []*Node{textNode(s)}
	}
	return cb
}

var alertRe = lazyRegexp{pattern: `^\[!([A-Za-z]+)\][ \t]*`}

var alertTypes = lazy[map[string]string]{build: func() map[string]string {
	return map[string]string{
		"INFO": "info", "NOTE": "note", "TIP": "tip", "WARNING": "warning",
		"ERROR": "error", "SUCCESS": "success", "IMPORTANT": "note",
		"CAUTION": "error",
	}
}}

// quote converts a blockquote, or a GitHub alert (> [!NOTE]) to a panel.
func (c *converter) quote(q *gast.Blockquote, line int) ([]*Node, error) {
	p, ok := q.FirstChild().(*gast.Paragraph)
	if ok && p.Lines().Len() > 0 {
		seg := p.Lines().At(0)
		raw := seg.Value(c.src)
		if m := alertRe.get().FindSubmatchIndex(raw); m != nil {
			name := strings.ToUpper(string(raw[m[2]:m[3]]))
			typ, ok := alertTypes.get()[name]
			if !ok {
				return nil, errAt(line, "unknown alert type [!%s]; use "+
					"NOTE, INFO, TIP, WARNING, ERROR or SUCCESS", name)
			}
			return c.panel(q, p, typ, seg.Start+m[1], line)
		}
	}
	kids, err := c.blocks(q)
	if err != nil {
		return nil, err
	}
	return []*Node{c.at(&Node{Type: "blockquote", Content: kids}, line)},
		nil
}

func (c *converter) panel(q *gast.Blockquote, first *gast.Paragraph,
	typ string, cut, line int) ([]*Node, error) {
	inl, err := c.inlines(first, cut)
	if err != nil {
		return nil, err
	}
	kids := c.splitImages(inl, line)
	for ch := first.NextSibling(); ch != nil; ch = ch.NextSibling() {
		ns, err := c.block(ch)
		if err != nil {
			return nil, err
		}
		kids = append(kids, ns...)
	}
	kids, err = c.groupDetails(kids)
	if err != nil {
		return nil, err
	}
	pn := &Node{Type: "panel", Attrs: map[string]any{"panelType": typ},
		Content: kids}
	return []*Node{c.at(pn, line)}, nil
}

func taskBox(item gast.Node) *east.TaskCheckBox {
	tb := item.FirstChild()
	if tb == nil {
		return nil
	}
	box, _ := tb.FirstChild().(*east.TaskCheckBox)
	return box
}

// list converts a list. Runs of task items become taskLists, since ADF
// can't mix checkboxes into a bullet list.
func (c *converter) list(l *gast.List) ([]*Node, error) {
	var out []*Node
	var cur *Node
	curTask := false
	index := 0
	for item := l.FirstChild(); item != nil; item = item.NextSibling() {
		line := c.lineOf(item)
		isTask := taskBox(item) != nil
		if cur == nil || isTask != curTask {
			cur = c.newList(l, isTask, index, line)
			curTask = isTask
			out = append(out, cur)
		}
		index++
		if isTask {
			ns, err := c.taskItem(item, line)
			if err != nil {
				return nil, err
			}
			cur.Content = append(cur.Content, ns...)
			continue
		}
		kids, err := c.blocks(item)
		if err != nil {
			return nil, err
		}
		li := c.at(&Node{Type: "listItem", Content: kids}, line)
		cur.Content = append(cur.Content, li)
	}
	return out, nil
}

func (c *converter) newList(l *gast.List, task bool, index,
	line int) *Node {
	switch {
	case task:
		if l.IsOrdered() {
			c.warn(line, "a numbered checklist became a plain checklist")
		}
		return c.at(&Node{Type: "taskList",
			Attrs: map[string]any{"localId": c.newID()}}, line)
	case l.IsOrdered():
		return c.at(&Node{Type: "orderedList",
			Attrs: map[string]any{"order": l.Start + index}}, line)
	}
	return c.at(&Node{Type: "bulletList"}, line)
}

// taskItem converts a checklist item and any nested checklists, which ADF
// keeps as siblings of the item.
func (c *converter) taskItem(item gast.Node, line int) ([]*Node, error) {
	box := taskBox(item)
	state := "TODO"
	if box.IsChecked {
		state = "DONE"
	}
	inl, err := c.inlines(item.FirstChild(), 0)
	if err != nil {
		return nil, err
	}
	if hasImage(inl) {
		return nil, errAt(line, "images can't go in a checklist item")
	}
	ti := c.at(&Node{Type: "taskItem", Attrs: map[string]any{
		"localId": c.newID(), "state": state,
	}, Content: trimInline(inl)}, line)
	out := []*Node{ti}
	for ch := item.FirstChild().NextSibling(); ch != nil; ch = ch.NextSibling() {
		l, ok := ch.(*gast.List)
		if !ok || !allTasks(l) {
			return nil, errAt(c.lineOf(ch), "a checklist item can only "+
				"hold text and nested checklist items")
		}
		ns, err := c.list(l)
		if err != nil {
			return nil, err
		}
		out = append(out, ns...)
	}
	return out, nil
}

func allTasks(l *gast.List) bool {
	for item := l.FirstChild(); item != nil; item = item.NextSibling() {
		if taskBox(item) == nil {
			return false
		}
	}
	return true
}

func (c *converter) table(t *east.Table, line int) ([]*Node, error) {
	tbl := c.at(&Node{Type: "table"}, line)
	cols := 0
	if h := t.FirstChild(); h != nil {
		cols = h.ChildCount()
	}
	for row := t.FirstChild(); row != nil; row = row.NextSibling() {
		// GFM drops cells past the header's count without a word.
		if l := c.lineOf(row); l > 0 {
			if n := rowCells(c.lineText(l)); n > cols {
				return nil, errAt(l, "table row has %d cells but the header "+
					"has %d; escape a literal | as \\| (GFM splits cells at "+
					"every unescaped |, even inside `code`)", n, cols)
			}
		}
		_, header := row.(*east.TableHeader)
		typ := "tableCell"
		if header {
			typ = "tableHeader"
		}
		r := c.at(&Node{Type: "tableRow"}, c.lineOf(row))
		empty := true
		for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
			inl, err := c.inlines(cell, 0)
			if err != nil {
				return nil, err
			}
			kids := c.splitImages(cellItems(inl), c.lineOf(row))
			if len(kids) > 0 {
				empty = false
			} else {
				kids = []*Node{{Type: "paragraph"}}
			}
			r.Content = append(r.Content, &Node{Type: typ, Content: kids})
		}
		// An all-empty header row means "no header": that is how
		// ToMarkdown writes tables whose first row is not a header.
		if header && empty && row.NextSibling() != nil {
			continue
		}
		if header && empty {
			for _, cell := range r.Content {
				cell.Type = "tableCell"
			}
		}
		tbl.Content = append(tbl.Content, r)
	}
	return []*Node{tbl}, nil
}

// lineText is the source text of 1-based line l, without its newline.
func (c *converter) lineText(l int) string {
	i := l - c.lineOff - 1
	if i < 0 || i >= len(c.starts) {
		return ""
	}
	end := len(c.src)
	if i+1 < len(c.starts) {
		end = c.starts[i+1]
	}
	return strings.TrimRight(string(c.src[c.starts[i]:end]), "\r\n")
}

// rowCells counts the cells GFM splits a table row line into, as
// goldmark does: a leading and a trailing pipe are dropped, and every
// other pipe not right after a backslash ends a cell. Container markers
// (> of a quote, indentation) are ignored.
func rowCells(line string) int {
	s := strings.TrimLeft(line, " \t>")
	s = strings.TrimRight(s, " \t")
	s = strings.TrimPrefix(s, "|")
	s = strings.TrimSuffix(s, "|")
	n := 1
	for i := 0; i < len(s); i++ {
		if s[i] == '|' && (i == 0 || s[i-1] != '\\') {
			n++
		}
	}
	return n
}

// footnotes turns footnote definitions into a numbered list after a rule.
func (c *converter) footnotes(fl *east.FootnoteList, line int) ([]*Node,
	error) {
	ol := c.at(&Node{Type: "orderedList",
		Attrs: map[string]any{"order": 1}}, line)
	for fn := fl.FirstChild(); fn != nil; fn = fn.NextSibling() {
		kids, err := c.blocks(fn)
		if err != nil {
			return nil, err
		}
		ol.Content = append(ol.Content, c.at(&Node{Type: "listItem",
			Content: kids}, c.lineOf(fn)))
	}
	if len(ol.Content) == 0 {
		return nil, nil
	}
	return []*Node{c.at(&Node{Type: "rule"}, line), ol}, nil
}
