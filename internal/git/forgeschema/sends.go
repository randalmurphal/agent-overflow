package forgeschema

import (
	"errors"
	"fmt"
	goast "go/ast"
	goparser "go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

// Send is one operation and document one call site can send.
type Send struct {
	Site, Operation string
	// Name is the document's constant or table row; Forge is its prefix.
	Name, Forge, Text string
}

// Sends finds every call of forgeapi.Client.GraphQL in dir's non-test
// source and traces its operation and document arguments back through
// local assignments, parameters (to every call of the function, a
// function literal's included) and range loops over a table to
// package-level constants. tables are the package-level tables a send
// ranges over, by name, with their rows, whose fields are read by the
// selector's name; a listed table with nil rows is accounted for but
// sends nothing here. A document's forge is its constant's or table's
// name prefix (github, gitlab): a client reaches a send through helpers'
// parameters and returns, so the call site does not name its forge. A
// call site whose arguments cannot be traced is an error naming it.
// sites is the number of call sites.
func Sends(dir string, tables map[string]any) (sends []Send, sites int, err error) {
	src, err := parseGraphQLSource(dir)
	if err != nil {
		return nil, 0, err
	}
	var problems []string
	for _, call := range src.sends {
		site := src.fset.Position(call.call.Pos()).String()
		tuples, err := src.resolve([]elem{{expr: call.call.Args[1]}, {expr: call.call.Args[2]}}, call.scope, tables, 0)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", site, err))
			continue
		}
		for _, tuple := range tuples {
			op, doc := tuple[0], tuple[1]
			forge := doc.name[:min(len(doc.name), len("github"))]
			if forge != "github" && forge != "gitlab" {
				problems = append(problems, fmt.Sprintf("%s: the document %q names no forge: send a package-level constant whose name starts with github or gitlab", site, cmpOr(doc.name, doc.text)))
				continue
			}
			sends = append(sends, Send{Site: site, Operation: op.text, Name: doc.name, Forge: forge, Text: doc.text})
		}
	}
	if len(problems) > 0 {
		return nil, 0, errors.New(strings.Join(problems, "\n"))
	}
	return sends, len(src.sends), nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// graphQLSource is a package's parsed source for tracing sends.
type graphQLSource struct {
	fset   *token.FileSet
	values map[string]goast.Expr        // package-level const and var values
	funcs  map[string][]*goast.FuncDecl // functions and methods, by name
	// calls are every call in the package by the called name, with the
	// scope it sits in; sends are the GraphQL calls among them.
	calls map[string][]scopedCall
	sends []scopedCall
	// values used other than called, by name.
	notCalled map[string]bool
}

// scope is a function body: a declared function's or a function
// literal's, inside parent.
type scope struct {
	params []string
	body   *goast.BlockStmt
	parent *scope
	// name is a declared function's name or the local a literal is
	// assigned to; method marks a declared method.
	name   string
	method bool
}

type scopedCall struct {
	call  *goast.CallExpr
	scope *scope
}

// value is a resolved string: a package constant's name and text, or a
// literal's text, or a table row.
type value struct{ name, text string }

// elem is one argument being traced: an expression still to resolve, or
// its value.
type elem struct {
	expr goast.Expr
	val  *value
}

func parseGraphQLSource(dir string) (*graphQLSource, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	src := &graphQLSource{fset: token.NewFileSet(), values: map[string]goast.Expr{}, funcs: map[string][]*goast.FuncDecl{}, calls: map[string][]scopedCall{}, notCalled: map[string]bool{}}
	var parsed []*goast.File
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := goparser.ParseFile(src.fset, name, nil, goparser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		parsed = append(parsed, file)
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *goast.GenDecl:
				for _, spec := range decl.Specs {
					if vs, ok := spec.(*goast.ValueSpec); ok && len(vs.Names) == len(vs.Values) {
						for i, n := range vs.Names {
							src.values[n.Name] = vs.Values[i]
						}
					}
				}
			case *goast.FuncDecl:
				src.funcs[decl.Name.Name] = append(src.funcs[decl.Name.Name], decl)
			}
		}
	}
	if len(src.funcs["GraphQL"]) > 0 {
		return nil, errors.New("the package declares a GraphQL function: a call of it would read as a forge send")
	}
	for _, file := range parsed {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*goast.FuncDecl); ok && fn.Body != nil {
				src.walk(fn.Body, &scope{params: paramNames(fn.Type), body: fn.Body, name: fn.Name.Name, method: fn.Recv != nil})
			}
		}
		// A function named in a package-level value is used other than
		// called.
		for _, decl := range file.Decls {
			if gen, ok := decl.(*goast.GenDecl); ok {
				goast.Inspect(gen, func(n goast.Node) bool {
					if id, ok := n.(*goast.Ident); ok {
						src.notCalled[id.Name] = true
					}
					return true
				})
			}
		}
	}
	return src, nil
}

