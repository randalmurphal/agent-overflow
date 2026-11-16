package app

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
	"strconv"
	"strings"
)

// driveGraph is a whole-program call graph over the module packages
// internal/app links, built from type-checked source so it follows calls
// across package boundaries, through interfaces and through function
// values. TestEveryThreadDrivingMethodIsListed walks it.
//
// Nodes are keys:
//
//   - a declared function or method: its types.Func full name;
//   - a function literal: "lit@" and its position;
//   - a function value's holder: "field:<struct>.<name>", "param:<func>#<i>",
//     "result:<func>" or "var@<position>". Its edges are every value stored
//     in it, so calling it reaches each of them;
//   - "via:<param>@<n>": one caller's call through a parameter the callee
//     calls, leading to the value that caller passed.
//
// An interface method's edges are its module implementations. A function
// value is reached only where something calls it: in place, through the
// holder it was stored in, or through a parameter it was passed for. A value
// handed to code outside the module, or kept where the graph does not model
// (a slice, a map, a channel), counts as called by the function that made it,
// so what the graph cannot follow it over-reports rather than misses.
type driveGraph struct {
	fset  *token.FileSet
	edges map[string][]string
	nodes map[string]bool
	// invokes marks the parameters a function calls, hands to a function
	// that does, or uses in a way the graph does not model. A function
	// value passed for one is called on behalf of the caller that passed
	// it, so the graph links that caller to it instead of merging every
	// caller's value into one parameter holder.
	invokes map[string]bool
	// passes is where a function hands a parameter on: "param:F#i" to the
	// "param:G#j" it is passed as.
	passes map[string][]string
	// sites are the function values passed to module functions, linked
	// once invokes is complete.
	sites []argSite
	// bound is every bound App method: name, //ao:scope and node key.
	bound []boundMethodNode
}

type boundMethodNode struct {
	name, scope, key string
}

type argSite struct {
	owner, param, value string
}

// listedDrivePackage is the part of `go list -json` the graph reads.
type listedDrivePackage struct {
	ImportPath      string
	Dir             string
	Export          string
	CompiledGoFiles []string
	Module          *struct{ Main bool }
	Error           *struct{ Err string }
}

const appPackagePath = "agent-overflow/internal/app"

// goBuildCache is the build cache go would use before TestMain detached the
// process home, so the graph's `go list -export` reads the cache the test
// build just filled instead of compiling every dependency again.
var goBuildCache string

func resolveGoBuildCache() string {
	if cache := os.Getenv("GOCACHE"); cache != "" {
		return cache
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "go-build")
}

