package adf

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The package runs no code at init: every table and expression waits for
// its first use. The probe in testdata links the package and nothing else
// of this module, so its init trace is the package's own, free of the
// test variables a test binary adds.
func TestInitCost(t *testing.T) {
	t.Parallel()
	bin := filepath.Join(t.TempDir(), "initcost")
	build := exec.CommandContext(t.Context(), "go", "build", "-trimpath", "-o", bin, "./testdata/initcost")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build the probe: %v\n%s", err, out)
	}
	run := exec.CommandContext(t.Context(), bin)
	run.Env = append(os.Environ(), "GODEBUG=inittrace=1")
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("run the probe: %v\n%s", err, out)
	}
	for line := range strings.Lines(string(out)) {
		if strings.HasPrefix(line, "init agent-overflow/internal/tracker/adf ") {
			t.Errorf("the package runs code at init: %s", strings.TrimSpace(line))
		}
	}
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
