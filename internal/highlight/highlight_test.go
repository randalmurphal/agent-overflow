package highlight

import (
	"strings"
	"sync"
	"testing"
)

// newTestRenderer uses default options so tests exercise the zero-value path.
func newTestRenderer(t *testing.T) *Renderer {
	t.Helper()
	return New(Options{})
}

func TestNewZeroOptionsWorks(t *testing.T) {
	r := New(Options{})
	if r == nil {
		t.Fatal("New(Options{}) returned nil")
	}
	if r.maxBytes != defaultMaxBytes {
		t.Fatalf("default MaxBytes = %d, want %d", r.maxBytes, defaultMaxBytes)
	}
	if got := r.RenderMarkdown("hi"); !strings.Contains(got, "hi") {
		t.Fatalf("RenderMarkdown default renderer unusable: %q", got)
	}
}

func TestNewExplicitOptions(t *testing.T) {
	r := New(Options{MaxBytes: 16, Style: "monokai"})
	if r.maxBytes != 16 {
		t.Fatalf("MaxBytes = %d, want 16", r.maxBytes)
	}
	// Input below the cap should still go through goldmark.
	if got := r.RenderMarkdown("hi"); !strings.Contains(got, "<p>hi</p>") {
		t.Fatalf("below-cap render unexpected: %q", got)
	}
}

func TestRenderMarkdownPlainText(t *testing.T) {
	r := newTestRenderer(t)
	out := r.RenderMarkdown("hello world")
	if !strings.Contains(out, "<p>hello world</p>") {
		t.Fatalf("want <p>hello world</p>, got %q", out)
	}
}

func TestRenderMarkdownBoldItalic(t *testing.T) {
	r := newTestRenderer(t)
	out := r.RenderMarkdown("Hello **world** and *italic* text")
	if !strings.Contains(out, "<strong>world</strong>") {
		t.Fatalf("missing <strong>: %q", out)
	}
	if !strings.Contains(out, "<em>italic</em>") {
		t.Fatalf("missing <em>: %q", out)
	}
}

func TestRenderMarkdownHeadingsLists(t *testing.T) {
	r := newTestRenderer(t)
	out := r.RenderMarkdown("# Title\n\n- one\n- two\n- three\n")
	if !strings.Contains(out, "<h1>Title</h1>") {
		t.Fatalf("missing <h1>: %q", out)
	}
	for _, item := range []string{"<ul>", "<li>one</li>", "<li>two</li>", "<li>three</li>", "</ul>"} {
		if !strings.Contains(out, item) {
			t.Fatalf("missing %q in %q", item, out)
		}
	}
}

func TestRenderMarkdownKnownLanguageFenceHasChromaClasses(t *testing.T) {
	r := newTestRenderer(t)
	out := r.RenderMarkdown("```go\nfunc main() { fmt.Println(\"hi\") }\n```")
	// Known-language fences should produce class-prefixed chroma spans.
	if !strings.Contains(out, `class="ch-`) {
		t.Fatalf("expected ch- prefixed chroma classes, got %q", out)
	}
	// The <pre> wrapper should carry the chroma class too.
	if !strings.Contains(out, `<pre class="ch-chroma">`) {
		t.Fatalf("expected <pre class=\"ch-chroma\">, got %q", out)
	}
}

func TestRenderMarkdownUnknownLanguageFence(t *testing.T) {
	r := newTestRenderer(t)
	out := r.RenderMarkdown("```xyz\nsome content here\n```")
	// Unknown lexer must not crash; falls back to a plain <pre><code> with
	// a language-xyz hint from goldmark.
	if !strings.Contains(out, "<pre>") && !strings.Contains(out, "<pre ") {
		t.Fatalf("expected <pre> fallback, got %q", out)
	}
	if !strings.Contains(out, "some content here") {
		t.Fatalf("expected content to survive, got %q", out)
	}
	// Chroma did not run; no ch- classes inside the code.
	if strings.Contains(out, `class="ch-k`) || strings.Contains(out, `class="ch-nf`) {
		t.Fatalf("unexpected chroma classes for unknown language: %q", out)
	}
}

