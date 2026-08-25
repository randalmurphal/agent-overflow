package highlight

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// The original bug report: a Python docstring whose prose contains
// keywords (`and`, `for`, `is`) rendered them keyword-colored because
// the old per-line tokenizers lost the docstring state between lines.
const pythonDocstringSample = `def route_template_path(request):
    """Return the matched route's template path.

    Routing populates the scope before authentication and
    authorization dependencies run, for every handler.
    """
    route = request.scope.get("route")
    return route.path if route is not None else None
`

// expandRuns turns one line's runs back into a per-byte class buffer.
func expandRuns(t *testing.T, line EncodedLine, byteLen int) []uint16 {
	t.Helper()
	classes := make([]uint16, byteLen)
	if line.Runs == nil {
		return classes
	}
	pos := 0
	for i := 0; i < len(line.Runs); i += 2 {
		n, class := int(line.Runs[i]), line.Runs[i+1]
		for j := 0; j < n; j++ {
			if pos >= byteLen {
				t.Fatalf("runs overflow line length %d: %v", byteLen, line.Runs)
			}
			classes[pos] = class
			pos++
		}
	}
	if pos != byteLen {
		t.Fatalf("runs cover %d bytes, line has %d: %v", pos, byteLen, line.Runs)
	}
	return classes
}

// classOf returns the class of substr's first occurrence in text.
// Fails if the substring's bytes are not all one class.
func classOf(t *testing.T, line EncodedLine, text, substr string) uint16 {
	t.Helper()
	idx := strings.Index(text, substr)
	if idx < 0 {
		t.Fatalf("substring %q not in line %q", substr, text)
	}
	classes := expandRuns(t, line, len(text))
	class := classes[idx]
	for i := idx; i < idx+len(substr); i++ {
		if classes[i] != class {
			t.Fatalf("substring %q spans classes %d and %d in line %q", substr, class, classes[i], text)
		}
	}
	return class
}

func assertNoClass(t *testing.T, line EncodedLine, text string, class uint16, label string) {
	t.Helper()
	for i, c := range expandRuns(t, line, len(text)) {
		if c == class {
			t.Errorf("%s: line %q has class %d at byte %d, want none", label, text, class, i)
		}
	}
}

func TestPythonDocstringRegression(t *testing.T) {
	src := pythonDocstringSample
	res := Highlight(LangPython, []byte(src))
	lines := strings.Split(src, "\n")
	if len(res.Lines) != len(lines) {
		t.Fatalf("lines = %d, want %d", len(res.Lines), len(lines))
	}
	if res.Truncated {
		t.Fatal("unexpected truncation")
	}

	if got := classOf(t, res.Lines[0], lines[0], "def"); got != ClassKeyword {
		t.Errorf("`def` class = %d, want keyword %d", got, ClassKeyword)
	}
	if got := classOf(t, res.Lines[0], lines[0], "route_template_path"); got != ClassFunction {
		t.Errorf("function name class = %d, want function %d", got, ClassFunction)
	}

	// The heart of the regression: keywords inside docstring prose must
	// NOT be keyword-classed, and the prose must be string-classed.
	for _, i := range []int{1, 2, 3, 4, 5} {
		assertNoClass(t, res.Lines[i], lines[i], ClassKeyword, "docstring line")
	}
	if got := classOf(t, res.Lines[3], lines[3], "authentication and"); got != ClassString {
		t.Errorf("docstring prose class = %d, want string %d", got, ClassString)
	}

	// Real keywords after the docstring still highlight.
	if got := classOf(t, res.Lines[7], lines[7], "return"); got != ClassKeyword {
		t.Errorf("`return` class = %d, want keyword %d", got, ClassKeyword)
	}
	if got := classOf(t, res.Lines[7], lines[7], "is"); got != ClassKeyword {
		t.Errorf("`is` class = %d, want keyword %d", got, ClassKeyword)
	}
}

const goMultilineSample = "package main\n" +
	"\n" +
	"/* block comment with for and if keywords\n" +
	"   spanning lines */\n" +
	"const query = `SELECT id\n" +
	"FROM users WHERE for func`\n" +
	"\n" +
	"func main() {\n" +
	"\tfor i := 0; i < 10; i++ {\n" +
	"\t}\n" +
	"}\n"

