package adf

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A goldmark node kind is registered by a package variable's initializer,
// never from a function: goldmark's kind table is not locked, so a kind
// registered at first use races with every goroutine reading a kind's
// name.
func TestNodeKindsAreRegisteredAtInit(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			_, inFunc := decl.(*ast.FuncDecl)
			ast.Inspect(decl, func(n ast.Node) bool {
				if _, lit := n.(*ast.FuncLit); lit {
					for range countNodeKinds(n) {
						t.Errorf("%s: NewNodeKind is called from a function literal", fset.Position(n.Pos()))
					}
					return false
				}
				call, ok := n.(*ast.CallExpr)
				if !ok || !isNewNodeKind(call) {
					return true
				}
				found++
				if inFunc {
					t.Errorf("%s: NewNodeKind is called from a function", fset.Position(call.Pos()))
				}
				return true
			})
		}
	}
	if found == 0 {
		t.Fatal("found no NewNodeKind call")
	}
}

func isNewNodeKind(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "NewNodeKind"
}

// countNodeKinds is how many NewNodeKind calls n holds.
func countNodeKinds(n ast.Node) int {
	count := 0
	ast.Inspect(n, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && isNewNodeKind(call) {
			count++
		}
		return true
	})
	return count
}

// Every lazyRegexp pattern is a constant that compiles: one that does not
// would panic at its first use instead of at init.
func TestLazyRegexpsCompile(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "lazyRegexp" {
				return true
			}
			found++
			pos := fset.Position(lit.Pos())
			if len(lit.Elts) != 1 {
				t.Errorf("%s: a lazyRegexp sets only its pattern", pos)
				return false
			}
			kv, ok := lit.Elts[0].(*ast.KeyValueExpr)
			pattern, constant := "", ok
			if ok {
				pattern, constant = stringConstant(kv.Value)
			}
			if !constant {
				t.Errorf("%s: a lazyRegexp pattern is a string literal or a sum of them", pos)
				return false
			}
			if _, err := regexp.Compile(pattern); err != nil {
				t.Errorf("%s: %v", pos, err)
			}
			return false
		})
	}
	if found == 0 {
		t.Fatal("found no lazyRegexp")
	}
}

// stringConstant evaluates a string literal or a sum of them.
func stringConstant(e ast.Expr) (string, bool) {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		x, ok := stringConstant(e.X)
		if !ok {
			return "", false
		}
		y, ok := stringConstant(e.Y)
		return x + y, ok
	case *ast.ParenExpr:
		return stringConstant(e.X)
	}
	return "", false
}
