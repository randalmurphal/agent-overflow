package app

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// Go's untyped string constants are the hole the eventchan.Channel
// newtype cannot close on its own: `a.emit("provider:usage", x)` still
// compiles, because an untyped literal is assignable to any string type.
// The type stops a channel VARIABLE crossing without an explicit
// conversion; this test stops a literal.
//
// Together they are the guarantee: a Go emit site names a channel that
// internal/eventchan declares (and therefore that channelPolicies
// classifies — internal/transport's two cross-check tests pin that), or
// it spells eventchan.Channel(...) and is visibly an escape hatch.
//
// Scope is production sources only. Test files legitimately emit
// throwaway channel names ("ch1", "test:seq") to exercise the bus
// itself, and those are not emit sites anyone ships.

// eventEmitFuncNames are the call names whose FIRST argument is an event
// channel. Matched on the function name alone — `a.emit`, `h.app.emit`,
// `r.emit`, `e.emitter.Emit` and a bare `emit` all resolve to the same
// name — which is why unrelated Emit methods need the exemption below.
var eventEmitFuncNames = map[string]bool{
	"emit":      true,
	"emitEvent": true,
	"Emit":      true,
}

// nonChannelEmitters are packages whose `Emit` takes something other
// than an event channel, so their literals are not this test's business.
// Keyed by the path prefix a scanned file starts with.
var nonChannelEmitters = []string{
	// mcpstatus.StatusBus.Emit(ServerStatus) — an in-process fan-out
	// with no wire channel at all.
	"internal/mcpstatus/",
}

// TestEmitSitesNameAnEventChannelConstant walks every production Go
// source under the build's package roots and fails on an emit call whose
// channel argument is a bare string literal — or a named UNTYPED string
// constant from the same package, which is the same hole one indirection
// away: `const fooChannel = "provider:foo"` is assignable to
// eventchan.Channel without a conversion, so it evades the newtype AND
// the literal check, and it is exactly the pattern X4 deleted from four
// files (root/store lens finding 1, 2026-08-25). A TYPED constant
// (`const x = eventchan.Foo`, screenshot.InstallEventName) and a typed
// parameter/variable stay legitimate: the newtype already vouches for
// those.
//
// Known residual holes (verified absent today; do not over-trust the
// guard): a FUNCTION-LOCAL untyped const (the scan walks top-level
// decls only), a const built by string CONCATENATION (the scan requires
// a plain literal), and a CROSS-PACKAGE exported untyped const spelled
// as a SelectorExpr. Closing them needs go/types resolution; revisit if
// one ever appears.
func TestEmitSitesNameAnEventChannelConstant(t *testing.T) {
	fset := token.NewFileSet()
	var offenders []string

	// Pass 1: per-directory (= per-package) names of constants declared
	// as an untyped string literal. Only those are assignable to
	// eventchan.Channel without a conversion — `const x string = "…"`
	// would not compile at an emit site.
	untypedStringConsts := map[string]map[string]bool{}
	sources := collectGoSources(t)
	parsed := make(map[string]*ast.File, len(sources))
	for _, path := range sources {
		if strings.HasSuffix(path, "_test.go") || skipEmitScan(path) {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		parsed[path] = file
		dir := sourceDir(path)
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok || value.Type != nil {
					continue
				}
				for i, name := range value.Names {
					if i >= len(value.Values) {
						continue
					}
					if lit, ok := value.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if untypedStringConsts[dir] == nil {
							untypedStringConsts[dir] = map[string]bool{}
						}
						untypedStringConsts[dir][name.Name] = true
					}
				}
			}
		}
	}

	for path, file := range parsed {
		dir := sourceDir(path)
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			if !eventEmitFuncNames[emitCallName(call.Fun)] {
				return true
			}
			switch arg := call.Args[0].(type) {
			case *ast.BasicLit:
				if arg.Kind == token.STRING {
					offenders = append(offenders, fmt.Sprintf("%s: %s(%s, …)",
						fset.Position(call.Pos()), emitCallName(call.Fun), arg.Value))
				}
			case *ast.Ident:
				if untypedStringConsts[dir][arg.Name] {
					offenders = append(offenders, fmt.Sprintf("%s: %s(%s, …) — %s is an untyped string constant",
						fset.Position(call.Pos()), emitCallName(call.Fun), arg.Name, arg.Name))
				}
			}
			return true
		})
	}

	if len(offenders) == 0 {
		return
	}
	sort.Strings(offenders)
	t.Errorf("%d emit site(s) name a channel as a bare string literal:\n  %s\n\n"+
		"Use the internal/eventchan constant instead. If the channel is genuinely "+
		"caller-named (the harness escape hatches), spell eventchan.Channel(name) so "+
		"the fail-closed loopback-only default is a visible choice.",
		len(offenders), strings.Join(offenders, "\n  "))
}

// sourceDir is the per-package grouping key for the untyped-const scan:
// one directory is one package in this repo.
func sourceDir(path string) string {
	normalized := strings.ReplaceAll(path, "\\", "/")
	if i := strings.LastIndex(normalized, "/"); i >= 0 {
		return normalized[:i]
	}
	return "."
}

func skipEmitScan(path string) bool {
	normalized := strings.ReplaceAll(path, "\\", "/")
	for _, prefix := range nonChannelEmitters {
		if strings.HasPrefix(normalized, prefix) {
			return true
		}
	}
	return false
}

// emitCallName returns the called function's own name, ignoring whatever
// it hangs off: `emit`, `a.emit`, and `e.emitter.Emit` all yield the
// trailing identifier.
func emitCallName(fun ast.Expr) string {
	switch typed := fun.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.SelectorExpr:
		return typed.Sel.Name
	default:
		return ""
	}
}
