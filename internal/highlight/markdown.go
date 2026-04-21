package highlight

import (
	"bytes"
	"html"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
)

// newGoldmark builds the goldmark parser once at construction. The formatter
// emits class-based spans prefixed with "ch-" so the frontend can style them
// via CSS without coupling to a Chroma style baked into the HTML.
func newGoldmark(style string) goldmark.Markdown {
	return goldmark.New(
		goldmark.WithExtensions(
			highlighting.NewHighlighting(
				highlighting.WithStyle(style),
				highlighting.WithFormatOptions(
					chromahtml.WithClasses(true),
					chromahtml.ClassPrefix("ch-"),
				),
			),
		),
	)
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
