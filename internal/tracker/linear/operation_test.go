package linear

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An operation can only come from newQuery, newMutation or a family's
// member, whose constructors register
// it for the schema test and the exercise check: the package's source
// holds no other operation literal.
func TestOperationsOnlyFromConstructors(t *testing.T) {
	t.Parallel()
	problems, err := strayOperations(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Error(p)
	}
}

// strayOperations names every operation composite literal in dir's
// non-test Go files outside the constructors' own file.
func strayOperations(dir string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var problems []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		f, err := parser.ParseFile(fset, file, src, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && filepath.Base(file) == "operation.go" &&
				(fn.Name.Name == "newQuery" || fn.Name.Name == "newMutation" || fn.Name.Name == "member") {
				continue
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if ok {
					if id, ok := lit.Type.(*ast.Ident); ok && id.Name == "operation" {
						problems = append(problems, fset.Position(lit.Pos()).String()+": an operation literal; use newQuery or newMutation")
					}
				}
				return true
			})
		}
	}
	return problems, nil
}

// The source check finds the literal a reviewer once slipped in.
func TestStrayOperationsFound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stray := "package linear\n\nvar opStray = operation{name: \"Stray\", text: `query Stray { nope }`}\n"
	if err := os.WriteFile(filepath.Join(dir, "stray.go"), []byte(stray), 0o600); err != nil {
		t.Fatal(err)
	}
	problems, err := strayOperations(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "stray.go:3") {
		t.Errorf("problems %q", problems)
	}
}
