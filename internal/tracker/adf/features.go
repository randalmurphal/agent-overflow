package adf

import (
	"math"
	"slices"
	"strings"
)

// feature is something in an ADF block that ToMarkdown can't carry back
// through FromMarkdown. warn marks the ones a person would notice losing.
type feature struct {
	phrase string
	warn   bool
}

// lossy lists the features in n that a Markdown round trip loses,
// sorted and deduplicated.
func lossy(n *Node) []feature {
	seen := map[feature]bool{}
	add := func(phrase string, warn bool) {
		seen[feature{phrase, warn}] = true
	}
	var rec func(n, parent *Node)
	rec = func(n, parent *Node) {
		nodeFeatures(n, parent, add, true)
		for _, m := range n.Marks {
			markFeatures(m, n, add)
		}
		for _, c := range n.Content {
			rec(c, n)
		}
	}
	rec(n, nil)
	out := make([]feature, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b feature) int {
		return strings.Compare(a.phrase, b.phrase)
	})
	return out
}

// featureCounts counts, per feature, the nodes and marks in n's subtree
// that have it: two lozenges are two.
func featureCounts(n *Node) map[string]int {
	counts := map[string]int{}
	var rec func(n, parent *Node)
	rec = func(n, parent *Node) {
		seen := map[string]bool{}
		add := func(p string, _ bool) {
			if !seen[p] {
				seen[p] = true
				counts[p]++
			}
		}
		nodeFeatures(n, parent, add, true)
		for _, m := range n.Marks {
			clear(seen)
			markFeatures(m, n, add)
		}
		for _, c := range n.Content {
			rec(c, n)
		}
	}
	rec(n, nil)
	return counts
}

// warnFeatures lists the noticeable features of n that Markdown loses.
func warnFeatures(n *Node) []string {
	var out []string
	for _, f := range lossy(n) {
		if f.warn {
			out = append(out, f.phrase)
		}
	}
	return out
}

// lossPhrases lists every feature of n that Markdown loses.
func lossPhrases(n *Node) []string {
	var out []string
	for _, f := range lossy(n) {
		if len(out) == 0 || out[len(out)-1] != f.phrase {
			out = append(out, f.phrase)
		}
	}
	return out
}

// cosmeticPhrases are the features that only dress content up: losing one
// in an edit is a warning, and the edit goes ahead. Losing any other (a
// macro, a lozenge, a mention, a header cell, a merged cell, an
// attachment) loses content, which an edit refuses (a Flattens warning)
// unless the user accepts it.
var cosmeticPhrases = map[string]bool{
	"text color": true, "highlighting": true, "inline comments": true,
	"alignment": true, "indentation": true, "small text": true,
	"full-width layout": true, "image borders": true,
	"link details": true, "links on attachments": true,
	"formatting on inline code": true, "formatting Markdown can't show": true,
	"emphasis starting or ending mid-word with punctuation": true,
	"line breaks inside text":                               true, "empty paragraphs": true,
	"code block settings": true, "list numbering": true,
	"table width or layout": true, "numbered table rows": true,
	"table column widths": true, "cell colors": true, "cell alignment": true,
	"image size or position": true, "image dimensions": true,
	"paragraph breaks in table cells": true, "checklist nesting": true,
	"list items starting with a block": true, "misplaced headings": true,
}

// cosmetic reports whether losing every feature in phrases loses no
// content (see cosmeticPhrases).
func cosmetic(phrases ...string) bool {
	for _, p := range phrases {
		if !cosmeticPhrases[p] {
			return false
		}
	}
	return true
}

var markPhrases = map[string]string{
	"textColor": "text color", "backgroundColor": "highlighting",
	"alignment": "alignment", "breakout": "full-width layout",
	"indentation": "indentation", "fontSize": "small text",
	"annotation": "inline comments", "border": "image borders",
	"dataConsumer": "macro data links", "fragment": "macro data links",
}

