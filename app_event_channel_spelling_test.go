package main

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
// channel argument is a bare string literal.
func TestEmitSitesNameAnEventChannelConstant(t *testing.T) {
	fset := token.NewFileSet()
	var offenders []string

	for _, path := range collectGoSources(t) {
		if strings.HasSuffix(path, "_test.go") || skipEmitScan(path) {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			if !eventEmitFuncNames[emitCallName(call.Fun)] {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			offenders = append(offenders, fmt.Sprintf("%s: %s(%s, …)",
				fset.Position(call.Pos()), emitCallName(call.Fun), lit.Value))
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
