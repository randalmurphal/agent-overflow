//go:build !noremote

package attachedbackends

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"slices"
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/testutil/reporoot"
)

// waitingUnderManagerMutex are the packages whose calls can wait on a lock,
// a file or the network. Manager.mu is a leaf (computer_state.go): a
// transition takes it inside a session file's lock, and a client's mutex
// holder waits on that lock, so nothing under Manager.mu may wait on
// either.
var waitingUnderManagerMutex = []string{
	"agent-overflow/internal/atomicfile",
	"agent-overflow/internal/backendproxy",
	"agent-overflow/internal/deviceclient",
	"agent-overflow/internal/filelock",
	"agent-overflow/internal/keyedlock",
	"agent-overflow/internal/rpcclient",
	"net",
	"net/http",
	"os",
}

// TestNothingWaitsUnderTheManagerMutex type-checks this package, finds every
// region that holds Manager.mu (from `X.mu.Lock()` to the `X.mu.Unlock()` in
// the same block, or to the end of the block after `defer X.mu.Unlock()`),
// follows the calls made there into this package's functions, and fails on
// a call into waitingUnderManagerMutex, a channel operation, a select, a
// WaitGroup or Cond wait, or a sleep. It cannot see a call made through a
// function value; function literals are not walked, since they run later.
func TestNothingWaitsUnderTheManagerMutex(t *testing.T) {
	t.Parallel()
	reporoot.ObserveGoSources(t)
	pkg, fset, files, info := typeCheckAttachedBackends(t)
	c := &mutexChecker{fset: fset, info: info, decls: map[*types.Func]*ast.FuncDecl{}}
	for _, file := range files {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				if obj, ok := info.Defs[fn.Name].(*types.Func); ok {
					c.decls[obj] = fn
				}
			}
		}
	}
	regions := 0
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			block, ok := n.(*ast.BlockStmt)
			if !ok {
				return true
			}
			for i, stmt := range block.List {
				if !c.managerMutexCall(stmt, "Lock") {
					continue
				}
				regions++
				where := c.position(stmt.Pos())
				rest := block.List[i+1:]
				if len(rest) > 0 {
					if deferred, ok := rest[0].(*ast.DeferStmt); ok && c.isManagerMutexCall(deferred.Call, "Unlock") {
						rest = rest[1:]
					}
				}
				c.walkList(rest, where, map[*ast.FuncDecl]bool{})
			}
			return true
		})
	}
	if regions == 0 {
		c.found = append(c.found, "no Manager.mu regions found in "+pkg.Path())
	}
	if len(c.found) > 0 {
		t.Fatalf("Manager.mu is held across a wait; release it first:\n%s", strings.Join(c.found, "\n"))
	}
}

type mutexChecker struct {
	fset  *token.FileSet
	info  *types.Info
	decls map[*types.Func]*ast.FuncDecl
	found []string
}

// managerMutexCall reports whether stmt is `X.mu.<method>()` on a Manager.
func (c *mutexChecker) managerMutexCall(stmt ast.Stmt, method string) bool {
	expr, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := expr.X.(*ast.CallExpr)
	return ok && c.isManagerMutexCall(call, method)
}

func (c *mutexChecker) isManagerMutexCall(call *ast.CallExpr, method string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != method {
		return false
	}
	field, ok := sel.X.(*ast.SelectorExpr)
	if !ok || field.Sel.Name != "mu" {
		return false
	}
	selection, ok := c.info.Selections[field]
	if !ok {
		return false
	}
	recv := selection.Recv()
	if ptr, ok := recv.(*types.Pointer); ok {
		recv = ptr.Elem()
	}
	named, ok := recv.(*types.Named)
	return ok && named.Obj().Name() == "Manager" && named.Obj().Pkg().Path() == attachedBackendsPkgPath
}

// walkList checks stmts, which run with Manager.mu held, up to the first
// statement that releases it.
func (c *mutexChecker) walkList(stmts []ast.Stmt, where string, visited map[*ast.FuncDecl]bool) {
	for _, stmt := range stmts {
		if c.managerMutexCall(stmt, "Unlock") {
			return
		}
		c.walkNode(stmt, where, visited)
	}
}

