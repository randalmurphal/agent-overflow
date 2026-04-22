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

// Agents emit LaTeX math inside `$...$` (inline) and `$$...$$` (block)
// delimiters whenever they talk about anything technical — ML, stats,
// algorithm complexity, or just a `$O(n \log n)$` drop into prose.
// Without a math extension goldmark treats the dollar signs as literal
// text and the user sees `$\sqrt{2}$` rather than a rendered root.
//
// The mathExtension below carves out two custom AST node kinds:
//
//   - inlineMath — triggered by a `$` not followed by whitespace and
//     terminated by a matching `$` not preceded by whitespace.
//   - blockMath — triggered by `$$` at the start of a line, consumed
//     until a line that contains only `$$`.
//
// The renderer emits minimal markers the frontend's KaTeX lazy-loader
// picks up: `<span class="math-inline">LATEX</span>` for inline and
// `<div class="math-display">LATEX</div>` for block. KaTeX reads the
// element's textContent, so the HTML escaping in util.EscapeHTML keeps
// any HTML that LaTeX might contain from breaking out of the node
// while still round-tripping through textContent for rendering.

var (
	kindInlineMath = ast.NewNodeKind("InlineMath")
	kindBlockMath  = ast.NewNodeKind("BlockMath")
)

type inlineMathNode struct {
	ast.BaseInline
	source []byte
}

func (inlineMathNode) Kind() ast.NodeKind             { return kindInlineMath }
func (m *inlineMathNode) Dump(src []byte, lvl int)    { ast.DumpHelper(m, src, lvl, nil, nil) }
func (m *inlineMathNode) IsRaw() bool                 { return true }

type blockMathNode struct {
	ast.BaseBlock
	source []byte
	// tight is true when the opening line was the single-line form
	// `$$...$$` — Open captured the full source and Continue should
	// close on its first visit. Stored on the node so the parser stays
	// concurrent-safe (no shared global state).
	tight bool
	// closed is true once Open/Continue have seen the explicit closing
	// `$$` delimiter. Streaming callers render a cumulative prefix that
	// may not contain the closer yet — without this flag we'd emit a
	// `<div class="math-display">` for unterminated blocks and the
	// frontend KaTeX renderer would parse-fail on every 50 ms tick.
	// Close fires on EOF regardless, so an unterminated node reaches
	// the renderer with closed=false.
	closed bool
}

func (blockMathNode) Kind() ast.NodeKind          { return kindBlockMath }
func (m *blockMathNode) Dump(src []byte, lvl int) { ast.DumpHelper(m, src, lvl, nil, nil) }

// --- Inline parser ---

type inlineMathParser struct{}

func (inlineMathParser) Trigger() []byte { return []byte{'$'} }

func (p inlineMathParser) Parse(_ ast.Node, block text.Reader, _ parser.Context) ast.Node {
	line, _ := block.PeekLine()
	if len(line) < 3 || line[0] != '$' {
		return nil
	}
	// `$$` opens block math; let the block parser handle it.
	if line[1] == '$' {
		return nil
	}
	// `$` followed by whitespace is a literal dollar sign (the classic
	// "it costs $5" case). Skip the match, let the rune be text.
	if isSpace(line[1]) {
		return nil
	}
	// Scan for closing `$` whose previous byte is NOT whitespace and
	// whose next byte is NOT a digit (again the "$5" case).
	for i := 2; i < len(line); i++ {
		if line[i] != '$' {
			continue
		}
		if isSpace(line[i-1]) {
			continue
		}
		// Reject `$5$` / `$10$` etc. as prose — a pure-digit payload
		// between two dollar signs is essentially always a currency
		// range, not math. Requiring at least one non-digit character
		// keeps `$x$`, `$O(n)$`, `$\pi$` as math without eating
		// "$5$ is prose" in the middle of a sentence.
		content := line[1:i]
		if onlyDigits(content) {
			return nil
		}
		out := make([]byte, len(content))
		copy(out, content)
		block.Advance(i + 1)
		return &inlineMathNode{source: out}
	}
	return nil
}

// --- Block parser ---

type blockMathParser struct{}

func (blockMathParser) Trigger() []byte { return []byte{'$'} }

