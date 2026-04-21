package highlight

import (
	"bytes"
	"html"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"
)

// newGoldmark builds the goldmark parser once at construction. The formatter
// emits class-based spans prefixed with "ch-" so the frontend can style them
// via CSS without coupling to a Chroma style baked into the HTML. Every
// fenced block is wrapped in a <div class="ch-wrap"> with a sibling
// <button class="ch-copy"> so the frontend's delegated click listener
// (frontend/src/lib/utils/codeCopy.ts) can copy the <pre>'s innerText
// without having to coordinate a base64 payload with Go.
func newGoldmark(style string) goldmark.Markdown {
	return goldmark.New(
		goldmark.WithExtensions(
			highlighting.NewHighlighting(
				highlighting.WithStyle(style),
				highlighting.WithFormatOptions(
					chromahtml.WithClasses(true),
					chromahtml.ClassPrefix("ch-"),
				),
				highlighting.WithWrapperRenderer(codeBlockWrapper),
			),
		),
		// safeAutoLinkRenderer runs at priority 1 so it registers *after*
		// goldmark's default html renderer (priority 1000) and wins for
		// ast.KindAutoLink in the kind->func map. Without this override,
		// `<javascript:alert(1)>` autolinks leak a live href= into {@html}.
		goldmark.WithRendererOptions(
			renderer.WithNodeRenderers(util.Prioritized(safeAutoLinkRenderer{}, 1)),
		),
	)
}

// codeBlockWrapper emits a container + copy button around each fenced code
// block so the frontend delegated click listener can find the sibling <pre>
// and copy its textContent. No base64 encoding: the handler reads innerText
// which strips the span markup and leaves just the visible code.
//
// When the Chroma lexer could not be resolved (unknown language) the
// highlighting extension still calls this wrapper but does NOT emit its
// own <pre><code> — goldmark-highlighting delegates that to the
// WrapperRenderer, so we emit it here. Highlighted fences already have
// Chroma's <pre class="ch-chroma"> sandwiched between our calls, so we
// only add the scaffolding the outer container needs.
func codeBlockWrapper(w util.BufWriter, ctx highlighting.CodeBlockContext, entering bool) {
	if entering {
		_, _ = w.WriteString(`<div class="ch-wrap"><button type="button" class="ch-copy" aria-label="Copy code">Copy</button>`)
		if !ctx.Highlighted() {
			_, _ = w.WriteString(`<pre><code>`)
		}
		return
	}
	if !ctx.Highlighted() {
		_, _ = w.WriteString(`</code></pre>`)
	}
	_, _ = w.WriteString(`</div>`)
}

// renderMarkdown runs goldmark against s, subject to the MaxBytes cap. On any
// failure path (oversized input, goldmark Convert error) the fallback is an
// HTML-escaped <pre><code> block that callers can still hand to {@html}.
func (r *Renderer) renderMarkdown(s string) string {
	if len(s) > r.maxBytes {
		return fallbackPreCode(s)
	}
	var buf bytes.Buffer
	if err := r.md.Convert([]byte(s), &buf); err != nil {
		return fallbackPreCode(s)
	}
	return buf.String()
}

// fallbackPreCode wraps arbitrary text as an HTML-escaped <pre><code>. Used
// both as the oversize-input fallback and as the generic error fallback so
// callers never have to branch on "did rendering succeed".
func fallbackPreCode(s string) string {
	return "<pre><code>" + html.EscapeString(s) + "</code></pre>"
}
