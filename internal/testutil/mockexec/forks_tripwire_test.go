package mockexec

import (
	"errors"
	"fmt"
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

// controlSite is the one raw executable write test code may make: the test
// that shows what the hold prevents.
var controlSite = scanSite{
	file: "internal/procutil/forks_linux_test.go",
	fn:   "TestAChildHoldingTheWriteDescriptorMakesTheExecutableBusy",
}

// TestEveryTestExecutableIsWrittenWithForksHeldOff is a tripwire, because
// nothing in the type system stops test code from calling os.WriteFile with
// an executable mode. Test code writes an executable only through
// procutil.InstallExecutable or CopyExecutable, which mockexec's WriteFile
// and CopyFile use, or as a link with mockexec.Write: a child that a
// parallel test forks while the file is open for writing keeps it busy, and
// running it fails with "text file busy".
//
// It scans the files of the Makefile's GO_PACKAGE_ROOTS that are test code:
// test files and the files of test-support packages (under internal/testutil,
// or in a directory named *test or *fake). It reports os.WriteFile, and
// os.OpenFile for writing, with a mode that may be executable, and File.Chmod
// to such a mode, whatever name the file imports os under. A mode may be
// executable when its execute bits are set or are not constants the scan can
// read in the package. It does not judge os.Chmod, which cannot tell a file
// from a directory, or what a function does later with a path it wrote.
func TestEveryTestExecutableIsWrittenWithForksHeldOff(t *testing.T) {
	found, err := scanTree(reporoot.Dir(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range checkFindings(found) {
		t.Error(problem)
	}
}

// scanTree scans the test code of the project rooted at root.
func scanTree(root string) ([]scanSite, error) {
	dirs, err := packageRootDirs(root)
	if err != nil {
		return nil, err
	}
	var found []scanSite
	for _, dir := range dirs {
		sites, err := scanDir(root, dir.path, dir.recursive)
		if err != nil {
			return nil, err
		}
		found = append(found, sites...)
	}
	return found, nil
}

type scanSite struct {
	file, fn string
	line     int
}

// checkFindings reports every finding but one at the control site, and the
// control site when the scan did not find it exactly once.
func checkFindings(found []scanSite) []string {
	var problems []string
	control := 0
	for _, site := range found {
		if site.file == controlSite.file && site.fn == controlSite.fn {
			control++
			continue
		}
		problems = append(problems, fmt.Sprintf("%s:%d (%s) writes an executable without procutil.InstallExecutable; "+
			"a process a parallel test starts during the write makes running it fail with \"text file busy\". "+
			"Write it with mockexec.WriteFile or mockexec.CopyFile, or mockexec.Write for a mock script",
			site.file, site.line, site.fn))
	}
	if control != 1 {
		problems = append(problems, fmt.Sprintf("the scan found the raw write in %s %s %d times, want once; fix the matcher",
			controlSite.file, controlSite.fn, control))
	}
	return problems
}

type rootDir struct {
	path      string
	recursive bool
}

// packageRootDirs reads GO_PACKAGE_ROOTS from the Makefile, the roots make
// go-test runs.
func packageRootDirs(root string) ([]rootDir, error) {
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(makefile), "\n") {
		value, ok := strings.CutPrefix(line, "GO_PACKAGE_ROOTS := ")
		if !ok {
			continue
		}
		var dirs []rootDir
		for _, pattern := range strings.Fields(value) {
			dir, recursive := strings.CutSuffix(pattern, "/...")
			dirs = append(dirs, rootDir{path: filepath.Join(root, filepath.FromSlash(dir)), recursive: recursive})
		}
		return dirs, nil
	}
	return nil, fmt.Errorf("the Makefile sets no GO_PACKAGE_ROOTS")
}

// scanDir scans the test code of the packages in dir, and under it when
// recursive, skipping what a ./... pattern skips: a directory whose name
// starts with "." or "_", testdata, and a nested module.
func scanDir(root, dir string, recursive bool) ([]scanSite, error) {
	var found []scanSite
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if path != dir {
			name := d.Name()
			if !recursive || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir
			} else if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		sites, err := scanPackageDir(root, path)
		found = append(found, sites...)
		return err
	})
	return found, err
}

