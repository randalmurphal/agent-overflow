package mockexec

import (
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
// procutil.InstallExecutable, which mockexec's WriteFile and CopyFile use, or
// as a link with mockexec.Write: a child that a parallel test forks while the
// file is open for writing keeps it busy, and running it fails with "text
// file busy".
//
// It scans the files of the Makefile's GO_PACKAGE_ROOTS that are test code:
// test files and the files of test-support packages (under internal/testutil,
// or in a directory named *test or *fake). Within a function it reports:
//
//   - os.WriteFile, or os.OpenFile for writing, with a mode that may be
//     executable;
//   - File.Chmod, or os.Chmod of a path the function wrote, to such a mode;
//   - os.WriteFile, os.OpenFile for writing, os.Create or os.Truncate of a
//     path the function installed as an executable.
//
// A mode may be executable when its execute bits are set or are not
// constants the scan can read in the package.
func TestEveryTestExecutableIsWrittenWithForksHeldOff(t *testing.T) {
	root := reporoot.Dir(t)
	dirs, err := packageRootDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	var found []scanSite
	for _, dir := range dirs {
		sites, err := scanDir(root, dir.path, dir.recursive)
		if err != nil {
			t.Fatal(err)
		}
		found = append(found, sites...)
	}
	for _, problem := range checkFindings(found) {
		t.Error(problem)
	}
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
// recursive, skipping what a ./... pattern skips.
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
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") {
			goFiles = append(goFiles, entry.Name())
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

// installers install an executable at their first argument and return it.
var installers = map[string]bool{
	"mockexec.Write":             true,
	"mockexec.WriteFile":         true,
	"mockexec.CopyFile":          true,
	"procutil.InstallExecutable": true,
	"Write":                      true,
	"WriteFile":                  true,
	"CopyFile":                   true,
	"InstallExecutable":          true,
}

// executableWrites returns the executable writes in file, named by
// function. A path's history is followed within one function body; a
// function literal has its own. resolve looks up a constant the file does
// not declare.
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
	var scan func(fnName string, body *ast.BlockStmt)
	scan = func(fnName string, body *ast.BlockStmt) {
		installed := map[string]bool{} // paths installed as executables
		written := map[string]bool{}   // paths written raw
		report := func(call *ast.CallExpr) {
			found = append(found, scanSite{fn: fnName, line: fset.Position(call.Pos()).Line})
		}
		ast.Inspect(body, func(n ast.Node) bool {
			if lit, ok := n.(*ast.FuncLit); ok {
				scan(fnName, lit.Body)
				return false
			}
			if assign, ok := n.(*ast.AssignStmt); ok && len(assign.Lhs) == 1 && len(assign.Rhs) == 1 {
				// path := mockexec.WriteFile(...) names the installed path.
				if call, ok := assign.Rhs[0].(*ast.CallExpr); ok && installers[calleeName(call)] {
					installed[spelling(assign.Lhs[0])] = true
				}
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if installers[calleeName(call)] {
				if path := installedPathArg(call); path != nil {
					installed[spelling(path)] = true
				}
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, _ := sel.X.(*ast.Ident)
			if osName == "" || pkg == nil || pkg.Name != osName {
				if sel.Sel.Name == "Chmod" && len(call.Args) == 1 && mayExecute(call.Args[0], consts) {
					report(call) // File.Chmod
				}
				return true
			}
			var path string
			if len(call.Args) > 0 {
				path = spelling(call.Args[0])
			}
			switch {
			case sel.Sel.Name == "WriteFile" && len(call.Args) == 3:
				written[path] = true
				if installed[path] || mayExecute(call.Args[2], consts) {
					report(call)
				}
			case sel.Sel.Name == "OpenFile" && len(call.Args) == 3:
				if !opensForWriting(call.Args[1], osName) {
					return true
				}
				written[path] = true
				if installed[path] || mayExecute(call.Args[2], consts) {
					report(call)
				}
			case (sel.Sel.Name == "Create" && len(call.Args) == 1) || (sel.Sel.Name == "Truncate" && len(call.Args) == 2):
				written[path] = true
				if installed[path] {
					report(call)
				}
			case sel.Sel.Name == "Chmod" && len(call.Args) == 2:
				if written[path] && mayExecute(call.Args[1], consts) {
					report(call)
				}
			}
			return true
		})
	}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
			scan(fn.Name.Name, fn.Body)
		}
	}
	return found
}

// installedPathArg is the path argument of an installer call: the second of
// mockexec's, after the testing.TB, and the first of procutil's.
func installedPathArg(call *ast.CallExpr) ast.Expr {
	index := 1
	if strings.HasSuffix(calleeName(call), "InstallExecutable") {
		index = 0
	}
	if len(call.Args) <= index {
		return nil
	}
	return call.Args[index]
}

// calleeName spells a call's function as pkg.Name or Name.
func calleeName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		if pkg, ok := fn.X.(*ast.Ident); ok {
			return pkg.Name + "." + fn.Sel.Name
		}
	case *ast.Ident:
		return fn.Name
	}
	return ""
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
		name, body string
		want       bool
	}{
		{"WriteFile with an executable literal", `os.WriteFile(p, nil, 0o755)`, true},
		{"WriteFile with a constant of the file", `os.WriteFile(p, nil, perm)`, true},
		{"WriteFile with a converted literal", `os.WriteFile(p, nil, fs.FileMode(0o700))`, true},
		{"WriteFile with a mode it cannot read", `os.WriteFile(p, nil, info.Mode())`, true},
		{"WriteFile with execute bits cleared", `os.WriteFile(p, nil, info.Mode()&^0o111)`, false},
		{"WriteFile of a data file", `os.WriteFile(p, nil, 0o644)`, false},
		{"OpenFile that creates an executable", `os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o755)`, true},
		{"OpenFile for reading", `os.OpenFile(p, os.O_RDONLY, 0o755)`, false},
		{"Create then Chmod to executable", "os.Create(p)\nos.Chmod(p, 0o755)", true},
		{"Chmod of a path it did not write", `os.Chmod(dir, 0o755)`, false},
		{"Chmod of a path a sibling closure wrote", "_ = func() { os.WriteFile(p, nil, 0o644) }\n_ = func() { os.Chmod(p, 0o700) }", false},
		{"Chmod in a closure of a path it wrote", "_ = func() { os.WriteFile(p, nil, 0o644); os.Chmod(p, 0o700) }", true},
		{"File.Chmod to executable", `f.Chmod(0o700)`, true},
		{"an installed executable", `mockexec.WriteFile(t, p, nil, 0o755)`, false},
		{"a data file written over another path", "mockexec.WriteFile(t, p, nil, 0o755)\nos.WriteFile(q, nil, 0o644)", false},
		{"a Write link written through", "mockexec.Write(t, p, \"\")\nos.WriteFile(p, nil, 0o644)", true},
		// The reviewer's cases: a write after its hold is released, and a
		// reopen of an existing executable.
		{"a write after its hold is released", "release := procutil.HoldForks()\nrelease()\nos.WriteFile(p, nil, 0o755)", true},
		{"a write a hold covers", "release := procutil.HoldForks()\ndefer release()\nos.WriteFile(p, nil, 0o755)", true},
		{"a truncating reopen of an installed executable", "mockexec.WriteFile(t, p, nil, 0o755)\nos.OpenFile(p, os.O_WRONLY|os.O_TRUNC, 0)", true},
		{"a reopen of the path an installer returned", "q := mockexec.WriteFile(t, p, nil, 0o755)\nos.OpenFile(q, os.O_WRONLY, 0)", true},
		{"Create of an installed executable", "procutil.InstallExecutable(p, r, 0o755)\nos.Create(p)", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := "package p\n\nimport \"os\"\n\nconst perm = 0o750\n\nfunc f() {\n" + tc.body + "\n}\n"
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