func paramNames(ft *goast.FuncType) []string {
	var out []string
	for _, field := range ft.Params.List {
		if len(field.Names) == 0 {
			out = append(out, "_")
		}
		for _, n := range field.Names {
			out = append(out, n.Name)
		}
	}
	return out
}

// walk records the calls in body, entering each function literal as a
// scope of its own, named for the local it is assigned to.
func (src *graphQLSource) walk(body goast.Node, s *scope) {
	bound := map[*goast.FuncLit]string{}
	called := map[goast.Expr]bool{}
	goast.Inspect(body, func(n goast.Node) bool {
		switch n := n.(type) {
		case *goast.AssignStmt:
			for i, rhs := range n.Rhs {
				if lit, ok := rhs.(*goast.FuncLit); ok && len(n.Lhs) == len(n.Rhs) {
					if id, ok := n.Lhs[i].(*goast.Ident); ok {
						bound[lit] = id.Name
					}
				}
			}
		case *goast.FuncLit:
			src.walk(n.Body, &scope{params: paramNames(n.Type), body: n.Body, parent: s, name: bound[n]})
			return false
		case *goast.CallExpr:
			fun := n.Fun
			switch f := fun.(type) {
			case *goast.IndexExpr:
				fun = f.X
			case *goast.IndexListExpr:
				fun = f.X
			}
			called[fun] = true
			switch f := fun.(type) {
			case *goast.Ident:
				src.calls[f.Name] = append(src.calls[f.Name], scopedCall{n, s})
			case *goast.SelectorExpr:
				called[f.Sel] = true
				src.calls[f.Sel.Name] = append(src.calls[f.Sel.Name], scopedCall{n, s})
				if f.Sel.Name == "GraphQL" && len(n.Args) == 5 {
					src.sends = append(src.sends, scopedCall{n, s})
				}
			}
		case *goast.Ident:
			if !called[n] {
				src.notCalled[n.Name] = true
			}
		}
		return true
	})
}

// definitions finds where name, used at pos, is defined in s or the
// scopes around it: as a parameter of the scope it returns, or by the
// statements of that scope that assign it, a range loop around pos
// standing alone. Neither means a package-level name.
func definitions(s *scope, name string, pos token.Pos) (owner *scope, param int, defs []goast.Node) {
	for ; s != nil; s = s.parent {
		if i := slices.Index(s.params, name); i >= 0 {
			return s, i, nil
		}
		var loop *goast.RangeStmt
		goast.Inspect(s.body, func(n goast.Node) bool {
			switch n := n.(type) {
			case *goast.FuncLit:
				return false
			case *goast.AssignStmt:
				for _, lhs := range n.Lhs {
					if id, ok := lhs.(*goast.Ident); ok && id.Name == name {
						defs = append(defs, n)
					}
				}
			case *goast.ValueSpec:
				for _, id := range n.Names {
					if id.Name == name {
						defs = append(defs, n)
					}
				}
			case *goast.RangeStmt:
				for _, e := range []goast.Expr{n.Key, n.Value} {
					if id, ok := e.(*goast.Ident); ok && id.Name == name && n.Body.Pos() <= pos && pos < n.Body.End() {
						loop = n
					}
				}
			}
			return true
		})
		if loop != nil {
			return s, -1, []goast.Node{loop}
		}
		if len(defs) > 0 {
			return s, -1, defs
		}
	}
	return nil, -1, nil
}

