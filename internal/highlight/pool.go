package highlight

import (
	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

// Parsers are not thread-safe and not tied to a language until
// SetLanguage, so one pool serves every grammar. Pooled parsers live
// for the process (a handful of ~small C objects); they are never
// Closed.
var parserPool = struct {
	pool chan *tree_sitter.Parser
}{pool: make(chan *tree_sitter.Parser, 8)}

func acquireParser() *tree_sitter.Parser {
	select {
	case p := <-parserPool.pool:
		return p
	default:
		p := tree_sitter.NewParser()
		// Timeout via the parser-level deadline, NOT ParseWithOptions:
		// go-tree-sitter v0.25.0's ParseWithOptions leaks its options
		// payload on every call (pointer.Save with no matching Unref,
		// parser.go:350), permanently retaining each parse's callback.
		// SetTimeoutMicros is deprecated upstream but leak-free; the
		// setting persists on the parser across pooled reuses. Revisit
		// when upgrading past v0.25 (0.26 removes this API).
		p.SetTimeoutMicros(uint64(parseTimeout.Microseconds()))
		return p
	}
}

// releaseParser returns a parser whose last parse COMPLETED to the
// pool. Parsers whose parse was cancelled must be Closed instead:
// upstream's ts_parser_reset does not clear canceled_balancing, so a
// parse cancelled during tree balancing trips a C assertion (and
// aborts the process) on the parser's next use — Reset is not enough.
func releaseParser(p *tree_sitter.Parser) {
	// A previous use may have narrowed the parser to injection ranges;
	// clear that before the next borrower.
	if err := p.SetIncludedRanges(nil); err != nil {
		p.Close()
		return
	}
	select {
	case parserPool.pool <- p:
	default:
		p.Close()
	}
}

// parseWithDeadline parses src, halting at the parser's configured
// timeout (set once at construction in acquireParser). Returns nil on
// timeout — the caller must Close the parser (see releaseParser) and
// degrade to plain text.
func parseWithDeadline(p *tree_sitter.Parser, src []byte) *tree_sitter.Tree {
	return p.Parse(src, nil)
}
