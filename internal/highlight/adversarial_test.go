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

func TestMarkdownJavascriptAutoLinkIsNeutralized(t *testing.T) {
	// Autolink syntax — `<javascript:...>` — goes through goldmark's
	// renderAutoLink, which (unlike renderLink) does NOT consult
	// IsDangerousURL. Our safeAutoLinkRenderer restores that check.
	r := New(Options{})
	for _, raw := range []string{
		`<javascript:alert(1)>`,
		`<JavaScript:alert(2)>`,
		`<vbscript:msgbox(3)>`,
		`<data:text/html,<script>alert(4)</script>>`,
		`<file:///etc/passwd>`,
	} {
		out := r.RenderMarkdown(raw)
		lower := strings.ToLower(out)
		if strings.Contains(lower, `href="javascript:`) ||
			strings.Contains(lower, `href="vbscript:`) ||
			strings.Contains(lower, `href="data:`) ||
			strings.Contains(lower, `href="file:`) {
			t.Fatalf("dangerous scheme survived autolink for %q: %q", raw, out)
		}
	}
}

func TestMarkdownSafeAutoLinkPreserved(t *testing.T) {
	// Regression guard: benign URLs still produce live hrefs.
	r := New(Options{})
	out := r.RenderMarkdown(`<https://example.com/a?b=1>`)
	lower := strings.ToLower(out)
	if !strings.Contains(lower, `href="https://example.com/a?b=1"`) {
		t.Fatalf("expected benign autolink href, got %q", out)
	}
}

func TestMarkdownImageSVGDataURIIsNeutralized(t *testing.T) {
	// goldmark's IsDangerousURL whitelists `data:image/svg+xml`, but a
	// crafted SVG can carry script payloads that some webviews execute
	// when the image loads. safeLinkRenderer's Image override adds that
	// scheme back to the block list; the other raster data: URIs are
	// still allowed.
	r := New(Options{})
	svg := `![evil](data:image/svg+xml;base64,PHN2Zy8+)`
	out := r.RenderMarkdown(svg)
	lower := strings.ToLower(out)
	if strings.Contains(lower, "data:image/svg") {
		t.Fatalf("svg data URI survived: %q", out)
	}

	// Benign raster data URI should still render.
	png := `![ok](data:image/png;base64,iVBORw0KGgo=)`
	outPng := r.RenderMarkdown(png)
	if !strings.Contains(outPng, `src="data:image/png;`) {
		t.Fatalf("benign PNG data URI unexpectedly dropped: %q", outPng)
	}
}

