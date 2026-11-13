package mdsuggest

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body string
		want       []Block
	}{
		{"none", "just text\n```go\ncode\n```\n", nil},
		{"one", "Try:\n```suggestion\nreturn nil\n```\n", []Block{{Lines: []string{"return nil"}}}},
		{"several in order", "```suggestion\na\n```\nand\n```suggestion\nb\nc\n```", []Block{{Lines: []string{"a"}}, {Lines: []string{"b", "c"}}}},
		{"empty block deletes", "```suggestion\n```\n", []Block{{Lines: []string{}}}},
		{"blank line kept", "```suggestion\n\n```\n", []Block{{Lines: []string{""}}}},
		{"longer fence holds a shorter one", "````suggestion\n```go\nx := 1\n```\n````\n", []Block{{Lines: []string{"```go", "x := 1", "```"}}}},
		{"a fence with an info string is content", "```suggestion\n```go\n```\n", []Block{{Lines: []string{"```go"}}}},
		{"a shorter closing fence is content", "````suggestion\na\n```\nb\n````", []Block{{Lines: []string{"a", "```", "b"}}}},
		{"tildes", "~~~suggestion\nx\n~~~\n", []Block{{Lines: []string{"x"}}}},
		{"backticks do not close tildes", "~~~suggestion\n```\nx\n~~~\n", []Block{{Lines: []string{"```", "x"}}}},
		{"CRLF", "Use:\r\n```suggestion\r\nfoo()\r\nbar()\r\n```\r\n", []Block{{Lines: []string{"foo()", "bar()"}}}},
		{"indented fence strips its indent", "  ```suggestion\n  a\n    b\n  ```\n", []Block{{Lines: []string{"a", "  b"}}}},
		{"four spaces is an indented code block", "    ```suggestion\n    a\n    ```\n", nil},
		{"blockquote is quoted text", "> ```suggestion\n> a\n> ```\n", nil},
		{"list item is nested text", "- item\n  ```suggestion\n  a\n  ```\n", nil},
		{"fence on the list item's own line", "1. ```suggestion\n   a\n   ```\n", nil},
		{"after a list ends", "- item\n\ntext\n```suggestion\na\n```\n", []Block{{Lines: []string{"a"}}}},
		{"quoted then real", "> ```suggestion\n> old\n> ```\n\n```suggestion\nnew\n```", []Block{{Lines: []string{"new"}}}},
		{"unterminated is none", "```suggestion\na\nb\n", nil},
		{"terminated then unterminated", "```suggestion\na\n```\n```suggestion\nb\n", []Block{{Lines: []string{"a"}}}},
		{"GitLab widened header", "```suggestion:-1+2\nx\n```\n", []Block{{Lines: []string{"x"}, Above: 1, Below: 2, Widened: true}}},
		{"widened past GitLab's bound is code", "```suggestion:-101+0\nx\n```\n", nil},
		{"another language is code", "```suggestions\nx\n```\n", nil},
		{"info string after the word", "```suggestion extra\nx\n```\n", []Block{{Lines: []string{"x"}}}},
		{"closing fence with trailing spaces", "```suggestion\nx\n```   \n", []Block{{Lines: []string{"x"}}}},
		{"closing fence may be longer", "```suggestion\nx\n`````\n", []Block{{Lines: []string{"x"}}}},
		{"closer less indented than its opener", "  ```suggestion\n  a\n```\n", []Block{{Lines: []string{"a"}}}},
		{"closer more indented than its opener", "```suggestion\na\n   ```\n", []Block{{Lines: []string{"a"}}}},
		{"closer indented four is content", "```suggestion\na\n    ```\n", nil},
		{"opener 3 closer 1", "   ```suggestion\n   a\n ```\n", []Block{{Lines: []string{"a"}}}},
		{"opener 1 closer 3", " ```suggestion\n a\n   ```\n", []Block{{Lines: []string{"a"}}}},
		{"inside an HTML comment", "<!--\n```suggestion\nx\n```\n-->\n", nil},
		{"inside a details block", "<details>\n```suggestion\nx\n```\n</details>\n", nil},
		{"after an HTML block ends", "<details>\n<summary>s</summary>\n\n```suggestion\nx\n```\n</details>\n", []Block{{Lines: []string{"x"}}}},
		{"CRLF with an empty line", "```suggestion\r\na\r\n\r\nb\r\n```\r\n", []Block{{Lines: []string{"a", "", "b"}}}},
		{"CRLF closer", "```suggestion\r\nx\r\n```", []Block{{Lines: []string{"x"}}}},
		{"tab in content kept", "```suggestion\n\tx := 1\n```\n", []Block{{Lines: []string{"\tx := 1"}}}},
		{"tab before the info string's word", "```\tsuggestion\nx\n```\n", []Block{{Lines: []string{"x"}}}},
		{"tilde fence holds backtick fences", "~~~suggestion\n```go\nx\n```\n~~~\n", []Block{{Lines: []string{"```go", "x", "```"}}}},
		{"empty block at the end of input", "```suggestion\n```", []Block{{Lines: []string{}}}},
		{"a closer with text is content", "```suggestion\nx\n``` y\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Parse(tc.body); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Parse(%q) = %#v, want %#v", tc.body, got, tc.want)
			}
		})
	}
}

// A body at the larger forge limit (GitLab's 1,000,000 characters) of
// the shapes that make a naive reader quadratic parses in bounded time.
func TestParseLargeBodies(t *testing.T) {
	t.Parallel()
	const limit = 1_000_000
	for name, unit := range map[string]string{
		"openers":       "```suggestion\n",
		"closed blocks": "```suggestion\nx\n```\n",
		"nested quotes": "> > > > > ```suggestion\n",
		"backticks":     "`",
		"list items":    "- ```suggestion\n",
	} {
		body := strings.Repeat(unit, limit/len(unit))
		start := time.Now()
		got := Parse(body)
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("%s: %d bytes took %v", name, len(body), elapsed)
		}
		if name == "closed blocks" && len(got) != limit/len(unit) {
			t.Fatalf("closed blocks: %d, want %d", len(got), limit/len(unit))
		}
	}
}
