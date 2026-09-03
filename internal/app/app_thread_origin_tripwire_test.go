package app

import (
	"fmt"
	"go/ast"
	"testing"
)

// Creation provenance is write-once: `created_by_device` and the three
// `created_*` git columns are absent from updateThreadSetSQL, so the insert is
// the only chance to record them. A creation path that forgets leaves a thread
// permanently blank, and nothing downstream can tell that apart from a thread
// whose workspace genuinely had nothing to report.
//
// This test makes forgetting structural rather than remembered. Every
// new-thread composite literal in the repository must either set Origin itself
// or be listed below with the reason it does not.
//
// The literals are found by shape: a store.Thread (or, inside package store, a
// bare Thread) with a CreatedAt key. That key is what separates a row being
// born from the many `return store.Thread{}, err` zero values.
var threadLiteralsThatDoNotSetOriginThemselves = map[string]string{
	"internal/store/thread_forks.go:BuildForkedThread": "the caller stamps it. This builds the shape of a fork; " +
		"the fork's coordinates are observed in internal/app, where the git core lives, and a fork observes them " +
		"fresh rather than copying the source's because the shared workspace has moved on since the source began.",
	"internal/app/app_workflow_host.go:createWorkflowThread": "stamped by stampThreadCreation a few lines below, " +
		"after sanitizeThreadModelSettings has had its say.",
	"internal/app/app_workflow_host.go:newWorkflowTriageThread": "stamped by stampThreadCreation before it returns.",
}

// packagesThatCreateThreads is every package that constructs a thread row,
// named from the repository root (these suites chdir there; see TestMain).
// A new one shows up here as a build-visible edit, which is the point.
var packagesThatCreateThreads = []string{
	"internal/app",
	"internal/store",
	"internal/threadapp",
	"internal/discussion",
	"internal/sessionimport",
}

func TestEveryNewThreadRecordsWhereItCameFrom(t *testing.T) {
	found := 0
	for _, dir := range packagesThatCreateThreads {
		for name, file := range parsePackageFiles(t, dir) {
			path := dir + "/" + name
			ast.Inspect(file, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok {
					return true
				}
				ast.Inspect(fn.Body, func(inner ast.Node) bool {
					lit, ok := inner.(*ast.CompositeLit)
					if !ok || !isThreadLiteral(lit) || !literalHasKey(lit, "CreatedAt") {
						return true
					}
					found++
					if literalHasKey(lit, "Origin") {
						return true
					}
					key := fmt.Sprintf("%s:%s", path, fn.Name.Name)
					if _, exempt := threadLiteralsThatDoNotSetOriginThemselves[key]; !exempt {
						t.Errorf(
							"%s builds a thread without setting Origin.\n"+
								"Creation provenance is write-once, so a row that misses it here can never "+
								"acquire it. Either set Origin (observe the workspace, or inherit a parent's), "+
								"or add %q to threadLiteralsThatDoNotSetOriginThemselves with the reason.",
							key, key,
						)
					}
					return true
				})
				return false
			})
		}
	}
	if found == 0 {
		t.Fatal("scanned every thread-creating package and found no new-thread literals; the shape test has drifted")
	}
}

// TestEveryOriginExemptionStillExists keeps the exemption list from outliving
// the code it describes. A stale entry is worse than no entry: it reads as a
// considered decision about a function that has since been rewritten.
func TestEveryOriginExemptionStillExists(t *testing.T) {
	seen := map[string]bool{}
	for _, dir := range packagesThatCreateThreads {
		for name, file := range parsePackageFiles(t, dir) {
			path := dir + "/" + name
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				seen[fmt.Sprintf("%s:%s", path, fn.Name.Name)] = true
			}
		}
	}
	for key, reason := range threadLiteralsThatDoNotSetOriginThemselves {
		if !seen[key] {
			t.Errorf("exemption %q (%s) names a function that no longer exists; drop or update the entry", key, reason)
		}
	}
}

// isThreadLiteral matches `store.Thread{...}` and, inside package store,
// `Thread{...}`.
func isThreadLiteral(lit *ast.CompositeLit) bool {
	switch typ := lit.Type.(type) {
	case *ast.Ident:
		return typ.Name == "Thread"
	case *ast.SelectorExpr:
		pkg, ok := typ.X.(*ast.Ident)
		return ok && pkg.Name == "store" && typ.Sel.Name == "Thread"
	}
	return false
}

func literalHasKey(lit *ast.CompositeLit, key string) bool {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		ident, ok := kv.Key.(*ast.Ident)
		if ok && ident.Name == key {
			return true
		}
	}
	return false
}