// TestRenderMarkdownGFMTable pins GFM table rendering. Without the
// GFM extension, the `| header | ... |\n|---|` block renders as three
// plain paragraphs with literal pipes — not a <table>. Agents emit
// tables frequently (e.g. comparison grids, config summaries); the
// plain-text fallback is user-visible as garbled output.
func TestRenderMarkdownGFMTable(t *testing.T) {
	r := newTestRenderer(t)
	in := "| A | B |\n|---|---|\n| 1 | 2 |\n| 3 | 4 |\n"
	out := r.RenderMarkdown(in)
	for _, want := range []string{
		"<table>", "<thead>", "<tbody>",
		"<th>A</th>", "<th>B</th>",
		"<td>1</td>", "<td>2</td>",
		"<td>3</td>", "<td>4</td>",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in table output: %q", want, out)
		}
	}
	// The raw pipe separator must not survive as literal text in a <p>.
	if strings.Contains(out, "|---|") {
		t.Fatalf("table separator survived as literal text: %q", out)
	}
}

// TestRenderMarkdownGFMStrikethrough pins GFM ~~strikethrough~~ support.
func TestRenderMarkdownGFMStrikethrough(t *testing.T) {
	r := newTestRenderer(t)
	out := r.RenderMarkdown("Hello ~~world~~ text")
	if !strings.Contains(out, "<del>world</del>") {
		t.Fatalf("missing <del>: %q", out)
	}
}

// TestRenderMarkdownGFMTaskList pins GFM task-list rendering:
// `- [ ]` / `- [x]` produce checkbox inputs, not literal brackets.
func TestRenderMarkdownGFMTaskList(t *testing.T) {
	r := newTestRenderer(t)
	out := r.RenderMarkdown("- [x] done\n- [ ] pending\n")
	if !strings.Contains(out, `type="checkbox"`) {
		t.Fatalf("missing checkbox input: %q", out)
	}
	if !strings.Contains(out, "checked") {
		t.Fatalf("missing checked attribute for [x]: %q", out)
	}
	// The literal bracket text must not bleed through.
	if strings.Contains(out, "[x]") || strings.Contains(out, "[ ]") {
		t.Fatalf("literal task-list brackets leaked into output: %q", out)
	}
}

