package deviceclient

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// Every file this package deletes, a session file above all, is deleted
// through atomicfile.Remove, which syncs the directory after the unlink: a
// bare os.Remove is only in the page cache, and a power loss can bring a
// forgotten pairing back.
func TestEveryDeleteIsDurable(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var found []string
	parsed := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed++
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "os" && (sel.Sel.Name == "Remove" || sel.Sel.Name == "RemoveAll") {
				found = append(found, fset.Position(sel.Pos()).String()+": delete through atomicfile.Remove")
			}
			return true
		})
	}
	if parsed == 0 {
		t.Fatal("no sources found; the check would pass with nothing checked")
	}
	if len(found) > 0 {
		t.Fatalf("a delete is not made durable:\n%s", strings.Join(found, "\n"))
	}
}