// scanPackageDir scans the test code in one directory. Each file is parsed
// at most once: the test code always, another file of its package only when
// a mode names a constant the test code's file does not declare.
func scanPackageDir(root, dir string) ([]scanSite, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}
	parse := func(name string) (*ast.File, error) {
		if file, ok := parsed[name]; ok {
			return file, nil
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		parsed[name] = file
		return file, nil
	}
	var goFiles []string
	for _, entry := range entries {
		// The go command ignores a file whose name starts with "_" or ".".
		name := entry.Name()
		if !entry.IsDir() && strings.HasSuffix(name, ".go") && !strings.HasPrefix(name, "_") && !strings.HasPrefix(name, ".") {
			goFiles = append(goFiles, name)
		}
	}
	var packageConsts map[string]map[string]ast.Expr // package name -> constants
	var resolveErr error
	resolve := func(pkg, name string) (ast.Expr, bool) {
		if packageConsts == nil {
			packageConsts = map[string]map[string]ast.Expr{}
			ambiguous := map[string]map[string]bool{}
			for _, goFile := range goFiles {
				file, err := parse(goFile)
				if err != nil {
					resolveErr = err
					return nil, false
				}
				p := file.Name.Name
				if packageConsts[p] == nil {
					packageConsts[p], ambiguous[p] = map[string]ast.Expr{}, map[string]bool{}
				}
				for constName, value := range fileConstants(file) {
					if _, seen := packageConsts[p][constName]; seen {
						ambiguous[p][constName] = true // one per build tag
					}
					packageConsts[p][constName] = value
				}
			}
			for p, names := range ambiguous {
				for constName := range names {
					delete(packageConsts[p], constName)
				}
			}
		}
		value, ok := packageConsts[pkg][name]
		return value, ok
	}
	var found []scanSite
	for _, goFile := range goFiles {
		rel, err := filepath.Rel(root, filepath.Join(dir, goFile))
		if err != nil {
			return nil, err
		}
		rel = filepath.ToSlash(rel)
		if !inTestScope(rel) {
			continue
		}
		file, err := parse(goFile)
		if err != nil {
			return nil, err
		}
		pkg := file.Name.Name
		for _, site := range executableWrites(file, fset, func(name string) (ast.Expr, bool) { return resolve(pkg, name) }) {
			site.file = rel
			found = append(found, site)
		}
	}
	return found, resolveErr
}

func inTestScope(rel string) bool {
	if strings.HasSuffix(rel, "_test.go") || strings.HasPrefix(rel, "internal/testutil/") {
		return true
	}
	dir := filepath.Base(filepath.Dir(rel))
	return strings.HasPrefix(rel, "internal/") && (strings.HasSuffix(dir, "test") || strings.HasSuffix(dir, "fake"))
}

// executableWrites returns the executable writes in file, named by the
// function they are in. resolve looks up a constant the file does not
// declare.
func executableWrites(file *ast.File, fset *token.FileSet, resolve func(string) (ast.Expr, bool)) []scanSite {
	osName := importName(file, "os")
	local := fileConstants(file)
	consts := func(name string) (ast.Expr, bool) {
		if value, ok := local[name]; ok {
			return value, true
		}
		if resolve == nil {
			return nil, false
		}
		return resolve(name)
	}
	var found []scanSite
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name, fromOS := osFunc(call, osName)
			executable := false
			switch {
			case fromOS && name == "WriteFile" && len(call.Args) == 3:
				executable = mayExecute(call.Args[2], consts)
			case fromOS && name == "OpenFile" && len(call.Args) == 3:
				executable = opensForWriting(call.Args[1], osName) && mayExecute(call.Args[2], consts)
			case !fromOS && name == "Chmod" && len(call.Args) == 1:
				executable = mayExecute(call.Args[0], consts) // File.Chmod
			}
			if executable {
				found = append(found, scanSite{fn: fn.Name.Name, line: fset.Position(call.Pos()).Line})
			}
			return true
		})
	}
	return found
}

// osFunc names the function or method call calls, and reports whether it
// is a function of package os, which the file imports as osName ("." for a
// dot import, "" for none).
func osFunc(call *ast.CallExpr, osName string) (string, bool) {
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		pkg, ok := fn.X.(*ast.Ident)
		return fn.Sel.Name, ok && osName != "" && osName != "." && pkg.Name == osName
	case *ast.Ident:
		return fn.Name, osName == "."
	}
	return "", false
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

// fileConstants maps the constants file declares to their expressions.
func fileConstants(file *ast.File) map[string]ast.Expr {
	consts := map[string]ast.Expr{}
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
	return consts
}

