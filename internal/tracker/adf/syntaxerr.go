package adf

import (
	"errors"
	"strconv"
	"strings"
)

// SyntaxError is Markdown that FromMarkdown or Edit can't convert, found at
// a line: a construct ADF has no place for (a table in a list item), HTML
// it doesn't take, or a tag or marker left open. The writer fixes the
// Markdown; Hint says how. Errors a Resolver returns (a user not found,
// an upload failing) are never a SyntaxError.
type SyntaxError struct {
	Line int    // 1-based Markdown line, or 0 when not known
	Msg  string // what is wrong, without the line
	Hint string // how to fix it
}

func (e *SyntaxError) Error() string {
	if e.Line <= 0 {
		return e.Msg
	}
	return "line " + strconv.Itoa(e.Line) + ": " + e.Msg
}

// syntaxHints give the fix for a conversion error, by a phrase its message
// holds; the first that matches wins.
var syntaxHints = []struct{ phrase, hint string }{
	{"a table can't go in", "a table must stand at the top level: unindent it out of " +
		"the list item or quote, with a blank line before it"},
	{"images can't go in", "put the image on a line of its own, outside it"},
	{"can't go in", "move it out to the top level, unindented, with a blank line before it"},
	{"is never closed", "add the closing tag on a line of its own after the content, " +
		"or delete the opening tag"},
	{"without a matching", "add the opening tag before it, or delete this one"},
	{"raw HTML", "write it in Markdown instead, or escape the < as \\< to show it as text"},
	{"unsupported Markdown", "rewrite it with paragraphs, lists, tables, code or links"},
	{"table row has", "give every row as many cells as the header row"},
	{"unknown alert type", "use one of the alert types named, or escape the [ as \\[ " +
		"to show it as text"},
	{"a checklist item can only", "move the other blocks out of the checklist, below it"},
	{"is empty; wrap text in", "put text between the tags, or delete them"},
	{"needs an ID", "give the ID after the colon, or escape the [ as \\[ to show it as text"},
	{"needs a URL", "give the URL after the colon, or escape the [ as \\[ to show it as text"},
	{"needs a name", "put a display name, an email or me between the brackets, or " +
		"escape the [ as \\[ to show it as text"},
}

// syntaxError returns err as a *SyntaxError when it is a conversion error
// of the Markdown itself, its message made public, or nil when it is not:
// one that wraps a Resolver's error, or one about a lookup the caller did
// not provide.
func syntaxError(err error) *SyntaxError {
	var le *lineError
	if !errors.As(err, &le) || le.err != nil ||
		strings.Contains(le.msg, "which is not available here") {
		return nil
	}
	msg := internalNames.get().Replace(le.msg)
	hint := "fix the Markdown at that line and try again"
	if le.line <= 0 {
		hint = "fix the Markdown and try again"
	}
	for _, h := range syntaxHints {
		if strings.Contains(msg, h.phrase) {
			hint = h.hint
			break
		}
	}
	return &SyntaxError{Line: le.line, Msg: msg, Hint: hint}
}