// buildDriveGraph type-checks internal/app and every module package it
// links from source, against their imports' export data.
func buildDriveGraph() (*driveGraph, error) {
	cmd := exec.Command("go", "list", "-e", "-json", "-export", "-compiled", "-deps", "./internal/app")
	if goBuildCache != "" {
		cmd.Env = append(os.Environ(), "GOCACHE="+goBuildCache)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %v\n%s", err, stderr.String())
	}
	var pkgs []listedDrivePackage
	for dec := json.NewDecoder(bytes.NewReader(out)); ; {
		var pkg listedDrivePackage
		if err := dec.Decode(&pkg); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode go list: %w", err)
		}
		if pkg.Error != nil {
			return nil, fmt.Errorf("go list %s: %s", pkg.ImportPath, pkg.Error.Err)
		}
		pkgs = append(pkgs, pkg)
	}
	exports := make(map[string]string, len(pkgs))
	for _, pkg := range pkgs {
		exports[pkg.ImportPath] = pkg.Export
	}
	g := &driveGraph{
		fset: token.NewFileSet(), edges: map[string][]string{}, nodes: map[string]bool{},
		invokes: map[string]bool{}, passes: map[string][]string{},
	}
	imp := importer.ForCompiler(g.fset, "gc", func(path string) (io.ReadCloser, error) {
		file, ok := exports[path]
		if !ok || file == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(file)
	})
	checked := 0
	for _, pkg := range pkgs {
		if pkg.Module == nil || !pkg.Module.Main {
			continue
		}
		mode := parser.SkipObjectResolution
		if pkg.ImportPath == appPackagePath {
			mode |= parser.ParseComments
		}
		var files []*ast.File
		for _, name := range pkg.CompiledGoFiles {
			if !filepath.IsAbs(name) {
				name = filepath.Join(pkg.Dir, name)
			}
			file, err := parser.ParseFile(g.fset, name, nil, mode)
			if err != nil {
				return nil, err
			}
			files = append(files, file)
		}
		info := &types.Info{
			Types:      map[ast.Expr]types.TypeAndValue{},
			Uses:       map[*ast.Ident]types.Object{},
			Defs:       map[*ast.Ident]types.Object{},
			Selections: map[*ast.SelectorExpr]*types.Selection{},
		}
		conf := types.Config{Importer: imp, Sizes: types.SizesFor("gc", runtime.GOARCH)}
		checkedPkg, err := conf.Check(pkg.ImportPath, g.fset, files, info)
		if err != nil {
			return nil, fmt.Errorf("type-check %s: %w", pkg.ImportPath, err)
		}
		checked++
		w := &graphWalker{g: g, info: info, params: map[*types.Var]string{}, modeled: map[*ast.Ident]bool{}}
		for _, file := range files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				obj, ok := info.Defs[fn.Name].(*types.Func)
				if !ok {
					continue
				}
				key := funcNodeKey(obj)
				w.walkFunc(key, fn.Type, fn.Body)
				if pkg.ImportPath == appPackagePath && isAppMethod(fn) && fn.Name.IsExported() {
					if scope := boundScope(fn); scope != "" {
						g.bound = append(g.bound, boundMethodNode{name: fn.Name.Name, scope: scope, key: key})
					}
				}
			}
		}
		g.addImplementations(checkedPkg)
	}
	if checked < 50 {
		return nil, fmt.Errorf("type-checked %d packages; the listing is incomplete", checked)
	}
	g.resolveSites()
	return g, nil
}

// resolveSites closes invokes over passes, then links every passed function
// value: to the parameter holder it was stored as, and to the caller that
// passed it when the callee calls that parameter.
func (g *driveGraph) resolveSites() {
	for changed := true; changed; {
		changed = false
		for param, onto := range g.passes {
			if g.invokes[param] {
				continue
			}
			for _, next := range onto {
				if g.invokes[next] {
					g.invokes[param] = true
					changed = true
					break
				}
			}
		}
	}
	for i, site := range g.sites {
		g.edge(site.param, site.value)
		if g.invokes[site.param] {
			via := "via:" + site.param + "@" + strconv.Itoa(i)
			g.edge(site.owner, via)
			g.edge(via, site.value)
		}
	}
}

// viaParam is the parameter a "via:" node calls through, or empty.
func viaParam(node string) string {
	rest, ok := strings.CutPrefix(node, "via:")
	if !ok {
		return ""
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		return rest[:at]
	}
	return rest
}

func (g *driveGraph) edge(from, to string) {
	if from != "" && to != "" {
		g.edges[from] = append(g.edges[from], to)
		g.nodes[from], g.nodes[to] = true, true
	}
}

// known reports whether key is a node of the graph.
func (g *driveGraph) known(key string) bool {
	return g.nodes[key]
}