// fold evaluates a package-level constant expression: string literals
// and package-level names of such joined by +.
func (src *graphQLSource) fold(e goast.Expr, depth int) (string, bool) {
	if depth > 64 {
		return "", false
	}
	switch e := e.(type) {
	case *goast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		return s, err == nil
	case *goast.ParenExpr:
		return src.fold(e.X, depth+1)
	case *goast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		x, ok := src.fold(e.X, depth+1)
		if !ok {
			return "", false
		}
		y, ok := src.fold(e.Y, depth+1)
		return x + y, ok
	case *goast.Ident:
		if v, ok := src.values[e.Name]; ok {
			return src.fold(v, depth+1)
		}
	}
	return "", false
}

// constant resolves e when it names no local: a literal, or a package
// constant (named by it).
func (src *graphQLSource) constant(e goast.Expr, s *scope) (*value, bool) {
	if id, ok := e.(*goast.Ident); ok {
		if owner, _, _ := definitions(s, id.Name, id.Pos()); owner != nil {
			return nil, false
		}
		text, ok := src.fold(id, 0)
		return &value{name: id.Name, text: text}, ok
	}
	if text, ok := src.fold(e, 0); ok && !mentionsIdent(e) {
		return &value{text: text}, true
	}
	return nil, false
}

func mentionsIdent(e goast.Expr) bool {
	found := false
	goast.Inspect(e, func(n goast.Node) bool {
		_, isIdent := n.(*goast.Ident)
		found = found || isIdent
		return !found
	})
	return found
}

// resolve traces elems, arguments evaluated together in s, to every
// tuple of values they can hold.
func (src *graphQLSource) resolve(elems []elem, s *scope, tables map[string]any, depth int) ([][]value, error) {
	if depth > 32 {
		return nil, errors.New("the arguments trace through more than 32 steps")
	}
	var open []int
	for i := range elems {
		if elems[i].val != nil {
			continue
		}
		if v, ok := src.constant(elems[i].expr, s); ok {
			elems[i] = elem{val: v}
			continue
		}
		open = append(open, i)
	}
	if len(open) == 0 {
		tuple := make([]value, len(elems))
		for i, e := range elems {
			tuple[i] = *e.val
		}
		return [][]value{tuple}, nil
	}
	describe := func(e goast.Expr) string { return source(src.fset, e) }
	// Fields of one range variable over a listed table.
	if sel, ok := elems[open[0]].expr.(*goast.SelectorExpr); ok {
		x, ok := sel.X.(*goast.Ident)
		if !ok {
			return nil, fmt.Errorf("cannot trace %s", describe(sel))
		}
		fields := map[int]string{}
		for _, i := range open {
			other, ok := elems[i].expr.(*goast.SelectorExpr)
			if id, isIdent := other.X.(*goast.Ident); !ok || !isIdent || id.Name != x.Name {
				return nil, fmt.Errorf("cannot trace %s beside %s", describe(elems[i].expr), describe(sel))
			}
			fields[i] = other.Sel.Name
		}
		_, _, defs := definitions(s, x.Name, x.Pos())
		if len(defs) != 1 {
			return nil, fmt.Errorf("cannot trace %s: %s is not one range variable", describe(sel), x.Name)
		}
		loop, ok := defs[0].(*goast.RangeStmt)
		table, isIdent := loop.X.(*goast.Ident)
		if !ok || !isIdent || loop.Value == nil || loop.Value.(*goast.Ident).Name != x.Name {
			return nil, fmt.Errorf("cannot trace %s: %s is not the value of a range over a named table", describe(sel), x.Name)
		}
		rows, listed := tables[table.Name]
		if !listed {
			return nil, fmt.Errorf("cannot trace %s: list the table %s", describe(sel), table.Name)
		}
		if rows == nil {
			return nil, nil
		}
		rv := reflect.ValueOf(rows)
		var out [][]value
		for r := range rv.Len() {
			tuple := make([]value, len(elems))
			for i, e := range elems {
				if e.val != nil {
					tuple[i] = *e.val
					continue
				}
				field := rv.Index(r).FieldByName(fields[i])
				if field.Kind() != reflect.String {
					return nil, fmt.Errorf("%s[%d].%s is not a string", table.Name, r, fields[i])
				}
				tuple[i] = value{name: fmt.Sprintf("%s[%d]", table.Name, r), text: field.String()}
			}
			out = append(out, tuple)
		}
		return out, nil
	}
	// Locals or parameters, all of one scope.
	names := map[int]string{}
	var owner *scope
	param := map[int]int{}
	var defs []goast.Node
	for n, i := range open {
		id, ok := elems[i].expr.(*goast.Ident)
		if !ok {
			return nil, fmt.Errorf("cannot trace %s", describe(elems[i].expr))
		}
		o, p, d := definitions(s, id.Name, id.Pos())
		if o == nil {
			return nil, fmt.Errorf("cannot trace %s: a package-level value that is no constant", id.Name)
		}
		if n > 0 && (o != owner || (p >= 0) != (len(param) > 0)) {
			return nil, fmt.Errorf("cannot trace %s and %s together: define them in the same statement or parameter list", names[open[0]], id.Name)
		}
		owner, names[i] = o, id.Name
		if p >= 0 {
			param[i] = p
		}
		if n == 0 {
			defs = d
		}
	}
	var out [][]value
	if len(param) > 0 {
		callers, err := src.callersOf(owner)
		if err != nil {
			return nil, err
		}
		for _, c := range callers {
			next := slices.Clone(elems)
			for i, p := range param {
				if p >= len(c.call.Args) {
					return nil, fmt.Errorf("%s: a call of %s passes no argument %d", src.fset.Position(c.call.Pos()), owner.name, p)
				}
				next[i] = elem{expr: c.call.Args[p]}
			}
			tuples, err := src.resolve(next, c.scope, tables, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, tuples...)
		}
		return out, nil
	}
	for _, def := range defs {
		var lhs []*goast.Ident
		var rhs []goast.Expr
		switch d := def.(type) {
		case *goast.AssignStmt:
			for _, e := range d.Lhs {
				id, _ := e.(*goast.Ident)
				lhs = append(lhs, id)
			}
			rhs = d.Rhs
		case *goast.ValueSpec:
			lhs, rhs = d.Names, d.Values
		default:
			return nil, fmt.Errorf("cannot trace %s: it is a range key or value", names[open[0]])
		}
		if len(lhs) != len(rhs) {
			return nil, fmt.Errorf("cannot trace %s: %s assigns it a call's result", names[open[0]], src.fset.Position(def.Pos()))
		}
		next := slices.Clone(elems)
		for _, i := range open {
			at := slices.IndexFunc(lhs, func(id *goast.Ident) bool { return id != nil && id.Name == names[i] })
			if at < 0 {
				return nil, fmt.Errorf("%s assigns %s apart from %s: assign them together", src.fset.Position(def.Pos()), names[open[0]], names[i])
			}
			next[i] = elem{expr: rhs[at]}
		}
		tuples, err := src.resolve(next, owner, tables, depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, tuples...)
	}
	return out, nil
}

