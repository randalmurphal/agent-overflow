package mdsuggest

import (
	"reflect"
	"testing"
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Parse(tc.body); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Parse(%q) = %#v, want %#v", tc.body, got, tc.want)
			}
		})
	}
}
