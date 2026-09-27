package highlight

import (
	"errors"
	"testing"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"

	"agent-overflow/internal/highlight/grammars"
)

// A malformed injections query fails with the tree_sitter.QueryError value,
// the shape the highlights query's failure has.
func TestCompileInjectionsReturnsQueryErrorValue(t *testing.T) {
	g, ok := grammars.Get("go")
	if !ok {
		t.Fatal("go grammar is not registered")
	}
	_, err := compileInjections(g.Language, "((")
	var queryErr tree_sitter.QueryError
	if !errors.As(err, &queryErr) {
		t.Fatalf("error = %#v, want a tree_sitter.QueryError value", err)
	}
}