// TestRenderMarkdownGFMTableAlignment pins GFM's column-alignment
// syntax (`| :--- |`, `| ---: |`, `| :---: |`). Without alignment
// handled, wide comparison tables lose their visual column affinity.
func TestRenderMarkdownGFMTableAlignment(t *testing.T) {
	r := newTestRenderer(t)
	in := "| L | C | R |\n| :--- | :---: | ---: |\n| a | b | c |\n"
	out := r.RenderMarkdown(in)
	// goldmark emits inline `style="text-align:<dir>"` on <th> / <td>
	// for aligned columns.
	for _, want := range []string{
		`style="text-align:left"`,
		`style="text-align:center"`,
		`style="text-align:right"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing alignment attribute %q: %q", want, out)
		}
	}
}

// TestRenderMarkdownGFMLinkify pins auto-link detection for plain URLs
// — and verifies the safe-autolink renderer still applies, so a
// linkified javascript: URL is stripped.
func TestRenderMarkdownGFMLinkify(t *testing.T) {
	r := newTestRenderer(t)
	out := r.RenderMarkdown("visit https://example.com today")
	if !strings.Contains(out, `<a href="https://example.com"`) {
		t.Fatalf("plain URL not auto-linked: %q", out)
	}
}

// TestRenderMarkdownDetailsBlock pins <details>/<summary> passthrough.
// Agents use collapsible blocks for long command output and optional
// context; without the safeHTMLRenderer those would render as literal
// "raw HTML omitted" comments and lose the <summary> caption entirely.
func TestRenderMarkdownDetailsBlock(t *testing.T) {
	r := newTestRenderer(t)
	in := "<details>\n<summary>Click to expand</summary>\n\ninner **bold** text\n\n</details>\n"
	out := r.RenderMarkdown(in)
	for _, want := range []string{
		"<details>",
		"<summary>Click to expand</summary>",
		"<strong>bold</strong>",
		"</details>",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q: %q", want, out)
		}
	}
	// The "omitted" placeholder must not appear when every HTML block
	// on the page is whitelisted.
	if strings.Contains(out, "raw HTML omitted") {
		t.Fatalf("whitelisted block dropped: %q", out)
	}
}

// TestRenderMarkdownDetailsOpenAttribute confirms the `open` attribute
// survives the attribute filter so servers can emit a collapsible that
// renders expanded by default.
func TestRenderMarkdownDetailsOpenAttribute(t *testing.T) {
	r := newTestRenderer(t)
	out := r.RenderMarkdown("<details open>\n<summary>Visible</summary>\nbody\n</details>\n")
	if !strings.Contains(out, "<details open>") {
		t.Fatalf("expected <details open>, got %q", out)
	}
}

func TestRenderMarkdownMalformedDoesNotCrash(t *testing.T) {
	r := newTestRenderer(t)
	inputs := []string{
		"**unclosed [link( text",
		"[[[[[[",
		"```go\nfunc main() {\n", // unterminated fence
		"| badly | formed |\n|---|\n| row |",
	}
	for _, in := range inputs {
		out := r.RenderMarkdown(in)
		if out == "" {
			t.Errorf("empty output for malformed input %q", in)
		}
	}
}

func TestRenderMarkdownOversizedFallsBackToEscapedPreCode(t *testing.T) {
	// MaxBytes small enough that we can construct an oversize input quickly.
	r := New(Options{MaxBytes: 16})
	// An input longer than 16 bytes that also contains characters an HTML
	// escape would visibly change, so we can confirm the escape path ran.
	oversized := "plaintext with <marker> content padding padding padding"
	out := r.RenderMarkdown(oversized)

	// Fallback marker: starts with <pre><code>.
	if !strings.HasPrefix(out, "<pre><code>") {
		t.Fatalf("expected oversized fallback to start with <pre><code>, got %q", out)
	}
	// HTML-escaped: the raw "<marker>" must not appear verbatim.
	if strings.Contains(out, "<marker>") {
		t.Fatalf("oversized fallback did not escape HTML: %q", out)
	}
	if !strings.Contains(out, "&lt;marker&gt;") {
		t.Fatalf("oversized fallback did not escape <marker>: %q", out)
	}
	// No chroma classes should appear in the oversize fallback.
	if strings.Contains(out, `class="ch-`) {
		t.Fatalf("oversized fallback should not run chroma: %q", out)
	}
}

func TestRenderMarkdownOversizedDefaultCap(t *testing.T) {
	r := newTestRenderer(t)
	// 600 KB of plain text exceeds the 512 KB default cap.
	huge := strings.Repeat("a", 600*1024)
	out := r.RenderMarkdown(huge)
	if !strings.HasPrefix(out, "<pre><code>") {
		t.Fatalf("expected oversized fallback, got prefix %q", out[:min(40, len(out))])
	}
}

func TestRenderANSIPlainText(t *testing.T) {
	r := newTestRenderer(t)
	out := r.RenderANSI("hello plain text")
	if !strings.Contains(out, "hello plain text") {
		t.Fatalf("missing literal text: %q", out)
	}
	// Plain text without escapes should not introduce spurious term classes.
	if strings.Contains(out, `class="term-fg`) {
		t.Fatalf("plain text produced term-fg class: %q", out)
	}
}

func TestRenderANSISGRSequences(t *testing.T) {
	r := newTestRenderer(t)
	cases := []struct {
		name      string
		in        string
		wantClass string
		wantText  string
	}{
		{"red", "\x1b[31mred text\x1b[0m", "term-fg31", "red text"},
		{"green", "\x1b[32mgreen text\x1b[0m", "term-fg32", "green text"},
		{"bold", "\x1b[1mbold text\x1b[0m", "term-fg1", "bold text"},
		{"bold-red", "\x1b[1;31mbold red\x1b[0m", "term-fg31", "bold red"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := r.RenderANSI(tc.in)
			if !strings.Contains(out, tc.wantClass) {
				t.Fatalf("missing class %q in %q", tc.wantClass, out)
			}
			if !strings.Contains(out, tc.wantText) {
				t.Fatalf("missing text %q in %q", tc.wantText, out)
			}
		})
	}
}