// addImplementations links each module interface method visible to pkg to
// pkg's methods that implement it.
func (g *driveGraph) addImplementations(pkg *types.Package) {
	var ifaces []*types.Named
	collect := func(scope *types.Scope) {
		for _, name := range scope.Names() {
			tn, ok := scope.Lookup(name).(*types.TypeName)
			if !ok || tn.IsAlias() {
				continue
			}
			named, ok := tn.Type().(*types.Named)
			if !ok || named.TypeParams().Len() > 0 {
				continue
			}
			if iface, ok := named.Underlying().(*types.Interface); ok && iface.NumMethods() > 0 {
				ifaces = append(ifaces, named)
			}
		}
	}
	collect(pkg.Scope())
	for _, imported := range pkg.Imports() {
		if strings.HasPrefix(imported.Path(), "agent-overflow/") {
			collect(imported.Scope())
		}
	}
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		tn, ok := scope.Lookup(name).(*types.TypeName)
		if !ok || tn.IsAlias() {
			continue
		}
		named, ok := tn.Type().(*types.Named)
		if !ok || named.TypeParams().Len() > 0 || types.IsInterface(named) {
			continue
		}
		ptr := types.NewPointer(named)
		for _, iface := range ifaces {
			underlying := iface.Underlying().(*types.Interface)
			if !types.Implements(ptr, underlying) {
				continue
			}
			for i := range underlying.NumMethods() {
				method := underlying.Method(i)
				impl, _, _ := types.LookupFieldOrMethod(ptr, true, pkg, method.Name())
				if fn, ok := impl.(*types.Func); ok {
					g.edge(funcNodeKey(method), funcNodeKey(fn))
				}
			}
		}
	}
}

// reach reports whether from reaches a node keep accepts, and the first path
// to it. stop names nodes the walk does not enter.
func (g *driveGraph) reach(from string, keep, stop func(string) bool) []string {
	seen := map[string]bool{}
	parent := map[string]string{}
	queue := []string{from}
	seen[from] = true
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		if node != from && keep(node) {
			var path []string
			for at := node; at != from; at = parent[at] {
				path = append([]string{at}, path...)
			}
			return path
		}
		if node != from && stop(node) {
			continue
		}
		for _, next := range g.edges[node] {
			if !seen[next] {
				seen[next] = true
				parent[next] = node
				queue = append(queue, next)
			}
		}
	}
	return nil
}

func funcNodeKey(fn *types.Func) string {
	return fn.Origin().FullName()
}

// graphWalker adds one package's edges.
type graphWalker struct {
	g    *driveGraph
	info *types.Info
	// params maps a parameter to its holder node.
	params map[*types.Var]string
	// modeled marks the parameter uses a handler below accounted for. Any
	// other use counts as a call.
	modeled map[*ast.Ident]bool
}

func (w *graphWalker) pos(p token.Pos) string {
	position := w.g.fset.Position(p)
	return position.Filename + ":" + strconv.Itoa(position.Line) + ":" + strconv.Itoa(position.Column)
}

// walkFunc adds the edges of one function body owned by key.
func (w *graphWalker) walkFunc(key string, ftype *ast.FuncType, body *ast.BlockStmt) {
	index := 0
	if ftype.Params != nil {
		for _, field := range ftype.Params.List {
			names := field.Names
			if len(names) == 0 {
				index++
				continue
			}
			for _, name := range names {
				if v, ok := w.info.Defs[name].(*types.Var); ok {
					w.params[v] = "param:" + key + "#" + strconv.Itoa(index)
				}
				index++
			}
		}
	}
	// handled marks the literals whose value flows somewhere the graph
	// models; the rest count as called by key.
	handled := map[*ast.FuncLit]bool{}
	ast.Inspect(body, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.FuncLit:
			lit := w.litKey(node)
			if !handled[node] {
				w.g.edge(key, lit)
			}
			w.walkFunc(lit, node.Type, node.Body)
			return false
		case *ast.CallExpr:
			w.call(key, node, handled)
		case *ast.CompositeLit:
			w.compositeLit(key, node, handled)
		case *ast.AssignStmt:
			if len(node.Lhs) == len(node.Rhs) {
				for i, lhs := range node.Lhs {
					w.store(key, w.holder(lhs), node.Rhs[i], handled)
				}
			}
		case *ast.ValueSpec:
			if len(node.Names) == len(node.Values) {
				for i, name := range node.Names {
					w.store(key, w.holder(name), node.Values[i], handled)
				}
			}
		case *ast.SendStmt:
			w.store(key, "", node.Value, handled)
		case *ast.ReturnStmt:
			for _, result := range node.Results {
				w.store(key, "result:"+key, result, handled)
			}
		case *ast.Ident:
			if v, ok := w.info.Uses[node].(*types.Var); ok && !w.modeled[node] {
				if param, ok := w.params[v]; ok {
					w.g.invokes[param] = true
				}
			}
		}
		return true
	})
}

