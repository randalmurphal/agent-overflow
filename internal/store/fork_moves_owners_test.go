package store

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// forkMoveReporters commit a transaction and report the fork moves it
// recorded (fork_moves.go).
var forkMoveReporters = map[string]bool{
	"Store.commitReportingForks":     true,
	"Store.writeItemsReportingForks": true,
}

// forkMoveReportingWrappers names, for a transaction helper that commits
// without reporting, the one reporter allowed to hand it a callback that
// records fork moves.
var forkMoveReportingWrappers = map[string]string{
	"Store.writeItems": "Store.writeItemsReportingForks",
}

// TestForkMoveOwnersReport: every transaction that can record a fork move
// is committed through a reporter, so the forks it moved are told. The
// check reads the package source and builds the call graph of its
// functions and methods. A function records in its caller's transaction
// when it takes the transaction (a *sql.Tx, sqlExecutor or sqlQueryer
// parameter, or a receiver holding one) and calls recordForkMovesTx,
// recordForkReadersTx or such a function. The check fails
//   - a function that commits directly (a Commit call) and runs one of them,
//   - a function that opens a transaction, runs one of them, and does not
//     commit it with commitReportingForks and defer dropForkMovesTx,
//   - a call that hands a callback running one of them to a helper that
//     runs the callback in a transaction it commits without reporting.
//
// TestMain's check sees only the paths tests run; this one sees every path
// in the source.
func TestForkMoveOwnersReport(t *testing.T) {
	g := loadForkMoveGraph(t)
	recording := g.recordingTxFuncs()
	for _, name := range []string{"handOffCopyTx", "handOffIDsTx", "bumpPayloadReadersTx", "bumpHistoryRevForItemTx", "Store.detachForkDescendantsTx", "readMutableSubagentRowTx"} {
		if !recording[name] {
			t.Fatalf("the call graph does not find %s recording fork moves", name)
		}
	}
	reaches := func(refs map[string]bool) bool {
		for name := range refs {
			if recording[name] {
				return true
			}
		}
		return false
	}
	var owners, violations []string
	for _, fn := range g.funcs {
		if forkMoveReporters[fn.name] || !reaches(fn.refs) {
			continue
		}
		if fn.commits {
			violations = append(violations, fn.name+" commits a transaction that records fork moves without commitReportingForks")
		}
		if fn.begins {
			owners = append(owners, fn.name)
			if !fn.refs["Store.commitReportingForks"] || !fn.defersDrop {
				violations = append(violations, fn.name+" opens a transaction that records fork moves without committing it through commitReportingForks and deferring dropForkMovesTx")
			}
		}
	}
	unreported := g.unreportingCallbackRunners()
	for _, fn := range g.funcs {
		for _, call := range fn.callbackCalls {
			if !unreported[call.callee] || !reaches(call.argRefs) || forkMoveReportingWrappers[call.callee] == fn.name {
				continue
			}
			violations = append(violations, fn.name+" hands "+call.callee+" a callback that records fork moves; "+call.callee+" commits it without reporting them")
		}
	}
	for _, name := range []string{"Store.DeleteEmptyDraftThread", "Store.UpdatePayloadSpans", "Store.detachForkDescendants"} {
		if !slices.Contains(owners, name) {
			t.Fatalf("owners found %v, missing %s", owners, name)
		}
	}
	if !unreported["Store.writeItems"] || !unreported["Store.cardTxLocked"] {
		t.Fatalf("callback runners found %v, missing writeItems or cardTxLocked", unreported)
	}
	slices.Sort(violations)
	for _, v := range slices.Compact(violations) {
		t.Error(v)
	}
}

// forkMoveFunc is one function or method of the package as the check
// reads it.
type forkMoveFunc struct {
	name string
	// txBound: it runs in a caller's transaction.
	txBound bool
	// refs: the package functions its body calls or names, closures
	// included.
	refs map[string]bool
	// commits: it calls Commit. begins: it opens a transaction.
	commits, begins bool
	defersDrop      bool
	funcParams      map[string]bool
	callbackCalls   []forkMoveCallbackCall
}

