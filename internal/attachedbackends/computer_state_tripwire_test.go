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

// TestOnlyTheComputerTransactionWritesItsState holds what the types already
// arrange (computer_state.go): a computerTx is built only by transact, the
// two shared files are written only by computerTx's methods, and only
// transact holds a session file.
func TestOnlyTheComputerTransactionWritesItsState(t *testing.T) {
	t.Parallel()
	reporoot.ObserveGoSources(t)
	_, fset, files, info := typeCheckAttachedBackends(t)
	pos := func(p token.Pos) string { return (&lockChecker{fset: fset}).position(p) }
	shared := map[string]bool{agentAccessFile: true, ownExclusionsFile: true}
	var found []string
	writers := 0
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
				ast.Inspect(decl.Body, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.BasicLit:
						if text, err := strconv.Unquote(n.Value); n.Kind == token.STRING && err == nil && shared[text] {
							found = append(found, pos(n.Pos())+": spell the shared file by its constant")
						}
					case *ast.CompositeLit:
						if ident, ok := n.Type.(*ast.Ident); ok && ident.Name == "computerTx" && name != "transact" {
							found = append(found, pos(n.Pos())+": only transact builds a computerTx")
						}
					case *ast.CallExpr:
						fn := (&lockChecker{info: info}).callee(n)
						if fn == nil {
							return true
						}
						switch fn.Origin().FullName() {
						case "agent-overflow/internal/deviceclient.WithSession":
							if name != "transact" {
								found = append(found, pos(n.Pos())+": only transact holds a session file")
							}
						case "agent-overflow/internal/atomicfile.WriteJSON":
							if writesShared(n, info) {
								writers++
								if !onTx {
									found = append(found, pos(n.Pos())+": only computerTx's methods write the shared files")
								}
							}
						}
					}
					return true
				})
			}
		}
	}
	if writers != 2 {
		found = append(found, fmt.Sprintf("found %d writes of the shared files; want the two computerTx methods", writers))
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

// writesShared reports whether a WriteJSON call's path names one of the
// shared files' constants.
func writesShared(call *ast.CallExpr, info *types.Info) bool {
	if len(call.Args) == 0 {
		return false
	}
	named := false
	ast.Inspect(call.Args[0], func(n ast.Node) bool {
		if ident, ok := n.(*ast.Ident); ok {
			if obj, ok := info.Uses[ident].(*types.Const); ok && (obj.Name() == "agentAccessFile" || obj.Name() == "ownExclusionsFile") {
				named = true
			}
		}
		return true
	})
	return named
}
