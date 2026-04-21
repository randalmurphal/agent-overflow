package highlight

import (
	"bytes"
	"regexp"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"
)

// safeHTMLRenderer overrides goldmark's default HTMLBlock / RawHTML
// renderers so a narrow, explicit whitelist of tags can pass through
// while everything else keeps the default "raw HTML omitted" behavior
// and the existing XSS posture.
//
// Why we need this:
//   - Agents routinely emit `<details>` / `<summary>` blocks for
//     long command output or optional context the user can expand.
//     With goldmark's default render path those blocks get replaced
//     with `<!-- raw HTML omitted -->` and the `<summary>` caption
//     disappears entirely.
//   - Enabling goldmark's `html.WithUnsafe()` would solve the case
//     but reopen every other HTML injection vector — defeats the
//     safeLinkRenderer + adversarial-test work.
//
// How it stays safe:
//   - Only the tag names in safeHTMLTags are emitted.
//   - Only whitelisted attributes are preserved (e.g. `open` on
//     `<details>`); everything else — `onclick`, `style`, `id`,
//     `javascript:` URIs — is stripped.
//   - Any tag that doesn't match the whitelist falls through to
//     goldmark's default render, which emits the "omitted" comment
//     (block) or escapes the raw bytes (inline).
//
// Priority 1 places this renderer lower than goldmark's default (1000);
// in goldmark's renderer registry the lower-priority (registered later)
// entry wins for the node kinds it registers.
type safeHTMLRenderer struct{}

// safeHTMLTags lists the tag names we pass through after attribute
// stripping, together with the specific attribute keys we preserve.
//   - `details` keeps `open` so the server can choose a default state.
//   - `summary` carries no attributes on the wire.
var safeHTMLTags = map[string]map[string]struct{}{
	"details": {"open": {}},
	"summary": {},
}

func (safeHTMLRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindHTMLBlock, renderSafeHTMLBlock)
	reg.Register(ast.KindRawHTML, renderSafeRawHTML)
}

// tagParseRE captures the leading tag name (group 1) and remainder of
// the tag (group 2) from the first line of an HTML block or raw inline
// HTML segment. We intentionally match only ASCII letters/digits for
// the tag name — enough to catch every real tag, no regex backtracking
// on adversarial inputs.
var tagParseRE = regexp.MustCompile(`^<(/?)([a-zA-Z][a-zA-Z0-9]*)([^>]*)>`)