// forkMoveCallbackCall is a call that passes a function: the package
// functions the passed function calls or names, and whether it runs one of
// the caller's own function parameters.
type forkMoveCallbackCall struct {
	callee          string
	argRefs         map[string]bool
	passesOwnParams bool
}

type forkMoveGraph struct {
	funcs []*forkMoveFunc
}

// forkMoveStubImporter answers every import with an empty package: the
// check resolves the package's own identifiers, and a call into another
// package is not an edge.
type forkMoveStubImporter struct{}

func (forkMoveStubImporter) Import(path string) (*types.Package, error) {
	pkg := types.NewPackage(path, path[strings.LastIndex(path, "/")+1:])
	pkg.MarkComplete()
	return pkg, nil
}

func loadForkMoveGraph(t *testing.T) *forkMoveGraph {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		ok, err := build.Default.MatchFile(".", name)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}}
	// Every reference to another package fails to resolve; the check needs
	// only the package's own.
	conf := types.Config{Importer: forkMoveStubImporter{}, Error: func(error) {}}
	pkg, _ := conf.Check("agent-overflow/internal/store", fset, files, info)

	// The struct types that hold a transaction, and the functions that
	// return one.
	txStructs, txOpeners := map[string]bool{}, map[string]bool{}
	for _, file := range files {
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					if st, ok := ts.Type.(*ast.StructType); ok {
						for _, field := range st.Fields.List {
							if forkMoveTxType(field.Type) {
								txStructs[ts.Name.Name] = true
							}
						}
					}
				}
			case *ast.FuncDecl:
				if d.Type.Results == nil {
					continue
				}
				for _, field := range d.Type.Results.List {
					if types.ExprString(field.Type) == "*sql.Tx" {
						if obj, ok := info.Defs[d.Name].(*types.Func); ok {
							txOpeners[forkMoveFuncKey(obj)] = true
						}
					}
				}
			}
		}
	}

	packageFunc := func(id *ast.Ident) (string, bool) {
		fn, ok := info.Uses[id].(*types.Func)
		if !ok || fn.Pkg() != pkg {
			return "", false
		}
		return forkMoveFuncKey(fn), true
	}
	refsOf := func(n ast.Node) map[string]bool {
		refs := map[string]bool{}
		ast.Inspect(n, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				if name, ok := packageFunc(id); ok {
					refs[name] = true
				}
			}
			return true
		})
		return refs
	}

	g := &forkMoveGraph{}
	for _, file := range files {
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			obj, ok := info.Defs[fd.Name].(*types.Func)
			if !ok {
				t.Fatalf("no function object for %s", fd.Name.Name)
			}
			fn := &forkMoveFunc{name: forkMoveFuncKey(obj), refs: refsOf(fd.Body), funcParams: map[string]bool{}}
			if fd.Recv != nil {
				for _, field := range fd.Recv.List {
					if txStructs[forkMoveBaseTypeName(field.Type)] {
						fn.txBound = true
					}
				}
			}
			for _, field := range fd.Type.Params.List {
				if forkMoveTxType(field.Type) {
					fn.txBound = true
				}
				if _, ok := field.Type.(*ast.FuncType); ok {
					for _, name := range field.Names {
						fn.funcParams[name.Name] = true
					}
				}
			}
			runsOwnParams := func(n ast.Node) bool {
				found := false
				ast.Inspect(n, func(n ast.Node) bool {
					if id, ok := n.(*ast.Ident); ok && fn.funcParams[id.Name] {
						found = true
					}
					return !found
				})
				return found
			}
			// A closure held in a local variable before it is passed.
			locals := map[types.Object]*ast.FuncLit{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if as, ok := n.(*ast.AssignStmt); ok && len(as.Lhs) == len(as.Rhs) {
					for i, rhs := range as.Rhs {
						lit, ok := rhs.(*ast.FuncLit)
						id, isIdent := as.Lhs[i].(*ast.Ident)
						if !ok || !isIdent {
							continue
						}
						if obj := info.Defs[id]; obj != nil {
							locals[obj] = lit
						} else if obj := info.Uses[id]; obj != nil {
							locals[obj] = lit
						}
					}
				}
				return true
			})
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.DeferStmt:
					if id, ok := n.Call.Fun.(*ast.Ident); ok && id.Name == "dropForkMovesTx" {
						fn.defersDrop = true
					}
				case *ast.CallExpr:
					callee := ""
					switch f := n.Fun.(type) {
					case *ast.Ident:
						callee, _ = packageFunc(f)
					case *ast.SelectorExpr:
						callee, _ = packageFunc(f.Sel)
						if callee == "" {
							switch f.Sel.Name {
							case "Commit":
								fn.commits = fn.commits || len(n.Args) == 0
							case "Begin", "BeginTx":
								fn.begins = true
							}
						}
					}
					if callee == "" {
						return true
					}
					if txOpeners[callee] {
						fn.begins = true
					}
					for _, arg := range n.Args {
						call := forkMoveCallbackCall{callee: callee}
						switch a := arg.(type) {
						case *ast.FuncLit:
							call.argRefs, call.passesOwnParams = refsOf(a.Body), runsOwnParams(a.Body)
						case *ast.Ident:
							if fn.funcParams[a.Name] {
								call.passesOwnParams = true
							} else if name, ok := packageFunc(a); ok {
								call.argRefs = map[string]bool{name: true}
							} else if lit := locals[info.Uses[a]]; lit != nil {
								call.argRefs, call.passesOwnParams = refsOf(lit.Body), runsOwnParams(lit.Body)
							} else {
								continue
							}
						case *ast.SelectorExpr:
							name, ok := packageFunc(a.Sel)
							if !ok {
								continue
							}
							call.argRefs = map[string]bool{name: true}
						default:
							continue
						}
						fn.callbackCalls = append(fn.callbackCalls, call)
					}
				}
				return true
			})
			g.funcs = append(g.funcs, fn)
		}
	}
	return g
}

