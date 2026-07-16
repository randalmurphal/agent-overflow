package highlight

import (
	"reflect"
	"testing"
)

func TestScanFences(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []Fence
	}{
		{
			name: "no fences",
			text: "plain prose\nwith lines\n",
			want: nil,
		},
		{
			name: "closed fence with lang",
			text: "before\n```python\ndef f():\n    pass\n```\nafter",
			want: []Fence{{Lang: "python", Source: "def f():\n    pass", Closed: true}},
		},
		{
			name: "unclosed fence keeps the raw tail",
			text: "```go\nfunc main() {\n\tfmt.Pr",
			want: []Fence{{Lang: "go", Source: "func main() {\n\tfmt.Pr"}},
		},
		{
			name: "unclosed fence with trailing newline",
			text: "```go\nx := 1\n",
			want: []Fence{{Lang: "go", Source: "x := 1\n"}},
		},
		{
			name: "opener with no content yet",
			text: "```py",
			want: []Fence{{Lang: "py"}},
		},
		{
			name: "empty closed fence",
			text: "```py\n```",
			want: []Fence{{Lang: "py", Source: "", Closed: true}},
		},
		{
			name: "multiple fences, last open",
			text: "```js\nlet a\n```\ntext\n```rust\nfn main()",
			want: []Fence{
				{Lang: "js", Source: "let a", Closed: true},
				{Lang: "rust", Source: "fn main()"},
			},
		},
		{
			name: "info string extras keep only the first word",
			text: "```py title=x linenums\npass\n```",
			want: []Fence{{Lang: "py", Source: "pass", Closed: true}},
		},
		{
			name: "tilde fence",
			text: "~~~sql\nSELECT 1;\n~~~\n",
			want: []Fence{{Lang: "sql", Source: "SELECT 1;", Closed: true}},
		},
		{
			name: "bare triple backticks inside a four-backtick fence are content",
			text: "````md\n```\ninner\n```\n````\n",
			want: []Fence{{Lang: "md", Source: "```\ninner\n```", Closed: true}},
		},
		{
			name: "mismatched fence char is content",
			text: "```py\n~~~\npass\n```\n",
			want: []Fence{{Lang: "py", Source: "~~~\npass", Closed: true}},
		},
		{
			name: "closer tolerates trailing spaces and up to 3 leading spaces",
			text: "```py\npass\n   ```  \n",
			want: []Fence{{Lang: "py", Source: "pass", Closed: true}},
		},
		{
			name: "indented opener does not match (marked strips list indentation)",
			text: "- item\n  ```py\n  pass\n  ```\n",
			want: nil,
		},
		{
			name: "backtick in backtick-fence info string is inline code, not an opener",
			text: "``` `code` ```\n",
			want: nil,
		},
		{
			name: "longer opener run needs an equal-or-longer closer",
			text: "````py\npass\n```\nstill inside\n````\n",
			want: []Fence{{Lang: "py", Source: "pass\n```\nstill inside", Closed: true}},
		},
		{
			name: "bare fence has empty lang",
			text: "```\nplain\n```\n",
			want: []Fence{{Lang: "", Source: "plain", Closed: true}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ScanFences(tc.text)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ScanFences(%q) = %#v, want %#v", tc.text, got, tc.want)
			}
		})
	}
}

// The scanner's Source must byte-match marked's token.text for the
// common streaming shapes, because the seed hash chain is computed
// over it. The frontend integration test asserts the same source
// reaches HighlightCode (ChatMarkdown.codeSpans.test.ts), pinning the
// other side of this contract.
func TestScanFencesSourceMatchesMarkedTokenText(t *testing.T) {
	// '```python\n' + SOURCE + '\n```' → token.text === SOURCE.
	source := "def route():\n    pass"
	fences := ScanFences("```python\n" + source + "\n```")
	if len(fences) != 1 || fences[0].Source != source {
		t.Fatalf("got %#v, want single fence with source %q", fences, source)
	}
}
