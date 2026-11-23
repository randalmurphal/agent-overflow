package mockexec

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/testutil/reporoot"
)

// TestEveryTestExecutableIsWrittenWithForksHeldOff is a tripwire, because
// nothing in the type system stops test code from calling os.WriteFile with
// an executable mode. Test code writes an executable with Write, WriteFile or
// CopyFile, or in a function that calls procutil.HoldForks itself: a child
// that a parallel test forks while the file is open for writing keeps it
// busy, and running it fails with "text file busy".
//
// It scans test files and the non-test files of test-support packages (under
// internal/testutil, or in a directory named *test or *fake). It reads
// os.WriteFile, os.OpenFile with os.O_CREATE, File.Chmod, and os.Chmod of a
// path the same function wrote. A mode counts as executable when it has an
// execute bit or is not a constant the scan can evaluate.
func TestEveryTestExecutableIsWrittenWithForksHeldOff(t *testing.T) {
	root := reporoot.Dir(t)
	var found []string
	constsByDir := map[string]map[string]ast.Expr{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch name := d.Name(); {
			case path != root && strings.HasPrefix(name, "."),
				name == "node_modules", name == "testdata", name == "frontend", name == "bin":
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !strings.HasSuffix(rel, ".go") || !inTestScope(rel) {
			return nil
		}
		consts, ok := constsByDir[filepath.Dir(path)]
		if !ok {
			if consts, err = packageConstants(filepath.Dir(path)); err != nil {
				return err
			}
			constsByDir[filepath.Dir(path)] = consts
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, line := range unheldExecutableWrites(file, consts, fset) {
			found = append(found, rel+":"+strconv.Itoa(line))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A scan that matches nothing would read as coverage. The known site is
	// the deliberately unheld write that shows what the hold prevents.
	known := "internal/testutil/mockexec/write_file_linux_test.go"
	if !slices.ContainsFunc(found, func(site string) bool { return strings.HasPrefix(site, known+":") }) {
		t.Fatalf("the scan no longer finds the unheld write in %s; fix the matcher", known)
	}
	found = slices.DeleteFunc(found, func(site string) bool { return strings.HasPrefix(site, known+":") })
	if len(found) > 0 {
		t.Errorf("test code writes an executable without holding process starts off; a process a parallel test "+
			"starts during the write makes running it fail with \"text file busy\". Write it with "+
			"mockexec.WriteFile, or mockexec.Write for a mock script:\n  %s", strings.Join(found, "\n  "))
	}
}

func inTestScope(rel string) bool {
	if strings.HasSuffix(rel, "_test.go") {
		return true
	}
	if strings.HasPrefix(rel, "internal/testutil/") {
		return true
	}
	dir := filepath.Base(filepath.Dir(rel))
	return strings.HasPrefix(rel, "internal/") && (strings.HasSuffix(dir, "test") || strings.HasSuffix(dir, "fake"))
}

// packageConstants maps every constant declared in dir's Go files to its
// expression.
func packageConstants(dir string) (map[string]ast.Expr, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	consts := map[string]ast.Expr{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, entry.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				for i, name := range value.Names {
					if i < len(value.Values) {
						consts[name.Name] = value.Values[i]
					}
				}
			}
		}
	}
	return consts, nil
}

// unheldExecutableWrites returns the lines of executable writes in file that
// no enclosing function holds forks off for.
func unheldExecutableWrites(file *ast.File, consts map[string]ast.Expr, fset *token.FileSet) []int {
	osName := importName(file, "os")
	var lines []int
	var visit func(body *ast.BlockStmt, held bool)
	visit = func(body *ast.BlockStmt, held bool) {
		held = held || holdsForks(body)
		written := map[string]bool{}
		ast.Inspect(body, func(n ast.Node) bool {
			if lit, ok := n.(*ast.FuncLit); ok {
				visit(lit.Body, held)
				return false
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, _ := sel.X.(*ast.Ident)
			fromOS := osName != "" && pkg != nil && pkg.Name == osName
			executable := false
			switch {
			case fromOS && sel.Sel.Name == "WriteFile" && len(call.Args) == 3:
				written[spelling(call.Args[0])] = true
				executable = mayExecute(call.Args[2], consts)
			case fromOS && sel.Sel.Name == "OpenFile" && len(call.Args) == 3:
				written[spelling(call.Args[0])] = true
				executable = mayCreate(call.Args[1], osName) && mayExecute(call.Args[2], consts)
			case fromOS && sel.Sel.Name == "Create" && len(call.Args) == 1:
				written[spelling(call.Args[0])] = true
			case fromOS && sel.Sel.Name == "Chmod" && len(call.Args) == 2:
				executable = written[spelling(call.Args[0])] && mayExecute(call.Args[1], consts)
			case !fromOS && sel.Sel.Name == "Chmod" && len(call.Args) == 1:
				executable = mayExecute(call.Args[0], consts)
			}
			if executable && !held {
				lines = append(lines, fset.Position(call.Pos()).Line)
			}
			return true
		})
	}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
			visit(fn.Body, false)
		}
	}
	return lines
}

// spelling spells an expression so two mentions of one path compare equal.
func spelling(expr ast.Expr) string {
	var b strings.Builder
	ast.Inspect(expr, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.Ident:
			b.WriteString(v.Name + " ")
		case *ast.BasicLit:
			b.WriteString(v.Value + " ")
		}
		return true
	})
	return b.String()
}

