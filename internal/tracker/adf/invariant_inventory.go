package adf

import (
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// rich is one node the invariant inventories: sig identifies it, with
// the attributes Markdown can't show; rep is how ToMarkdown writes it,
// which removing removes it; label names it in a warning.
type rich struct {
	n *Node
	// kind is what the node is (a cell's header type and its settings are
	// two kinds): one removed representation explains one node per kind.
	kind, sig, rep, label string
	// outer marks a rep borrowed from the table or cell around the node:
	// removing that removes every node that borrowed it.
	outer bool
	// plain marks a rep that is text as typed: it counts in md read
	// without its backslash escapes.
	plain bool
	// block is the Markdown of a block whose data (alignment, settings)
	// the entry is: that data goes only with the whole block (see
	// blockGone), not when its first line changes.
	block string
	// pinned marks an inline comment, which belongs to its words: removing
	// or rewriting them doesn't explain its loss.
	pinned bool
	// word marks a word mark's entry (a color, a comment) for one word,
	// its rep: kept only on that word, edited a little at most.
	word bool
	// cosmetic marks dress, not content (see cosmeticPhrases): losing it
	// warns without refusing.
	cosmetic bool
}

// id tells nodes apart for the invariant: two with one sig are alike
// only where md shows them the same, so a cell's settings belong to its
// text, and settings that trade places between two cells are a loss.
func (r rich) id() string { return r.sig + "\x00" + r.rep }

// inventorySkip is inventory without the subtrees rooted at nodes in
// skip.
func inventorySkip(doc *Node, skip map[*Node]bool) []rich {
	var out []rich
	var rec func(n, parent *Node, outer string, inCell bool)
	rec = func(n, parent *Node, outer string, inCell bool) {
		if skip[n] {
			return
		}
		rs := richOf(n, outer, inCell)
		out = append(out, rs...)
		out = append(out, payloadOf(n, parent, outer, inCell, len(rs) > 0)...)
		if isInlineParent(n.Type) {
			out = append(out, wordEntries(n)...)
		}
		switch n.Type {
		case "table":
			if len(rs) > 0 {
				outer = rs[0].rep
			}
		case "tableCell", "tableHeader":
			inCell = true
			if t := renderCell(n); strings.TrimSpace(t) != "" {
				outer = t
			}
		}
		for _, c := range n.Content {
			rec(c, n, outer, inCell)
		}
	}
	rec(doc, nil, "", false)
	return out
}

// payloadOf gives an entry for each of n's features that are data
// Markdown drops (see payloadFeatures): one per mark feature, and one per
// node feature unless richOf covers the node already.
func payloadOf(n, parent *Node, outer string, inCell, covered bool) []rich {
	nodePh, markPh := payloadFeatures(n, parent)
	if covered {
		nodePh = nil
	}
	if len(nodePh) == 0 && len(markPh) == 0 {
		return nil
	}
	rep, plain := payloadRep(n, outer, inCell)
	borrowed := rep == outer
	if rep == "" && parent != nil && isInlineParent(parent.Type) {
		// It shows nothing of its own (a placeholder, colored spaces):
		// its block's text stands for it.
		rep, _ = payloadRep(parent, outer, inCell)
		borrowed, plain = true, false
	}
	if rep == "" {
		return nil
	}
	var out []rich
	block := ""
	if n.Type != "text" && !allowed("paragraph", n.Type) && allowed("doc", n.Type) {
		// As md writes it where it is: a block in a cell is a cell's line.
		if inCell {
			block = unescapeMD(renderCell(&Node{Type: "tableCell", Content: []*Node{n}}))
		} else {
			block = unescapeMD(ToMarkdown(Doc(n)))
		}
	}
	add := func(phrase, data string) {
		// A node's phrase carries its article ("an image caption").
		named := strings.TrimPrefix(strings.TrimPrefix(phrase, "an "), "a ")
		out = append(out, rich{n: n, kind: n.Type + "|" + phrase,
			sig: n.Type + "|" + phrase + "|" + data, rep: rep, outer: borrowed, plain: plain,
			label: "the " + named + " of " + strconv.Quote(rep), block: block,
			pinned: phrase == markPhrases.get()["annotation"], cosmetic: cosmetic(phrase)})
	}
	if len(nodePh) > 0 {
		data := jsonText(unshown(n.Attrs, func(a map[string]any) *Node {
			c := *n
			c.Attrs = a
			return &c
		}, n, parent))
		for _, p := range nodePh {
			add(p, data)
		}
	}
	for i, m := range n.Marks {
		if len(markPh[i]) == 0 || n.Type == "text" && wordMarks.get()[m.Type] &&
			strings.IndexFunc(n.Text, isAlnum) >= 0 {
			continue // by word, in wordEntries
		}
		data := m.Type + jsonText(unshown(m.Attrs, func(a map[string]any) *Node {
			c := *n
			c.Marks = slices.Clone(n.Marks)
			c.Marks[i] = Mark{Type: m.Type, Attrs: a}
			return &c
		}, n, parent))
		for _, p := range markPh[i] {
			add(p, data)
		}
	}
	return out
}

// wordEntries gives an entry for each word mark (a color, a highlight, an
// inline comment) on each word of inline parent p: the mark belongs to
// the word, so it is kept only on it (see wordMarksFor), and goes only
// when the word does. A text node with no word (a colored ").") has its
// own entry, from payloadOf.
func wordEntries(p *Node) []rich {
	ts := runeToks(p.Content, false)
	var out []rich
	add := func(m Mark, rep string, plain, borrowed bool, n *Node) {
		phrase := markPhrases.get()[m.Type]
		out = append(out, rich{n: n, kind: "text|" + phrase,
			sig: "text|" + phrase + "|" + m.Type + jsonText(m.Attrs), rep: rep,
			plain: plain, outer: borrowed, word: plain,
			label:  "the " + phrase + " of " + strconv.Quote(rep),
			pinned: m.Type == "annotation", cosmetic: cosmetic(phrase)})
	}
	for _, w := range wordSpans(ts) {
		var seen []Mark
		for x := w.lo; x < w.hi; x++ {
			for _, m := range hiddenOf(ts[x].marks) {
				if !hasMarkEq(seen, m) {
					seen = append(seen, m)
					add(m, w.text, true, false, ts[w.lo].node)
				}
			}
		}
	}
	return out
}

// unshown is the attributes of a, n's or one of its marks', that
// ToMarkdown doesn't show: those whose removal leaves n's Markdown as it
// was. with gives n with other attributes in their place. These are what
// a node or mark keeps only in ADF, so what an edit must not lose; those
// md shows are the user's to change.
func unshown(a map[string]any, with func(map[string]any) *Node, n, parent *Node) map[string]any {
	render := func(x *Node) (string, bool) {
		switch {
		case allowed("doc", x.Type):
			return ToMarkdown(Doc(x)), true
		case allowed("paragraph", x.Type):
			return ToMarkdown(Doc(&Node{Type: "paragraph", Content: []*Node{x}})), true
		case parent != nil && allowed("doc", parent.Type):
			p := *parent
			p.Content = slices.Clone(parent.Content)
			if i := slices.Index(p.Content, n); i >= 0 {
				p.Content[i] = x
			}
			return ToMarkdown(Doc(&p)), true
		}
		return "", false
	}
	base, ok := render(n)
	out := map[string]any{}
	for k, v := range a {
		if k == "localId" {
			continue
		}
		if ok {
			b := maps.Clone(a)
			delete(b, k)
			if got, _ := render(with(b)); got != base {
				continue // md shows it
			}
		}
		out[k] = v
	}
	return out
}

// payloadRep is how md shows n: a text node's text, a block's first
// line, an image by its file; outer where n shows nothing of its own.
func payloadRep(n *Node, outer string, inCell bool) (rep string, plain bool) {
	firstLine := func(s string) string {
		for l := range strings.SplitSeq(s, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				return l
			}
		}
		return ""
	}
	switch {
	case n.Type == "text":
		if t := firstLine(n.Text); t != "" {
			return t, true
		}
		return outer, false
	case n.Type == "codeBlock":
		var b strings.Builder
		for _, c := range n.Content {
			b.WriteString(c.Text)
		}
		return cmpOr(firstLine(b.String()), outer), false
	case isInlineParent(n.Type):
		ctx := ctxPara
		if inCell {
			ctx = ctxCell
		}
		return cmpOr(firstLine(renderInline(n.Content, ctx)), outer), false
	case n.Type == "mediaSingle" || n.Type == "mediaGroup":
		for _, c := range n.Content {
			if c.Type == "media" {
				return mediaRep(c), false
			}
		}
	case n.Type == "media" || n.Type == "mediaInline":
		return mediaRep(n), false
	case allowed("doc", n.Type):
		return cmpOr(firstLine(ToMarkdown(Doc(n))), outer), false
	case allowed("paragraph", n.Type):
		return cmpOr(firstLine(ToMarkdown(Doc(&Node{Type: "paragraph",
			Content: []*Node{n}}))), outer), false
	}
	return outer, false
}

