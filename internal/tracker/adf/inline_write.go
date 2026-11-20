package adf

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// emitTokens renders tokens built by markTokens at the same level.
func emitTokens(toks []tok, ctx inlineCtx, lvl escLevel) string {
	w := &writer{toks: toks, ctx: ctx, lvl: lvl}
	w.countRuns()
	return guardHTMLLines(w.write(), ctx)
}

var (
	tagsOnly = lazyRegexp{pattern: `^(\s*(</?[A-Za-z][A-Za-z0-9-]*` +
		`(\s[^>]*)?/?>|<!--.*?-->))+\s*$`}
	htmlBlockish = lazyRegexp{pattern: `^(<!--|<\?|<![A-Za-z]|<!\[CDATA\[)`}
)

// guardHTMLLines keeps a line that holds only tags (or starts a comment)
// from being read as an HTML block, by starting it with an encoded space,
// which the write path trims. A paragraph of only <br> tags needs no
// guard: the write path reads that HTML block as hard breaks.
func guardHTMLLines(s string, ctx inlineCtx) string {
	if ctx == ctxCell || ctx == ctxHeading || !strings.Contains(s, "<") ||
		brOnlyRe.get().MatchString(s) {
		return s
	}
	ls := strings.Split(s, "\n")
	for i, l := range ls {
		first := i == 0 && ctx == ctxPara
		if (first && tagsOnly.get().MatchString(l)) || (i > 0 || first) &&
			htmlBlockish.get().MatchString(l) {
			ls[i] = "&#32;" + l
		}
	}
	return strings.Join(ls, "\n")
}

type writer struct {
	toks []tok
	ctx  inlineCtx
	lvl  escLevel
	b    strings.Builder
	runs [3]int // delimiter-capable runs of * _ ~ (delimSlot)
	tick int
}

func (w *writer) write() string {
	lastContent := -1
	firstContent := len(w.toks)
	for i, t := range w.toks {
		if t.kind != tkBreak && t.kind != tkOpen && t.kind != tkClose {
			lastContent = i
			firstContent = min(firstContent, i)
		}
	}
	lineStart := w.ctx == ctxPara
	linkStart := false // the previous token opened a link at a line start
	for i, t := range w.toks {
		opensLink := lineStart && t.kind == tkOpen && t.mark.Type == "link"
		switch t.kind {
		case tkText:
			w.b.WriteString(w.escape(i, lineStart, linkStart))
		case tkRaw:
			w.b.WriteString(t.s)
		case tkCode:
			w.b.WriteString(codeSpan(t.s, w.ctx == ctxCell))
		case tkOpen, tkClose:
			w.b.WriteString(w.delim(i))
		case tkBreak:
			// goldmark misreads "\\\" at a line end, so a break after
			// a backslash is written as <br> too.
			if w.ctx == ctxHeading || w.ctx == ctxCell ||
				i < firstContent || i > lastContent || w.lastRune() == '\\' {
				w.b.WriteString("<br>")
			} else {
				w.b.WriteString("\\\n")
				lineStart, linkStart = true, false
				continue
			}
		}
		lineStart, linkStart = false, opensLink
	}
	return w.b.String()
}

func (w *writer) lastRune() rune {
	s := w.b.String()
	if s == "" {
		return '\n'
	}
	r, _ := utf8.DecodeLastRuneInString(s)
	return r
}

// firstRune is the first character token i will write.
func (w *writer) firstRune(i int) rune {
	if i >= len(w.toks) {
		return '\n'
	}
	t := w.toks[i]
	switch t.kind {
	case tkText, tkRaw:
		r, _ := utf8.DecodeRuneInString(t.s)
		return r
	case tkCode:
		return '`'
	case tkBreak:
		if w.ctx == ctxHeading || w.ctx == ctxCell {
			return '<'
		}
		return '\\'
	case tkOpen:
		return rune(w.openText(i)[0])
	case tkClose:
		return rune(w.closeText(i)[0])
	}
	return '\n'
}