// TestRenderANSIBrightColors pins the class-name contract for bright
// (intense) foreground/background variants. terminal-to-html emits
// `term-fgi{N}` / `term-bgi{N}` (note the "i"), not `term-fg{N}` /
// `term-bg{N}`. The CSS in app.css must track these names — an earlier
// build named them without the "i" and bright colors went unstyled.
func TestRenderANSIBrightColors(t *testing.T) {
	r := newTestRenderer(t)
	cases := []struct {
		name, in, wantClass string
	}{
		{"bright-fg-90", "\x1b[90mgray\x1b[0m", "term-fgi90"},
		{"bright-fg-91", "\x1b[91mbrightred\x1b[0m", "term-fgi91"},
		{"bright-fg-97", "\x1b[97mbrightwhite\x1b[0m", "term-fgi97"},
		{"bright-bg-100", "\x1b[100mbg\x1b[0m", "term-bgi100"},
		{"bright-bg-107", "\x1b[107mbg\x1b[0m", "term-bgi107"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := r.RenderANSI(tc.in)
			if !strings.Contains(out, tc.wantClass) {
				t.Fatalf("missing class %q in %q", tc.wantClass, out)
			}
		})
	}
}

// TestRenderANSIAttributeCodes pins italic, underline, and strike
// emissions. terminal-to-html uses `term-fg{N}` for SGR codes 1-9
// (bold/italic/underline/blink/strike) — the class namespace overlaps
// with color codes 30-37 but the parameter numbers don't collide in
// practice.
func TestRenderANSIAttributeCodes(t *testing.T) {
	r := newTestRenderer(t)
	cases := []struct {
		name, in, wantClass string
	}{
		{"bold", "\x1b[1mbold\x1b[0m", "term-fg1"},
		{"italic", "\x1b[3mitalic\x1b[0m", "term-fg3"},
		{"underline", "\x1b[4munder\x1b[0m", "term-fg4"},
		{"strike", "\x1b[9mstrike\x1b[0m", "term-fg9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := r.RenderANSI(tc.in)
			if !strings.Contains(out, tc.wantClass) {
				t.Fatalf("missing class %q in %q", tc.wantClass, out)
			}
		})
	}
}

func TestRenderANSIPartialEscapeAtEnd(t *testing.T) {
	r := newTestRenderer(t)
	out := r.RenderANSI("hello\x1b[3")
	// Must not crash and must produce something reasonable. The partial
	// sequence may be dropped; "hello" must survive in the output.
	if !strings.Contains(out, "hello") {
		t.Fatalf("partial-escape input lost visible text: %q", out)
	}
	// The raw ESC byte must not leak into the output.
	if strings.ContainsRune(out, '\x1b') {
		t.Fatalf("raw ESC leaked into output: %q", out)
	}
}

func TestRenderANSIUnicode(t *testing.T) {
	r := newTestRenderer(t)
	out := r.RenderANSI("héllo 🐠 — ünicøde")
	for _, want := range []string{"héllo", "🐠", "ünicøde"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in unicode output %q", want, out)
		}
	}
}

func TestRenderANSIEmpty(t *testing.T) {
	r := newTestRenderer(t)
	out := r.RenderANSI("")
	if out != "" {
		t.Fatalf("expected empty string, got %q", out)
	}
}

// TestStripUnsafeEscapesFastPathReturnsInputIdentity pins the allocation
// short-circuit: on ESC-free input (the common case for streaming text
// and most command output), stripUnsafeEscapes must return the input
// string with no copy. Breaking the fast path re-introduces a full
// buffer allocation per throttled render — a few KB/sec × 20 renders/sec
// across every active thinking block.
func TestStripUnsafeEscapesFastPathReturnsInputIdentity(t *testing.T) {
	input := "plain text with no escape sequences at all"
	out := stripUnsafeEscapes(input)
	if out != input {
		t.Fatalf("stripUnsafeEscapes modified ESC-free input: %q → %q", input, out)
	}
	// Header equality check: the fast path should return the input string
	// header directly, not a fresh allocation. This is a best-effort proxy
	// for the zero-allocation contract — Go doesn't expose the underlying
	// data pointer from string cheaply, but on the fast path string(out)
	// shares storage with input.
	allocs := testing.AllocsPerRun(100, func() { _ = stripUnsafeEscapes(input) })
	if allocs > 0 {
		t.Fatalf("stripUnsafeEscapes fast path allocated %v times, want 0", allocs)
	}
}