func markFeatures(m Mark, n *Node, add func(string, bool)) {
	if p, ok := markPhrases[m.Type]; ok {
		add(p, true)
		return
	}
	if m.Type == "link" {
		for k := range m.Attrs {
			if k != "href" && k != "title" {
				add("link details", false)
			}
		}
		if n.Type != "text" && n.Type != "mediaSingle" {
			add("links on attachments", true)
		}
	}
	if m.Type == "code" && len(n.Marks) > 1 {
		for _, o := range n.Marks {
			if o.Type != "code" && o.Type != "link" {
				add("formatting on inline code", true)
			}
		}
	}
}

var nodePhrases = map[string]string{
	"status": "a status lozenge", "date": "a date", "emoji": "an emoji",
	"placeholder": "placeholder text", "extension": "a macro",
	"bodiedExtension": "a macro", "inlineExtension": "a macro",
	"layoutSection": "a column layout", "decisionList": "decisions",
	"mediaGroup": "an attachment group", "blockCard": "a link card",
	"embedCard": "an embedded link", "caption": "an image caption",
	"blockTaskItem": "a checklist item with blocks",
	"syncBlock":     "a synced block", "bodiedSyncBlock": "a synced block",
}

// shapePhrases are the features about how content is arranged (a line
// break inside text, a ragged table), which an edit that rewrites the
// content may change. Every other feature is data Markdown drops (a mark,
// an attribute, a node it can't write), which the edit invariant
// inventories: a new feature is inventoried unless it is listed here.
var shapePhrases = map[string]bool{
	"line breaks inside text":                               true,
	"emphasis starting or ending mid-word with punctuation": true,
	"empty paragraphs":                                      true,
	"formatting Markdown can't show":                        true,
	"checklist nesting":                                     true,
	"list items starting with a block":                      true,
	"expand nesting":                                        true,
	"misplaced headings":                                    true,
	"header cells outside the first row":                    true,
	"an empty header row":                                   true,
	"empty table rows":                                      true,
	"ragged table rows":                                     true,
	"blocks inside table cells":                             true,
	"paragraph breaks in table cells":                       true,
}

// payloadFeatures lists n's own features that are data Markdown drops:
// those of the node (its type or attributes), and per mark index those of
// each mark.
func payloadFeatures(n, parent *Node) (node []string, marks map[int][]string) {
	add := func(p string, _ bool) {
		if !shapePhrases[p] && !slices.Contains(node, p) {
			node = append(node, p)
		}
	}
	nodeFeatures(n, parent, add, false)
	for i, m := range n.Marks {
		markFeatures(m, n, func(p string, _ bool) {
			if !shapePhrases[p] && !slices.Contains(marks[i], p) {
				if marks == nil {
					marks = map[int][]string{}
				}
				marks[i] = append(marks[i], p)
			}
		})
	}
	return node, marks
}