func TestMarkdownImageJavascriptURIIsNeutralized(t *testing.T) {
	// Standard dangerous schemes must be dropped on image src just like
	// on link href.
	r := New(Options{})
	for _, raw := range []string{
		`![x](javascript:alert(1))`,
		`![x](vbscript:msgbox(1))`,
		`![x](file:///etc/passwd)`,
	} {
		out := r.RenderMarkdown(raw)
		lower := strings.ToLower(out)
		if strings.Contains(lower, `src="javascript:`) ||
			strings.Contains(lower, `src="vbscript:`) ||
			strings.Contains(lower, `src="file:`) {
			t.Fatalf("dangerous scheme survived image for %q: %q", raw, out)
		}
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

func TestANSIOSC8HyperlinkStripped(t *testing.T) {
	// OSC 8 hyperlinks wrap visible text in
	//   ESC]8;;URL(BEL|ESC\)text ESC]8;;(BEL|ESC\)
	// terminal-to-html by default turns that into <a href="URL">text</a>,
	// which would let untrusted tool output smuggle `javascript:` URIs
	// through the ANSI render path into {@html}. stripUnsafeEscapes drops every
	// OSC 8 introducer and terminator (both ST forms) before handing the
	// bytes to terminal-to-html.
	r := New(Options{})
	cases := []string{
		"\x1b]8;;javascript:alert(1)\x1b\\click\x1b]8;;\x1b\\",
		"\x1b]8;;javascript:alert(2)\x07click\x1b]8;;\x07",
		"\x1b]8;;http://evil.example/\x1b\\evil\x1b]8;;\x1b\\",
		"\x1b]8;id=1;data:text/html,<script>x()</script>\x07evil\x1b]8;;\x07",
	}
	for _, raw := range cases {
		out := r.RenderANSI(raw)
		lower := strings.ToLower(out)
		if strings.Contains(lower, "<a ") || strings.Contains(lower, "href=") {
			t.Fatalf("OSC 8 link leaked for %q: %q", raw, out)
		}
		if strings.Contains(lower, "javascript:") || strings.Contains(lower, "data:text/html") {
			t.Fatalf("dangerous URI leaked for %q: %q", raw, out)
		}
	}
}

func TestANSIOSC8PartialSequenceDropped(t *testing.T) {
	// A stream can end mid-OSC8 (`\x1b]8;;http://evil`). stripUnsafeEscapes drops
	// the trailing partial sequence rather than passing its bytes through,
	// matching terminal-to-html's end-of-input discard for partial SGR.
	r := New(Options{})
	out := r.RenderANSI("visible\x1b]8;;javascript:alert(1)")
	lower := strings.ToLower(out)
	if strings.Contains(lower, "javascript:") {
		t.Fatalf("partial OSC 8 URI leaked: %q", out)
	}
	if !strings.Contains(lower, "visible") {
		t.Fatalf("visible prefix dropped: %q", out)
	}
}

// TestANSIBuildkiteHyperlinkStripped closes a vector missed by the
// initial OSC 8 fix: terminal-to-html also renders Buildkite-style OSC
// 1339 links as live <a href>, and its URL sanitizer only blocks the
// `javascript:` scheme. `vbscript:`, `data:text/html`, `file:` etc. all
// slipped through until stripUnsafeEscapes widened the strip to every
// OSC/APC envelope.
func TestANSIBuildkiteHyperlinkStripped(t *testing.T) {
	r := New(Options{})
	cases := []string{
		"\x1b]1339;url=data:text/html,<script>alert(1)</script>;content=click\x07",
		"\x1b]1339;url=vbscript:msgbox(2);content=click\x07",
		"\x1b]1339;url=file:///etc/passwd;content=read\x07",
		"\x1b]1339;url=http://evil.example/;content=hover\x1b\\",
	}
	for _, raw := range cases {
		out := r.RenderANSI(raw)
		lower := strings.ToLower(out)
		if strings.Contains(lower, "<a ") || strings.Contains(lower, "href=") {
			t.Fatalf("OSC 1339 link leaked for %q: %q", raw, out)
		}
		if strings.Contains(lower, "javascript:") ||
			strings.Contains(lower, "vbscript:") ||
			strings.Contains(lower, "data:text/html") ||
			strings.Contains(lower, "file:") {
			t.Fatalf("dangerous scheme leaked for %q: %q", raw, out)
		}
	}
}

// TestANSIITermImageStripped closes the iTerm2 inline-image route
// (OSC 1337). terminal-to-html emits
//
//	<img alt="..." src="data:TYPE;base64,CONTENT">
//
// from `ESC]1337;File=...:BASE64`; attacker-controlled `TYPE` lets
// `data:image/svg+xml` slip through as `<img src=...>` with script-
// eligible content in older webviews. stripUnsafeEscapes drops the
// whole sequence.
func TestANSIITermImageStripped(t *testing.T) {
	r := New(Options{})
	cases := []string{
		"\x1b]1337;File=name=x.svg;inline=1:PHN2Zy8+\x07",
		"\x1b]1337;File=name=ev.svg;inline=1:PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciPjxzY3JpcHQ+YWxlcnQoMSk8L3NjcmlwdD48L3N2Zz4=\x1b\\",
	}
	for _, raw := range cases {
		out := r.RenderANSI(raw)
		lower := strings.ToLower(out)
		if strings.Contains(lower, "<img ") {
			t.Fatalf("OSC 1337 image leaked for %q: %q", raw, out)
		}
		if strings.Contains(lower, "data:") || strings.Contains(lower, "base64") {
			t.Fatalf("data URI leaked for %q: %q", raw, out)
		}
	}
}

// TestANSIBuildkiteExternalImageStripped closes OSC 1338 — Buildkite
// external images emitted as `<img src=URL>`. sanitizeURL lets through
// `data:` and `file:` schemes.
func TestANSIBuildkiteExternalImageStripped(t *testing.T) {
	r := New(Options{})
	cases := []string{
		"\x1b]1338;url=data:image/svg+xml,%3Csvg%3E%3Cscript%3Ealert(1)%3C%2Fscript%3E%3C%2Fsvg%3E\x07",
		"\x1b]1338;url=file:///etc/passwd\x1b\\",
	}
	for _, raw := range cases {
		out := r.RenderANSI(raw)
		lower := strings.ToLower(out)
		if strings.Contains(lower, "<img ") {
			t.Fatalf("OSC 1338 image leaked for %q: %q", raw, out)
		}
		if strings.Contains(lower, "data:image") || strings.Contains(lower, "file:") {
			t.Fatalf("dangerous img URI leaked for %q: %q", raw, out)
		}
	}
}

// TestANSIApplicationProgramCommandStripped closes the APC (`ESC_…ST`)
// route — terminal-to-html treats APC-wrapped `1337;…` and `1339;…`
// payloads the same way it treats OSC-wrapped ones. Any attacker who
// can inject an APC gets the same vectors.
func TestANSIApplicationProgramCommandStripped(t *testing.T) {
	r := New(Options{})
	cases := []string{
		"\x1b_1339;url=javascript:alert(1);content=click\x1b\\",
		"\x1b_1337;File=name=x.svg;inline=1:PHN2Zy8+\x1b\\",
	}
	for _, raw := range cases {
		out := r.RenderANSI(raw)
		lower := strings.ToLower(out)
		if strings.Contains(lower, "<a ") || strings.Contains(lower, "<img ") {
			t.Fatalf("APC element leaked for %q: %q", raw, out)
		}
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