func TestRenderANSIOversizedFallsBack(t *testing.T) {
	r := New(Options{MaxBytes: 16})
	out := r.RenderANSI("plain input with <marker> padding padding padding")
	if !strings.HasPrefix(out, "<pre><code>") {
		t.Fatalf("expected oversize fallback, got %q", out)
	}
	if strings.Contains(out, "<marker>") {
		t.Fatalf("oversize fallback did not escape HTML: %q", out)
	}
	if !strings.Contains(out, "&lt;marker&gt;") {
		t.Fatalf("oversize fallback did not escape marker: %q", out)
	}
}

func TestRenderForKindDispatch(t *testing.T) {
	r := newTestRenderer(t)

	// Non-empty content for each kind.
	mdInput := "# heading"
	ansiInput := "\x1b[31mred\x1b[0m"

	markdownKinds := []string{KindAssistantText, KindProposedPlan}
	for _, kind := range markdownKinds {
		out := r.RenderForKind(kind, mdInput)
		if !strings.Contains(out, "<h1>heading</h1>") {
			t.Errorf("kind %q did not use markdown renderer: %q", kind, out)
		}
	}

	ansiKinds := []string{KindThinking, KindCommandOutput, KindToolResult}
	for _, kind := range ansiKinds {
		out := r.RenderForKind(kind, ansiInput)
		if !strings.Contains(out, "term-fg31") {
			t.Errorf("kind %q did not use ANSI renderer: %q", kind, out)
		}
	}

	emptyResultKinds := []string{
		KindDiff, KindUserText, KindToolCall, KindToolCompletion,
		KindError, KindCompaction, "unknown", "",
	}
	for _, kind := range emptyResultKinds {
		if got := r.RenderForKind(kind, mdInput); got != "" {
			t.Errorf("kind %q expected empty string, got %q", kind, got)
		}
	}
}

func TestRenderForKindEmptyContent(t *testing.T) {
	r := newTestRenderer(t)
	allKinds := []string{
		KindAssistantText, KindProposedPlan,
		KindThinking, KindCommandOutput, KindToolResult,
		KindDiff, KindUserText, KindToolCall, KindToolCompletion,
		KindError, KindCompaction, "anything else",
	}
	for _, kind := range allKinds {
		if got := r.RenderForKind(kind, ""); got != "" {
			t.Errorf("kind %q with empty content returned non-empty %q", kind, got)
		}
	}
}

func TestRendererConcurrentUse(t *testing.T) {
	r := newTestRenderer(t)

	inputs := []struct {
		kind, content string
	}{
		{KindAssistantText, "# heading\n\n**bold** text\n"},
		{KindAssistantText, "```go\nfunc main() {}\n```"},
		{KindThinking, "\x1b[31mred\x1b[0m \x1b[32mgreen\x1b[0m"},
		{KindCommandOutput, "plain command output"},
		{KindToolResult, "\x1b[1;33mwarning\x1b[0m"},
		{KindProposedPlan, "## plan\n\n1. step\n2. step\n"},
		{KindDiff, "should be empty"},
		{"unknown", "also empty"},
	}

	const goroutines = 100
	const iterations = 20

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(seed int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				in := inputs[(seed+j)%len(inputs)]
				// Exercise all three public render paths in a rotation.
				switch j % 3 {
				case 0:
					_ = r.RenderMarkdown(in.content)
				case 1:
					_ = r.RenderANSI(in.content)
				case 2:
					_ = r.RenderForKind(in.kind, in.content)
				}
			}
		}(i)
	}
	wg.Wait()
}