// callersOf is every call of the function s is the body of. A function
// also used other than called cannot be traced.
func (src *graphQLSource) callersOf(s *scope) ([]scopedCall, error) {
	if s.name == "" {
		return nil, errors.New("cannot trace a parameter of a function literal not assigned to a local")
	}
	if s.parent == nil && src.notCalled[s.name] {
		return nil, fmt.Errorf("cannot trace the parameters of %s: it is used other than called", s.name)
	}
	var out []scopedCall
	for _, c := range src.calls[s.name] {
		_, isMethodCall := c.call.Fun.(*goast.SelectorExpr)
		if s.parent != nil {
			// A literal's calls are inside the scope that assigns it.
			if inside(c.scope, s.parent) && !isMethodCall {
				out = append(out, c)
			}
			continue
		}
		if isMethodCall == s.method {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("cannot trace the parameters of %s: nothing calls it", s.name)
	}
	return out, nil
}

func inside(s, outer *scope) bool {
	for ; s != nil; s = s.parent {
		if s == outer {
			return true
		}
	}
	return false
}

// source prints e for a message.
func source(fset *token.FileSet, e goast.Expr) string {
	var b strings.Builder
	if err := printer.Fprint(&b, fset, e); err != nil {
		return fmt.Sprintf("%T", e)
	}
	return b.String()
}

// OperationName is the name of the one operation document defines, or a
// description of why there is none.
func OperationName(document string) string {
	doc, err := parser.ParseQuery(&ast.Source{Input: document})
	if err != nil {
		return "unparsable: " + err.Error()
	}
	if len(doc.Operations) != 1 {
		return fmt.Sprintf("%d operations", len(doc.Operations))
	}
	return doc.Operations[0].Name
}
