package jira

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
// never reach Jira, so initializing it does no work: no map, regular
// expression or non-constant string is built.

// initProbeEnv marks the run of this test binary TestPackageInit starts.
const initProbeEnv = "AO_JIRA_INIT_PROBE"

// initPin bounds this package's init in its test binary as
// GODEBUG=inittrace reports it: the package itself does no init work
// (measured with a binary that links it alone), and this is the test
// files' package variables (measured 14,704 bytes and 36 allocations),
// with a little room. The regular expressions it compiled at init cost
// 31 KB and about 300 allocations.
var initPin = struct{ bytes, allocs int }{15_500, 40}

// TestPackageInit runs this test binary again with inittrace and checks
// the package's init against initPin.
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
	cmd := exec.Command(exe, "-test.run=^$", "-test.count=1")
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

// No package-level variable outside the tests is initialized by work
// that allocates: a map literal, or a call other than errors.New.
func TestNoInitWork(t *testing.T) {
	t.Parallel()
	for _, p := range initWork(t, ".", map[string]bool{"errors.New": true}) {
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
	src := "package jira\n\nvar shape = regexp.MustCompile(`x`)\n\nvar names = map[string]int{}\n\nvar fine = errors.New(\"x\")\n"
	if err := os.WriteFile(filepath.Join(dir, "work.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	got := initWork(t, dir, map[string]bool{"errors.New": true})
	if len(got) != 2 || !strings.Contains(got[0], "regexp.MustCompile") || !strings.Contains(got[1], "map") {
		t.Errorf("init work found %q", got)
	}
}

// Every kind has its rule, at its index.
func TestEveryKindHasARule(t *testing.T) {
	t.Parallel()
	if len(rules) != int(kindTeam)+1 {
		t.Fatalf("%d rules for %d kinds", len(rules), kindTeam+1)
	}
	for k, r := range rules {
		if r.what == "" || (k != int(kindUnknown)) != (r.build != nil) {
			t.Errorf("kind %d: rule %+v", k, r)
		}
	}
}
