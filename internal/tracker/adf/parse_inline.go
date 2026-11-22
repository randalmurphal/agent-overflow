package adf

import (
	"strconv"
	"strings"
	"unicode/utf8"

	gast "github.com/yuin/goldmark/ast"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/util"
)

// inlineWalk converts one run of goldmark inline nodes. Marks come from
// two stacks: Markdown structure (emphasis, links) and the inline HTML
// tags we accept (<u>, <sub>, <sup>), which goldmark leaves as siblings.
type inlineWalk struct {
	c     *converter
	cut   int // skip source before this offset (an alert's first line)
	marks []Mark
	html  []htmlTag
	out   []*Node
}

type htmlTag struct {
	name string
	mark Mark
	line int
}

func (c *converter) inlines(n gast.Node, cut int) ([]*Node, error) {
	w := &inlineWalk{c: c, cut: cut}
	if err := w.children(n); err != nil {
		return nil, err
	}
	if k := len(w.html); k > 0 {
		t := w.html[k-1]
		return nil, errAt(t.line, "<%s> is never closed", t.name)
	}
	return mergeText(w.out), nil
}

func (w *inlineWalk) children(n gast.Node) error {
	for ch := n.FirstChild(); ch != nil; ch = ch.NextSibling() {
		if err := w.node(ch); err != nil {
			return err
		}
	}
	return nil
}

func (w *inlineWalk) node(n gast.Node) error {
	c := w.c
	line := c.lineOf(n)
	if w.cut > 0 {
		if t, ok := n.(*gast.Text); ok {
			if t.Segment.Stop <= w.cut {
				return nil
			}
		} else if p := n.Pos(); p >= 0 && p < w.cut {
			return nil
		}
	}
	switch n := n.(type) {
	case *gast.Text:
		seg := n.Segment
		if seg.Start < w.cut {
			seg = seg.WithStart(w.cut)
		}
		text := unescape(seg.Value(c.src))
		if loneEndSlash(n, c.src) {
			// A hard break with nothing after it, as deleting the last
			// line of a \-continued block leaves: no text, so dropped.
			text = strings.TrimRight(strings.TrimSuffix(text, "\\"), " \t")
		}
		w.text(text, line, false)
		if n.HardLineBreak() {
			w.out = append(w.out, &Node{Type: "hardBreak"})
		} else if n.SoftLineBreak() {
			w.text(" ", line, false)
		}
	case *gast.String:
		w.text(string(n.Value), line, false)
	case *gast.CodeSpan:
		var b strings.Builder
		for ch := n.FirstChild(); ch != nil; ch = ch.NextSibling() {
			switch t := ch.(type) {
			case *gast.Text:
				b.Write(t.Segment.Value(c.src))
				if t.SoftLineBreak() {
					b.WriteByte(' ')
				}
			case *gast.String:
				b.Write(t.Value)
			}
		}
		w.text(b.String(), line, true)
	case *gast.Emphasis:
		m := Mark{Type: "em"}
		if n.Level >= 2 {
			m.Type = "strong"
		}
		return w.with(m, n)
	case *east.Strikethrough:
		return w.with(Mark{Type: "strike"}, n)
	case *gast.Link:
		return w.link(n, line)
	case *gast.Image:
		return w.image(n, line)
	case *gast.AutoLink:
		w.autoLink(n, line)
	case *gast.RawHTML:
		var b []byte
		for i := 0; i < n.Segments.Len(); i++ {
			seg := n.Segments.At(i)
			b = append(b, seg.Value(c.src)...)
		}
		return w.rawHTML(string(b), line)
	case *east.FootnoteLink:
		w.text("["+strconv.Itoa(n.Index)+"]", line, false)
	case *east.FootnoteBacklink, *east.TaskCheckBox:
	case *mentionNode:
		m, err := c.mention(n.who, line)
		if err != nil {
			return err
		}
		w.out = append(w.out, c.at(m, line))
	default:
		return errAt(line, "unsupported Markdown (%s)", n.Kind())
	}
	return nil
}

func (w *inlineWalk) with(m Mark, n gast.Node) error {
	w.marks = append(w.marks, m)
	err := w.children(n)
	w.marks = w.marks[:len(w.marks)-1]
	return err
}

func (w *inlineWalk) text(s string, line int, code bool) {
	if s == "" {
		return
	}
	w.out = append(w.out, textNode(s, w.active(line, code)...))
}

