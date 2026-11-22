package adf

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ToMarkdown renders doc as Markdown a person or an agent can read and
// edit. Every node renders as something readable; what Markdown can't
// express (colors, column widths, status lozenges, macros) is simplified,
// and Edit keeps the original ADF for blocks whose Markdown is not changed.
// Text is escaped only where Markdown would otherwise misread it. Nil
// nodes, which Validate rejects, are skipped.
func ToMarkdown(doc *Node) string {
	if doc == nil {
		return ""
	}
	b := renderBlocks(dropNil(doc).Content)
	if len(b.lines) == 0 {
		return ""
	}
	return strings.Join(b.lines, "\n") + "\n"
}

// blk is rendered block Markdown plus the list kind at each edge, so
// neighbors that Markdown would merge into one list can be separated.
type blk struct {
	lines       []string
	first, last string
}

func lines(ls ...string) blk { return blk{lines: ls} }

func renderBlocks(ns []*Node) blk {
	return joinBlocks(ns, false)
}

// joinBlocks renders blocks separated by blank lines. In a list item
// (tight), a list right after a paragraph needs none.
func joinBlocks(ns []*Node, tight bool) blk {
	var out blk
	var prev *Node
	for _, n := range ns {
		b := renderBlock(n)
		if len(b.lines) == 0 {
			continue
		}
		switch {
		case len(out.lines) == 0:
			out.first = b.first
		case tight && prev.Type == "paragraph" && startsList(n):
			// no blank line: the nested list stays tight
		default:
			out.lines = append(out.lines, "")
			if out.last != "" && out.last == b.first {
				out.lines = append(out.lines, "<!-- -->", "")
			}
		}
		out.lines = append(out.lines, b.lines...)
		out.last = b.last
		prev = n
	}
	return out
}

// startsList reports whether n renders as a list that may interrupt a
// paragraph (an ordered list only when it starts at 1).
func startsList(n *Node) bool {
	switch n.Type {
	case "bulletList", "taskList":
		return true
	case "orderedList":
		f, ok := n.attrNum("order")
		return !ok || f == 1
	}
	return false
}

func renderBlock(n *Node) blk {
	switch n.Type {
	case "paragraph":
		return inlineBlock(renderInline(n.Content, ctxPara))
	case "heading":
		level := 1
		if f, ok := n.attrNum("level"); ok && f >= 1 && f <= 6 {
			level = int(f)
		}
		h := strings.Repeat("#", level)
		if s := renderInline(n.Content, ctxHeading); s != "" {
			h += " " + s
		}
		return lines(h)
	case "codeBlock":
		return renderCode(n)
	case "rule":
		return lines("---")
	case "blockquote":
		return lines(quoteLines(renderBlocks(n.Content).lines)...)
	case "panel":
		if customPanel(n) {
			return renderContainer("panel: "+commentSafe(n.attrStr("panelType")),
				"/panel", n.Content)
		}
		return renderPanel(n)
	case "bulletList", "orderedList":
		return renderList(n)
	case "taskList":
		return blk{lines: renderTasks(n), first: "-", last: "-"}
	case "decisionList":
		return renderDecisions(n)
	case "listItem":
		return renderList(&Node{Type: "bulletList", Content: []*Node{n}})
	case "taskItem", "blockTaskItem":
		return renderBlock(&Node{Type: "taskList", Content: []*Node{n}})
	case "decisionItem":
		return renderBlock(&Node{Type: "decisionList",
			Content: []*Node{n}})
	case "table":
		return lines(renderTable(n)...)
	case "expand", "nestedExpand":
		return renderDetails(n)
	case "mediaSingle":
		b := inlineBlock(renderMediaSingle(n))
		if c := inlineBlock(renderCaption(n)); len(c.lines) > 0 {
			b.lines = append(append(b.lines, ""), c.lines...)
		}
		return b
	case "mediaGroup":
		return renderMediaGroup(n)
	case "extension":
		return lines(macroComment(n))
	case "bodiedExtension":
		return renderContainer(macroName(n), "/"+macroName(n), n.Content)
	case "layoutSection":
		return renderLayout(n)
	case "blockCard", "embedCard":
		if u := n.attrStr("url"); u != "" {
			return inlineBlock(renderInline([]*Node{{Type: "inlineCard",
				Attrs: map[string]any{"url": u}}}, ctxPara))
		}
		return lines("<!-- adf: " + n.Type + " -->")
	}
	if isInline(n.Type) {
		return inlineBlock(renderInline([]*Node{n}, ctxPara))
	}
	if len(n.Content) > 0 {
		return renderBlocks(n.Content)
	}
	return lines("<!-- adf: " + commentSafe(n.Type) + " -->")
}