func TestGoMultilineConstructs(t *testing.T) {
	res := Highlight(LangGo, []byte(goMultilineSample))
	lines := strings.Split(goMultilineSample, "\n")

	// Both block-comment lines are comment-classed; the `for`/`if`
	// inside are not keywords.
	for _, i := range []int{2, 3} {
		assertNoClass(t, res.Lines[i], lines[i], ClassKeyword, "block comment line")
	}
	if got := classOf(t, res.Lines[2], lines[2], "block comment"); got != ClassComment {
		t.Errorf("block comment class = %d, want comment %d", got, ClassComment)
	}
	if got := classOf(t, res.Lines[3], lines[3], "spanning"); got != ClassComment {
		t.Errorf("comment continuation class = %d, want comment %d", got, ClassComment)
	}

	// Raw string continuation line: `for`/`func` inside stay string.
	assertNoClass(t, res.Lines[5], lines[5], ClassKeyword, "raw string line")
	if got := classOf(t, res.Lines[5], lines[5], "FROM users"); got != ClassString {
		t.Errorf("raw string continuation class = %d, want string %d", got, ClassString)
	}

	// Real keywords still highlight.
	if got := classOf(t, res.Lines[8], lines[8], "for"); got != ClassKeyword {
		t.Errorf("`for` class = %d, want keyword %d", got, ClassKeyword)
	}
}

func TestHighlightUnknownLangIsPlain(t *testing.T) {
	// No engine → nil lines (absent lines render plain); allocating a
	// per-line result for unhighlightable input would let a huge
	// unknown-lang request allocate proportional to its line count.
	src := []byte("some plain text\nwith lines\n")
	res := Highlight(LangPlaintext, src)
	if len(res.Lines) != 0 || res.Truncated {
		t.Fatalf("unexpected result: %d lines, truncated=%v", len(res.Lines), res.Truncated)
	}
}

func TestHighlightBoundsAllocationForPathologicalInputs(t *testing.T) {
	// Over the request cap: nothing is counted or allocated.
	over := Highlight(LangPython, bytes.Repeat([]byte("\n"), MaxRequestBytes+1))
	if !over.Truncated || len(over.Lines) != 0 {
		t.Errorf("over-request-cap = {truncated %v, %d lines}, want truncated with no lines", over.Truncated, len(over.Lines))
	}

	// Under every byte cap but with more lines than maxResultLines
	// (sub-4-byte average lines): degrade without a per-line result.
	dense := Highlight(LangPython, bytes.Repeat([]byte("\n"), maxResultLines+1))
	if !dense.Truncated || len(dense.Lines) != 0 {
		t.Errorf("over-line-cap = {truncated %v, %d lines}, want truncated with no lines", dense.Truncated, len(dense.Lines))
	}
}

func TestHighlightPatchTextBoundsPathologicalInputs(t *testing.T) {
	over := HighlightPatchText(LangPython, strings.Repeat("\n", MaxRequestBytes+1))
	if !over.Truncated || len(over.Lines) != 0 {
		t.Errorf("over-request-cap patch = {truncated %v, %d lines}, want truncated with no lines", over.Truncated, len(over.Lines))
	}
	dense := HighlightPatchText(LangPython, strings.Repeat("\n", maxResultLines+1))
	if !dense.Truncated || len(dense.Lines) != 0 {
		t.Errorf("over-line-cap patch = {truncated %v, %d lines}, want truncated with no lines", dense.Truncated, len(dense.Lines))
	}
}

func TestHighlightPatchTextBudgetExhaustionRendersPlain(t *testing.T) {
	prev := patchParseBudget
	patchParseBudget = 0
	defer func() { patchParseBudget = prev }()

	res := HighlightPatchText(LangPython, pythonDocstringPatch)
	lines := strings.Split(strings.TrimSuffix(pythonDocstringPatch, "\n"), "\n")
	if len(res.Lines) != len(lines) {
		t.Fatalf("lines = %d, want %d (alignment survives budget exhaustion)", len(res.Lines), len(lines))
	}
	if !res.Truncated {
		t.Error("want Truncated when the parse budget is exhausted")
	}
	if !res.Incomplete {
		t.Error("want incomplete: budget exhaustion is load-dependent and must not be memoized")
	}
	for i, line := range res.Lines {
		if line.Runs != nil {
			t.Errorf("line %d not plain after budget exhaustion: %v", i, line.Runs)
		}
	}
}

func TestPrimerMatchesSplitJoin(t *testing.T) {
	contents := []string{
		"a\nb\nc\nd",
		"a\nb\nc\n",
		"single",
		"",
		"\n\n",
	}
	for _, content := range contents {
		pr := primer{content: content}
		// Ascending, repeated, then descending (scan restart) requests.
		for _, newStart := range []int{1, 2, 3, 3, 10, 2} {
			var want string
			if content != "" && newStart > 1 {
				fileLines := strings.Split(content, "\n")
				n := min(newStart-1, len(fileLines))
				want = strings.Join(fileLines[:n], "\n")
			}
			if got := pr.primeFor(newStart); got != want {
				t.Errorf("primeFor(%d) on %q = %q, want %q", newStart, content, got, want)
			}
		}
	}
}