var markNames = lazy[map[string]string]{build: func() map[string]string {
	return map[string]string{
		"strong": "bold", "em": "italic", "strike": "struck through",
		"underline": "underlined", "subsup": "sub/superscript",
	}
}}

// active returns the marks in force. Inline code combines only with
// link in Jira, so anything else is dropped with a warning.
func (w *inlineWalk) active(line int, code bool) []Mark {
	var ms []Mark
	seen := map[string]bool{}
	add := func(m Mark) {
		if !seen[m.Type] {
			seen[m.Type] = true
			ms = append(ms, m)
		}
	}
	for _, m := range w.marks {
		add(m)
	}
	for _, h := range w.html {
		add(h.mark)
	}
	if code {
		kept := []Mark{{Type: "code"}}
		var dropped []string
		for _, m := range ms {
			if m.Type == "link" {
				kept = append(kept, m)
			} else {
				dropped = append(dropped, markNames.get()[m.Type])
			}
		}
		if len(dropped) > 0 {
			w.c.warn(line, "inline code can't also be %s in Jira; kept "+
				"only the code formatting", strings.Join(dropped, " or "))
		}
		ms = kept
	}
	sortMarks(ms)
	return ms
}

func (w *inlineWalk) link(n *gast.Link, line int) error {
	dest := unescape(n.Destination)
	if id, ok := strings.CutPrefix(dest, "media:"); ok && id != "" {
		alt := plainInline(n, w.c.src)
		mi := &Node{Type: "mediaInline", Attrs: map[string]any{
			"type": "file", "id": id, "collection": w.c.st.coll,
		}}
		if alt != "" && alt != "attachment" {
			mi.Attrs["alt"] = alt
		}
		w.out = append(w.out, mi)
		return nil
	}
	m := Mark{Type: "link", Attrs: map[string]any{"href": dest}}
	if len(n.Title) > 0 {
		m.Attrs["title"] = unescape(n.Title)
	}
	return w.with(m, n)
}

func (w *inlineWalk) image(n *gast.Image, line int) error {
	dest := unescape(n.Destination)
	media := &Node{Type: "media"}
	lower := strings.ToLower(dest)
	switch id, isRef := strings.CutPrefix(dest, "media:"); {
	case isRef:
		if id == "" {
			return errAt(line, "media: reference needs an ID")
		}
		media.Attrs = map[string]any{"type": "file", "id": id,
			"collection": w.c.st.coll}
	case strings.HasPrefix(dest, externalPrefix):
		url := strings.TrimPrefix(dest, externalPrefix)
		if url == "" {
			return errAt(line, "external: image needs a URL")
		}
		media.Attrs = map[string]any{"type": "external", "url": url}
	case strings.HasPrefix(lower, "http://") ||
		strings.HasPrefix(lower, "https://") || hasScheme(dest):
		// Any URL is external; only a scheme-less path is a local file.
		media.Attrs = map[string]any{"type": "external", "url": dest}
	default:
		id, err := w.c.upload(dest, line)
		if err != nil {
			return err
		}
		media.Attrs = map[string]any{"type": "file", "id": id,
			"collection": w.c.st.coll}
		w.c.at(media, line)
		if id == pendingID {
			media.Attrs[attrPath] = dest
		}
	}
	if alt := plainInline(n, w.c.src); alt != "" {
		media.Attrs["alt"] = alt
	}
	ms := &Node{Type: "mediaSingle", Attrs: map[string]any{
		"layout": "center"}, Content: []*Node{media}}
	for _, m := range w.marks {
		if m.Type == "link" {
			ms.Marks = []Mark{m}
		}
	}
	w.c.at(ms, line)
	w.out = append(w.out, &Node{Type: tImage, Content: []*Node{ms}})
	return nil
}

// autoLink maps <url> to an inline smart link and a bare URL or email
// (found by linkify) to an ordinary link.
func (w *inlineWalk) autoLink(n *gast.AutoLink, line int) {
	src := w.c.src
	url := string(n.URL(src))
	label := string(n.Label(src))
	p := n.Pos()
	explicit := p >= 0 && p < len(src) && src[p] == '<'
	if explicit && n.AutoLinkType == gast.AutoLinkURL {
		w.out = append(w.out, &Node{Type: "inlineCard",
			Attrs: map[string]any{"url": url}})
		return
	}
	href := url
	if n.AutoLinkType == gast.AutoLinkEmail &&
		!strings.HasPrefix(strings.ToLower(href), "mailto:") {
		href = "mailto:" + href
	}
	w.marks = append(w.marks, Mark{Type: "link",
		Attrs: map[string]any{"href": href}})
	w.text(label, line, false)
	w.marks = w.marks[:len(w.marks)-1]
}

