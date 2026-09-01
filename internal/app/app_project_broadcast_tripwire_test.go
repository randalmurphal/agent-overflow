package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The classification every exported projectapp.Service method must carry.
//
// A project mutation that persists but announces nothing is invisible to every
// other attached client, and it fails silently — the initiating client looks
// correct, so nobody notices until two screens disagree. The two tests below
// make that structural instead of remembered: a new Service method fails
// projectAppMethodIsClassified until someone places it here, and placing it as
// a WRITE then forces every App call site to broadcast.
var (
	projectAppReads = []string{
		"GetWorktreeSetup",
		"List",
		"ProjectForWorkspaceOperation",
		"ResolveSourceWorkspace",
		"WorkflowFootprint",
	}
	projectAppWrites = []string{
		"Archive",
		"BackfillIdentity",
		"Create",
		"EnsureForWorkspace",
		"Rename",
		"SetWorktreeSetup",
		"Unarchive",
		"UpdateSortPositions",
	}
)

// These suites run from the repository root (see TestMain), so packages are
// named explicitly rather than scanned as ".".
const (
	appPackageDir        = "internal/app"
	projectAppPackageDir = "../projectapp"
)

func parsePackageFiles(t *testing.T, dir string) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	files := make(map[string]*ast.File, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files[name] = file
	}
	if len(files) == 0 {
		t.Fatalf("no production sources found under %s", dir)
	}
	return files
}

// isServiceMethod reports whether fn is a method on projectapp.Service. The
// package also carries WorkflowFootprint's own methods, which describe a
// value rather than reaching the store.
func isServiceMethod(fn *ast.FuncDecl) bool {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return false
	}
	receiver := fn.Recv.List[0].Type
	if star, ok := receiver.(*ast.StarExpr); ok {
		receiver = star.X
	}
	ident, ok := receiver.(*ast.Ident)
	return ok && ident.Name == "Service"
}

// TestEveryProjectServiceMethodIsClassified keeps the read/write split above
// total. An unclassified method is one nobody decided about, which is exactly
// how a mutation ships without a broadcast.
func TestEveryProjectServiceMethodIsClassified(t *testing.T) {
	var found []string
	for _, file := range parsePackageFiles(t, filepath.Join(appPackageDir, projectAppPackageDir)) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !fn.Name.IsExported() || !isServiceMethod(fn) {
				continue
			}
			found = append(found, fn.Name.Name)
		}
	}
	slices.Sort(found)
	found = slices.Compact(found)

	classified := append(append([]string{}, projectAppReads...), projectAppWrites...)
	slices.Sort(classified)
	for _, name := range found {
		if !slices.Contains(classified, name) {
			t.Errorf("projectapp.Service.%s is neither in projectAppReads nor projectAppWrites; "+
				"a method that persists must broadcast, so classify it", name)
		}
	}
	for _, name := range classified {
		if !slices.Contains(found, name) {
			t.Errorf("projectAppReads/Writes names %q, which projectapp.Service no longer has", name)
		}
	}
}

// TestEveryProjectMutationCallSiteBroadcasts is the rule with teeth: an App
// function that reaches a project-writing service method must also emit on
// `project:updated` in the same function. DeleteProject is the one shape that
// does not go through the service at all — it drives the store directly, and
// it is covered by TestProjectMutationsBroadcastTheChangedRow instead.
func TestEveryProjectMutationCallSiteBroadcasts(t *testing.T) {
	for name, file := range parsePackageFiles(t, appPackageDir) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			var writes []string
			broadcasts := false
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if strings.HasPrefix(sel.Sel.Name, "broadcastProject") {
					broadcasts = true
					return true
				}
				// Match `<...>.projectApplication().Method(...)`: the selector's
				// own receiver is the projectApplication() call.
				inner, ok := sel.X.(*ast.CallExpr)
				if !ok {
					return true
				}
				innerSel, ok := inner.Fun.(*ast.SelectorExpr)
				if !ok || innerSel.Sel.Name != "projectApplication" {
					return true
				}
				if slices.Contains(projectAppWrites, sel.Sel.Name) {
					writes = append(writes, sel.Sel.Name)
				}
				return true
			})
			if len(writes) > 0 && !broadcasts {
				t.Errorf("%s: %s calls the project write(s) %v and never broadcasts; "+
					"a persisted project mutation no other client hears about is a silent desync",
					name, fn.Name.Name, writes)
			}
		}
	}
}
