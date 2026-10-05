package highlight

import (
	"testing"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

// A pooled parser takes the parse deadline current when it is acquired, so
// a test that lifts parseTimeout leaves no pooled parser without one.
func TestAcquiredParserCarriesCurrentTimeout(t *testing.T) {
	prevPool, prevTimeout := parserPool.pool, parseTimeout
	parserPool.pool = make(chan *tree_sitter.Parser, 1)
	defer func() { parserPool.pool, parseTimeout = prevPool, prevTimeout }()

	parseTimeout = 0
	lifted := acquireParser()
	if got := lifted.TimeoutMicros(); got != 0 {
		t.Fatalf("lifted deadline: parser timeout %d µs, want 0", got)
	}
	releaseParser(lifted)

	parseTimeout = prevTimeout
	p := acquireParser()
	defer p.Close()
	if p != lifted {
		t.Fatal("acquire did not reuse the pooled parser")
	}
	if got, want := p.TimeoutMicros(), uint64(prevTimeout.Microseconds()); got != want {
		t.Fatalf("pooled parser timeout %d µs, want %d", got, want)
	}
}