func TestPrimerSuffixFrom(t *testing.T) {
	contents := []string{
		"a\nb\nc\nd",
		"a\nb\nc\n",
		"single",
		"",
		"\n\n",
	}
	for _, content := range contents {
		pr := primer{content: content}
		// Interleaved prefix-then-suffix requests in ascending order —
		// the shape HighlightPatchTextPrimed drives per hunk — then a
		// descending request (scan restart).
		for _, at := range []int{1, 2, 3, 3, 10, 2} {
			prime := pr.primeFor(at)
			suffix := pr.suffixFrom(at + 1)
			var wantPrime, wantSuffix string
			if content != "" {
				fileLines := strings.Split(content, "\n")
				if at > 1 {
					wantPrime = strings.Join(fileLines[:min(at-1, len(fileLines))], "\n")
				}
				if at < len(fileLines) {
					wantSuffix = strings.Join(fileLines[at:], "\n")
				}
			}
			if prime != wantPrime {
				t.Errorf("primeFor(%d) on %q = %q, want %q", at, content, prime, wantPrime)
			}
			if suffix != wantSuffix {
				t.Errorf("suffixFrom(%d) on %q = %q, want %q", at+1, content, suffix, wantSuffix)
			}
		}
	}
}

// A hunk INSIDE a raw-text element (svelte/html <script>) must still
// highlight when primed: without the file content BELOW the hunk the
// element never closes, the grammar never emits the raw_text node the
// TypeScript injection anchors on, and every hunk line painted plain —
// which then persisted as a primed "best possible" span blob
// (2026-07-19 regression).
func TestHighlightPatchTextPrimedClosesRawTextElement(t *testing.T) {
	content := `<script lang="ts">
  function above(): number {
    return 1;
  }
  function target(): number {
    return 2;
  }
</script>

<div class="x">{above()}</div>
`
	patch := "diff --git a/x.svelte b/x.svelte\n" +
		"--- a/x.svelte\n" +
		"+++ b/x.svelte\n" +
		"@@ -5,3 +5,3 @@\n" +
		"   function target(): number {\n" +
		"-    return 0;\n" +
		"+    return 2;\n" +
		"   }\n"
	lang := LangFromPath("x.svelte")

	res := HighlightPatchTextPrimed(lang, patch, content)
	if res.Incomplete {
		t.Fatal("primed parse reported incomplete")
	}
	// Patch lines 4-7 are the hunk body; the function signature and the
	// add line are TypeScript and must carry spans.
	for _, i := range []int{4, 6} {
		if len(res.Lines[i].Runs) == 0 {
			t.Errorf("patch line %d rendered plain; primed script-body hunks must highlight", i)
		}
	}

	// Control: the unprimed parse of the same hunk sees markup text and
	// stays plain — the priming is what carries the script context.
	unprimed := HighlightPatchText(lang, patch)
	if got := unprimed.Lines[6].Runs; len(got) != 0 {
		t.Logf("unprimed add line unexpectedly has runs %v (grammar change?)", got)
	}
}

func TestHighlightOverCapTruncates(t *testing.T) {
	// Build > maxInputBytes of python using long (but under
	// maxLineBytes) lines so the head parses quickly; everything must
	// render (plain past the cap) with Truncated set.
	var b strings.Builder
	line := "x = 1  # " + strings.Repeat("y", 900) + "\n"
	for b.Len() <= maxInputBytes {
		b.WriteString(line)
	}
	src := b.String()
	res := Highlight(LangPython, []byte(src))
	if !res.Truncated {
		t.Fatal("want Truncated for over-cap input")
	}
	want := len(strings.Split(src, "\n"))
	if len(res.Lines) != want {
		t.Fatalf("lines = %d, want %d", len(res.Lines), want)
	}
	if res.Lines[0].Runs == nil {
		t.Error("head of over-cap input should still highlight")
	}
	if last := res.Lines[len(res.Lines)-2]; last.Runs != nil {
		t.Error("tail past the cap should be plain")
	}
}

const pythonDocstringPatch = `diff --git a/route.py b/route.py
--- a/route.py
+++ b/route.py
@@ -1,6 +1,7 @@
 def route_template_path(request):
-    """Old docstring.
+    """Routing populates the scope before authentication and
+    authorization dependencies run, for every handler.
     """
     route = request.scope.get("route")
     return route.path if route is not None else None
`

