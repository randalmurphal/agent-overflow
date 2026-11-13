// Package mdsuggest reads the suggested-change blocks of a forge comment
// body. Both forges write a suggestion as a fenced block whose info
// string is `suggestion` (GitLab also `suggestion:-N+M`, which widens the
// replaced lines past the comment's anchor). It is the one reader of
// them: the GitHub adapter derives a thread's suggestions from it (GitHub
// reports none of its own), the fake forge derives GitLab's from it, and
// the review pane's editor preview asks for it through
// App.ParseCommentSuggestions.
package mdsuggest

import (
	"regexp"
	"strconv"
	"strings"
)

// Block is one suggestion block of a body, in body order.
// Lines are the replacement lines without their line endings; an empty
// block deletes the anchored lines. Above and Below are a GitLab
// `suggestion:-N+M` header's N and M (Widened set), which widen the
// replaced lines past the anchor; GitHub has no such header.
type Block struct {
	Lines   []string `json:"lines"`
	Above   int      `json:"above,omitempty"`
	Below   int      `json:"below,omitempty"`
	Widened bool     `json:"widened,omitempty"`
}

// headerMax bounds a -N+M header's numbers, as GitLab bounds
// them (100 lines each way).
const headerMax = 100

var (
	fenceOpenPattern  = regexp.MustCompile("^(`{3,}|~{3,})(.*)$")
	fenceClosePattern = regexp.MustCompile("^(`{3,}|~{3,})[ \t]*$")
	listMarkerPattern = regexp.MustCompile(`^([-+*]|\d{1,9}[.)])( +|$)`)
	widenedPattern    = regexp.MustCompile(`^suggestion:-(\d+)\+(\d+)$`)
)

// fence is an open fenced code block: its marker character and length,
// the column its content is relative to, and whether it is a top-level
// suggestion being collected.
type fence struct {
	char       byte
	length     int
	indent     int
	suggestion bool
	header     Block
	lines      []string
}

// Parse returns the suggestion blocks of body, in order, read
// as CommonMark reads fenced code: a fence opens with three or more
// backticks or tildes indented at most three columns, and closes with a
// line of the same character, at least as long, carrying nothing else
// (so a shorter or other fence inside, and a fence with an info string,
// is content). Only a fence at the top level of the body is a
// suggestion: one inside a blockquote or a list item is quoted or nested
// text. A fence never closed is not a suggestion. CRLF bodies read as LF.
func Parse(body string) []Block {
	var out []Block
	var open *fence
	listContent := -1 // the content column of the list item the lines are in, -1 outside one
	for raw := range strings.SplitSeq(body, "\n") {
		line := strings.TrimSuffix(raw, "\r")
		indent, rest := leadingColumns(line)
		if open != nil {
			rel := indent - open.indent
			if rel >= 0 && rel <= 3 && fenceCloses(rest, open) {
				if open.suggestion {
					open.header.Lines = open.lines
					if open.header.Lines == nil {
						open.header.Lines = []string{}
					}
					out = append(out, open.header)
				}
				open = nil
				continue
			}
			if open.suggestion {
				open.lines = append(open.lines, stripColumns(line, open.indent))
			}
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if listContent >= 0 && indent < listContent && !listMarkerPattern.MatchString(rest) {
			listContent = -1
		}
		if indent <= 3 && listMarkerPattern.MatchString(rest) {
			marker := listMarkerPattern.FindStringSubmatch(rest)
			listContent = indent + len(marker[0])
			if marker[2] == "" {
				listContent++
			}
			// A fence can open on the item's own line: it is the item's.
			indent, rest = leadingColumns(rest[len(marker[0]):])
			indent += listContent
			if f := openFence(rest, indent, false); f != nil {
				open = f
			}
			continue
		}
		if indent <= 3 && strings.HasPrefix(rest, ">") {
			// A blockquote's lines, fences included, are quoted text.
			continue
		}
		inList := listContent >= 0 && indent >= listContent
		base := 0
		if inList {
			base = listContent
		}
		if indent-base > 3 {
			continue // an indented code block, or deeper list content
		}
		if f := openFence(rest, indent, !inList); f != nil {
			open = f
		}
	}
	return out
}

// openFence reads rest, a line after its indent columns, as a fence
// opener; topLevel decides whether a suggestion info string makes it a
// suggestion. A backtick fence's info string may hold no backtick.
func openFence(rest string, indent int, topLevel bool) *fence {
	m := fenceOpenPattern.FindStringSubmatch(rest)
	if m == nil {
		return nil
	}
	info := strings.TrimSpace(m[2])
	if m[1][0] == '`' && strings.Contains(info, "`") {
		return nil
	}
	f := &fence{char: m[1][0], length: len(m[1]), indent: indent}
	word, _, _ := strings.Cut(info, " ")
	if !topLevel {
		return f
	}
	if word == "suggestion" {
		f.suggestion = true
	} else if w := widenedPattern.FindStringSubmatch(word); w != nil {
		above, err1 := strconv.Atoi(w[1])
		below, err2 := strconv.Atoi(w[2])
		if err1 == nil && err2 == nil && above <= headerMax && below <= headerMax {
			f.suggestion = true
			f.header = Block{Above: above, Below: below, Widened: true}
		}
	}
	return f
}

// fenceCloses reports whether rest closes f.
func fenceCloses(rest string, f *fence) bool {
	m := fenceClosePattern.FindStringSubmatch(rest)
	return m != nil && m[1][0] == f.char && len(m[1]) >= f.length
}

// leadingColumns counts line's leading whitespace in columns (a tab
// advances to the next multiple of four) and returns the rest.
func leadingColumns(line string) (int, string) {
	col := 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case ' ':
			col++
		case '\t':
			col += 4 - col%4
		default:
			return col, line[i:]
		}
	}
	return col, ""
}

// stripColumns removes up to n columns of leading spaces from a content
// line, as CommonMark strips a fence's indent from its content.
func stripColumns(line string, n int) string {
	i := 0
	for i < len(line) && i < n && line[i] == ' ' {
		i++
	}
	return line[i:]
}
