package highlight

import (
	"github.com/yuin/goldmark/ast"
)

// fencedBlockIsClosed reports whether a FencedCodeBlock was terminated
// by a matching closing fence in the source. Goldmark's AST does not
// expose this — the same `*ast.FencedCodeBlock` is produced for fences
// that hit a real closer and fences that ran into EOF. During streaming
// we need to know the difference so we don't rewrite an unterminated
// ```mermaid fence into a <pre class="mermaid"> node (the frontend
// renderer can't parse partial source).
//
// The heuristic: locate the opener line by walking backward from the
// first content line, then scan forward from the last content line for
// a line whose first non-blank run is the same fence character as the
// opener and at least as long. Mirrors CommonMark §4.5 closer rules.
//
// Limitations: fences nested inside blockquotes / list items are not
// recognized as closed. Goldmark strips the block prefix (`> `, `- `,
// etc.) from content lines but leaves it on opener/closer in the raw
// source, which would require re-implementing each block type's prefix
// parser to reliably match. Those fences stay as plain code blocks,
// which is a UX degradation but not a correctness bug. Top-level
// fences (the common agent case) work correctly.
func fencedBlockIsClosed(source []byte, cb *ast.FencedCodeBlock) bool {
	lines := cb.Lines()
	if lines.Len() == 0 {
		// Empty-content block (e.g. ```mermaid\n```). We can't
		// distinguish "```mermaid\n```" (closed, empty) from
		// "```mermaid\n" followed by nothing else (open, empty) just
		// from the AST. Default to "open" — the frontend skips empty
		// sources anyway and an unclosed empty diagram is degenerate.
		return false
	}
	first := lines.At(0)
	last := lines.At(lines.Len() - 1)
	if first.Start == 0 {
		return false
	}
	// Goldmark strips any leading indent from content lines, so
	// first.Start-1 is not necessarily the newline terminating the
	// opener line. Walk back from first.Start looking for the first
	// '\n' we encounter — that is the opener line's terminator.
	openerLineEnd := first.Start - 1
	for openerLineEnd > 0 && source[openerLineEnd] != '\n' {
		openerLineEnd--
	}
	if source[openerLineEnd] != '\n' {
		return false
	}
	openerLineStart := openerLineEnd
	for openerLineStart > 0 && source[openerLineStart-1] != '\n' {
		openerLineStart--
	}
	openerLine := source[openerLineStart:openerLineEnd]
	openerIndent := 0
	for openerIndent < len(openerLine) && (openerLine[openerIndent] == ' ' || openerLine[openerIndent] == '\t') {
		openerIndent++
	}
	openerFence := openerLine[openerIndent:]
	if len(openerFence) == 0 {
		return false
	}
	fenceChar := openerFence[0]
	if fenceChar != '`' && fenceChar != '~' {
		return false
	}
	openerLen := 0
	for openerLen < len(openerFence) && openerFence[openerLen] == fenceChar {
		openerLen++
	}
	// Scan forward from last.Stop looking for a closer line.
	pos := last.Stop
	for pos < len(source) {
		end := pos
		for end < len(source) && source[end] != '\n' {
			end++
		}
		line := source[pos:end]
		i := 0
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		fenceLen := 0
		for i+fenceLen < len(line) && line[i+fenceLen] == fenceChar {
			fenceLen++
		}
		if fenceLen >= openerLen {
			allSpace := true
			for j := i + fenceLen; j < len(line); j++ {
				// `\r` is allowed as part of CRLF line endings. A closer
				// of the form `"```\r\n"` reaches this loop as a line
				// slice of `"```\r"` (the `\n` was the terminator), and
				// the trailing `\r` is whitespace for our purposes.
				if line[j] != ' ' && line[j] != '\t' && line[j] != '\r' {
					allSpace = false
					break
				}
			}
			if allSpace {
				return true
			}
		}
		if end >= len(source) {
			break
		}
		pos = end + 1
	}
	return false
}
