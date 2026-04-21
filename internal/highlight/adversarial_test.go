package highlight

import (
	"strings"
	"testing"
)

// The adversarial tests confirm that content that makes it through the
// renderer — markdown or ANSI — cannot smuggle in executable HTML. The
// frontend paints output via `{@html}`, so any live <script>, javascript:
// href, or inline event handler would be a cross-site scripting vector.

func TestMarkdownScriptTagInlineIsNeutralized(t *testing.T) {
	r := New(Options{})
	out := r.RenderMarkdown("before <script>alert(1)</script> after")
	// goldmark's default policy is to drop raw HTML blocks/inlines and
	// replace them with a comment. The live tag must not appear and the
	// literal payload must not execute as HTML.
	if containsLiveScript(out) {
		t.Fatalf("live <script> tag leaked: %q", out)
	}
	// Specifically, the opening and closing tags must not be present as
	// markup. If goldmark ever changed to escape them, the angle brackets
	// would still be present as entity references, which is also safe.
	assertNoLiveTag(t, out, "script")
}

func TestMarkdownScriptTagInsideFenceIsEscaped(t *testing.T) {
	r := New(Options{})
	// Inside a fenced code block, chroma treats the content as source and
	// HTML-escapes everything before emitting spans.
	out := r.RenderMarkdown("```\n<script>alert(1)</script>\n```")
	if containsLiveScript(out) {
		t.Fatalf("live <script> tag inside fence leaked: %q", out)
	}
	// The escaped form must appear so the user actually sees the literal
	// text they pasted.
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Fatalf("expected escaped <script> inside fence, got %q", out)
	}
}

func TestMarkdownJavascriptURIInLink(t *testing.T) {
	r := New(Options{})
	out := r.RenderMarkdown(`[click](javascript:void(0))`)
	// goldmark's default URL filter strips dangerous schemes. The output
	// either omits the href entirely or has an empty one — the key
	// property is that no `href="javascript:...` survives.
	lower := strings.ToLower(out)
	if strings.Contains(lower, `href="javascript:`) {
		t.Fatalf("javascript: URL survived: %q", out)
	}
}

func TestMarkdownInlineEventHandlerImg(t *testing.T) {
	r := New(Options{})
	out := r.RenderMarkdown(`<img src=x onerror=alert(1)>`)
	// Raw HTML block: goldmark drops it. No live <img> should appear in a
	// form that would execute when inserted into the DOM.
	if strings.Contains(strings.ToLower(out), "<img ") {
		t.Fatalf("live <img> survived: %q", out)
	}
	// `onerror=alert(1)` is only dangerous when it's inside an unescaped
	// tag; goldmark's default either drops the block entirely or escapes
	// the brackets, both of which defuse the handler.
}

func TestANSIHTMLInsideEscapeIsEscaped(t *testing.T) {
	r := New(Options{})
	out := r.RenderANSI("\x1b[31m<script>pwn()</script>\x1b[0m")
	if containsLiveScript(out) {
		t.Fatalf("live <script> leaked through ANSI renderer: %q", out)
	}
	// The color span still wraps the escaped payload.
	if !strings.Contains(out, "term-fg31") {
		t.Fatalf("expected term-fg31 color span, got %q", out)
	}
	// The payload must be HTML-escaped.
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Fatalf("expected escaped <script> inside ANSI span, got %q", out)
	}
}

func TestANSIRawHTMLIsEscaped(t *testing.T) {
	r := New(Options{})
	out := r.RenderANSI("<img src=x onerror=alert(1)>")
	// The key invariant: no live <img> tag. The word "onerror" can
	// legitimately appear inside an escaped text node — it only becomes
	// dangerous when it sits on an unescaped tag, which we check for via
	// the "<img " substring (the raw opening bracket form).
	if strings.Contains(strings.ToLower(out), "<img ") {
		t.Fatalf("live <img> survived ANSI renderer: %q", out)
	}
	// The angle brackets must be entity-escaped rather than passed through.
	if !strings.Contains(out, "&lt;img") {
		t.Fatalf("expected &lt;img in output, got %q", out)
	}
	if !strings.Contains(out, "&gt;") {
		t.Fatalf("expected &gt; in output, got %q", out)
	}
}

// containsLiveScript returns true if out contains a start <script> tag that
// could execute when inserted into the DOM. We match on the case-insensitive
// substring "<script" followed by a character that would close the tag name
// (whitespace or '>'), which catches both `<script>` and `<script src=...>`.
func containsLiveScript(out string) bool {
	lower := strings.ToLower(out)
	idx := 0
	for {
		at := strings.Index(lower[idx:], "<script")
		if at < 0 {
			return false
		}
		start := idx + at + len("<script")
		if start >= len(lower) {
			return false
		}
		next := lower[start]
		if next == '>' || next == ' ' || next == '\t' || next == '\n' || next == '/' {
			return true
		}
		idx = start
	}
}

func assertNoLiveTag(t *testing.T, out, tag string) {
	t.Helper()
	lower := strings.ToLower(out)
	// A live tag would look like "<tag " or "<tag>" or "<tag/". The
	// escaped form "&lt;tag&gt;" must not match.
	needle := "<" + tag
	if strings.Contains(lower, needle+">") || strings.Contains(lower, needle+" ") {
		t.Fatalf("found live <%s> tag in %q", tag, out)
	}
}
