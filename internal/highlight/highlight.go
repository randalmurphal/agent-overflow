package highlight

import "github.com/yuin/goldmark"

// defaultMaxBytes caps the size of input passed to the full parsers. Inputs
// above this length skip parsing and emit an HTML-escaped <pre><code> block.
// 512 KB fits the largest assistant_text / thinking / command_output buffers
// we see in practice with comfortable headroom; anything bigger is either a
// pathological paste or a runaway generation and should not monopolize the
// renderer.
const defaultMaxBytes = 512 * 1024

// defaultStyle is the Chroma style used when Options.Style is empty. The
// choice is cosmetic when WithClasses is enabled (CSS picks the colors); we
// still need a valid style name for the formatter to initialize.
const defaultStyle = "github-dark"

// Options configures a Renderer. Zero value (Options{}) is valid and produces
// a working renderer with the defaults above.
type Options struct {
	// MaxBytes is the hard cap on input length. Zero means use the default.
	MaxBytes int
	// Style is the Chroma style name (e.g. "github-dark"). Empty means use
	// the default.
	Style string
}

// Renderer converts raw markdown and terminal (ANSI) text to display HTML.
// It is safe for concurrent use after construction: the underlying goldmark
// parser and chroma formatter are configured once in New and never mutated.
type Renderer struct {
	md       goldmark.Markdown
	maxBytes int
}

// New constructs a Renderer with the given options. It never returns an error;
// a misconfigured style falls back to the default. The returned renderer is
// concurrent-safe.
func New(opts Options) *Renderer {
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	style := opts.Style
	if style == "" {
		style = defaultStyle
	}
	return &Renderer{
		md:       newGoldmark(style),
		maxBytes: maxBytes,
	}
}

// RenderMarkdown converts CommonMark markdown to HTML with Chroma-highlighted
// code fences. The result is always a valid HTML fragment; on any internal
// failure it falls back to an HTML-escaped <pre><code> block.
func (r *Renderer) RenderMarkdown(s string) string {
	return r.renderMarkdown(s)
}

// RenderANSI converts terminal output (with or without ANSI escape codes) to
// HTML with color-class spans. The result is always a valid HTML fragment; on
// any internal failure it falls back to an HTML-escaped <pre><code> block.
func (r *Renderer) RenderANSI(s string) string {
	return r.renderANSI(s)
}

// RenderForKind dispatches to the correct renderer for a timeline item or
// payload kind. Kinds that should not be server-rendered (diffs, plain user
// text, structured tool events) return the empty string so callers can skip
// the write without branching on the kind themselves.
func (r *Renderer) RenderForKind(kind, content string) string {
	return r.renderForKind(kind, content)
}
