package highlight

import (
	"bytes"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Agents emit Mermaid diagrams inside ```mermaid fences. With the
// default goldmark pipeline those blocks fall through the unknown-
// language branch of goldmark-highlighting and render as a plain
// `<pre><code>graph TD\n A --> B\n</code></pre>` — the user sees raw
// Mermaid syntax instead of a diagram.
//
// The mermaid extension below swaps any ```mermaid fence with a
// `<pre class="mermaid">...source...</pre>` block. A lightweight
// lazy-loader on the frontend detects the class on screen and pulls
// in mermaid.js on demand, keeping the main bundle free of Mermaid
// for threads that don't contain a diagram.

// kindMermaidBlock is the AST node kind the transformer rewrites
// mermaid fences into. The renderer below emits the raw source inside
// `<pre class="mermaid">`.
var kindMermaidBlock = ast.NewNodeKind("MermaidBlock")

// mermaidBlock holds the raw Mermaid source copied out of the fenced
// code block's lines. We keep the source as bytes so the renderer can
// HTML-escape once at emission time.
type mermaidBlock struct {
	ast.BaseBlock
	source []byte
}

func (mermaidBlock) Kind() ast.NodeKind { return kindMermaidBlock }

func (b *mermaidBlock) Dump(source []byte, level int) {
	ast.DumpHelper(b, source, level, nil, nil)
}

// mermaidExtension installs the transformer + renderer.
type mermaidExtension struct{}

func (mermaidExtension) Extend(md goldmark.Markdown) {
	md.Parser().AddOptions(
		parser.WithASTTransformers(
			util.Prioritized(mermaidTransformer{}, 100),
		),
	)
	md.Renderer().AddOptions(
		renderer.WithNodeRenderers(
			util.Prioritized(mermaidRenderer{}, 10),
		),
	)
}

// mermaidTransformer walks the document after parsing and replaces
// every ```mermaid fenced code block with a mermaidBlock node. This
// runs before goldmark-highlighting sees the block so chroma never
// attempts to lex Mermaid source as code.
type mermaidTransformer struct{}

func (mermaidTransformer) Transform(doc *ast.Document, reader text.Reader, _ parser.Context) {
	source := reader.Source()
	type replacement struct {
		old ast.Node
		new ast.Node
	}
	var todo []replacement
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		cb, ok := n.(*ast.FencedCodeBlock)
		if !ok {
			return ast.WalkContinue, nil
		}
		if cb.Info == nil {
			return ast.WalkContinue, nil
		}
		info := cb.Info.Value(source)
		// The info string may carry trailing attributes (e.g.
		// ```mermaid title="x"); match only the language token.
		lang := info
		if sp := bytes.IndexAny(lang, " \t"); sp >= 0 {
			lang = lang[:sp]
		}
		if !bytes.EqualFold(lang, []byte("mermaid")) {
			return ast.WalkContinue, nil
		}
		// Copy the raw source out of the block's Lines segments. This
		// is the text between the opening and closing fences.
		var buf bytes.Buffer
		for i := 0; i < cb.Lines().Len(); i++ {
			seg := cb.Lines().At(i)
			buf.Write(seg.Value(source))
		}
		mb := &mermaidBlock{source: buf.Bytes()}
		todo = append(todo, replacement{old: n, new: mb})
		return ast.WalkSkipChildren, nil
	})
	for _, r := range todo {
		r.old.Parent().ReplaceChild(r.old.Parent(), r.old, r.new)
	}
}

// mermaidRenderer emits the stored source inside `<pre class="mermaid">`.
// The content is HTML-escaped — Mermaid.js on the frontend reads
// element.textContent when parsing, which returns the un-escaped form,
// so the diagram renders correctly while any HTML/script tags the
// source might contain cannot escape the <pre> as markup.
type mermaidRenderer struct{}

func (mermaidRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(kindMermaidBlock, renderMermaidBlock)
}

func renderMermaidBlock(w util.BufWriter, _ []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	n := node.(*mermaidBlock)
	_, _ = w.WriteString(`<pre class="mermaid">`)
	_, _ = w.Write(util.EscapeHTML(n.source))
	_, _ = w.WriteString(`</pre>`)
	return ast.WalkContinue, nil
}
