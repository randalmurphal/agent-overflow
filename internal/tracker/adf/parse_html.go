package adf

import (
	"strings"

	gast "github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// HTML blocks may hold only <details>, <summary>, </details>, comments
// and, alone on a line, <br> tags. Markdown between those tags (written without the blank lines
// CommonMark needs to end an HTML block) is parsed as Markdown.
var htmlTokRe = lazyRegexp{pattern: `(?is)<!--(.*?)-->|` +
	`<details(?:\s[^>]*)?>\s*(?:<summary>(.*?)</summary>)?|</details\s*>`}

// brOnlyRe is a line of nothing but <br> tags: CommonMark reads it as an
// HTML block, but it is a paragraph of hard breaks.
var brOnlyRe = lazyRegexp{pattern: `(?i)^[ \t]*(<br\s*/?>[ \t]*)+\n?$`}

// brStartRe is an HTML block that starts with a <br> tag: CommonMark
// reads it, and every line up to a blank one, as HTML, but it is a
// paragraph that starts with hard breaks.
var brStartRe = lazyRegexp{pattern: `(?i)^[ \t]*<br\s*/?>`}

// brLineEndRe is a line end right after a <br>: the break is the line
// break, so the newline adds no space.
var brLineEndRe = lazyRegexp{pattern: `(?i)(<br\s*/?>)[ \t]*\r?\n`}

// blockStartRe is a line that starts a block of its own (a list item, a
// heading, a quote, a fence), which a <br> line end must not pull into
// the paragraph.
var blockStartRe = lazyRegexp{pattern: "^[ \t]*([-*+][ \t]|[0-9]{1,9}[.)][ \t]|" +
	"#{1,6}([ \t]|$)|>|```|~~~)"}

// joinBrLines drops each line end after a <br>, except before a line
// that starts a block: a list nested under a break-only item stays a list.
func joinBrLines(s string) string {
	var b strings.Builder
	rest := s
	for {
		m := brLineEndRe.get().FindStringSubmatchIndex(rest)
		if m == nil {
			b.WriteString(rest)
			return b.String()
		}
		next := rest[m[1]:]
		if blockStartRe.get().MatchString(next) {
			b.WriteString(rest[:m[1]])
		} else {
			b.WriteString(rest[:m[3]])
		}
		rest = next
	}
}

func (c *converter) htmlBlock(n *gast.HTMLBlock) ([]*Node, error) {
	var b strings.Builder
	lines := n.Lines()
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		b.Write(seg.Value(c.src))
	}
	if n.HasClosure() {
		closure := n.ClosureLine
		b.Write(closure.Value(c.src))
	}
	raw := b.String()
	start := 0
	if lines.Len() > 0 {
		start = lines.At(0).Start
	}
	// A comment in the block is a marker (a macro, a section); the <br>
	// text before it goes through htmlChunk, which takes this path again
	// without the comment.
	if brStartRe.get().MatchString(raw) && !strings.Contains(raw, "<!--") {
		return c.brParagraph(raw, start)
	}
	var out []*Node
	pos := 0
	for _, m := range htmlTokRe.get().FindAllStringSubmatchIndex(raw, -1) {
		chunk, err := c.htmlChunk(raw[pos:m[0]], start+pos, raw)
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		tokLine := c.lineAt(start + m[0])
		tok := raw[m[0]:m[1]]
		switch {
		case m[2] >= 0:
			if body := strings.TrimSpace(raw[m[2]:m[3]]); body != "" {
				out = append(out, c.at(&Node{Type: tMarker,
					Attrs: map[string]any{"text": body}}, tokLine))
			}
		case strings.HasPrefix(tok, "</"):
			out = append(out, c.at(&Node{Type: tDetailsClose}, tokLine))
		default:
			open := &Node{Type: tDetailsOpen}
			if m[4] >= 0 {
				title := strings.TrimSpace(unescape([]byte(raw[m[4]:m[5]])))
				open.Attrs = map[string]any{"title": title}
			}
			out = append(out, c.at(open, tokLine))
		}
		pos = m[1]
	}
	chunk, err := c.htmlChunk(raw[pos:], start+pos, raw)
	if err != nil {
		return nil, err
	}
	return append(out, chunk...), nil
}

// brParagraph parses an HTML block that starts with <br> as the paragraph
// it is: line ends after a <br> drop, and a leading encoded space (which
// the paragraph trims) stops the line from reading as HTML again. CRLF
// endings, <br> lines with no blank line between them, and text after a
// <br> all work.
func (c *converter) brParagraph(raw string, start int) ([]*Node, error) {
	s := joinBrLines(strings.TrimRight(raw, "\r\n"))
	sub := newSubConverter(c.ctx, "&#32;"+s+"\n", c.r, c.deferR, c.st,
		c.lineAt(start)-1)
	root := markdownParser().Parse(text.NewReader(sub.src))
	var out []*Node
	for ch := root.FirstChild(); ch != nil; ch = ch.NextSibling() {
		ns, err := sub.block(ch)
		if err != nil {
			return nil, err
		}
		out = append(out, ns...)
	}
	return out, nil
}

// htmlChunk parses non-tag text found inside an HTML block.
func (c *converter) htmlChunk(s string, off int, whole string) ([]*Node,
	error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	if s == whole || strings.Contains(s, "<summary") {
		first := strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
		return nil, errAt(c.lineAt(off+strings.Index(s, first)),
			"raw HTML %q is not supported; HTML blocks may only be "+
				"<details>, <summary> or comments", clip(first, 40))
	}
	sub := newSubConverter(c.ctx, s, c.r, c.deferR, c.st,
		c.lineAt(off)-1)
	root := markdownParser().Parse(text.NewReader(sub.src))
	var out []*Node
	for ch := root.FirstChild(); ch != nil; ch = ch.NextSibling() {
		ns, err := sub.block(ch)
		if err != nil {
			return nil, err
		}
		out = append(out, ns...)
	}
	return out, nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