func (w *writer) delim(i int) string {
	if w.toks[i].kind == tkOpen {
		return w.openText(i)
	}
	return w.closeText(i)
}

func (w *writer) openText(i int) string {
	m := w.toks[i].mark
	switch m.Type {
	case "strong":
		return "**"
	case "em":
		return w.emDelim(i)
	case "strike":
		return "~~"
	case "underline":
		return "<u>"
	case "subsup":
		return "<" + subsupTag(m) + ">"
	case "link":
		return "["
	}
	return ""
}

func (w *writer) closeText(i int) string {
	m := w.toks[i].mark
	switch m.Type {
	case "strong":
		return "**"
	case "em":
		return w.emDelim(w.toks[i].pair)
	case "strike":
		return "~~"
	case "underline":
		return "</u>"
	case "subsup":
		return "</" + subsupTag(m) + ">"
	case "link":
		href, _ := m.Attrs["href"].(string)
		s := "](" + linkDest(href, w.ctx == ctxCell)
		if title, ok := m.Attrs["title"].(string); ok && title != "" {
			s += ` "` + titleEscaper.get().Replace(title) + `"`
		}
		return s + ")"
	}
	return ""
}

func subsupTag(m Mark) string {
	if t, _ := m.Attrs["type"].(string); t == "sup" {
		return "sup"
	}
	return "sub"
}

// emDelim picks _ for emphasis unless a neighbor is a letter or digit,
// where only * works. The neighbors come from the tokens, not from what
// has been written so far, because lookahead asks before the text in
// front of the delimiter is written.
func (w *writer) emDelim(open int) string {
	before := w.rawEdge(open-1, true)
	after := '\n'
	if closeAt := w.toks[open].pair; closeAt > open && closeAt+1 < len(w.toks) {
		next := w.toks[closeAt+1]
		if next.kind == tkText || next.kind == tkRaw {
			after, _ = utf8.DecodeRuneInString(next.s)
		}
	}
	if isAlnum(before) || isAlnum(after) {
		return "*"
	}
	return "_"
}

func isAlnum(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func codeSpan(s string, inCell bool) string {
	fence := strings.Repeat("`", longestRun(s, '`')+1)
	pad := ""
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") ||
		(strings.HasPrefix(s, " ") && strings.HasSuffix(s, " ") &&
			strings.Trim(s, " ") != "") {
		pad = " "
	}
	if inCell {
		s = strings.ReplaceAll(s, "|", `\|`)
	}
	return fence + pad + s + pad + fence
}

var needsAngle = lazyRegexp{pattern: `[\s<>]`}

// linkDest writes a link destination so it parses back unchanged.
func linkDest(href string, inCell bool) string {
	esc := func(s string, chars string) string {
		var b strings.Builder
		for i := 0; i < len(s); i++ {
			c := s[i]
			switch {
			case strings.IndexByte(chars, c) >= 0:
				b.WriteByte('\\')
			case c == '&' && entityAt(s[i:]):
				b.WriteByte('\\')
			case c == '|' && inCell:
				b.WriteByte('\\')
			}
			b.WriteByte(c)
		}
		return b.String()
	}
	if href == "" || needsAngle.get().MatchString(href) || !balanced(href) {
		return "<" + esc(strings.ReplaceAll(href, "\n", "%0A"), `\<>`) + ">"
	}
	return esc(href, `\`)
}

func balanced(s string) bool {
	depth := 0
	for _, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

func entityAt(s string) bool {
	_, n := charRef([]byte(s))
	return n > 0
}

// escapeAlt escapes image alt and attachment labels, which Markdown
// parses as inline text. Underscores inside words (file names) are safe.
func escapeAlt(s string) string {
	rs := []rune(strings.ReplaceAll(s, "\n", " "))
	var b strings.Builder
	for i, r := range rs {
		esc := strings.ContainsRune("\\[]*`<~|", r)
		switch r {
		case '_':
			esc = i == 0 || i == len(rs)-1 || !isAlnum(rs[i-1]) ||
				!isAlnum(rs[i+1])
		case '&':
			esc = entityAt(string(rs[i:]))
		}
		if esc {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
