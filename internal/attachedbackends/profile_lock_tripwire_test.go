package attachedbackends

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/testutil/reporoot"
)

const attachedBackendsPkgPath = "agent-overflow/internal/attachedbackends"

// lockedContextCalls may take a context under a profile lock, each for the
// reason given. Every other call that takes one is a wait the lock's other
// callers would sit behind.
var lockedContextCalls = map[string]string{
	"agent-overflow/internal/deviceclient.WithProfileLock": "a local file lock, bounded by the caller's short timeout",
	"agent-overflow/internal/deviceclient.WithLifetime":    "records the manager's lifetime on a client it builds; waits on nothing",
}

// TestNothingUnderAProfileLockWaitsOnAContext type-checks this package and
// follows every call made while a profile lock is held, into this package's
// own functions, and fails on a call to another package that takes a
// context.Context outside lockedContextCalls and package context itself. A
// call that takes a context is how Go code waits on the network or on
// another owner; under a profile lock, Forget, agent access, the
// reconciler and every carried request for that computer would wait behind
// it (Manager.profiles).
//
// A profile lock is acquired at the top level of a function and released by
// a defer on the next statement (after an `if err != nil` for LockCtx), so
// the held region is the rest of that function; the test fails on any other
// shape. Goroutines a region starts are not part of it.
//
// It cannot see a call made through a function value or a field holding
// one, or into an interface method's implementations: it checks only an
// interface method's own signature.
//
// The calls in sessionFileDecisions run code of this package under the
// computer's session file lock, deviceclient's fence across processes, which
// this walk cannot enter. Each must be made inside a profile-locked region,
// so the code it is handed is checked here like the rest of that region.
func TestNothingUnderAProfileLockWaitsOnAContext(t *testing.T) {
	t.Parallel()
	reporoot.ObserveGoSources(t)
	pkg, fset, files, info := typeCheckAttachedBackends(t)
	found := profileLockViolations(pkg, fset, files, info)
	if len(found) > 0 {
		t.Fatalf("a profile lock is held across a call that waits on a context; release the lock first, or name a bounded call in lockedContextCalls with its bound:\n%s",
			strings.Join(found, "\n"))
	}
}

// sessionFileDecisions are the deviceclient calls that run a callback of
// this package under a session file's OS lock.
var sessionFileDecisions = []string{
	"(*agent-overflow/internal/deviceclient.Redemption).Save",
	"agent-overflow/internal/deviceclient.ForgetSessionIf",
}

type lockedListedPackage struct {
	ImportPath string
	Dir        string
	Export     string
	GoFiles    []string
	Error      *struct{ Err string }
}

func typeCheckAttachedBackends(t *testing.T) (*types.Package, *token.FileSet, []*ast.File, *types.Info) {
	t.Helper()
	cmd := exec.Command("go", "list", "-e", "-json", "-export", "-deps", "./internal/attachedbackends")
	cmd.Dir = reporoot.Dir(t)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}
	exports := map[string]string{}
	var self lockedListedPackage
	for dec := json.NewDecoder(bytes.NewReader(out)); ; {
		var listed lockedListedPackage
		if err := dec.Decode(&listed); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decode go list: %v", err)
		}
		if listed.Error != nil {
			t.Fatalf("go list %s: %s", listed.ImportPath, listed.Error.Err)
		}
		exports[listed.ImportPath] = listed.Export
		if listed.ImportPath == attachedBackendsPkgPath {
			self = listed
		}
	}
	if len(self.GoFiles) == 0 {
		t.Fatalf("go list did not list %s", attachedBackendsPkgPath)
	}
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file := exports[path]
		if file == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(file)
	})
	var files []*ast.File
	for _, name := range self.GoFiles {
		file, err := parser.ParseFile(fset, filepath.Join(self.Dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	info := &types.Info{
		Uses:       map[*ast.Ident]types.Object{},
		Defs:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
		Instances:  map[*ast.Ident]types.Instance{},
	}
	conf := types.Config{Importer: imp, Sizes: types.SizesFor("gc", runtime.GOARCH)}
	pkg, err := conf.Check(attachedBackendsPkgPath, fset, files, info)
	if err != nil {
		t.Fatalf("type-check %s: %v", attachedBackendsPkgPath, err)
	}
	return pkg, fset, files, info
}

type lockChecker struct {
	fset    *token.FileSet
	info    *types.Info
	decls   map[*types.Func]*ast.FuncDecl
	visited map[*ast.FuncDecl]bool
	found   []string
	// fenced holds the session file decisions made under a profile lock.
	fenced map[token.Pos]bool
}

func profileLockViolations(pkg *types.Package, fset *token.FileSet, files []*ast.File, info *types.Info) []string {
	c := &lockChecker{fset: fset, info: info, decls: map[*types.Func]*ast.FuncDecl{}, visited: map[*ast.FuncDecl]bool{}, fenced: map[token.Pos]bool{}}
	for _, file := range files {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				if obj, ok := info.Defs[fn.Name].(*types.Func); ok {
					c.decls[obj] = fn
				}
			}
		}
	}
	acquisitions := 0
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			acquisitions += c.checkFunction(fn)
		}
	}
	if acquisitions == 0 {
		// A rename of the lock's type or methods would otherwise pass
		// this test with nothing checked.
		c.found = append(c.found, "no profile lock acquisitions found in "+pkg.Path())
	}
	decisions := 0
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !c.decidesSessionFile(call) {
				return true
			}
			decisions++
			if !c.fenced[call.Pos()] {
				c.found = append(c.found, c.position(call.Pos())+": a session file decision outside a profile-locked region runs code this test does not check")
			}
			return true
		})
	}
	if decisions == 0 {
		c.found = append(c.found, "no session file decisions found in "+pkg.Path())
	}
	return c.found
}