func (blockMathParser) Open(_ ast.Node, reader text.Reader, _ parser.Context) (ast.Node, parser.State) {
	line, _ := reader.PeekLine()
	if len(line) < 2 || line[0] != '$' || line[1] != '$' {
		return nil, parser.NoChildren
	}
	// Accept either `$$` alone on its own line (multi-line block) or
	// `$$...$$` on a single line (tight block).
	rest := line[2:]
	rest = bytes.TrimRight(rest, "\r\n")
	if closeIdx := bytes.Index(rest, []byte("$$")); closeIdx >= 0 {
		// Tight form: opens and closes on the same line.
		inner := make([]byte, closeIdx)
		copy(inner, rest[:closeIdx])
		node := &blockMathNode{source: inner, tight: true, closed: true}
		return node, parser.NoChildren
	}
	// Multi-line open: bare `$$` on its own line, optional trailing
	// whitespace only.
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, parser.NoChildren
	}
	return &blockMathNode{}, parser.NoChildren
}

func (blockMathParser) Continue(node ast.Node, reader text.Reader, _ parser.Context) parser.State {
	n := node.(*blockMathNode)
	if n.tight {
		// Tight-form block was fully parsed in Open; close now.
		return parser.Close
	}
	line, _ := reader.PeekLine()
	if bytes.HasPrefix(bytes.TrimLeft(line, " \t"), []byte("$$")) {
		// Advance past the closing `$$` line. The length of the line
		// (including its trailing newline, which PeekLine returns as
		// part of the slice) is the exact byte count to consume so
		// the framework's next iteration starts on the line after
		// the close marker and doesn't re-open another math block.
		n.closed = true
		reader.Advance(len(line))
		return parser.Close
	}
	n.source = append(n.source, line...)
	return parser.Continue | parser.NoChildren
}

func (blockMathParser) Close(_ ast.Node, _ text.Reader, _ parser.Context) {}
func (blockMathParser) CanInterruptParagraph() bool                       { return true }
func (blockMathParser) CanAcceptIndentedLine() bool                       { return false }

// --- Renderer ---

type mathRenderer struct{}

func (mathRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(kindInlineMath, renderInlineMath)
	reg.Register(kindBlockMath, renderBlockMath)
}

func renderInlineMath(w util.BufWriter, _ []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	n := node.(*inlineMathNode)
	_, _ = w.WriteString(`<span class="math-inline">`)
	_, _ = w.Write(util.EscapeHTML(n.source))
	_, _ = w.WriteString(`</span>`)
	return ast.WalkSkipChildren, nil
}

func renderBlockMath(w util.BufWriter, _ []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	n := node.(*blockMathNode)
	src := bytes.TrimRight(n.source, "\r\n")
	if !n.closed {
		// Unterminated block — the user is still streaming the body
		// between the opening `$$` and a closer that hasn't arrived.
		// Emit the raw source (with the opener reinstated so the user
		// sees that it's a math block in progress) as a plain code
		// block. Mirrors how the mermaid transformer leaves unclosed
		// fences as FencedCodeBlocks: the frontend sees nothing that
		// triggers KaTeX, and the flip to rendered math is a single
		// atomic paint once the closer arrives.
		_, _ = w.WriteString(`<pre><code>$$`)
		if len(src) > 0 {
			_, _ = w.WriteString("\n")
			_, _ = w.Write(util.EscapeHTML(src))
		}
		_, _ = w.WriteString(`</code></pre>`)
		return ast.WalkContinue, nil
	}
	_, _ = w.WriteString(`<div class="math-display">`)
	_, _ = w.Write(util.EscapeHTML(src))
	_, _ = w.WriteString(`</div>`)
	return ast.WalkContinue, nil
}

// --- Extension plumbing ---

type mathExtension struct{}

func (mathExtension) Extend(md goldmark.Markdown) {
	md.Parser().AddOptions(
		parser.WithInlineParsers(util.Prioritized(inlineMathParser{}, 100)),
		parser.WithBlockParsers(util.Prioritized(blockMathParser{}, 100)),
	)
	md.Renderer().AddOptions(
		renderer.WithNodeRenderers(util.Prioritized(mathRenderer{}, 10)),
	)
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// onlyDigits reports whether content is non-empty and every byte is an
// ASCII digit (0-9). Used to reject prose-style `$5$` patterns that
// are almost never real math.
func onlyDigits(content []byte) bool {
	if len(content) == 0 {
		return false
	}
	for _, b := range content {
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}
