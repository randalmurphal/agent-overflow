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
	"bytes"
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// Block is one suggestion block of a body, in body order.
// Lines are the replacement lines without their line endings; an empty
// block deletes the anchored lines. Above and Below are a GitLab
// `suggestion:-N+M` header's N and M (Widened set), which widen the
// replaced lines past the anchor; GitHub has no such header. StartLine
// and EndLine are the 0-based indices, in the body split on "\n", of the
// opening and the closing fence lines.
type Block struct {
	Lines     []string `json:"lines"`
	Above     int      `json:"above,omitempty"`
	Below     int      `json:"below,omitempty"`
	Widened   bool     `json:"widened,omitempty"`
	StartLine int      `json:"startLine"`
	EndLine   int      `json:"endLine"`
}

// headerMax bounds a -N+M header's numbers, as GitLab bounds
// them (100 lines each way).
const headerMax = 100

var markdown = goldmark.New()

// Parse returns the suggestion blocks of body, in order: the fenced code
// blocks CommonMark reads at the top level of the body (goldmark's block
// parse) whose info string's first word is `suggestion` or
// `suggestion:-N+M`. A fence inside a blockquote, a list item or an HTML
// block is not one, and neither is a fence the body never closes
// (CommonMark closes it at the end of input; the forges do not apply it).
func Parse(body string) []Block {
	src := []byte(body)
	doc := markdown.Parser().Parse(text.NewReader(src))
	var out []Block
	// lineOf counts the line breaks before an offset, moving forward only:
	// blocks come in body order, so the whole parse counts each byte once.
	at, line := 0, 0
	lineOf := func(offset int) int {
		line += bytes.Count(src[at:offset], []byte{'\n'})
		at = offset
		return line
	}
	for node := doc.FirstChild(); node != nil; node = node.NextSibling() {
		code, ok := node.(*ast.FencedCodeBlock)
		if !ok || code.Info == nil {
			continue
		}
		block, ok := header(string(code.Info.Segment.Value(src)))
		if !ok {
			continue
		}
		openStart, closeStart, ok := fence(src, code)
		if !ok {
			continue
		}
		block.StartLine, block.EndLine = lineOf(openStart), lineOf(closeStart)
		block.Lines = []string{}
		segments := code.Lines()
		for i := 0; i < segments.Len(); i++ {
			segment := segments.At(i)
			block.Lines = append(block.Lines, trimLineEnding(string(segment.Value(src))))
		}
		out = append(out, block)
	}
	return out
}

// header reads an info string's first word as a suggestion header.
func header(info string) (Block, bool) {
	word, _, _ := strings.Cut(strings.TrimSpace(info), " ")
	word, _, _ = strings.Cut(word, "\t")
	if word == "suggestion" {
		return Block{}, true
	}
	rest, ok := strings.CutPrefix(word, "suggestion:-")
	if !ok {
		return Block{}, false
	}
	a, b, ok := strings.Cut(rest, "+")
	above, err1 := strconv.Atoi(a)
	below, err2 := strconv.Atoi(b)
	if !ok || err1 != nil || err2 != nil || !digits(a) || !digits(b) || above > headerMax || below > headerMax {
		return Block{}, false
	}
	return Block{Above: above, Below: below, Widened: true}, true
}

func digits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// fence answers the offsets of code's opening and closing fence lines in
// src, ok false when the fence is not closed: the line after its last
// content line (or after its opening line, with no content) must be a
// closing fence of the opener's character, at least as long, indented at
// most three columns, with nothing else on it.
func fence(src []byte, code *ast.FencedCodeBlock) (openStart, closeStart int, ok bool) {
	openStart = bytes.LastIndexByte(src[:code.Info.Segment.Start], '\n') + 1
	char, length := fenceRun(src[openStart:])
	after := lineEnd(src, code.Info.Segment.Stop)
	if n := code.Lines().Len(); n > 0 {
		after = lineEnd(src, code.Lines().At(n-1).Stop-1)
	}
	if after >= len(src) {
		return 0, 0, false
	}
	line := src[after:]
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	cols, i := 0, 0
	for ; i < len(line) && (line[i] == ' ' || line[i] == '\t'); i++ {
		if line[i] == '\t' {
			cols += 4 - cols%4
		} else {
			cols++
		}
	}
	if cols > 3 {
		return 0, 0, false
	}
	j := i
	for j < len(line) && line[j] == char {
		j++
	}
	return openStart, after, j-i >= length && len(bytes.Trim(line[j:], " \t\r")) == 0
}

// fenceRun is the fence character and run length opening line.
func fenceRun(line []byte) (byte, int) {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	if i == len(line) {
		return 0, 0
	}
	char, j := line[i], i
	for j < len(line) && line[j] == char {
		j++
	}
	return char, j - i
}

// lineEnd is the offset just past the line ending of the line holding
// src[at], or len(src) when that line has none.
func lineEnd(src []byte, at int) int {
	if at < 0 {
		at = 0
	}
	if i := bytes.IndexByte(src[at:], '\n'); i >= 0 {
		return at + i + 1
	}
	return len(src)
}

func trimLineEnding(line string) string {
	if s, ok := strings.CutSuffix(line, "\r\n"); ok {
		return s
	}
	if s, ok := strings.CutSuffix(line, "\n"); ok {
		return s
	}
	return strings.TrimSuffix(line, "\r")
}