func (w *graphWalker) litKey(lit *ast.FuncLit) string {
	return "lit@" + w.pos(lit.Pos())
}

// call adds the edge for one call and the flow of each function argument
// into the callee's parameter.
func (w *graphWalker) call(owner string, call *ast.CallExpr, handled map[*ast.FuncLit]bool) {
	if tv, ok := w.info.Types[call.Fun]; ok && tv.IsType() {
		return // a conversion
	}
	callee, fn := w.callee(call.Fun)
	if lit, ok := ast.Unparen(call.Fun).(*ast.FuncLit); ok {
		handled[lit] = true
	}
	if id, ok := ast.Unparen(call.Fun).(*ast.Ident); ok {
		w.modeled[id] = true
	}
	w.called(owner, callee)
	inModule := fn != nil && fn.Pkg() != nil && strings.HasPrefix(fn.Pkg().Path(), "agent-overflow/")
	sig, _ := w.info.TypeOf(call.Fun).Underlying().(*types.Signature)
	for i, arg := range call.Args {
		if !isFuncValued(w.info, arg) {
			continue
		}
		if !inModule || sig == nil {
			// Outside the module nothing models what the callee does
			// with it, so it is called here.
			w.called(owner, w.value(arg, handled))
			continue
		}
		param := i
		if sig.Variadic() && param >= sig.Params().Len()-1 {
			param = sig.Params().Len() - 1
		}
		target := "param:" + funcNodeKey(fn) + "#" + strconv.Itoa(param)
		value := w.value(arg, handled)
		if strings.HasPrefix(value, "param:") {
			w.g.passes[value] = append(w.g.passes[value], target)
		}
		w.g.sites = append(w.g.sites, argSite{owner: owner, param: target, value: value})
	}
}

// called records that owner calls node. A parameter is marked instead: each
// of the function's callers is linked to what it passed (resolveSites).
func (w *graphWalker) called(owner, node string) {
	if strings.HasPrefix(node, "param:") {
		w.g.invokes[node] = true
		return
	}
	w.g.edge(owner, node)
}

// callee is the node a call reaches, and the declared function when it names
// one.
func (w *graphWalker) callee(fun ast.Expr) (string, *types.Func) {
	switch fun := ast.Unparen(fun).(type) {
	case *ast.FuncLit:
		return w.litKey(fun), nil
	case *ast.Ident:
		return w.object(w.info.Uses[fun])
	case *ast.SelectorExpr:
		if sel, ok := w.info.Selections[fun]; ok {
			switch sel.Kind() {
			case types.FieldVal:
				return fieldKey(sel.Recv(), fun.Sel.Name), nil
			default:
				fn, _ := sel.Obj().(*types.Func)
				if fn == nil {
					return "", nil
				}
				return funcNodeKey(fn), fn
			}
		}
		return w.object(w.info.Uses[fun.Sel])
	case *ast.CallExpr:
		inner, _ := w.callee(fun.Fun)
		if inner == "" {
			return "", nil
		}
		return "result:" + inner, nil
	case *ast.IndexExpr:
		return w.callee(fun.X)
	case *ast.IndexListExpr:
		return w.callee(fun.X)
	}
	return "", nil
}