func inlineBlock(s string) blk {
	if s == "" {
		return blk{}
	}
	return lines(strings.Split(s, "\n")...)
}

func isInline(t string) bool {
	return slices.Contains(inlineTypes, t)
}

func quoteLines(ls []string) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		if l == "" {
			out[i] = ">"
		} else {
			out[i] = "> " + l
		}
	}
	return out
}

// renderPanel writes a panel as a GitHub alert. The label line and a
// first paragraph share one Markdown paragraph, so a first block that
// can't interrupt a paragraph (a rule would make the label a setext
// heading) gets a blank line before it.
func renderPanel(n *Node) blk {
	body := renderBlocks(n.Content)
	ls := []string{"[!" + panelLabel(n.attrStr("panelType")) + "]"}
	// A blank line keeps the label off a first line that would join it:
	// a rule, a list not starting at 1, or text a table delimiter row
	// would read with the label as its header.
	if len(body.lines) > 0 && (body.lines[0] == "---" ||
		strings.HasPrefix(body.lines[0], "|") ||
		body.first == "." && !strings.HasPrefix(body.lines[0], "1.")) {
		ls = append(ls, "")
	}
	return lines(quoteLines(append(ls, body.lines...))...)
}

var panelLabels = lazy[map[string]string]{build: func() map[string]string {
	return map[string]string{
		"info": "INFO", "note": "NOTE", "tip": "TIP", "warning": "WARNING",
		"error": "ERROR", "success": "SUCCESS",
	}
}}

func panelLabel(t string) string {
	if l, ok := panelLabels.get()[t]; ok {
		return l
	}
	return "NOTE"
}

// codeText is a code block's text.
func codeText(n *Node) string {
	if len(n.Content) == 1 {
		return n.Content[0].Text
	}
	var b strings.Builder
	for _, t := range n.Content {
		b.WriteString(t.Text)
	}
	return b.String()
}

func renderCode(n *Node) blk {
	code := codeText(n)
	fence := strings.Repeat("`", max(3, longestRun(code, '`')+1))
	ls := []string{fence + infoString(n.attrStr("language"))}
	if code != "" {
		ls = append(ls, strings.Split(code, "\n")...)
	}
	return lines(append(ls, fence)...)
}

// infoString writes a code block language as a fence info string, where
// backslash escapes and entities are decoded. A backtick or line break
// can't appear there at all.
func infoString(lang string) string {
	if strings.ContainsAny(lang, "`\r\n") {
		return ""
	}
	var b strings.Builder
	for i := 0; i < len(lang); i++ {
		if lang[i] == '\\' || lang[i] == '&' && entityAt(lang[i:]) {
			b.WriteByte('\\')
		}
		b.WriteByte(lang[i])
	}
	return b.String()
}

func longestRun(s string, c byte) int {
	best, cur := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			cur++
			best = max(best, cur)
		} else {
			cur = 0
		}
	}
	return best
}

// itemLines puts marker before an item's first line and indents the
// rest to the item's content column.
func itemLines(marker string, body []string) []string {
	if len(body) == 0 {
		return []string{marker}
	}
	indent := strings.Repeat(" ", len(marker)+1)
	out := []string{marker + " " + body[0]}
	for _, l := range body[1:] {
		if l == "" {
			out = append(out, "")
		} else {
			out = append(out, indent+l)
		}
	}
	return out
}

func renderList(n *Node) blk {
	ordered := n.Type == "orderedList"
	num := 1
	if f, ok := n.attrNum("order"); ok && f >= 0 {
		num = int(f)
	}
	var out []string
	for _, it := range n.Content {
		marker := "-"
		if ordered {
			marker = listNumber(num) + "."
			num++
		}
		var body []string
		if it.Type == "listItem" {
			body = joinBlocks(it.Content, true).lines
		} else {
			body = renderBlock(it).lines
		}
		out = append(out, itemLines(marker, body)...)
	}
	kind := "-"
	if ordered {
		kind = "."
	}
	return blk{lines: out, first: kind, last: kind}
}

// maxListNumber is the largest number CommonMark reads as a list marker.
const maxListNumber = 999999999

func listNumber(n int) string {
	return strconv.Itoa(min(n, maxListNumber))
}