func (c *mutexChecker) walkNode(node ast.Node, where string, visited map[*ast.FuncDecl]bool) {
	ast.Inspect(node, func(n ast.Node) bool {
		switch n := n.(type) {
		case nil:
			return false
		case *ast.FuncLit, *ast.GoStmt:
			return false
		case *ast.BlockStmt:
			if n != node {
				c.walkList(n.List, where, visited)
				return false
			}
		case *ast.CaseClause:
			for _, expr := range n.List {
				c.walkNode(expr, where, visited)
			}
			c.walkList(n.Body, where, visited)
			return false
		case *ast.SelectStmt:
			c.flag(where, n.Pos(), "a select")
			return false
		case *ast.SendStmt:
			c.flag(where, n.Pos(), "a channel send")
		case *ast.UnaryExpr:
			if n.Op == token.ARROW {
				c.flag(where, n.Pos(), "a channel receive")
			}
		case *ast.CallExpr:
			c.checkCall(n, where, visited)
		}
		return true
	})
}

func (c *mutexChecker) checkCall(call *ast.CallExpr, where string, visited map[*ast.FuncDecl]bool) {
	fn := (&lockChecker{info: c.info}).callee(call)
	if fn == nil || fn.Pkg() == nil {
		return
	}
	fn = fn.Origin()
	path := fn.Pkg().Path()
	switch {
	case path == attachedBackendsPkgPath:
		if decl := c.decls[fn]; decl != nil && !visited[decl] {
			visited[decl] = true
			c.walkList(decl.Body.List, where, visited)
		}
	case slices.Contains(waitingUnderManagerMutex, path):
		c.flag(where, call.Pos(), "a call to "+fn.FullName())
	case fn.FullName() == "(*sync.WaitGroup).Wait" || fn.FullName() == "(*sync.Cond).Wait" || fn.FullName() == "time.Sleep":
		c.flag(where, call.Pos(), "a call to "+fn.FullName())
	}
}

func (c *mutexChecker) flag(where string, pos token.Pos, what string) {
	c.found = append(c.found, fmt.Sprintf("%s: Manager.mu held; %s makes %s", where, c.position(pos), what))
}

func (c *mutexChecker) position(pos token.Pos) string {
	return (&lockChecker{fset: c.fset}).position(pos)
}

// sharedFileReaders may name a shared file outside computerTx's methods,
// to read it; none of them writes.
var sharedFileReaders = []string{"AgentAccess", "ownDeviceExcluded", "ExcludedOwnDeviceIDs"}

// fileWriters are the calls that write or remove a file.
var fileWriters = []string{
	"agent-overflow/internal/atomicfile.Remove",
	"agent-overflow/internal/atomicfile.Write",
	"agent-overflow/internal/atomicfile.WriteJSON",
	"os.Remove",
	"os.RemoveAll",
	"os.Rename",
	"os.WriteFile",
}

// heldSessionWriters write a held session file. Only computerTx's methods
// call them, which record the write for the rollback (computerTx.wrote).
var heldSessionWriters = []string{
	"(*agent-overflow/internal/deviceclient.HeldSession).Write",
	"(*agent-overflow/internal/deviceclient.HeldSession).Remove",
	"(*agent-overflow/internal/deviceclient.HeldSession).Restore",
	"(*agent-overflow/internal/deviceclient.Redemption).Save",
}

