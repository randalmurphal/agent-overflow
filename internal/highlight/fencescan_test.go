package highlight

import (
	"math/rand"
	"reflect"
	"strings"
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

// streamedFences replays FenceStream steps into the fences they describe:
// received content per fence, trimmed of its final newline once closed.
type streamedFence struct {
	lang    string
	content []byte
	closed  bool
}

func applyFenceSteps(t *testing.T, fences []streamedFence, steps []FenceStep) []streamedFence {
	t.Helper()
	for _, step := range steps {
		switch step.Kind {
		case FenceOpened:
			if step.Index != len(fences) {
				t.Fatalf("fence %d opened after %d fences", step.Index, len(fences))
			}
			fences = append(fences, streamedFence{lang: step.Lang})
		case FenceContent:
			if step.Index != len(fences)-1 || fences[step.Index].closed {
				t.Fatalf("content for fence %d that is not the open fence", step.Index)
			}
			fences[step.Index].content = append(fences[step.Index].content, step.Text...)
		case FenceClosed:
			if step.Index != len(fences)-1 || fences[step.Index].closed {
				t.Fatalf("closing fence %d that is not the open fence", step.Index)
			}
			fences[step.Index].closed = true
		}
	}
	return fences
}

func checkStreamedFences(t *testing.T, text string, got []streamedFence, finished bool) {
	t.Helper()
	want := ScanFences(text)
	if len(got) != len(want) {
		// An opener line still arriving opens its fence only when complete.
		if !finished && len(got) == len(want)-1 && !want[len(want)-1].Closed && want[len(want)-1].Source == "" {
			want = want[:len(want)-1]
		} else {
			t.Fatalf("text %q: %d fences, want %d", text, len(got), len(want))
		}
	}
	for i, w := range want {
		g := got[i]
		source := string(g.content)
		if g.closed {
			source = strings.TrimSuffix(source, "\n")
		}
		if !finished && w.Closed && !g.closed && i == len(want)-1 && !strings.HasSuffix(text, "\n") {
			// The text ends in a closer the stream cannot know is complete.
			if g.lang != w.Lang || !strings.HasPrefix(w.Source+"\n", source) {
				t.Fatalf("text %q deferred fence %d: lang %q received %q, want %q within %q", text, i, g.lang, source, w.Lang, w.Source)
			}
			continue
		}
		switch {
		case g.lang != w.Lang || g.closed != w.Closed:
			t.Fatalf("text %q fence %d: lang %q closed %v, want %q %v", text, i, g.lang, g.closed, w.Lang, w.Closed)
		case finished || w.Closed:
			if source != w.Source {
				t.Fatalf("text %q fence %d: source %q, want %q", text, i, source, w.Source)
			}
		case !strings.HasPrefix(w.Source, source):
			t.Fatalf("text %q open fence %d: received %q, not a prefix of %q", text, i, source, w.Source)
		}
	}
}

func TestFenceStreamMatchesScanFences(t *testing.T) {
	pieces := []string{
		"```", "```go", "```js extra", "~~~", "~~~~py", "````", "``", "`", "~~",
		"   ```", "    ```", "``` ", "```\t", "``` x", "``` a`b",
		"code", "  indented", "text", "", "é", "\t", " ",
	}
	rng := rand.New(rand.NewSource(7))
	for round := 0; round < 3000; round++ {
		var b strings.Builder
		for n := rng.Intn(12); n >= 0; n-- {
			b.WriteString(pieces[rng.Intn(len(pieces))])
			if rng.Intn(5) > 0 {
				b.WriteByte('\n')
			}
		}
		text := b.String()
		var stream FenceStream
		var fences []streamedFence
		var steps []FenceStep
		for off := 0; off < len(text); {
			end := min(len(text), off+1+rng.Intn(6))
			steps = stream.Write([]byte(text[off:end]), steps[:0])
			fences = applyFenceSteps(t, fences, steps)
			off = end
			checkStreamedFences(t, text[:off], fences, false)
		}
		fences = applyFenceSteps(t, fences, stream.Finish(steps[:0]))
		checkStreamedFences(t, text, fences, true)
	}
}

func TestFenceStreamWithholdsOnlyPossibleClosers(t *testing.T) {
	var stream FenceStream
	var fences []streamedFence
	fences = applyFenceSteps(t, fences, stream.Write([]byte("```go\nx := 1\n``"), nil))
	if got := string(fences[0].content); got != "x := 1\n" {
		t.Fatalf("content %q: a possible closer must be withheld", got)
	}
	fences = applyFenceSteps(t, fences, stream.Write([]byte("x"), nil))
	if got := string(fences[0].content); got != "x := 1\n``x" {
		t.Fatalf("content %q: a line that cannot close must be released", got)
	}
}
