package highlight

import (
	"bytes"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/renderer"
	chtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/util"
)

// safeLinkRenderer replaces goldmark's default AutoLink and Image
// renderers with ones that enforce a stricter dangerous-URL policy.
//
// - AutoLink: upstream's renderAutoLink — unlike renderLink — does NOT
//   consult IsDangerousURL, so `<javascript:alert(1)>`, `<vbscript:…>`,
//   `<data:text/html,…>`, and `<file:…>` survive as clickable live HTML.
//   safeLinkRenderer runs the same IsDangerousURL check and drops the
//   URL when dangerous.
// - Image: upstream's renderImage uses IsDangerousURL, which WHITELISTS
//   `data:image/svg+xml;…`. An attacker-authored SVG data URI loaded
//   via `<img>` cannot run scripts in modern Chromium/WebKit, but the
//   webview policy is outside our control — defense-in-depth is cheap,
//   so we additionally block `data:image/svg+xml` (PNG/GIF/JPEG/WebP
//   data URIs still render).
//
// Priority 1 is lower than goldmark's default html renderer (priority
// 1000), and goldmark registers lower-priority renderers last — last
// write wins in the func-per-kind map, so this replaces the default
// only for AutoLink and Image.
type safeLinkRenderer struct{}

func (safeLinkRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindAutoLink, renderSafeAutoLink)
	reg.Register(ast.KindImage, renderSafeImage)
}

func renderSafeAutoLink(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	n := node.(*ast.AutoLink)
	if !entering {
		return ast.WalkContinue, nil
	}
	url := n.URL(source)
	label := n.Label(source)
	dangerous := chtml.IsDangerousURL(url)
	_, _ = w.WriteString(`<a href="`)
	if !dangerous {
		if n.AutoLinkType == ast.AutoLinkEmail && !bytes.HasPrefix(bytes.ToLower(url), []byte("mailto:")) {
			_, _ = w.WriteString("mailto:")
		}
		_, _ = w.Write(util.EscapeHTML(util.URLEscape(url, false)))
	}
	_, _ = w.WriteString(`">`)
	_, _ = w.Write(util.EscapeHTML(label))
	_, _ = w.WriteString(`</a>`)
	return ast.WalkContinue, nil
}

// bSVGImage is the prefix goldmark's IsDangerousURL explicitly allows.
// We reject it here because SVG can carry script payloads that some
// webviews execute when the image is loaded; the other raster formats
// (png/gif/jpeg/webp) have no equivalent risk.
var bSVGImage = []byte("data:image/svg")

func renderSafeImage(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	n := node.(*ast.Image)
	dst := n.Destination
	dangerous := chtml.IsDangerousURL(dst) ||
		bytes.HasPrefix(bytes.ToLower(dst), bSVGImage)
	_, _ = w.WriteString(`<img src="`)
	if !dangerous {
		_, _ = w.Write(util.EscapeHTML(util.URLEscape(dst, true)))
	}
	_, _ = w.WriteString(`" alt="`)
	// Mirror goldmark's renderTexts behaviour: walk children and
	// emit their text content as the alt. For our use case the
	// simple EscapeHTML(n.Text) is sufficient because goldmark does
	// not set Attributes on image text children we would care to
	// format — images in agent markdown are plain alt-text.
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if txt, ok := c.(*ast.Text); ok {
			_, _ = w.Write(util.EscapeHTML(txt.Segment.Value(source)))
		}
	}
	_ = w.WriteByte('"')
	if n.Title != nil {
		_, _ = w.WriteString(` title="`)
		_, _ = w.Write(util.EscapeHTML(n.Title))
		_ = w.WriteByte('"')
	}
	_, _ = w.WriteString(`>`)
	return ast.WalkSkipChildren, nil
}