// TestOnlyTheComputerTransactionWritesItsState holds what the types already
// arrange (computer_state.go): a computerTx is built only by transact, only
// transact holds a session file, only computerTx's methods write it or name
// a shared file other than to read it, the readers write nothing, and
// nothing deletes a file except through atomicfile.Remove.
func TestOnlyTheComputerTransactionWritesItsState(t *testing.T) {
	t.Parallel()
	reporoot.ObserveGoSources(t)
	_, fset, files, info := typeCheckAttachedBackends(t)
	pos := func(p token.Pos) string { return (&lockChecker{fset: fset}).position(p) }
	callee := func(call *ast.CallExpr) string {
		if fn := (&lockChecker{info: info}).callee(call); fn != nil {
			return fn.Origin().FullName()
		}
		return ""
	}
	shared := map[string]bool{agentAccessFile: true, ownExclusionsFile: true}
	var found []string
	named := map[string]int{}
	for _, file := range files {
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.GenDecl:
				// The shared files' names are spelled once, as constants.
				if decl.Tok == token.CONST {
					continue
				}
				ast.Inspect(decl, func(n ast.Node) bool {
					if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if name, err := strconv.Unquote(lit.Value); err == nil && shared[name] {
							found = append(found, pos(lit.Pos())+": spell the shared file by its constant")
						}
					}
					return true
				})
			case *ast.FuncDecl:
				if decl.Body == nil {
					continue
				}
				name := decl.Name.Name
				onTx := decl.Recv != nil && receiverName(decl) == "computerTx"
				reader := slices.Contains(sharedFileReaders, name)
				ast.Inspect(decl.Body, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.BasicLit:
						if text, err := strconv.Unquote(n.Value); n.Kind == token.STRING && err == nil && shared[text] {
							found = append(found, pos(n.Pos())+": spell the shared file by its constant")
						}
					case *ast.Ident:
						if obj, ok := info.Uses[n].(*types.Const); ok && obj.Pkg() != nil && obj.Pkg().Path() == attachedBackendsPkgPath && (obj.Name() == "agentAccessFile" || obj.Name() == "ownExclusionsFile") {
							named[obj.Name()]++
							if !onTx && !reader {
								found = append(found, pos(n.Pos())+": only computerTx's methods and the readers name a shared file")
							}
						}
					case *ast.CompositeLit:
						if ident, ok := n.Type.(*ast.Ident); ok && ident.Name == "computerTx" && name != "transact" {
							found = append(found, pos(n.Pos())+": only transact builds a computerTx")
						}
					case *ast.CallExpr:
						full := callee(n)
						switch {
						case full == "agent-overflow/internal/deviceclient.WithSession" && name != "transact":
							found = append(found, pos(n.Pos())+": only transact holds a session file")
						case slices.Contains(heldSessionWriters, full) && !onTx:
							found = append(found, pos(n.Pos())+": only computerTx's methods write a held session")
						case slices.Contains(fileWriters, full) && reader:
							found = append(found, pos(n.Pos())+": a shared file reader writes")
						case full == "os.Remove" || full == "os.RemoveAll":
							found = append(found, pos(n.Pos())+": delete through atomicfile.Remove, which makes the delete durable")
						}
					}
					return true
				})
			}
		}
	}
	for _, constant := range []string{"agentAccessFile", "ownExclusionsFile"} {
		if named[constant] == 0 {
			found = append(found, "no use of "+constant+" found; the check would pass with nothing checked")
		}
	}
	if len(found) > 0 {
		t.Fatalf("a computer's pairing state is written outside its transaction:\n%s", strings.Join(found, "\n"))
	}
}