// forkMoveTxType reports a parameter or field that carries a transaction.
func forkMoveTxType(expr ast.Expr) bool {
	switch types.ExprString(expr) {
	case "*sql.Tx", "sqlExecutor", "sqlQueryer":
		return true
	}
	return false
}

// forkMoveFuncKey names a function, or a method by its receiver type and
// name, so methods of one name on two types stay two nodes.
func forkMoveFuncKey(fn *types.Func) string {
	recv := fn.Type().(*types.Signature).Recv()
	if recv == nil {
		return fn.Name()
	}
	typ := recv.Type()
	if ptr, ok := typ.(*types.Pointer); ok {
		typ = ptr.Elem()
	}
	if named, ok := typ.(*types.Named); ok {
		return named.Obj().Name() + "." + fn.Name()
	}
	return fn.Name()
}

func forkMoveBaseTypeName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if id, ok := expr.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// recordingTxFuncs is every function that records a fork move in its
// caller's transaction: the recorders, and each transaction-bound function
// that calls or names one of them.
func (g *forkMoveGraph) recordingTxFuncs() map[string]bool {
	recording := map[string]bool{"recordForkMovesTx": true, "recordForkReadersTx": true}
	for changed := true; changed; {
		changed = false
		for _, fn := range g.funcs {
			if recording[fn.name] || !fn.txBound {
				continue
			}
			for name := range fn.refs {
				if recording[name] {
					recording[fn.name] = true
					changed = true
					break
				}
			}
		}
	}
	return recording
}

// unreportingCallbackRunners is every helper that runs a callback in a
// transaction it opens or commits without reporting, directly or by
// passing the callback on to such a helper. The reporters are not.
func (g *forkMoveGraph) unreportingCallbackRunners() map[string]bool {
	unreported := map[string]bool{}
	for _, fn := range g.funcs {
		if len(fn.funcParams) > 0 && (fn.commits || fn.begins) && !forkMoveReporters[fn.name] {
			unreported[fn.name] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for _, fn := range g.funcs {
			if unreported[fn.name] || forkMoveReporters[fn.name] || len(fn.funcParams) == 0 {
				continue
			}
			for _, call := range fn.callbackCalls {
				if call.passesOwnParams && unreported[call.callee] {
					unreported[fn.name] = true
					changed = true
					break
				}
			}
		}
	}
	return unreported
}
