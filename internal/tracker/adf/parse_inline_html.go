package adf

import (
	"fmt"
	"strings"
)

var tagRe = lazyRegexp{pattern: `^<(/?)([A-Za-z][A-Za-z0-9]*)\s*(/?)>$`}

var lineBreakRe = lazyRegexp{pattern: `\r?\n`}

// tagNameRe is the element name of a tag, with or without attributes.
var tagNameRe = lazyRegexp{pattern: `^</?([A-Za-z][A-Za-z0-9]*)`}

// htmlHints names the Markdown for HTML elements people paste, which are
// kept as literal text.
var htmlHints = lazy[map[string]string]{build: func() map[string]string {
	return map[string]string{
		"b": "**bold**", "strong": "**bold**", "i": "*italics*", "em": "*italics*",
		"a": "[text](url)", "img": "![](path)", "pre": "a ``` fence",
		"code": "`code`", "s": "~~strike~~", "del": "~~strike~~", "strike": "~~strike~~",
		"ul": "- items", "ol": "1. items", "li": "- items",
		"table": "a | table |", "tr": "a | table |", "td": "a | table |", "th": "a | table |",
		"h1": "# headings", "h2": "# headings", "h3": "# headings", "h4": "# headings",
		"h5": "# headings", "h6": "# headings",
		"p": "blank lines between paragraphs", "div": "blank lines between paragraphs",
		"span": "plain text",
	}
}}

// htmlAsText warns, once per element, that a pasted HTML tag such as <b>
// stays literal text, and names the Markdown to use instead.
func (w *inlineWalk) htmlAsText(s string, line int) {
	m := tagNameRe.get().FindStringSubmatch(s)
	if m == nil {
		return
	}
	name := strings.ToLower(m[1])
	hint, ok := htmlHints.get()[name]
	if !ok {
		return
	}
	msg := fmt.Sprintf("<%s> is kept as text; use %s", name, hint)
	for _, o := range w.c.st.warns {
		if o.Msg == msg {
			return
		}
	}
	w.c.warn(line, "%s", msg)
}

// literalTag is tag-shaped text kept as text. A soft line break inside it
// (<T extends\nComparable>) is a space, as it is anywhere in a paragraph.
func literalTag(s string) string {
	return lineBreakRe.get().ReplaceAllString(s, " ")
}

// rawHTML maps the inline tags ADF can hold (<u>, <ins>, <sub>, <sup>,
// <br>) to marks and breaks, comments to markers, and keeps any other
// tag-shaped text, such as the generic in List<String>, as literal text.
func (w *inlineWalk) rawHTML(s string, line int) error {
	if strings.HasPrefix(s, "<!--") {
		body := strings.TrimSuffix(strings.TrimPrefix(s, "<!--"), "-->")
		w.out = append(w.out, &Node{Type: tMarker,
			Attrs: map[string]any{"text": strings.TrimSpace(body)}})
		return nil
	}
	m := tagRe.get().FindStringSubmatch(s)
	if m == nil {
		w.htmlAsText(s, line)
		w.text(literalTag(s), line, false)
		return nil
	}
	name := strings.ToLower(m[2])
	closing, selfClosing := m[1] == "/", m[3] == "/"
	var mark Mark
	switch name {
	case "br":
		if closing {
			return errAt(line, "</br> is not valid; use <br>")
		}
		w.out = append(w.out, &Node{Type: "hardBreak"})
		return nil
	case "u", "ins":
		mark = Mark{Type: "underline"}
	case "sub", "sup":
		mark = Mark{Type: "subsup", Attrs: map[string]any{"type": name}}
	default:
		// Prose such as List<String> is text, not a tag to reject.
		w.htmlAsText(s, line)
		w.text(literalTag(s), line, false)
		return nil
	}
	if selfClosing {
		return errAt(line, "%s is empty; wrap text in <%s>...</%s>", s,
			name, name)
	}
	if !closing {
		w.html = append(w.html, htmlTag{name, mark, line})
		return nil
	}
	k := len(w.html) - 1
	if k < 0 || w.html[k].name != name {
		return errAt(line, "</%s> without a matching <%s>", name, name)
	}
	w.html = w.html[:k]
	return nil
}