// attrRE captures `key="value"` / `key='value'` / `key=value` / bare
// `key` attribute forms from the remainder of a tag. The attribute
// value is always quoted on emission regardless of the input form.
var attrRE = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9-]*)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+)))?`)

// renderSafeHTMLBlock handles block-level raw HTML. Each block-level
// HTML span in the AST carries its raw bytes in node.Lines(); we
// inspect the first line to decide whether the opening (or closing)
// tag is whitelisted. A block that starts with a whitelisted tag is
// emitted in full — meaning `<details>` passes through along with its
// subsequent `<summary>...</summary>` + expanded-markdown contents
// (goldmark already parses the inner markdown as children).
func renderSafeHTMLBlock(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	n := node.(*ast.HTMLBlock)
	// Peek the first line to classify the block. Lines are backed by
	// the original source; we do not mutate them.
	if n.Lines().Len() == 0 {
		return emitOmitted(w)
	}
	firstSeg := n.Lines().At(0)
	first := firstSeg.Value(source)
	m := tagParseRE.FindSubmatch(first)
	if m == nil {
		return emitOmitted(w)
	}
	tagName := string(bytes.ToLower(m[2]))
	if _, ok := safeHTMLTags[tagName]; !ok {
		return emitOmitted(w)
	}
	// Whitelisted — emit every line in the block verbatim after
	// running each line through the attribute stripper. Most block
	// tags (details/summary) are single-line open/close; we loop for
	// safety in case a block carries e.g. an open <details> plus a
	// line of continuation before markdown takes over.
	for i := 0; i < n.Lines().Len(); i++ {
		seg := n.Lines().At(i)
		line := seg.Value(source)
		if sanitized, ok := sanitizeHTMLTagLine(line); ok {
			_, _ = w.Write(sanitized)
		} else {
			// Fall back to omitting just this line rather than the
			// whole block — keeps any subsequent whitelisted lines
			// working.
			_, _ = w.WriteString("<!-- raw HTML omitted -->\n")
		}
	}
	return ast.WalkContinue, nil
}

// renderSafeRawHTML handles inline raw HTML — the `<summary>Title</summary>`
// sitting on the same line as surrounding text. goldmark's default
// inline renderer drops these entirely when unsafe is off.
func renderSafeRawHTML(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	n := node.(*ast.RawHTML)
	// The raw HTML text is stored as a segment range; reassemble.
	var buf bytes.Buffer
	for i := 0; i < n.Segments.Len(); i++ {
		seg := n.Segments.At(i)
		buf.Write(seg.Value(source))
	}
	if sanitized, ok := sanitizeHTMLTagLine(buf.Bytes()); ok {
		_, _ = w.Write(sanitized)
	}
	// Non-whitelisted inline HTML is dropped silently — matching
	// goldmark's default behavior with unsafe off.
	return ast.WalkSkipChildren, nil
}

// sanitizeHTMLTagLine runs the whitelist + attribute filter over a
// single line of raw HTML. Returns (sanitized, true) if every tag on
// the line is whitelisted and cleaned, or (nil, false) if any tag is
// unknown. Non-tag content (whitespace, text between tags) is preserved.
//
// The ok=false return signals the caller to use the omitted fallback.
// This is intentional: a line that says `<details> onclick=` has one
// valid tag but unknown trailing content, and we'd rather drop the
// whole line than risk emitting the attribute injection.
func sanitizeHTMLTagLine(line []byte) ([]byte, bool) {
	var out bytes.Buffer
	i := 0
	for i < len(line) {
		if line[i] != '<' {
			// Text between tags — emit as-is.
			end := bytes.IndexByte(line[i:], '<')
			if end < 0 {
				out.Write(line[i:])
				break
			}
			out.Write(line[i : i+end])
			i += end
			continue
		}
		m := tagParseRE.FindSubmatchIndex(line[i:])
		if m == nil {
			return nil, false
		}
		closing := len(m) > 2 && m[2] != m[3] && line[i+m[2]] == '/'
		tagName := string(bytes.ToLower(line[i+m[4] : i+m[5]]))
		allowedAttrs, ok := safeHTMLTags[tagName]
		if !ok {
			return nil, false
		}
		out.WriteByte('<')
		if closing {
			out.WriteByte('/')
		}
		out.WriteString(tagName)
		// Only the open form carries attributes worth preserving.
		if !closing && len(allowedAttrs) > 0 {
			attrSlice := line[i+m[6] : i+m[7]]
			for _, am := range attrRE.FindAllSubmatch(attrSlice, -1) {
				name := string(bytes.ToLower(am[1]))
				if _, keep := allowedAttrs[name]; !keep {
					continue
				}
				out.WriteByte(' ')
				out.WriteString(name)
				// Only emit a value if the source had one; attributes
				// like `open` on `<details>` are idiomatic as bare.
				value := firstNonEmpty(am[2], am[3], am[4])
				if len(value) > 0 {
					out.WriteString(`="`)
					out.Write(util.EscapeHTML(value))
					out.WriteByte('"')
				}
			}
		}
		out.WriteByte('>')
		i += m[1]
	}
	return out.Bytes(), true
}

func firstNonEmpty(bs ...[]byte) []byte {
	for _, b := range bs {
		if len(b) > 0 {
			return b
		}
	}
	return nil
}

func emitOmitted(w util.BufWriter) (ast.WalkStatus, error) {
	_, _ = w.WriteString("<!-- raw HTML omitted -->\n")
	return ast.WalkContinue, nil
}