// opensForWriting reports flags that include os.O_WRONLY or os.O_RDWR, or
// that the scan cannot read.
func opensForWriting(flags ast.Expr, osName string) bool {
	switch v := flags.(type) {
	case *ast.BinaryExpr:
		return opensForWriting(v.X, osName) || opensForWriting(v.Y, osName)
	case *ast.ParenExpr:
		return opensForWriting(v.X, osName)
	case *ast.SelectorExpr:
		pkg, ok := v.X.(*ast.Ident)
		return !ok || pkg.Name != osName || v.Sel.Name == "O_WRONLY" || v.Sel.Name == "O_RDWR"
	case *ast.Ident:
		if osName == "." && strings.HasPrefix(v.Name, "O_") {
			return v.Name == "O_WRONLY" || v.Name == "O_RDWR"
		}
	}
	return true
}

// mayExecute reports a mode whose execute bits may be set.
func mayExecute(mode ast.Expr, consts func(string) (ast.Expr, bool)) bool {
	return execBits(mode, consts, 0) != 0
}

// execBits is the execute bits mode may have set: those of a constant, all
// three for what the scan cannot read.
func execBits(expr ast.Expr, consts func(string) (ast.Expr, bool), depth int) uint64 {
	const unknown = 0o111
	if depth > 8 {
		return unknown
	}
	switch v := expr.(type) {
	case *ast.BasicLit:
		if bits, ok := constantBits(v, consts); ok {
			return bits
		}
	case *ast.ParenExpr:
		return execBits(v.X, consts, depth+1)
	case *ast.Ident:
		if value, ok := consts(v.Name); ok {
			return execBits(value, consts, depth+1)
		}
	case *ast.CallExpr:
		// A conversion such as fs.FileMode(0o755).
		if len(v.Args) == 1 {
			return execBits(v.Args[0], consts, depth+1)
		}
	case *ast.BinaryExpr:
		x := execBits(v.X, consts, depth+1)
		if v.Op == token.OR {
			return x | execBits(v.Y, consts, depth+1)
		}
		// Clearing or masking with a constant is exact whatever x is.
		y, known := constantBits(v.Y, consts)
		if v.Op == token.AND_NOT && known {
			return x &^ y
		}
		if v.Op == token.AND && known {
			return x & y
		}
	}
	return unknown
}

// constantBits is the execute bits of a literal mode or a constant naming
// one, and whether expr is one.
func constantBits(expr ast.Expr, consts func(string) (ast.Expr, bool)) (uint64, bool) {
	switch v := expr.(type) {
	case *ast.BasicLit:
		if v.Kind != token.INT {
			return 0, false
		}
		n, err := strconv.ParseUint(strings.ReplaceAll(v.Value, "_", ""), 0, 32)
		return n & 0o111, err == nil
	case *ast.ParenExpr:
		return constantBits(v.X, consts)
	case *ast.Ident:
		if value, ok := consts(v.Name); ok {
			return constantBits(value, consts)
		}
	}
	return 0, false
}

func TestTheTripwireFindsEachKindOfExecutableWrite(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, imports, body string
		want                bool
	}{
		{"WriteFile with an executable literal", `"os"`, `os.WriteFile(p, nil, 0o755)`, true},
		{"WriteFile with a constant of the file", `"os"`, `os.WriteFile(p, nil, perm)`, true},
		{"WriteFile with a converted literal", `"os"`, `os.WriteFile(p, nil, fs.FileMode(0o700))`, true},
		{"WriteFile with a mode it cannot read", `"os"`, `os.WriteFile(p, nil, info.Mode())`, true},
		{"WriteFile with execute bits cleared", `"os"`, `os.WriteFile(p, nil, info.Mode()&^0o111)`, false},
		{"WriteFile of a data file", `"os"`, `os.WriteFile(p, nil, 0o644)`, false},
		{"OpenFile that creates an executable", `"os"`, `os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o755)`, true},
		{"OpenFile for reading", `"os"`, `os.OpenFile(p, os.O_RDONLY, 0o755)`, false},
		{"File.Chmod to executable", `"os"`, `f.Chmod(0o700)`, true},
		{"a write in a closure", `"os"`, `t.Run("x", func(t *testing.T) { os.WriteFile(p, nil, 0o755) })`, true},
		{"os imported under another name", `xos "os"`, `xos.WriteFile(p, nil, 0o755)`, true},
		{"os imported with a dot", `. "os"`, `OpenFile(p, O_CREATE|O_WRONLY, 0o755)`, true},
		{"a dot import read only", `. "os"`, `OpenFile(p, O_RDONLY, 0o755)`, false},
		{"another package's WriteFile", `"os"; xos "example.com/os"`, `xos.WriteFile(p, nil, 0o755)`, false},
		// The reviewer's first case: a hold does not make a raw write pass.
		{"a write after its hold is released", `"os"`, "release := procutil.HoldForks()\nrelease()\nos.WriteFile(p, nil, 0o755)", true},
		{"a write a hold covers", `"os"`, "release := procutil.HoldForks()\ndefer release()\nos.WriteFile(p, nil, 0o755)", true},
		{"an installed executable", `"os"`, `mockexec.WriteFile(t, p, nil, 0o755)`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := "package p\n\nimport (" + tc.imports + ")\n\nconst perm = 0o750\n\nfunc f() {\n" + tc.body + "\n}\n"
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "p.go", src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(executableWrites(file, fset, nil)) > 0; got != tc.want {
				t.Fatalf("found = %v, want %v", got, tc.want)
			}
		})
	}
}