func receiverName(decl *ast.FuncDecl) string {
	expr := decl.Recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// lockFreeClientMethods are the deviceclient.Client methods a transition may
// call, each because it takes no lock
// (deviceclient.TestTheCarrierBuildTakesNoClientLock).
var lockFreeClientMethods = []string{
	"(*agent-overflow/internal/deviceclient.Client).WebSocketURL",
}

// transitionFunctionValues are the function-valued fields a transition
// calls, with the package function each holds outside tests.
var transitionFunctionValues = map[string]string{
	"buildCarrier": "newCarrier",
}

// TestNoTransitionUsesAClient type-checks this package and walks every
// transition (walkTransitions) once, for two rules:
//   - it fails on a call to a deviceclient.Client method outside
//     lockFreeClientMethods: a client's mutex is taken before a session
//     file's lock (computer_state.go), so a transition that takes it can
//     wait on a renewal that waits on the transition;
//   - it fails on a write of Manager.carriers or Manager.verdicts: a
//     transition only records its cache changes, and transact applies them
//     once it committed (applyCache), so a failed or rolled-back one changes
//     none.
func TestNoTransitionUsesAClient(t *testing.T) {
	t.Parallel()
	c := walkTransitions(t)
	if len(c.found) > 0 {
		t.Errorf("a transition calls a client method that may take its mutex:\n%s", strings.Join(c.found, "\n"))
	}
	if len(c.cacheWrites) > 0 {
		t.Errorf("a transition changes the cache before it committed:\n%s", strings.Join(c.cacheWrites, "\n"))
	}
}

// walkTransitions type-checks this package and walks every transition: the
// function literals passed to transact, and the one transact passes to
// deviceclient.WithSession, following calls into this package's functions
// and the function-valued fields in transitionFunctionValues. Goroutines
// are not walked.
func walkTransitions(t *testing.T) *clientChecker {
	t.Helper()
	reporoot.ObserveGoSources(t)
	_, fset, files, info := typeCheckAttachedBackends(t)
	c := &clientChecker{fset: fset, info: info, decls: map[string]*ast.FuncDecl{}, visited: map[*ast.FuncDecl]bool{}}
	for _, file := range files {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				if obj, ok := info.Defs[fn.Name].(*types.Func); ok {
					c.decls[obj.FullName()] = fn
				}
			}
		}
	}
	regions := 0
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := c.callee(call)
			if name != transactName && name != "agent-overflow/internal/deviceclient.WithSession" {
				return true
			}
			for _, arg := range call.Args {
				if lit, ok := arg.(*ast.FuncLit); ok {
					regions++
					c.walk(lit.Body, c.position(call.Pos()))
				}
			}
			return true
		})
	}
	if regions == 0 {
		t.Fatal("no transitions found")
	}
	return c
}

type clientChecker struct {
	fset    *token.FileSet
	info    *types.Info
	decls   map[string]*ast.FuncDecl
	visited map[*ast.FuncDecl]bool
	found   []string
	// cacheWrites are the writes of a cache map a transition makes.
	cacheWrites []string
}

func (c *clientChecker) callee(call *ast.CallExpr) string {
	if fn := (&lockChecker{info: c.info}).callee(call); fn != nil {
		return fn.Origin().FullName()
	}
	return ""
}

func (c *clientChecker) position(pos token.Pos) string {
	return (&lockChecker{fset: c.fset}).position(pos)
}

func (c *clientChecker) walk(node ast.Node, where string) {
	ast.Inspect(node, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.GoStmt:
			return false
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				if index, ok := ast.Unparen(lhs).(*ast.IndexExpr); ok && c.cacheMap(index.X) {
					c.cacheWrites = append(c.cacheWrites, fmt.Sprintf("%s: transition; %s writes the cache", where, c.position(n.Pos())))
				}
			}
		case *ast.CallExpr:
			if ident, ok := ast.Unparen(n.Fun).(*ast.Ident); ok && ident.Name == "delete" && len(n.Args) > 0 && c.cacheMap(n.Args[0]) {
				c.cacheWrites = append(c.cacheWrites, fmt.Sprintf("%s: transition; %s deletes from the cache", where, c.position(n.Pos())))
			}
			c.check(n, where)
		}
		return true
	})
}

// cacheMap reports whether expr is Manager.carriers or Manager.verdicts.
func (c *clientChecker) cacheMap(expr ast.Expr) bool {
	sel, ok := ast.Unparen(expr).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	field, ok := c.info.Uses[sel.Sel].(*types.Var)
	return ok && field.IsField() && field.Pkg() != nil && field.Pkg().Path() == attachedBackendsPkgPath &&
		(field.Name() == "carriers" || field.Name() == "verdicts")
}

func (c *clientChecker) check(call *ast.CallExpr, where string) {
	name := c.callee(call)
	if name == "" {
		if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
			if held, ok := transitionFunctionValues[sel.Sel.Name]; ok {
				name = attachedBackendsPkgPath + "." + held
			}
		}
	}
	if decl := c.decls[name]; decl != nil {
		if !c.visited[decl] {
			c.visited[decl] = true
			c.walk(decl.Body, where)
		}
		return
	}
	if strings.HasPrefix(name, "(*agent-overflow/internal/deviceclient.Client).") && !slices.Contains(lockFreeClientMethods, name) {
		c.found = append(c.found, fmt.Sprintf("%s: transition; %s calls %s", where, c.position(call.Pos()), name))
	}
}