// nodeFeatures adds n's own features; shape adds those that need n's
// content laid out (mid-word emphasis, formatting Markdown can't show).
func nodeFeatures(n, parent *Node, add func(string, bool), shape bool) {
	if p, ok := nodePhrases[n.Type]; ok {
		add(p, true)
	}
	if _, ok := specs[n.Type]; !ok {
		add("unsupported content ("+n.Type+")", true)
	}
	switch n.Type {
	case "text":
		breaks := "\r\n"
		if parent != nil && parent.Type == "codeBlock" {
			breaks = "\r"
		}
		if strings.ContainsAny(n.Text, breaks) {
			add("line breaks inside text", false)
		}
	case "paragraph", "taskItem", "decisionItem":
		if !shape {
			break
		}
		if midWordEmphasis(n.Content) {
			add("emphasis starting or ending mid-word with punctuation",
				false)
		}
		inlineLoss(n, parent, add)
		if n.Type == "paragraph" && len(n.Content) == 0 && !soleChild(parent) {
			add("empty paragraphs", false)
		}
	case "mention":
		if strings.TrimSpace(n.attrStr("id")) == "" {
			add("a mention with no account", true)
		}
		switch t := n.attrStr("text"); {
		case t == "":
		case mentionName(n) == "":
			add("an unnamed mention", false)
		case t != "@"+mentionName(n):
			add("mention display text", false)
		}
	case "orderedList":
		if f, ok := n.attrNum("order"); ok &&
			(f != math.Trunc(f) || f > maxListNumber) {
			add("list numbering", false)
		}
	case "codeBlock":
		for k := range n.Attrs {
			if k != "language" && k != "localId" {
				add("code block settings", false)
			}
		}
		if strings.ContainsAny(n.attrStr("language"), " \t\r\n`") {
			add("code block settings", false)
		}
	case "panel":
		if customPanel(n) {
			add("a custom panel", true)
		}
	case "table":
		tableFeatures(n, add)
	case "tableCell", "tableHeader":
		cellFeatures(n, add)
	case "mediaSingle", "media", "mediaInline":
		mediaFeatures(n, add)
	case "inlineCard":
		// A URL that can't be an autolink is written as a plain link.
		if u := n.attrStr("url"); !autolinkRe.MatchString(u) ||
			strings.Contains(u, "|") {
			add("a link card", true)
		}
	case "taskList":
		if len(n.Content) > 0 && n.Content[0].Type == "taskList" {
			add("checklist nesting", false)
		}
	case "listItem":
		if len(n.Content) > 0 && n.Content[0].Type != "paragraph" {
			add("list items starting with a block", false)
		}
	case "expand", "nestedExpand":
		if parent != nil && parent.Type != "doc" && n.Type == "expand" {
			add("expand nesting", false)
		}
	case "heading":
		if !shape {
			break
		}
		if midWordEmphasis(n.Content) {
			add("emphasis starting or ending mid-word with punctuation",
				false)
		}
		inlineLoss(n, parent, add)
		if parent != nil && !allowed(parent.Type, "heading") {
			add("misplaced headings", false)
		}
	}
	for k := range n.Attrs {
		if k == "localId" {
			continue
		}
		if !slices.Contains(specs[n.Type].attrs, k) {
			add("unknown attributes", false)
		}
	}
}

// customPanel reports a panel Markdown can't write: an unknown type, or
// attributes (an icon, a color) beyond its type and localId.
func customPanel(n *Node) bool {
	_, ok := panelLabels[n.attrStr("panelType")]
	return !ok || len(n.Attrs) > 2 || (len(n.Attrs) == 2 && n.attr("localId") == nil)
}

// mediaFeatures lists what Markdown loses from an image or attachment:
// it keeps only the file (or URL) and its alt text.
func mediaFeatures(n *Node, add func(string, bool)) {
	switch n.Type {
	case "mediaSingle":
		if l := n.attrStr("layout"); (l != "" && l != "center") ||
			n.attr("width") != nil {
			add("image size or position", true)
		}
	case "media":
		if n.attr("width") != nil || n.attr("height") != nil {
			add("image dimensions", false)
		}
		if n.attrStr("collection") != "" {
			add("an attachment collection", true)
		}
		if n.attrStr("type") == "link" || n.attr("occurrenceKey") != nil {
			add("attachment details", false)
		}
	case "mediaInline":
		for k := range n.Attrs {
			switch k {
			case "id", "collection", "alt", "localId":
			case "type":
				if n.attrStr("type") != "file" {
					add("attachment details", false)
				}
			default:
				add("attachment details", false)
			}
		}
		if n.attrStr("collection") != "" {
			add("an attachment collection", true)
		}
	}
}

// soleChild reports whether parent, a list item or cell, holds just one
// block, where an empty paragraph round-trips as an empty item or cell.
func soleChild(parent *Node) bool {
	if parent == nil || len(parent.Content) != 1 {
		return false
	}
	switch parent.Type {
	case "listItem", "tableCell", "tableHeader":
		return true
	}
	return false
}