// plainInline returns the text inside an inline node (an image's alt).
func plainInline(n gast.Node, src []byte) string {
	var b strings.Builder
	_ = gast.Walk(n, func(ch gast.Node, entering bool) (gast.WalkStatus,
		error) {
		if !entering {
			return gast.WalkContinue, nil
		}
		switch t := ch.(type) {
		case *gast.Text:
			if t.IsRaw() {
				b.Write(t.Segment.Value(src))
			} else {
				b.WriteString(unescape(t.Segment.Value(src)))
			}
			if t.SoftLineBreak() || t.HardLineBreak() {
				b.WriteByte(' ')
			}
		case *gast.String:
			b.Write(t.Value)
		case *gast.AutoLink:
			b.Write(t.Label(src))
		}
		return gast.WalkContinue, nil
	})
	return strings.TrimSpace(b.String())
}

// unescape resolves backslash escapes and character references the way
// CommonMark does for text.
func unescape(b []byte) string {
	if !strings.ContainsAny(string(b), "\\&\x00") {
		return string(b)
	}
	var out strings.Builder
	for i := 0; i < len(b); i++ {
		ch := b[i]
		switch {
		case ch == '\\' && i+1 < len(b) && isASCIIPunct(b[i+1]):
			out.WriteByte(b[i+1])
			i++
		case ch == '&':
			if r, n := charRef(b[i:]); n > 0 {
				out.WriteString(r)
				i += n - 1
			} else {
				out.WriteByte('&')
			}
		case ch == 0:
			out.WriteRune(utf8.RuneError)
		default:
			out.WriteByte(ch)
		}
	}
	return out.String()
}

var charRefRe = lazyRegexp{pattern: `^&(?:#[xX]([0-9a-fA-F]{1,6});|#([0-9]{1,7});|([A-Za-z][A-Za-z0-9]*);)`}

// charRef decodes an entity or numeric reference at the start of b.
func charRef(b []byte) (string, int) {
	m := charRefRe.get().FindSubmatchIndex(b)
	if m == nil {
		return "", 0
	}
	num := func(s string, base int) string {
		v, err := strconv.ParseUint(s, base, 32)
		if err != nil || v == 0 || v > utf8.MaxRune {
			return string(utf8.RuneError)
		}
		return string(rune(v))
	}
	switch {
	case m[2] >= 0:
		return num(string(b[m[2]:m[3]]), 16), m[1]
	case m[4] >= 0:
		return num(string(b[m[4]:m[5]]), 10), m[1]
	}
	e, ok := util.LookUpHTML5EntityByName(string(b[m[6]:m[7]]))
	if !ok {
		return "", 0
	}
	return string(e.Characters), m[1]
}

func humanType(t string) string {
	switch t {
	case "listItem":
		return "a list item"
	case "blockquote":
		return "a quote"
	case "panel":
		return "an alert panel"
	case "expand", "nestedExpand":
		return "a <details> section"
	case "tableCell", "tableHeader":
		return "a table cell"
	case "taskItem":
		return "a checklist item"
	case "doc":
		return "the document"
	}
	return "a " + t
}

// loneEndSlash reports whether text n ends its block with a backslash
// that escapes nothing (an odd run of them in src), which CommonMark reads
// as a literal backslash but ToMarkdown never writes there: it is a hard
// break whose following line was deleted.
func loneEndSlash(n *gast.Text, src []byte) bool {
	if n.NextSibling() != nil || n.HardLineBreak() || n.SoftLineBreak() ||
		n.Parent() == nil || n.Parent().Type() != gast.TypeBlock {
		return false
	}
	k := n.Segment.Stop
	for k > n.Segment.Start && src[k-1] == '\\' {
		k--
	}
	run := n.Segment.Stop - k
	for j := k; j > 0 && src[j-1] == '\\'; j-- {
		run++
	}
	return run%2 == 1
}