func (w *graphWalker) object(obj types.Object) (string, *types.Func) {
	switch obj := obj.(type) {
	case *types.Func:
		return funcNodeKey(obj), obj
	case *types.Var:
		return w.varKey(obj), nil
	}
	return "", nil
}

func (w *graphWalker) varKey(v *types.Var) string {
	if key, ok := w.params[v]; ok {
		return key
	}
	if v.Pkg() == nil {
		return ""
	}
	return "var@" + w.pos(v.Pos())
}

// holder is the node an assignment's left side stores into.
func (w *graphWalker) holder(lhs ast.Expr) string {
	switch lhs := ast.Unparen(lhs).(type) {
	case *ast.Ident:
		obj := w.info.Defs[lhs]
		if obj == nil {
			obj = w.info.Uses[lhs]
		}
		if v, ok := obj.(*types.Var); ok {
			return w.varKey(v)
		}
	case *ast.SelectorExpr:
		if sel, ok := w.info.Selections[lhs]; ok && sel.Kind() == types.FieldVal {
			return fieldKey(sel.Recv(), lhs.Sel.Name)
		}
		if v, ok := w.info.Uses[lhs.Sel].(*types.Var); ok {
			return w.varKey(v)
		}
	}
	return ""
}

// store records that value flows into holder. A value whose holder the graph
// does not model (a map entry, a slice element) counts as called by owner.
func (w *graphWalker) store(owner, holder string, value ast.Expr, handled map[*ast.FuncLit]bool) {
	if !isFuncValued(w.info, value) {
		return
	}
	if holder == "" {
		w.called(owner, w.value(value, handled))
		return
	}
	w.g.edge(holder, w.value(value, handled))
}

func (w *graphWalker) compositeLit(owner string, lit *ast.CompositeLit, handled map[*ast.FuncLit]bool) {
	typ := w.info.TypeOf(lit)
	if typ == nil {
		return
	}
	strct, ok := typ.Underlying().(*types.Struct)
	if !ok {
		// A slice or map of functions: each element counts as called by
		// owner.
		for _, elt := range lit.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				elt = kv.Value
			}
			w.store(owner, "", elt, handled)
		}
		return
	}
	for i, elt := range lit.Elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			if name, ok := kv.Key.(*ast.Ident); ok {
				w.store(owner, fieldKey(typ, name.Name), kv.Value, handled)
			}
			continue
		}
		if i < strct.NumFields() {
			w.store(owner, fieldKey(typ, strct.Field(i).Name()), elt, handled)
		}
	}
}

// value is the node a function-valued expression evaluates to.
func (w *graphWalker) value(expr ast.Expr, handled map[*ast.FuncLit]bool) string {
	if id, ok := ast.Unparen(expr).(*ast.Ident); ok {
		w.modeled[id] = true
	}
	switch expr := ast.Unparen(expr).(type) {
	case *ast.FuncLit:
		handled[expr] = true
		return w.litKey(expr)
	case *ast.CallExpr:
		if tv, ok := w.info.Types[expr.Fun]; ok && tv.IsType() && len(expr.Args) == 1 {
			return w.value(expr.Args[0], handled)
		}
		callee, _ := w.callee(expr.Fun)
		if callee == "" {
			return ""
		}
		return "result:" + callee
	default:
		key, _ := w.callee(expr)
		return key
	}
}

// fieldKey names a struct field's holder by the named struct it belongs to.
func fieldKey(recv types.Type, name string) string {
	for {
		ptr, ok := recv.(*types.Pointer)
		if !ok {
			break
		}
		recv = ptr.Elem()
	}
	if named, ok := recv.(*types.Named); ok {
		return "field:" + named.Origin().String() + "." + name
	}
	return "field:" + recv.String() + "." + name
}

func isFuncValued(info *types.Info, expr ast.Expr) bool {
	typ := info.TypeOf(expr)
	if typ == nil {
		return false
	}
	_, ok := typ.Underlying().(*types.Signature)
	return ok
}