func mediaRep(n *Node) string {
	if n.attrStr("type") == "external" {
		return n.attrStr("url")
	}
	return "media:" + n.attrStr("id")
}

// richOf gives n's entries: none for a plain node. outer is the
// representation of the nearest table or cell around n, for settings
// that show nowhere on their own.
func richOf(n *Node, outer string, inCell bool) []rich {
	var out []rich
	dress := false
	add := func(sig, rep, label string) {
		if rep != "" {
			kind, _, _ := strings.Cut(sig, "|")
			kind = n.Type + "|" + kind
			out = append(out, rich{n: n, kind: kind, sig: n.Type + "|" + sig,
				rep: rep, label: label, outer: rep == outer, cosmetic: dress})
		}
	}
	switch n.Type {
	case "extension", "bodiedExtension", "inlineExtension":
		k := n.attrStr("extensionKey")
		add(k+"|"+jsonText(n.attr("parameters")), macroComment(n), "the macro "+strconv.Quote(k))
	case "status":
		t := n.attrStr("text")
		add(n.attrStr("text")+"|"+n.attrStr("color"), "["+t+"]",
			"the status lozenge ["+t+"]")
	case "mention":
		if id := strings.TrimSpace(n.attrStr("id")); id != "" {
			rep := mentionMarkdown(n, ctxPara)
			add(id, rep, "the mention "+rep)
		}
	case "emoji":
		rep := emojiText(n)
		add(n.attrStr("shortName")+"|"+n.attrStr("id"), rep, "the emoji "+rep)
	case "date":
		rep := formatDate(n.attrStr("timestamp"))
		add(n.attrStr("timestamp"), rep, "the date "+rep)
	case "panel":
		rep := "[!" + panelLabel(n.attrStr("panelType")) + "]"
		if customPanel(n) {
			rep = "<!-- panel: " + commentSafe(n.attrStr("panelType")) + " -->"
		}
		add(attrsText(n), rep, "the "+n.attrStr("panelType")+" panel")
	case "expand", "nestedExpand":
		rep := "<details>"
		if t, ok := n.Attrs["title"].(string); ok {
			rep += "<summary>" + htmlEscape(t) + "</summary>"
		}
		add(n.attrStr("title"), rep, "the expand "+strconv.Quote(n.attrStr("title")))
	case "decisionItem":
		// Each decision by itself: editing one keeps the others.
		text := renderInline(n.Content, ctxTask)
		// Its state is data of this decision, not a kind: two decisions
		// that trade states are a loss.
		add("decision|"+n.attrStr("state"), "Decision: "+text, "the decision "+strconv.Quote(text))
	case "layoutSection":
		add(attrsText(n), "<!-- layout -->", "the column layout")
	case "layoutColumn":
		rep := "<!-- column -->"
		if w, ok := n.attrNum("width"); ok {
			rep = "<!-- column " + strconv.FormatFloat(w, 'f', -1, 64) + "% -->"
		}
		add(attrsText(n), rep, "the column "+strings.TrimSuffix(
			strings.TrimPrefix(rep, "<!-- "), " -->"))
	case "table":
		dress = true
		add(attrsText(n), tableRep(n), "the table's settings (layout, width, numbering)")
	case "tableHeader", "tableCell":
		text := renderCell(n)
		rep := cmpOr(strings.TrimSpace(text), outer)
		// The sig is content-free; id adds the text: an edited cell's
		// settings count as lost, explained when its old text is gone.
		if n.Type == "tableHeader" {
			add("header", rep, "the header cell "+strconv.Quote(text))
		}
		if a := cellAttrs(n, true); a != "" {
			add("merge|"+a, rep, "the merged cell "+strconv.Quote(text))
		}
		if a := cellAttrs(n, false); a != "" {
			dress = true
			add("attrs|"+a, rep, "the cell formatting (width, color) of "+
				strconv.Quote(text))
		}
	case "media", "mediaInline":
		// Its settings, but not the alt text, which md shows.
		a := maps.Clone(n.Attrs)
		delete(a, "alt")
		delete(a, "localId")
		size := map[string]any{}
		for _, k := range []string{"width", "height"} {
			if v, ok := a[k]; ok {
				size[k] = v
				delete(a, k)
			}
		}
		rep := mediaRep(n)
		add(jsonText(a), rep, "the image or file "+rep)
		if len(size) > 0 {
			dress = true
			add("size|"+jsonText(size), rep, "the image dimensions of "+rep)
		}
	case "inlineCard", "blockCard", "embedCard":
		add(n.attrStr("url"), n.attrStr("url"), "the link card "+n.attrStr("url"))
	}
	return out
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// tableRep is the first table line with text in it: the header row, or
// for a table without one, its first row.
func tableRep(n *Node) string {
	for _, l := range renderTable(n) {
		if strings.Trim(l, "|- ") != "" {
			return l
		}
	}
	return ""
}

// cellAttrs is a cell's settings beyond the defaults, as JSON: with
// merge, its spans (the table's shape), else the rest (widths, colors).
func cellAttrs(n *Node, merge bool) string {
	a := maps.Clone(n.Attrs)
	delete(a, "localId")
	for k := range a {
		span := k == "colspan" || k == "rowspan"
		if f, ok := n.attrNum(k); span != merge || span && ok && f == 1 {
			delete(a, k)
		}
	}
	if len(a) == 0 {
		return ""
	}
	return jsonText(a)
}

func attrsText(n *Node) string {
	a := maps.Clone(n.Attrs)
	delete(a, "localId")
	return jsonText(a)
}

func jsonText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
