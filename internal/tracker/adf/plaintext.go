package adf

import "strings"

// PlainText returns the text of doc without formatting: one line per
// paragraph, heading or list item, table cells separated by tabs.
// Mentions, statuses, emoji, dates and smart links contribute their
// visible text.
func PlainText(doc *Node) string {
	if doc == nil {
		return ""
	}
	var b strings.Builder
	plainBlock(&b, dropNil(doc))
	return strings.TrimSpace(b.String())
}

func plainBlock(b *strings.Builder, n *Node) {
	switch n.Type {
	case "tableRow":
		for i, c := range n.Content {
			if i > 0 {
				b.WriteString("\t")
			}
			var cell strings.Builder
			plainBlock(&cell, c)
			b.WriteString(strings.Join(strings.Fields(cell.String()), " "))
		}
		b.WriteString("\n")
		return
	case "expand", "nestedExpand":
		if t := n.attrStr("title"); t != "" {
			b.WriteString(t + "\n")
		}
	case "codeBlock":
		b.WriteString(codeText(n) + "\n")
		return
	case "mediaSingle", "mediaGroup":
		for _, m := range n.Content {
			if alt := m.attrStr("alt"); alt != "" {
				b.WriteString(alt + "\n")
			}
		}
		return
	}
	if isInlineParent(n.Type) {
		for _, c := range n.Content {
			b.WriteString(inlineText(c))
		}
		b.WriteString("\n")
		return
	}
	for _, c := range n.Content {
		plainBlock(b, c)
	}
}

// inlineText is the visible text of an inline node.
func inlineText(n *Node) string {
	switch n.Type {
	case "text":
		return n.Text
	case "hardBreak":
		return "\n"
	case "mention", "status", "placeholder":
		return n.attrStr("text")
	case "emoji":
		return emojiText(n)
	case "date":
		return formatDate(n.attrStr("timestamp"))
	case "inlineCard":
		return n.attrStr("url")
	case "mediaInline":
		return n.attrStr("alt")
	}
	var b strings.Builder
	for _, c := range n.Content {
		b.WriteString(inlineText(c))
	}
	return b.String()
}