// holdsForks reports a call of HoldForks directly in body, outside any
// nested function literal.
func holdsForks(body *ast.BlockStmt) bool {
	held := false
	ast.Inspect(body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			held = held || fn.Sel.Name == "HoldForks"
		case *ast.Ident:
			held = held || fn.Name == "HoldForks"
		}
		return !held
	})
	return held
}

func importName(file *ast.File, path string) string {
	for _, spec := range file.Imports {
		if p, _ := strconv.Unquote(spec.Path.Value); p == path {
			if spec.Name != nil {
				return spec.Name.Name
			}
			return filepath.Base(path)
		}
	}
	return ""
}

// mayCreate reports flags that include os.O_CREATE or that the scan cannot
// read.
func mayCreate(flags ast.Expr, osName string) bool {
	switch v := flags.(type) {
	case *ast.BinaryExpr:
		return mayCreate(v.X, osName) || mayCreate(v.Y, osName)
	case *ast.ParenExpr:
		return mayCreate(v.X, osName)
	case *ast.SelectorExpr:
		pkg, ok := v.X.(*ast.Ident)
		return !ok || pkg.Name != osName || v.Sel.Name == "O_CREATE"
	}
	return true
}

// mayExecute reports a mode with an execute bit or one the scan cannot
// evaluate.
func mayExecute(mode ast.Expr, consts map[string]ast.Expr) bool {
	value, ok := evalMode(mode, consts, 0)
	return !ok || value&0o111 != 0
}

func evalMode(expr ast.Expr, consts map[string]ast.Expr, depth int) (uint64, bool) {
	if depth > 8 {
		return 0, false
	}
	switch v := expr.(type) {
	case *ast.BasicLit:
		if v.Kind != token.INT {
			return 0, false
		}
		n, err := strconv.ParseUint(strings.ReplaceAll(v.Value, "_", ""), 0, 32)
		return n, err == nil
	case *ast.ParenExpr:
		return evalMode(v.X, consts, depth+1)
	case *ast.Ident:
		if value, ok := consts[v.Name]; ok {
			return evalMode(value, consts, depth+1)
		}
	case *ast.SelectorExpr:
		if v.Sel.Name == "ModePerm" {
			return 0o777, true
		}
	case *ast.CallExpr:
		// A conversion such as fs.FileMode(0o755).
		if len(v.Args) == 1 {
			return evalMode(v.Args[0], consts, depth+1)
		}
	case *ast.BinaryExpr:
		x, okX := evalMode(v.X, consts, depth+1)
		y, okY := evalMode(v.Y, consts, depth+1)
		if !okX || !okY {
			return 0, false
		}
		if v.Op == token.OR {
			return x | y, true
		}
		if v.Op == token.AND {
			return x & y, true
		}
		if v.Op == token.AND_NOT {
			return x &^ y, true
		}
	}
	return 0, false
}

func TestTheTripwireFindsEachKindOfUnheldExecutableWrite(t *testing.T) {
	t.Parallel()
	const perm = "const perm = 0o750\n"
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"WriteFile with an executable literal", `os.WriteFile(p, nil, 0o755)`, true},
		{"WriteFile with a package constant", `os.WriteFile(p, nil, perm)`, true},
		{"WriteFile with a converted literal", `os.WriteFile(p, nil, fs.FileMode(0o700))`, true},
		{"WriteFile with a mode it cannot evaluate", `os.WriteFile(p, nil, info.Mode())`, true},
		{"WriteFile of a data file", `os.WriteFile(p, nil, 0o644)`, false},
		{"OpenFile that creates an executable", `os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o755)`, true},
		{"OpenFile of an existing file", `os.OpenFile(p, os.O_WRONLY, 0o755)`, false},
		{"Create then Chmod to executable", "os.Create(p)\nos.Chmod(p, 0o755)", true},
		{"Chmod of a path it did not write", `os.Chmod(dir, 0o755)`, false},
		{"File.Chmod to executable", `f.Chmod(0o700)`, true},
		{"a write under HoldForks", "release := procutil.HoldForks()\ndefer release()\nos.WriteFile(p, nil, 0o755)", false},
		{"a write in a literal under HoldForks", "release := procutil.HoldForks()\ndefer release()\nfunc() { os.WriteFile(p, nil, 0o755) }()", false},
		{"a write in a literal the hold does not cover", "func() { release := procutil.HoldForks(); release() }()\nos.WriteFile(p, nil, 0o755)", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := "package p\n\nimport \"os\"\n\n" + perm + "func f() {\n" + tc.body + "\n}\n"
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "p.go", src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			consts := map[string]ast.Expr{}
			for _, decl := range file.Decls {
				if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.CONST {
					spec := gen.Specs[0].(*ast.ValueSpec)
					consts[spec.Names[0].Name] = spec.Values[0]
				}
			}
			if got := len(unheldExecutableWrites(file, consts, fset)) > 0; got != tc.want {
				t.Fatalf("found = %v, want %v", got, tc.want)
			}
		})
	}
}