func TestHighlightPatchTextDocstring(t *testing.T) {
	res := HighlightPatchText(LangPython, pythonDocstringPatch)
	lines := strings.Split(strings.TrimSuffix(pythonDocstringPatch, "\n"), "\n")
	if len(res.Lines) != len(lines) {
		t.Fatalf("lines = %d, want %d", len(res.Lines), len(lines))
	}

	// Meta lines are plain.
	for i := 0; i < 4; i++ {
		if res.Lines[i].Runs != nil {
			t.Errorf("meta line %d not plain: %v", i, res.Lines[i].Runs)
		}
	}

	// Added docstring lines: spans cover the prefix-stripped body and
	// must be string-classed with no keywords despite `and`/`for`.
	for _, i := range []int{6, 7} {
		body := lines[i][1:]
		assertNoClass(t, res.Lines[i], body, ClassKeyword, "added docstring line")
	}
	if got := classOf(t, res.Lines[6], lines[6][1:], "authentication and"); got != ClassString {
		t.Errorf("added docstring class = %d, want string %d", got, ClassString)
	}

	// Deleted docstring line highlights from the OLD document.
	if got := classOf(t, res.Lines[5], lines[5][1:], "Old docstring"); got != ClassString {
		t.Errorf("deleted docstring class = %d, want string %d", got, ClassString)
	}

	// Context line: full content including leading space; `def` is a
	// keyword, and the 1-byte pad keeps byte accounting aligned.
	if got := classOf(t, res.Lines[4], lines[4], "def"); got != ClassKeyword {
		t.Errorf("context `def` class = %d, want keyword %d", got, ClassKeyword)
	}
	// Keywords in the trailing context line (`is not ... else`) resolve
	// against the joined virtual document, proving cross-line state.
	last := len(lines) - 1
	if got := classOf(t, res.Lines[last], lines[last], "else"); got != ClassKeyword {
		t.Errorf("context `else` class = %d, want keyword %d", got, ClassKeyword)
	}
}

func TestHighlightPatchTextUnknownLang(t *testing.T) {
	res := HighlightPatchText(LangPlaintext, pythonDocstringPatch)
	for i, line := range res.Lines {
		if line.Runs != nil {
			t.Errorf("line %d not plain for plaintext patch: %v", i, line.Runs)
		}
	}
}

// When the patch matches the file, every hunk's NEW-side splice
// (prime + newDoc + suffix) reconstructs the identical full file
// content — without the per-call memo an H-hunk patch parsed that one
// document H times (591MB of allocation in 10 minutes of agent edits,
// measured 2026-08-25). Pins both halves: the parse count and span
// parity against un-memoized single-hunk baselines.
func TestHighlightPatchTextPrimedMemoizesMatchingSplices(t *testing.T) {
	var fileLines []string
	for i := 1; i <= 30; i++ {
		fileLines = append(fileLines, fmt.Sprintf("x%d = %d  # line", i, i))
	}
	content := strings.Join(fileLines, "\n") + "\n"

	header := "diff --git a/m.py b/m.py\n--- a/m.py\n+++ b/m.py\n"
	hunkAt := func(start int) string {
		return fmt.Sprintf("@@ -%d,3 +%d,3 @@\n", start, start) +
			" " + fileLines[start-1] + "\n" +
			fmt.Sprintf("-old%d = None\n", start+1) +
			"+" + fileLines[start] + "\n" +
			" " + fileLines[start+1] + "\n"
	}
	starts := []int{5, 15, 25}
	patch := header
	for _, s := range starts {
		patch += hunkAt(s)
	}

	before := hunkDocParses.Load()
	res := HighlightPatchTextPrimed(LangPython, patch, content)
	parses := hunkDocParses.Load() - before
	if res.Incomplete {
		t.Fatal("primed parse reported incomplete")
	}
	// 3 distinct old-side splices + 1 shared new-side splice. 6 means
	// the memo stopped deduplicating the identical new-side documents.
	if parses != 4 {
		t.Fatalf("parses = %d, want 4 (3 old sides + 1 memoized new side)", parses)
	}

	// Parity: each hunk's span lines must byte-match the same hunk
	// highlighted alone (its own call, memo cold — the un-memoized
	// baseline).
	const headerLines, hunkLines = 3, 5
	for k, s := range starts {
		single := HighlightPatchTextPrimed(LangPython, header+hunkAt(s), content)
		for j := 0; j < hunkLines; j++ {
			got := res.Lines[headerLines+k*hunkLines+j]
			want := single.Lines[headerLines+j]
			if !reflect.DeepEqual(got, want) {
				t.Errorf("hunk %d line %d: memoized %v, baseline %v", k, j, got, want)
			}
		}
	}
}