// renderTasks renders a taskList; nested taskLists sit under the item
// before them.
func renderTasks(n *Node) []string {
	var out []string
	for _, it := range n.Content {
		switch it.Type {
		case "taskList":
			nested := renderTasks(it)
			if len(out) == 0 {
				out = append(out, nested...)
				continue
			}
			for _, l := range nested {
				out = append(out, "  "+l)
			}
		case "taskItem", "blockTaskItem":
			box := checkbox(it)
			content := it.Content
			if it.Type == "blockTaskItem" {
				content = flattenParas(it.Content)
			}
			body := inlineBlock(renderInline(content, ctxTask)).lines
			if len(body) == 0 {
				out = append(out, "- "+box)
				continue
			}
			body[0] = box + " " + body[0]
			out = append(out, itemLines("-", body)...)
		default:
			out = append(out, itemLines("-", renderBlock(it).lines)...)
		}
	}
	return out
}

// checkbox is the Markdown checkbox for a task item.
func checkbox(it *Node) string {
	if it.attrStr("state") == "DONE" {
		return "[x]"
	}
	return "[ ]"
}

// flattenParas joins paragraphs' inline content with hard breaks.
func flattenParas(ns []*Node) []*Node {
	var out []*Node
	for i, p := range ns {
		if i > 0 {
			out = append(out, &Node{Type: "hardBreak"})
		}
		out = append(out, p.Content...)
	}
	return out
}

func renderDecisions(n *Node) blk {
	var out []string
	for _, it := range n.Content {
		content := append([]*Node{textNode("Decision: ")}, it.Content...)
		out = append(out, itemLines("-",
			inlineBlock(renderInline(content, ctxTask)).lines)...)
	}
	return blk{lines: out, first: "-", last: "-"}
}

func renderDetails(n *Node) blk {
	open := "<details>"
	if t, ok := n.Attrs["title"].(string); ok {
		open += "<summary>" + htmlEscape(t) + "</summary>"
	}
	ls := []string{open, ""}
	if body := renderBlocks(n.Content).lines; len(body) > 0 {
		ls = append(append(ls, body...), "")
	}
	return lines(append(ls, "</details>")...)
}

var (
	htmlEscaper = lazy[*strings.Replacer]{build: func() *strings.Replacer {
		return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\n", " ")
	}}
	commentEscaper = lazy[*strings.Replacer]{build: func() *strings.Replacer {
		return strings.NewReplacer("--", "- -", ">", "")
	}}
)

func htmlEscape(s string) string { return htmlEscaper.get().Replace(s) }

func commentSafe(s string) string { return commentEscaper.get().Replace(s) }

func macroComment(n *Node) string {
	return "<!-- " + macroName(n) + " -->"
}

// macroName is a macro's marker text, "macro: KEY ID": ID is a short
// digest of the macro's localId, else of its key and parameters, so two
// macros with one key read apart in the Markdown and Edit never confuses
// them. A macro built from markers keeps the ID they were written with.
func macroName(n *Node) string {
	key := n.attrStr("extensionKey")
	if key == "" {
		key = "unknown"
	}
	s := "macro: " + commentSafe(key)
	if id := macroID(n); id != "" {
		s += " " + id
	}
	return s
}

// macroID is the ID macroName writes for n.
func macroID(n *Node) string {
	if id, ok := n.Attrs[attrMacroID].(string); ok {
		return id
	}
	src := n.attrStr("localId")
	if src == "" {
		src = n.attrStr("extensionKey") + "\x00" + jsonText(n.attr("parameters"))
	}
	sum := sha256.Sum256([]byte(src))
	return hex.EncodeToString(sum[:2])
}

// renderContainer writes a container Markdown has no syntax for as its
// children between an open and a close marker, each its own HTML block:
// Edit keeps the container and edits the children while both markers
// survive.
func renderContainer(open, closer string, children []*Node) blk {
	ls := []string{"<!-- " + open + " -->", ""}
	if body := renderBlocks(children).lines; len(body) > 0 {
		ls = append(append(ls, body...), "")
	}
	return lines(append(ls, "<!-- "+closer+" -->")...)
}

// renderLayout writes a column layout as <!-- layout --> around one
// <!-- column W% --> ... <!-- /column --> per column.
func renderLayout(n *Node) blk {
	ls := []string{"<!-- layout -->"}
	for _, col := range n.Content {
		open := "column"
		if w, ok := col.attrNum("width"); ok {
			open += " " + strconv.FormatFloat(w, 'f', -1, 64) + "%"
		}
		ls = append(ls, "")
		ls = append(ls, renderContainer(open, "/column", col.Content).lines...)
	}
	return lines(append(ls, "", "<!-- /layout -->")...)
}

func formatDate(ts string) string {
	ms, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ts
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02")
}
