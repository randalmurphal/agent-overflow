package agy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/testutil/reporoot"
)

// Home.Launch is the one place that sets or clears the variables through
// which agy chooses its login store and home, and the account manager is the
// one maker of the canonical home. The types say that no agy launch is built
// without a Home; this scan says what they cannot: that no production code
// elsewhere names the variables or makes a canonical home itself.
func TestOnlyHomeLaunchSetsTheStoreAndOnlyTheManagerMakesTheCanonicalHome(t *testing.T) {
	reporoot.ObserveGoSources(t)
	root := reporoot.Dir(t)
	variables := map[string]bool{forceFileStorageEnv: true, geminiHomeEnv: true, appDataDirEnv: true}
	// Files that name a variable without launching agy: the reserved-name
	// lists that refuse them in a custom environment, and the test isolation
	// that clears them from a test process.
	namesWithoutLaunching := map[string]bool{
		"internal/provider/pinnedenv.go":   true,
		"internal/settings/providerenv.go": true,
		"internal/kerneltest/isolate.go":   true,
	}
	const canonicalMaker = "internal/provideraccountapp/"
	const agyDir = "internal/provider/agy/"

	var violations []string
	scan := func(rel string) {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		agyName := ""
		for _, spec := range file.Imports {
			if path, _ := strconv.Unquote(spec.Path.Value); path == "agent-overflow/internal/provider/agy" {
				agyName = "agy"
				if spec.Name != nil {
					agyName = spec.Name.Name
				}
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.BasicLit:
				if n.Kind != token.STRING || strings.HasPrefix(rel, agyDir) || namesWithoutLaunching[rel] {
					return true
				}
				if value, err := strconv.Unquote(n.Value); err == nil && variables[value] {
					violations = append(violations, fset.Position(n.Pos()).String()+": names "+value)
				}
			case *ast.SelectorExpr:
				if agyName == "" || n.Sel.Name != "CanonicalHome" || strings.HasPrefix(rel, canonicalMaker) {
					return true
				}
				if pkg, ok := n.X.(*ast.Ident); ok && pkg.Name == agyName {
					violations = append(violations, fset.Position(n.Pos()).String()+": makes agy.CanonicalHome")
				}
			}
			return true
		})
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if isProductionGo(entry.Name()) && !entry.IsDir() {
			scan(entry.Name())
		}
	}
	for _, sourceRoot := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, sourceRoot), func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !isProductionGo(entry.Name()) {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			scan(filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(violations) > 0 {
		t.Fatalf("agy's store and home are chosen outside their owners:\n%s", strings.Join(violations, "\n"))
	}
}

func isProductionGo(name string) bool {
	return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
}
