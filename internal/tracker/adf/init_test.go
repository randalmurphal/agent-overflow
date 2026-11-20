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

// What the package's init may allocate: nothing. Its one piece of work,
// registering a goldmark node kind, appends to goldmark's table within
// the room that table has (measured: 0 bytes). A goldmark release whose
// table grows at this registration fails the test; raise the bytes to
// that growth then, and nothing else.
const (
	initBytes  = 0
	initAllocs = 0
)

var initTrace = regexp.MustCompile(`(\d+) bytes, (\d+) allocs`)

// The package's init registers its goldmark node kind and does nothing
// else: every table and expression waits for its first use. The probe in
// testdata links the package and nothing else of this module, so its init
// trace is the package's own, free of the test variables a test binary
// adds.
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
		if !strings.HasPrefix(line, "init agent-overflow/internal/tracker/adf ") {
			continue
		}
		m := initTrace.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("an init trace line of another shape: %s", strings.TrimSpace(line))
		}
		bytes, _ := strconv.Atoi(m[1])
		allocs, _ := strconv.Atoi(m[2])
		if bytes > initBytes || allocs > initAllocs {
			t.Errorf("the package's init allocates %d bytes in %d allocations, over %d and %d: %s",
				bytes, allocs, initBytes, initAllocs, strings.TrimSpace(line))
		}
	}
}

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
