package linear

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The package is linked into every process of the app, most of which
// never reach Linear, so initializing it does no work: no family
// document is made before its first use, no map, regular expression or
// non-constant string is built.

// initProbeEnv marks the run of this test binary TestPackageInit starts.
const initProbeEnv = "AO_LINEAR_INIT_PROBE"

// initPin bounds this package's init in its test binary as
// GODEBUG=inittrace reports it: the package itself allocates nothing
// (measured with a binary that links it alone), and this is the test
// files' package variables (measured 60,768 bytes and 134 allocations),
// with a little room. Making every family document at init costs about
// 800 KB and 1,900 allocations.
var initPin = struct{ bytes, allocs int }{62_000, 140}

// TestPackageInit runs this test binary again with inittrace and checks
// the package's init against initPin; that run's TestInitProbe checks no
// family document was made.
func TestPackageInit(t *testing.T) {
	if os.Getenv(initProbeEnv) != "" {
		t.Skip("the probe run")
	}
	if raceOn || testing.CoverMode() != "" {
		t.Skip("the race detector and coverage change what init allocates")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestInitProbe$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), "GODEBUG=inittrace=1", initProbeEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the probe run failed: %v\n%s", err, out)
	}
	pkg := reflect.TypeFor[Client]().PkgPath()
	m := regexp.MustCompile(`(?m)^init ` + regexp.QuoteMeta(pkg) + ` @.*, ([0-9]+) bytes, ([0-9]+) allocs$`).FindSubmatch(out)
	if m == nil {
		// No line: the package's init does nothing at all.
		return
	}
	bytes, _ := strconv.Atoi(string(m[1]))
	allocs, _ := strconv.Atoi(string(m[2]))
	t.Logf("init of %s, test files included: %d bytes, %d allocs", pkg, bytes, allocs)
	if bytes > initPin.bytes || allocs > initPin.allocs {
		t.Errorf("init of %s allocates %d bytes in %d allocations, pinned at most %d and %d: make the new package-level work lazy",
			pkg, bytes, allocs, initPin.bytes, initPin.allocs)
	}
}

// TestInitProbe, run by TestPackageInit in a fresh process, finds every
// family document still to be made.
func TestInitProbe(t *testing.T) {
	if os.Getenv(initProbeEnv) == "" {
		t.Skip("run by TestPackageInit")
	}
	for _, f := range registry.families[:registry.nFam] {
		for i := range f.docs {
			if f.docs[i].op.text != "" {
				t.Errorf("%s for %d issues was made before its first use", f.name, i+1)
			}
		}
	}
}

// A family document is made on its first use, once: later uses return
// it with no allocation, and the single-issue read's history page is
// the constant written into its document.
func TestFamilyDocumentMadeOnce(t *testing.T) {
	first := renumberFamily.op(3)
	if again := renumberFamily.op(3); again.text != first.text || len(first.results) != 3 {
		t.Fatalf("op(3) %q then %q", first.results, again.results)
	}
	if !raceOn {
		if n := testing.AllocsPerRun(100, func() { _ = renumberFamily.op(3) }); n != 0 {
			t.Errorf("a made document costs %v allocations a use", n)
		}
	}
	if want := "history(first: " + strconv.Itoa(detailHistoryPage) + ")"; !strings.Contains(opIssue.text, want) {
		t.Errorf("the Issue document does not hold %s", want)
	}
}

// BenchmarkFamilyOp is the cost of a family document once made, against
// the slice index it replaced.
func BenchmarkFamilyOp(b *testing.B) {
	b.Run("once", func(b *testing.B) {
		_ = renumberFamily.op(maxBatch)
		for b.Loop() {
			_ = renumberFamily.op(maxBatch)
		}
	})
	b.Run("slice index", func(b *testing.B) {
		docs := make([]operation, maxBatch)
		docs[maxBatch-1] = renumberFamily.op(maxBatch)
		op := func(n int) operation {
			if n < 1 || n > len(docs) {
				panic("outside")
			}
			return docs[n-1]
		}
		for b.Loop() {
			_ = op(maxBatch)
		}
	})
}

// No package-level variable outside the tests is initialized by work
// that allocates: a map literal, or a call other than the operation
// constructors, errors.New and the decoder's options.
func TestNoInitWork(t *testing.T) {
	t.Parallel()
	allowed := map[string]bool{"newQuery": true, "newMutation": true, "newMutationFamily": true, "errors.New": true,
		"jsontext.AllowInvalidUTF8": true, "withMaxBody": true}
	for _, p := range initWork(t, ".", allowed) {
		t.Error(p)
	}
}

// initWork names each map literal and each call outside allowed in the
// initializers of the package-level variables of dir's non-test files.
// A call is named by its function, or package.function, or method.
func initWork(t *testing.T, dir string, allowed map[string]bool) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var problems []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			g, ok := decl.(*ast.GenDecl)
			if !ok || g.Tok != token.VAR {
				continue
			}
			for _, spec := range g.Specs {
				for _, v := range spec.(*ast.ValueSpec).Values {
					ast.Inspect(v, func(n ast.Node) bool {
						switch n := n.(type) {
						case *ast.FuncLit:
							return false // runs only when called, not at init
						case *ast.CompositeLit:
							if _, ok := n.Type.(*ast.MapType); ok {
								problems = append(problems, fset.Position(n.Pos()).String()+": a map built at init")
							}
						case *ast.CallExpr:
							if name := callName(n.Fun); name != "" && !allowed[name] {
								problems = append(problems, fset.Position(n.Pos()).String()+": "+name+" called at init")
							}
						}
						return true
					})
				}
			}
		}
	}
	return problems
}

// callName is the name a call is allowed by: f, pkg.f, or the method's
// name; empty for a conversion to a composite type.
func callName(fun ast.Expr) string {
	switch fun := fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		if pkg, ok := fun.X.(*ast.Ident); ok {
			return pkg.Name + "." + fun.Sel.Name
		}
		return fun.Sel.Name
	case *ast.IndexExpr:
		return callName(fun.X)
	}
	return ""
}

// The source check finds init work.
func TestInitWorkFound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := "package linear\n\nvar shape = regexp.MustCompile(`x`)\n\nvar names = map[string]int{}\n\nvar fine = errors.New(\"x\")\n"
	if err := os.WriteFile(filepath.Join(dir, "work.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	got := initWork(t, dir, map[string]bool{"errors.New": true})
	if len(got) != 2 || !strings.Contains(got[0], "regexp.MustCompile") || !strings.Contains(got[1], "map") {
		t.Errorf("init work found %q", got)
	}
}