func tableFeatures(n *Node, add func(string, bool)) {
	for k, v := range n.Attrs {
		switch k {
		case "localId":
		case "isNumberColumnEnabled":
			if v == true {
				add("numbered table rows", true)
			}
		case "layout", "width", "displayMode":
			add("table width or layout", false)
		}
	}
	for r, row := range n.Content {
		allHeader, anyHeader, empty := true, false, true
		for _, c := range row.Content {
			if c.Type == "tableHeader" {
				anyHeader = true
			} else {
				allHeader = false
			}
			if len(c.Content) != 1 || c.Content[0].Type != "paragraph" ||
				len(c.Content[0].Content) != 0 {
				empty = false
			}
		}
		if anyHeader && (r > 0 || !allHeader) {
			add("header cells outside the first row", true)
		}
		if r == 0 && allHeader && empty && len(n.Content) > 1 {
			add("an empty header row", false)
		}
		if len(row.Content) == 0 {
			add("empty table rows", false)
		}
	}
	if len(n.Content) > 0 {
		cols := -1
		for _, row := range n.Content {
			w := 0
			for _, c := range row.Content {
				w += span(c, "colspan")
			}
			if cols >= 0 && w != cols {
				add("ragged table rows", false)
			}
			cols = w
		}
	}
}

func cellFeatures(n *Node, add func(string, bool)) {
	for k := range n.Attrs {
		switch k {
		case "colwidth":
			add("table column widths", true)
		case "background":
			add("cell colors", true)
		case "valign":
			add("cell alignment", true)
		case "colspan", "rowspan":
			if f, ok := n.attrNum(k); !ok || f != 1 {
				add("merged cells", true)
			}
		}
	}
	paras := 0
	for _, c := range n.Content {
		if c.Type != "paragraph" {
			add("blocks inside table cells", true)
			return
		}
		paras++
	}
	if paras > 1 {
		add("paragraph breaks in table cells", false)
	}
}

// midWordEmphasis finds bold, italic or strikethrough that starts (or
// ends) with punctuation right after (or before) a letter or digit,
// which CommonMark can't express.
func midWordEmphasis(ns []*Node) bool {
	edgeRune := func(n *Node, last bool) rune {
		if n == nil || n.Type != "text" || n.Text == "" {
			return ' '
		}
		rs := []rune(n.Text)
		if last {
			return rs[len(rs)-1]
		}
		return rs[0]
	}
	for i, n := range ns {
		if n.Type != "text" {
			continue
		}
		var prev, next *Node
		if i > 0 {
			prev = ns[i-1]
		}
		if i+1 < len(ns) {
			next = ns[i+1]
		}
		for _, m := range n.Marks {
			if !flanking(m) {
				continue
			}
			if (prev == nil || !findMark(prev.Marks, m)) &&
				isPunctRune(edgeRune(n, false)) &&
				isAlnum(edgeRune(prev, true)) {
				return true
			}
			if (next == nil || !findMark(next.Marks, m)) &&
				isPunctRune(edgeRune(n, true)) &&
				isAlnum(edgeRune(next, false)) {
				return true
			}
		}
	}
	return false
}

// inlineLoss adds a feature when n's inline content renders only by
// dropping a mark no escaping lets Markdown show where it stands.
func inlineLoss(n, parent *Node, add func(string, bool)) {
	ctx := ctxPara
	switch {
	case n.Type == "heading":
		ctx = ctxHeading
	case n.Type == "taskItem":
		ctx = ctxTask
	case parent != nil && (parent.Type == "tableCell" || parent.Type == "tableHeader"):
		ctx = ctxCell
	}
	if _, dropped := renderVerified(inlineItems(n.Content, ctx), ctx, true); dropped {
		add("formatting Markdown can't show", true)
	}
}