// The scan reads what the go command builds in the package roots, and
// nothing else: not a file the go command ignores, a nested module, a
// directory outside the roots, or testdata.
func TestTheTripwireScansTheProjectsPackagesOnly(t *testing.T) {
	t.Parallel()
	const raw = "package p\n\nimport \"os\"\n\nfunc f() { os.WriteFile(p, nil, 0o755) }\n"
	for _, tc := range []struct {
		name string
		file string
		want int
	}{
		{"a test file of a package", "internal/p/p_test.go", 1},
		{"a test file of the root package", "root_test.go", 1},
		{"a file whose name starts with _", "internal/p/_draft_test.go", 0},
		{"a file whose name starts with .", "internal/p/.draft_test.go", 0},
		{"a nested module", "internal/nested/n_test.go", 0},
		{"a directory outside the roots", "scratch/example_test.go", 0},
		{"a directory under the root package", "sub/s_test.go", 0},
		{"testdata", "internal/p/testdata/t_test.go", 0},
		{"a directory whose name starts with _", "internal/_old/o_test.go", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			files := map[string]string{
				"Makefile":               "GO_PACKAGE_ROOTS := . ./cmd/... ./internal/...\n",
				"go.mod":                 "module example\n",
				"internal/nested/go.mod": "module nested\n",
				tc.file:                  raw,
			}
			for name, content := range files {
				path := filepath.Join(root, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(filepath.Join(root, "cmd"), 0o755); err != nil {
				t.Fatal(err)
			}
			found, err := scanTree(root)
			if err != nil || len(found) != tc.want {
				t.Fatalf("found %v, %v; want %d", found, err, tc.want)
			}
		})
	}
}

// The control site passes only alone: a second raw write in its function or
// its file (the reviewer's third case), or none at all, fails.
func TestTheTripwireAcceptsOnlyTheControlSiteOnce(t *testing.T) {
	t.Parallel()
	control := scanSite{file: controlSite.file, fn: controlSite.fn, line: 1}
	for _, tc := range []struct {
		name  string
		found []scanSite
		want  int
	}{
		{"the control site alone", []scanSite{control}, 0},
		{"no control site", nil, 1},
		{"a second write in the control function", []scanSite{control, control}, 1},
		{"a second write in the control file", []scanSite{control, {file: controlSite.file, fn: "other", line: 2}}, 1},
		{"a write elsewhere", []scanSite{control, {file: "internal/x/x_test.go", fn: "TestX", line: 3}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := checkFindings(tc.found); len(got) != tc.want {
				t.Fatalf("problems = %q, want %d", got, tc.want)
			}
		})
	}
}

// A mode named by a constant of another file of the package is read from
// that file; a name two files declare, one per build tag, cannot be read.
func TestTheTripwireReadsConstantsOfTheTestsPackage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  int
	}{
		{"a data mode of the package", map[string]string{"perm.go": "package p\n\nconst filePerm = 0o600\n"}, 0},
		{"an executable mode of the package", map[string]string{"perm.go": "package p\n\nconst filePerm = 0o700\n"}, 1},
		{"a constant of another package in the directory", map[string]string{"perm.go": "package q\n\nconst filePerm = 0o600\n"}, 1},
		{"a constant declared per build tag", map[string]string{
			"perm_linux.go": "package p\n\nconst filePerm = 0o600\n",
			"perm_other.go": "package p\n\nconst filePerm = 0o700\n",
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			dir := filepath.Join(root, "internal", "p")
			files := map[string]string{"p_test.go": "package p\n\nimport \"os\"\n\nfunc f() { os.WriteFile(p, nil, filePerm) }\n"}
			for name, src := range tc.files {
				files[name] = src
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for name, src := range files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			found, err := scanPackageDir(root, dir)
			if err != nil || len(found) != tc.want {
				t.Fatalf("found %v, %v; want %d", found, err, tc.want)
			}
		})
	}
}
