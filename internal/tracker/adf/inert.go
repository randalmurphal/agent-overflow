package adf

import (
	"strings"
	"unicode/utf8"
)

// inert reports whether toks, written with minimal escaping, are sure to
// parse the same as fully escaped text, so renderInline can skip its
// parse check. Minimal and full output differ only in backslashes before
// ASCII punctuation in text (and in bare links, which are refused), so
// it suffices that each such character is one Markdown can't read as
// syntax where it stands. Among raw tokens only mentions and <url> smart
// links are accepted. Anything unsure goes through the check;
// TestInertShortcut confirms the shortcut never changes output.
func inert(toks []tok, ctx inlineCtx) bool {
	oneLine := true
	for _, t := range toks {
		if t.kind == tkBreak {
			oneLine = false
		}
	}
	lineStart := ctx == ctxPara
	for _, t := range toks {
		switch t.kind {
		case tkText:
			if !inertText(t, lineStart, oneLine, ctx) {
				return false
			}
		case tkRaw:
			if !plainMention(t.s) && !plainCard(t.s) {
				return false
			}
		case tkBreak:
			lineStart = true
			continue
		}
		lineStart = false
	}
	return true
}

func inertText(t tok, lineStart, oneLine bool, ctx inlineCtx) bool {
	s := t.s
	if lineStart && mayStartBlock(s) ||
		strings.Contains(s, "://") || strings.Contains(s, "www.") {
		return false
	}
	last := len(s) - 1
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '$', '%', '\'', '(', ')', '+', ',', '-', '.', '/', ':',
			';', '=', '>', '?', '{', '}':
			// syntax only at a line start, or with characters refused here
		case '#': // a closing sequence in a heading
			if ctx == ctxHeading {
				return false
			}
		case '!': // ![ starts an image
			if i == last {
				return false
			}
		case '^': // [^ starts a footnote reference
			if i == 0 {
				return false
			}
		case '&': // a character reference
			if i == last || s[i+1] == '#' || isASCIIAlnum(s[i+1]) {
				return false
			}
		case '|': // a table needs a second line; a cell ends at one
			if ctx == ctxCell || !oneLine {
				return false
			}
		case '_':
			k := i
			for k < len(s) && s[k] == '_' {
				k++
			}
			if !intraword(s, i, k) {
				return false
			}
			i = k - 1
		case '[', ']':
			// Brackets make a link only when ]( or ][ follows, or with a
			// reference definition, which ToMarkdown never writes. A
			// bracket at the edge could pair with a neighbor token.
			if t.inLink || i == last || strings.Contains(s, "](") ||
				strings.Contains(s, "][") {
				return false
			}
		default:
			if isASCIIPunct(s[i]) {
				return false
			}
		}
	}
	return true
}

// intraword reports whether the run s[i:k] sits between letters or
// digits, where an underscore run can neither open nor close emphasis.
func intraword(s string, i, k int) bool {
	if i == 0 || k == len(s) {
		return false
	}
	before, _ := utf8.DecodeLastRuneInString(s[:i])
	after, _ := utf8.DecodeRuneInString(s[k:])
	return isAlnum(before) && isAlnum(after)
}

// mayStartBlock reports whether s, at a line start, could begin block
// syntax: a heading, quote, list, rule, setext underline, fence, HTML
// block, table delimiter row or reference definition.
func mayStartBlock(s string) bool {
	if s == "" {
		return false
	}
	if strings.IndexByte("#>-+*=_`~<|[:\\", s[0]) >= 0 {
		return true
	}
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 || i == len(s) || s[i] != '.' && s[i] != ')' {
		return false
	}
	return i+1 == len(s) || s[i+1] == ' ' || s[i+1] == '\t'
}

// plainMention reports whether raw is a mention as mentionMarkdown
// writes it, with a name that parses back.
func plainMention(raw string) bool {
	name, ok := strings.CutPrefix(raw, "@[")
	if !ok {
		return false
	}
	name, ok = strings.CutSuffix(name, "]")
	if !ok || strings.TrimSpace(name) == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		if name[i] < ' ' || name[i] == 0x7f {
			return false
		}
	}
	return true
}

// plainCard reports whether raw is a smart link written as an autolink.
func plainCard(raw string) bool {
	u, ok := strings.CutPrefix(raw, "<")
	if !ok {
		return false
	}
	u, ok = strings.CutSuffix(u, ">")
	return ok && autolinkRe.get().MatchString(u)
}

func isASCIIAlnum(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}
