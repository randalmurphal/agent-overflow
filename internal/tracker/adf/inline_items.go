package adf

import (
	"cmp"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

type tokKind uint8

const (
	tkText  tokKind = iota // text, escaped as needed
	tkRaw                  // markup written as is
	tkCode                 // inline code
	tkBreak                // hard break
	tkOpen                 // mark start
	tkClose                // mark end
)

type item struct {
	kind  tokKind
	s     string
	marks []Mark
}

// renderable lists the marks Markdown can show; the rest are dropped.
var renderable = lazy[map[string]bool]{build: func() map[string]bool {
	return map[string]bool{
		"link": true, "strong": true, "em": true, "strike": true,
		"underline": true, "subsup": true,
	}
}}

func inlineItems(ns []*Node, ctx inlineCtx) []item {
	var out []item
	raw := func(s string) { out = append(out, item{kind: tkRaw, s: s}) }
	txt := func(s string) { out = append(out, item{kind: tkText, s: s}) }
	for _, n := range ns {
		switch n.Type {
		case "text":
			var ms []Mark
			for _, m := range n.Marks {
				if renderable.get()[m.Type] {
					ms = append(ms, m)
				}
			}
			kind := tkText
			if hasMark(n, "code") {
				kind = tkCode
			}
			out = append(out, item{kind: kind, s: newlinesToSpaces(n.Text),
				marks: ms})
		case "hardBreak":
			out = append(out, item{kind: tkBreak})
		case "mention":
			if strings.TrimSpace(n.attrStr("id")) == "" {
				// @[accountid:] would not parse; keep what it showed.
				txt(cmp.Or(n.attrStr("text"), "@"))
			} else {
				raw(mentionMarkdown(n, ctx))
			}
		case "emoji":
			if t := emojiText(n); t != "" {
				txt(t)
			}
		case "status":
			// As stored: a lozenge typed "[In review]" reads back as typed.
			txt("[" + n.attrStr("text") + "]")
		case "date":
			txt(formatDate(n.attrStr("timestamp")))
		case "placeholder":
		case "inlineCard":
			raw(cardMarkdown(n, ctx))
		case "mediaInline":
			raw(cellPipes(mediaLink(n), ctx))
		case "inlineExtension":
			raw(macroComment(n))
		default:
			if len(n.Content) > 0 {
				out = append(out, inlineItems(n.Content, ctx)...)
			} else {
				raw("<!-- adf: " + commentSafe(n.Type) + " -->")
			}
		}
	}
	return trimItems(mergeItems(out))
}

var (
	newlineSpacer = lazy[*strings.Replacer]{build: func() *strings.Replacer {
		return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")
	}}
	bracketEscaper = lazy[*strings.Replacer]{build: func() *strings.Replacer {
		return strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`)
	}}
	titleEscaper = lazy[*strings.Replacer]{build: func() *strings.Replacer {
		return strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	}}
)

// newlinesToSpaces replaces line breaks inside a text node, which
// Markdown can't keep, with spaces.
func newlinesToSpaces(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	return newlineSpacer.get().Replace(s)
}

func cellPipes(s string, ctx inlineCtx) string {
	if ctx == ctxCell {
		return strings.ReplaceAll(s, "|", `\|`)
	}
	return s
}

func mentionMarkdown(n *Node, ctx inlineCtx) string {
	name := mentionName(n)
	if name == "" {
		name = "accountid:" + n.attrStr("id")
	}
	return cellPipes("@["+bracketEscaper.get().Replace(name)+"]", ctx)
}

// mentionName is the name @[...] is written with: the display text
// without its @, on one line and trimmed as the write path reads it.
func mentionName(n *Node) string {
	name := strings.TrimPrefix(n.attrStr("text"), "@")
	return strings.Join(strings.Fields(name), " ")
}

var autolinkRe = lazyRegexp{pattern: `^[A-Za-z][A-Za-z0-9+.-]{1,31}:[^\s<>]*$`}

func cardMarkdown(n *Node, ctx inlineCtx) string {
	u := n.attrStr("url")
	if u == "" {
		return "<!-- adf: inlineCard -->"
	}
	if autolinkRe.get().MatchString(u) && (ctx != ctxCell ||
		!strings.Contains(u, "|")) {
		return "<" + u + ">"
	}
	label := bracketEscaper.get().Replace(u)
	return cellPipes("["+label+"]("+linkDest(u, ctx == ctxCell)+")", ctx)
}

func mergeItems(in []item) []item {
	var out []item
	for _, it := range in {
		k := len(out) - 1
		if k >= 0 && it.kind == out[k].kind &&
			(it.kind == tkText || it.kind == tkCode) &&
			marksEqual(it.marks, out[k].marks) {
			out[k].s += it.s
			continue
		}
		out = append(out, it)
	}
	return out
}

// trimItems drops whitespace at line edges, which Markdown can't keep.
func trimItems(in []item) []item {
	out := append([]item(nil), in...)
	edge := func(i int) bool {
		return i < 0 || i >= len(out) || out[i].kind == tkBreak
	}
	for i := range out {
		if out[i].kind != tkText {
			continue
		}
		if edge(i - 1) {
			out[i].s = strings.TrimLeft(out[i].s, " \t")
		}
		if edge(i + 1) {
			out[i].s = strings.TrimRight(out[i].s, " \t")
		}
	}
	keep := out[:0]
	for _, it := range out {
		if it.kind == tkText && it.s == "" {
			continue
		}
		keep = append(keep, it)
	}
	if len(keep) != len(in) {
		return trimItems(mergeItems(keep))
	}
	return keep
}

// emojiText is what an emoji shows: its text, with \uXXXX escapes (some
// stored text holds "\ud83d\udccb" for 📋) decoded, or its short name
// when the text is empty or holds an escape that names no character.
func emojiText(n *Node) string {
	t := n.attrStr("text")
	if strings.Contains(t, `\u`) {
		d, ok := decodeUEscapes(t)
		if !ok {
			return n.attrStr("shortName")
		}
		t = d
	}
	return cmp.Or(t, n.attrStr("shortName"))
}

// decodeUEscapes decodes the \uXXXX escapes in s, surrogate pairs
// included; ok is false when one is malformed or a lone surrogate.
func decodeUEscapes(s string) (string, bool) {
	var b strings.Builder
	hex := func(i int) (rune, bool) {
		if i+6 > len(s) || s[i] != '\\' || s[i+1] != 'u' {
			return 0, false
		}
		v, err := strconv.ParseUint(s[i+2:i+6], 16, 16)
		return rune(v), err == nil
	}
	for i := 0; i < len(s); {
		if !strings.HasPrefix(s[i:], `\u`) {
			b.WriteByte(s[i])
			i++
			continue
		}
		r, ok := hex(i)
		if !ok {
			return "", false
		}
		i += 6
		if utf16.IsSurrogate(r) {
			lo, ok := hex(i)
			if r = utf16.DecodeRune(r, lo); !ok || r == utf8.RuneError {
				return "", false
			}
			i += 6
		}
		b.WriteRune(r)
	}
	return b.String(), true
}
