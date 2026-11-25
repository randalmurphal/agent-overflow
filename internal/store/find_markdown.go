package store

import (
	"regexp"
	"strings"
)

// findShownMarkdown reads an assistant row's Markdown as the frontend's
// lexer does (marked's grammar with the chat extensions, frontend
// lib/markdown/parser) for the one question thread find's candidate filter
// asks (find_candidate.go): which spans of a run's source show no letters.
//
// It erases a span only in a shape the lexer reads the same way wherever
// the scan meets it (findLinkAt, findTagAt), and tracks what can hide such
// a shape or be hidden by it: escapes, code spans paired within their line,
// autolinks and bare URLs. Anything else the lexer could read differently
// around one (a bracket that may open a link across lines, math, sub- or
// superscript or strikethrough over markup, a tag the scan does not erase,
// a reference definition) makes the text unsure. Each such check has a case
// in frontend test/fixtures/markdownFindCorpus.ts that fails without it.
//
// Fenced code shows as written. Fences are read only when each opens at the
// margin and no line before can open an HTML or MDX block holding one,
// where marked reads them the same way; otherwise every line but a fence's
// own is scanned, and the summary variant holds the code's runs.

// findShownMarkdown is text with the spans its Markdown erases removed,
// and whether every run the frontend gives the text lies in the result or
// in the text as written.
func findShownMarkdown(text string) (string, bool) {
	if !findMayErase(text) {
		return text, true
	}
	if strings.Contains(text, "][") && findDefinitionLine.MatchString(text) {
		// A reference link resolves within its block and shows no label.
		return "", false
	}
	lines := strings.SplitAfter(text, "\n")
	asWritten := findLinesAsWritten(lines)
	var out strings.Builder
	out.Grow(len(text))
	start := -1
	flush := func(end int) bool {
		if start < 0 {
			return true
		}
		shown, ok := scanFindRegion(strings.Join(lines[start:end], ""))
		start = -1
		out.WriteString(shown)
		return ok
	}
	for i, line := range lines {
		if asWritten[i] || strings.Trim(line, " \t\r\n") == "" {
			if !flush(i) {
				return "", false
			}
			out.WriteString(line)
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if !flush(len(lines)) {
		return "", false
	}
	return out.String(), true
}

// jsSpace is the class JavaScript's \s matches, which the frontend's
// expressions use.
const jsSpace = `\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

var (
	// A text with none of these erases nothing: every erased span starts
	// with one, and a reference label needs a definition.
	findDefinitionLine = regexp.MustCompile(`(?m)^[ \t>]*(?:[-*+][ \t]+|[0-9]+[.)][ \t]+)?\[[^\]\n]+\]:`)

	// A line marked reads as a fence opener (engine/rules.ts `fences`): it
	// is never text of a paragraph, whatever holds it.
	findFenceLine = regexp.MustCompile("^( {0,3})(?:(`{3,})[^`]*|(~{3,}).*)$")
	// A prose line that can open a block holding the fences after it: HTML
	// or MDX.
	findFenceHolder = regexp.MustCompile(`^ {0,3}<[A-Za-z/!?]`)

	// The spans erased or kept whole wherever the scan meets them: a link
	// or image with plain text and destination, an HTML tag or comment, an
	// autolink. None holds a line break, a table cell edge, or a character
	// that opens or closes another construct.
	findLinkAt = regexp.MustCompile("^\\[([^\\[\\]\\\\`<>$~^|\\n]*)\\]\\((?:[^" + jsSpace + "()<>\\[\\]\\\\`\"'$~^|]|\\([^" + jsSpace + "()<>\\[\\]\\\\`\"'$~^|]*\\))*\\)")
	findTagAt  = regexp.MustCompile("^(?:<[a-z][\\w-]*(?:[ \\t]+[a-zA-Z:_][\\w.:-]*(?:[ \\t]*=[ \\t]*(?:\"[^\"<>\\n|]*\"|'[^'<>\\n|]*'|[^" + jsSpace + "\"'=<>`|]+))?)*[ \\t]*/?>" +
		"|</[a-zA-Z][\\w:-]*[ \\t]*>|<!--(?:-?>|[^<>\\n|]*?-->))")
	findAutolinkAt = regexp.MustCompile("^<[a-zA-Z][a-zA-Z0-9+.-]{1,31}:[^" + jsSpace + "<>|\\x00-\\x1f]*>")
	// What marked or the MDX extension may read as a tag or autolink at a
	// `<` (engine/rules.ts `tag` and `autolink`, extensions/mdx.ts), and a
	// line that ends inside one, which may close on a line whose quote or
	// list markers this does not strip.
	findMaybeTagAt = regexp.MustCompile("^(?:</[a-zA-Z][\\w:-]*[" + jsSpace + "]*>" +
		"|<[a-zA-Z][\\w-]*(?:[" + jsSpace + "]+[a-zA-Z:_][\\w.:-]*(?:[" + jsSpace + "]*=[" + jsSpace + "]*(?:\"[^\"]*\"|'[^']*'|[^" + jsSpace + "\"'=<>`]+))?)*[" + jsSpace + "]*/?>" +
		"|<[a-zA-Z][a-zA-Z0-9+.-]{1,31}:[^" + jsSpace + "<>\\x00-\\x1f]*>" +
		"|<[A-Z][a-zA-Z0-9]*(?:[" + jsSpace + "]+\\w+=(?:\"[^\"]*\"|\\{[^}]*\\}))*[" + jsSpace + "]*/?>)")
	findTagLineEndAt = regexp.MustCompile("^</?[a-zA-Z][\\w:-]*(?:[" + jsSpace + "]+[a-zA-Z:_][\\w.:-]*(?:[" + jsSpace + "]*=[" + jsSpace + "]*(?:\"[^\"]*\"|'[^']*'|\\{[^}]*\\}|[^" + jsSpace + "\"'=<>`]+))?)*[" + jsSpace + "]*(?:=[" + jsSpace + "]*)?(?:\"[^\"]*|'[^']*|\\{[^}]*)?$")

	findSubAt = regexp.MustCompile("^~[^~`" + jsSpace + "]+~")
	findSupAt = regexp.MustCompile("^\\^[^\\^`" + jsSpace + "]+\\^")
)

// findMarkupEdge holds the characters that open or close a construct the
// scan tracks; a span that may hide one of them from the scan makes the
// text unsure.
const findMarkupEdge = "`[]<"

func hasFindTagStart(text string) bool {
	for i := 0; i < len(text); {
		rel := strings.IndexByte(text[i:], '<')
		if rel < 0 {
			return false
		}
		next := i + rel + 1
		if next >= len(text) {
			return false
		}
		c := text[next]
		if ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || c == '/' || c == '!' || c == '?' {
			return true
		}
		i = next
	}
	return false
}

func findMayErase(text string) bool {
	return strings.Contains(text, "](") || hasFindTagStart(text) ||
		(strings.Contains(text, "][") && findDefinitionLine.MatchString(text))
}

// findLinesAsWritten reports the lines that show as written: every line
// marked reads as a fence opener, and, when the fences are read here, the
// fences' closers and code.
func findLinesAsWritten(lines []string) []bool {
	written := make([]bool, len(lines))
	code := make([]bool, len(lines))
	read := true
	fence := ""
	for i, raw := range lines {
		line := strings.TrimRight(raw, "\r\n")
		opener := findFenceLine.FindStringSubmatch(line)
		written[i] = opener != nil
		if fence != "" {
			code[i] = true
			if findFenceCloses(line, fence) {
				fence = ""
			}
			continue
		}
		if opener == nil {
			if findFenceHolder.MatchString(line) {
				read = false
			}
			continue
		}
		fence = opener[2] + opener[3]
		info := strings.TrimSpace(line[len(opener[1])+len(fence):])
		// A fence in a container, or one the chat unwraps as Markdown
		// (chat/markdown/unwrapMarkdownFence.ts), is not read here.
		if opener[1] != "" || strings.EqualFold(info, "markdown") || strings.EqualFold(info, "md") {
			read = false
		}
	}
	if read {
		for i := range code {
			written[i] = written[i] || code[i]
		}
	}
	return written
}

// findFenceCloses reports whether line closes a fence opened by run, as
// marked reads it: up to three spaces, the run, any more fence characters,
// then only spaces.
func findFenceCloses(line, run string) bool {
	indent := len(line) - len(strings.TrimLeft(line, " "))
	if indent > 3 || !strings.HasPrefix(line[indent:], run) {
		return false
	}
	return strings.Trim(strings.TrimLeft(line[indent+len(run):], "~`"), " ") == ""
}

// scanFindRegion is a run of nonblank prose lines with its erased spans
// removed, or false when some line is unsure. A block never spans a blank
// line, so the bracket state is the region's; a code span is paired within
// its line, which any split of the region into blocks keeps.
func scanFindRegion(r string) (string, bool) {
	var out strings.Builder
	out.Grow(len(r))
	bracket := false
	// The current line runs to lineEnd; lastDollar is its last `$`, and
	// nextEdge the first findMarkupEdge byte at or after the scan.
	lineEnd, lastDollar, nextEdge := -1, -1, -1
	for i := 0; i < len(r); {
		if i > lineEnd {
			lineEnd = len(r)
			if at := strings.IndexByte(r[i:], '\n'); at >= 0 {
				lineEnd = i + at
			}
			lastDollar = strings.LastIndexByte(r[i:lineEnd], '$')
			if lastDollar >= 0 {
				lastDollar += i
			}
			nextEdge = -1
		}
		if nextEdge < i {
			nextEdge = lineEnd
			if at := strings.IndexAny(r[i:lineEnd], findMarkupEdge); at >= 0 {
				nextEdge = i + at
			}
		}
		c := r[i]
		rest := r[i:]
		line := r[i:lineEnd]
		switch c {
		case '\\':
			if len(rest) > 1 && isASCIIPunct(rest[1]) {
				out.WriteString(rest[:2])
				i += 2
				continue
			}
		case '`':
			end, ok := findCodeSpanEnd(line)
			if !ok || strings.Contains(line[:end], "|") {
				return "", false
			}
			out.WriteString(line[:end])
			i += end
			continue
		case '<':
			if m := findTagAt.FindString(rest); m != "" {
				i += len(m)
				continue
			}
			if m := findAutolinkAt.FindString(rest); m != "" {
				out.WriteString(m)
				i += len(m)
				continue
			}
			if len(rest) > 1 {
				switch next := rest[1]; {
				case next == '!' || next == '?':
					// A comment, declaration or instruction may run on.
					return "", false
				case isASCIIAlpha(next) || next == '/':
					if findMaybeTagAt.MatchString(rest) || findTagLineEndAt.MatchString(strings.TrimRight(line, "\r")) {
						return "", false
					}
				}
			}
		case '!':
			if len(rest) > 1 && rest[1] == '[' {
				// An image shows nothing and ends the run.
				if m := findLinkAt.FindString(rest[1:]); m != "" {
					i += 1 + len(m)
					continue
				}
			}
		case '[':
			if m := findLinkAt.FindStringSubmatch(rest); m != nil && !strings.HasPrefix(m[1], "!") {
				out.WriteString(m[1])
				i += len(m[0])
				continue
			}
			bracket = true
			// A citation or footnote reference holds what it encloses.
			if end := strings.IndexAny(rest[1:], "[]"); end >= 0 && rest[1+end] == ']' &&
				strings.ContainsAny(rest[1:1+end], "`<") {
				return "", false
			}
		case ']':
			if bracket && len(rest) > 1 && rest[1] == '(' {
				return "", false
			}
		case '$':
			// Math from here to a later `$` on the line hides what it holds.
			if nextEdge < lastDollar {
				return "", false
			}
		case '~':
			if m := findSubAt.FindString(rest); m != "" && strings.ContainsAny(m, findMarkupEdge) {
				return "", false
			}
			if len(rest) > 2 && rest[1] == '~' && rest[2] != '~' && !isFindSpace(rest[2]) {
				if end := strings.Index(rest[2:], "~~"); end >= 0 && strings.ContainsAny(rest[2:2+end], findMarkupEdge) {
					return "", false
				}
			}
		case '^':
			if m := findSupAt.FindString(rest); m != "" && strings.ContainsAny(m, findMarkupEdge) {
				return "", false
			}
		case 'h', 'H', 'f', 'F', 'w', 'W':
			if findURLStart(rest) {
				// A bare URL runs to white space or `<` as written.
				end := strings.IndexFunc(rest, func(r rune) bool { return r == '<' || isJSSpace(r) })
				if end < 0 {
					end = len(rest)
				}
				if strings.ContainsAny(rest[:end], "`[]") {
					return "", false
				}
				out.WriteString(rest[:end])
				i += end
				continue
			}
		}
		out.WriteByte(c)
		i++
	}
	return out.String(), true
}

// findCodeSpanEnd is the end of the code span a backtick run at the start of
// line opens: the next run of the same length on the line.
func findCodeSpanEnd(line string) (int, bool) {
	n := len(line) - len(strings.TrimLeft(line, "`"))
	for at := n; at < len(line); {
		next := strings.IndexByte(line[at:], '`')
		if next < 0 {
			return 0, false
		}
		start := at + next
		end := start + len(line[start:]) - len(strings.TrimLeft(line[start:], "`"))
		if end-start == n {
			return end, true
		}
		at = end
	}
	return 0, false
}

func findURLStart(text string) bool {
	for _, prefix := range [...]string{"http://", "https://", "ftp://", "www."} {
		if len(text) >= len(prefix) && strings.EqualFold(text[:len(prefix)], prefix) {
			return true
		}
	}
	return false
}

func isASCIIAlpha(c byte) bool {
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

func isASCIIPunct(c byte) bool {
	return c > ' ' && c < 0x7f && !isASCIIAlnum(c)
}

func isFindSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return 0x2000 <= r && r <= 0x200a
}
