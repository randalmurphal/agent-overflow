package adf

import (
	"encoding/json"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The edit invariant is Edit's safety net: whatever path an edit takes
// through the pairing code, every rich node of the original (a macro, a
// status lozenge, a panel, a column, a table's settings) that the new
// Markdown still shows must still be in the result, with the attributes
// Markdown can't show. A rich node may go only when the user removed what
// ToMarkdown wrote for it; any other loss is unexplained. Losing content
// (a macro, a lozenge, a header cell) is a Flattens warning, so callers
// refuse it without --allow-loss; losing dress (a color, an alignment, a
// width) is a plain warning, and the edit goes ahead.

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

// inventory lists the rich nodes in doc, in document order.
func inventory(doc *Node) []rich { return inventorySkip(doc, nil) }

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
		out = append(out, rich{n: n, kind: n.Type + "|" + phrase,
			sig: n.Type + "|" + phrase + "|" + data, rep: rep, outer: borrowed, plain: plain,
			label: "the " + phrase + " of " + strconv.Quote(rep), block: block,
			pinned: phrase == markPhrases["annotation"], cosmetic: cosmetic(phrase)})
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
		if len(markPh[i]) == 0 || n.Type == "text" && wordMarks[m.Type] &&
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
		phrase := markPhrases[m.Type]
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

// unkept lists the rich nodes of orig that out doesn't hold, and those
// it compared: subtrees the two share (every block the edit kept) hold the
// same nodes on both sides, so only the rest is inventoried. A node is
// kept by one with its id; failing that, by one with its sig whose text
// is new, when its own text is gone from out and, by gone, from the
// Markdown (an edited cell keeps its settings; two cells or items that
// trade them do not, as both texts still show). Nodes with one id are
// interchangeable; the last ones count as lost.
func unkept(orig, out *Node, gone func(rich) bool) (lost, compared []rich) {
	compared = inventorySkip(orig, treeSet(out))
	outRs := inventorySkip(out, treeSet(orig))
	have := map[string]int{}
	shown := map[[2]string]bool{} // kind and rep, on either side
	for _, r := range outRs {
		have[r.id()]++
		shown[[2]string{"out", r.kind + "\x00" + r.rep}] = true
	}
	for _, r := range compared {
		shown[[2]string{"orig", r.kind + "\x00" + r.rep}] = true
	}
	var left []rich
	for _, r := range compared {
		if have[r.id()] > 0 {
			have[r.id()]--
			continue
		}
		left = append(left, r)
	}
	if len(left) == 0 {
		return nil, compared
	}
	renamed := map[string][]string{} // sig -> the new texts of nodes with it
	for _, r := range outRs {
		if have[r.id()] > 0 {
			have[r.id()]--
			if !shown[[2]string{"orig", r.kind + "\x00" + r.rep}] {
				renamed[r.sig] = append(renamed[r.sig], r.rep)
			}
		}
	}
	took := map[string][]string{} // sig -> the renamed texts taken
	for _, r := range left {
		// Its new text holds the old (words added around it), or the old
		// text is gone from md: not two nodes that traded what they hold.
		// A block's data goes with the whole block (see blockGone), and a
		// merged block holding the text of each of its sources keeps the
		// data they agreed on for all of them.
		if r.block != "" && slices.ContainsFunc(took[r.sig], func(rep string) bool {
			return strings.Contains(rep, r.rep)
		}) {
			continue
		}
		k := -1
		if !shown[[2]string{"out", r.kind + "\x00" + r.rep}] {
			k = slices.IndexFunc(renamed[r.sig], func(rep string) bool {
				// A word's mark stays on that word alone (see wordMarksFor).
				return !r.word && strings.Contains(rep, r.rep)
			})
			if k < 0 && len(renamed[r.sig]) > 0 && (r.block != "" || gone(r)) {
				k = 0
			}
		}
		if k < 0 {
			lost = append(lost, r)
			continue
		}
		took[r.sig] = append(took[r.sig], renamed[r.sig][k])
		renamed[r.sig] = slices.Delete(renamed[r.sig], k, k+1)
	}
	return lost, compared
}

// goneFrom reports whether an entry's representation shows fewer times in
// md than in before, the original's Markdown: the user removed it.
func goneFrom(before, md string) func(rich) bool {
	plainBefore, plainMD := unescapeMD(before), unescapeMD(md)
	return func(r rich) bool {
		if r.plain {
			return countRep(plainMD, r.rep) < countRep(plainBefore, r.rep)
		}
		return countRep(md, r.rep) < countRep(before, r.rep)
	}
}

// checkKept warns, as Flattens, about each rich node of orig missing
// from out whose representation md still shows as often as before: the
// user didn't remove it, so the edit would lose it unasked. Nodes in skip
// already have a warning of their own.
func checkKept(orig, out *Node, md string, skip map[*Node]bool) []Warning {
	before := ToMarkdown(orig)
	plainBefore, plainMD := unescapeMD(before), unescapeMD(md)
	lostList, all := unkept(orig, out, goneFrom(before, md))
	lost := map[string]int{}
	for _, r := range lostList {
		lost[r.id()]++
	}
	if len(lost) == 0 {
		return nil
	}
	budget := map[[2]string]int{}
	dropped := func(r rich) bool {
		if r.pinned {
			return false
		}
		if r.block != "" {
			return blockGone(r.block, plainBefore, plainMD)
		}
		k := [2]string{r.kind, r.rep}
		b, ok := budget[k]
		if !ok {
			if r.plain {
				b = max(0, countRep(plainBefore, r.rep)-countRep(plainMD, r.rep))
			} else {
				b = max(0, countRep(before, r.rep)-countRep(md, r.rep))
			}
		}
		if !r.outer {
			budget[k] = max(0, b-1)
		} else {
			budget[k] = b
		}
		return b > 0
	}
	// Which of several like nodes was lost is open: those with a warning
	// of their own go first, then those whose representation was removed.
	for _, r := range all {
		if skip[r.n] && lost[r.id()] > 0 {
			lost[r.id()]--
		}
	}
	var unexplained []rich
	for _, r := range all {
		if skip[r.n] || lost[r.id()] == 0 {
			continue
		}
		if dropped(r) {
			lost[r.id()]--
		} else {
			unexplained = append(unexplained, r)
		}
	}
	var ws []Warning
	seen := map[Warning]bool{}
	// A word mark lost from several words of a line is one warning that
	// names them all.
	type wordsKey struct {
		line int
		kind string
	}
	words := map[wordsKey]*[]string{}
	var order []wordsKey
	for _, r := range unexplained {
		if lost[r.id()] == 0 {
			continue
		}
		lost[r.id()]--
		line := lineOf(md, r.rep)
		if r.word && r.cosmetic {
			k := wordsKey{line, r.kind}
			if words[k] == nil {
				words[k] = &[]string{}
				order = append(order, k)
			}
			if q := strconv.Quote(r.rep); !slices.Contains(*words[k], q) {
				*words[k] = append(*words[k], q)
			}
			continue
		}
		w := Warning{Line: line, Flattens: !r.cosmetic,
			Msg: r.label + " is still in the Markdown, but the edit would lose it"}
		switch {
		case r.pinned:
			w.Msg = "dropped " + r.label + ": the edit changed the words they comment on"
		case r.cosmetic:
			w.Msg = "dropped " + r.label + ", which Markdown can't show"
		}
		if !seen[w] {
			seen[w] = true
			ws = append(ws, w)
		}
	}
	for _, k := range order {
		_, phrase, _ := strings.Cut(k.kind, "|")
		msg := "dropped the " + phrase + " of " + strings.Join(*words[k], ", ")
		if phrase == markPhrases["annotation"] {
			msg += ": the edit changed the words they comment on"
		} else {
			msg += ", which Markdown can't show"
		}
		ws = append(ws, Warning{Line: k.line, Msg: msg})
	}
	return ws
}

// countRep counts s in md as a whole: where s starts (or ends) with a
// letter or digit, the occurrence must not run on into another, so a
// cell "Property" edited to "Propertyq" no longer counts. A rep of more
// than one line matches with any indent or quote markers after each line
// break, as md writes it inside a list item, a quote or a panel.
func countRep(md, s string) int {
	if s == "" {
		return 0
	}
	first, _ := utf8.DecodeRuneInString(s)
	last, _ := utf8.DecodeLastRuneInString(s)
	find := func(from int) (at, end int) {
		k := strings.Index(md[from:], s)
		if k < 0 {
			return -1, -1
		}
		return from + k, from + k + len(s)
	}
	if strings.Contains(s, "\n") {
		lines := strings.Split(s, "\n")
		for i, l := range lines {
			lines[i] = regexp.QuoteMeta(strings.TrimLeft(l, " \t"))
		}
		re := regexp.MustCompile(strings.Join(lines, `\n[ \t>]*`))
		find = func(from int) (at, end int) {
			loc := re.FindStringIndex(md[from:])
			if loc == nil {
				return -1, -1
			}
			return from + loc[0], from + loc[1]
		}
	}
	n := 0
	for i := 0; ; {
		at, end := find(i)
		if at < 0 {
			return n
		}
		before, _ := utf8.DecodeLastRuneInString(md[:at])
		after, _ := utf8.DecodeRuneInString(md[end:])
		runsOn := isAlnum(first) && isAlnum(before) || isAlnum(last) && isAlnum(after)
		if !runsOn {
			n++
		}
		i = max(end, at+1)
	}
}

// blockGone reports whether the user deleted block, a block's Markdown
// read without escapes: every one of its lines that md showed is gone
// from md, and at least half its words show fewer times than before. Rewording a sentence
// keeps the block; deleting it, or replacing it outright, does not.
func blockGone(block, before, md string) bool {
	for l := range strings.SplitSeq(block, "\n") {
		l = strings.TrimSpace(l)
		if was := countRep(before, l); l != "" && was > 0 && countRep(md, l) >= was {
			return false
		}
	}
	words := map[string]bool{}
	for _, w := range blockWordRe.FindAllString(block, -1) {
		words[w] = true
	}
	gone := 0
	for w := range words {
		if countRep(md, w) < countRep(before, w) {
			gone++
		}
	}
	return gone*2 >= len(words)
}

var blockWordRe = regexp.MustCompile(`[\p{L}\p{N}]{3,}`)

// unescapeMD is md without the backslash before escaped punctuation, so
// text as typed counts where it shows.
func unescapeMD(md string) string {
	if !strings.Contains(md, "\\") {
		return md
	}
	var b strings.Builder
	for i := 0; i < len(md); i++ {
		if md[i] == '\\' && i+1 < len(md) && isASCIIPunct(md[i+1]) {
			continue
		}
		b.WriteByte(md[i])
	}
	return b.String()
}

// lineOf is the 1-based line of the first s in md, or 0.
func lineOf(md, s string) int {
	i := strings.Index(md, s)
	if i < 0 {
		return 0
	}
	return strings.Count(md[:i], "\n") + 1
}
