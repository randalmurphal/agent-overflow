package highlight

import (
	"bytes"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/renderer"
	chtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/util"
)

// safeAutoLinkRenderer replaces goldmark's default AutoLink renderer with
// one that applies the same IsDangerousURL check renderLink already uses.
// Upstream's renderAutoLink writes n.URL(source) into href unconditionally,
// so `<javascript:alert(1)>`, `<vbscript:...>`, `<data:text/html,...>`, and
// `<file:...>` survive as clickable live HTML. We paint via {@html}, so that
// is a cross-site scripting vector.
//
// Priority 1 is lower than goldmark's default html renderer (priority 1000),
// and goldmark registers lower-priority renderers last — last write wins in
// the func-per-kind map, so this replaces the default only for AutoLink.
type safeAutoLinkRenderer struct{}

func (safeAutoLinkRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindAutoLink, renderSafeAutoLink)
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
