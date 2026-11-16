package adf

import (
	"errors"
	"strings"
	"testing"
)

// TestSyntaxErrorsNameLineAndFix proves FromMarkdown and Edit report the
// Markdown's own mistakes as a *SyntaxError with the line and a fix hint,
// its text free of internal node names.
func TestSyntaxErrorsNameLineAndFix(t *testing.T) {
	for _, tc := range []struct {
		md, msg, hint string
		line          int
	}{
		{"| a |\n| --- |\n| b |\n\n- x\n\n  | a |\n  | --- |\n  | b |",
			"a table can't go in a list item", "unindent it out of the list item", 7},
		{"text\n\n<div>x</div>", `raw HTML "<div>x</div>" is not supported`,
			"escape the < as \\<", 3},
		{"<details>\n\nx", "<details> is never closed with </details>",
			"add the closing tag", 1},
		{"x\n\n</details>", "</details> without a matching <details>",
			"add the opening tag before it", 3},
		{"| a |\n| --- |\n| b | c |", "table row has 2 cells but the header has 1",
			"as many cells as the header", 3},
		{"> [!BOGUS]\n> x", "unknown alert type [!BOGUS]", "alert types named", 1},
		{"@[accountid:]", "accountid: reference needs an ID", "give the ID", 1},
		{"x\n\n# Title ![logo](https://e.com/x.png)", "images can't go in a heading",
			"on a line of its own", 3},
	} {
		_, _, err := FromMarkdown(t.Context(), tc.md, newFakeResolver())
		var se *SyntaxError
		if !errors.As(err, &se) || se.Line != tc.line || !strings.Contains(se.Msg, tc.msg) ||
			!strings.Contains(se.Hint, tc.hint) || strings.Contains(se.Error(), "\x00") {
			t.Errorf("%q: %T %v (%+v)", tc.md, err, err, se)
			continue
		}
		if want := "line "; !strings.HasPrefix(se.Error(), want) {
			t.Errorf("%q: %s", tc.md, se.Error())
		}
		orig, _, _ := FromMarkdown(t.Context(), "first\n\nsecond", nil)
		if _, _, err := Edit(t.Context(), orig, tc.md, newFakeResolver()); !errors.As(err, &se) {
			t.Errorf("Edit %q: %T %v", tc.md, err, err)
		}
	}
}

// TestResolverErrorIsNotSyntax proves an error the Resolver returns (a user
// not found) stays itself, for errors.Is and the caller's exit code, and
// is never taken for a mistake in the Markdown.
func TestResolverErrorIsNotSyntax(t *testing.T) {
	_, _, err := FromMarkdown(t.Context(), "Ask @[Nobody Here]", newFakeResolver())
	var se *SyntaxError
	if err == nil || errors.As(err, &se) || !strings.Contains(err.Error(), "line 1: @[Nobody Here]") {
		t.Errorf("%T %v", err, err)
	}
	if _, _, err := FromMarkdown(t.Context(), "@[me]", nil); errors.As(err, &se) {
		t.Errorf("no resolver: %T %v", err, err)
	}
}

// TestLossKinds proves a refusal can tell an expand's loss from one of a
// container written with comment markers, and sees both in one warning.
func TestLossKinds(t *testing.T) {
	for _, tc := range []struct {
		msg              string
		markers, details bool
	}{
		{"replaced a block that had a custom panel", true, false},
		{`the expand "T" is still in the Markdown, but the edit would lose it`, false, true},
		{"replaced a block that had expand nesting and a status lozenge", true, true},
		{"<!-- /panel --> closes nothing; its container is not kept", true, false},
	} {
		m, d := LossKinds([]Warning{{Msg: tc.msg, Flattens: true}, {Msg: "x expand"}})
		if m != tc.markers || d != tc.details {
			t.Errorf("%q: markers %v details %v", tc.msg, m, d)
		}
	}
}