func (c *lockChecker) decidesSessionFile(call *ast.CallExpr) bool {
	fn := c.callee(call)
	return fn != nil && slices.Contains(sessionFileDecisions, fn.Origin().FullName())
}

// checkFunction finds the profile lock acquisitions in fn and checks the
// region each one holds.
func (c *lockChecker) checkFunction(fn *ast.FuncDecl) int {
	top := map[ast.Stmt]int{}
	for i, stmt := range fn.Body.List {
		top[stmt] = i
	}
	count := 0
	var enclosing []ast.Node
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if n == nil {
			enclosing = enclosing[:len(enclosing)-1]
			return true
		}
		enclosing = append(enclosing, n)
		call, ok := n.(*ast.CallExpr)
		if !ok || !c.acquiresProfileLock(call) {
			return true
		}
		count++
		where := c.position(call.Pos())
		var stmt ast.Stmt
		if len(enclosing) >= 2 {
			stmt, _ = enclosing[len(enclosing)-2].(ast.Stmt)
		}
		index, atTop := top[stmt]
		assign, isAssign := stmt.(*ast.AssignStmt)
		if !atTop || !isAssign || len(assign.Lhs) == 0 {
			c.found = append(c.found, where+": acquire a profile lock as `unlock := ...` at the top level of the function")
			return true
		}
		unlock, _ := assign.Lhs[0].(*ast.Ident)
		next := index + 1
		if len(assign.Lhs) == 2 {
			if next < len(fn.Body.List) {
				if _, ok := fn.Body.List[next].(*ast.IfStmt); ok {
					next++
				}
			}
		}
		if unlock == nil || next >= len(fn.Body.List) || !defersCall(fn.Body.List[next], unlock.Name) {
			c.found = append(c.found, where+": release a profile lock with `defer "+identName(unlock)+"()` on the next statement")
			return true
		}
		for _, held := range fn.Body.List[next+1:] {
			c.walk(held, where)
		}
		return true
	})
	return count
}

func identName(id *ast.Ident) string {
	if id == nil {
		return "unlock"
	}
	return id.Name
}

func defersCall(stmt ast.Stmt, name string) bool {
	deferred, ok := stmt.(*ast.DeferStmt)
	if !ok || len(deferred.Call.Args) != 0 {
		return false
	}
	id, ok := deferred.Call.Fun.(*ast.Ident)
	return ok && id.Name == name
}

func (c *lockChecker) acquiresProfileLock(call *ast.CallExpr) bool {
	fn := c.callee(call)
	if fn == nil {
		return false
	}
	name := fn.FullName()
	return name == "(*agent-overflow/internal/keyedlock.Registry).Lock" || name == "(*agent-overflow/internal/keyedlock.Registry).LockCtx"
}

// walk checks every call node makes while the lock taken at where is held.
func (c *lockChecker) walk(node ast.Node, where string) {
	ast.Inspect(node, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.GoStmt:
			return false
		case *ast.CallExpr:
			c.checkCall(n, where)
		}
		return true
	})
}

func (c *lockChecker) checkCall(call *ast.CallExpr, where string) {
	fn := c.callee(call)
	if fn == nil {
		return
	}
	fn = fn.Origin()
	if slices.Contains(sessionFileDecisions, fn.FullName()) {
		c.fenced[call.Pos()] = true
	}
	if fn.Pkg() != nil && fn.Pkg().Path() == attachedBackendsPkgPath {
		if decl := c.decls[fn]; decl != nil && !c.visited[decl] {
			c.visited[decl] = true
			c.walk(decl.Body, where)
		}
		return
	}
	if fn.Pkg() != nil && fn.Pkg().Path() == "context" {
		return
	}
	if _, allowed := lockedContextCalls[fn.FullName()]; allowed || !takesContext(fn) {
		return
	}
	c.found = append(c.found, fmt.Sprintf("%s: profile lock held; %s calls %s", where, c.position(call.Pos()), fn.FullName()))
}

// callee is the function a call names statically, or nil for a call
// through a function value, a conversion or a builtin.
func (c *lockChecker) callee(call *ast.CallExpr) *types.Func {
	fun := ast.Unparen(call.Fun)
	switch index := fun.(type) {
	case *ast.IndexExpr:
		fun = index.X
	case *ast.IndexListExpr:
		fun = index.X
	}
	switch fun := fun.(type) {
	case *ast.Ident:
		fn, _ := c.info.Uses[fun].(*types.Func)
		return fn
	case *ast.SelectorExpr:
		if selection, ok := c.info.Selections[fun]; ok {
			if selection.Kind() == types.FieldVal {
				return nil
			}
			fn, _ := selection.Obj().(*types.Func)
			return fn
		}
		fn, _ := c.info.Uses[fun.Sel].(*types.Func)
		return fn
	}
	return nil
}

func takesContext(fn *types.Func) bool {
	signature, ok := fn.Type().(*types.Signature)
	if !ok {
		return false
	}
	params := signature.Params()
	return slices.ContainsFunc(slices.Collect(params.Variables()), func(param *types.Var) bool {
		named, ok := param.Type().(*types.Named)
		return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "context" && named.Obj().Name() == "Context"
	})
}

func (c *lockChecker) position(pos token.Pos) string {
	at := c.fset.Position(pos)
	return fmt.Sprintf("%s:%d", filepath.Base(at.Filename), at.Line)
}
