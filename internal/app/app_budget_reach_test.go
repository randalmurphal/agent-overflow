package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/appupdate"
	"agent-overflow/internal/commitmsg"
	"agent-overflow/internal/dictation"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/mcpapp"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/providerupdate"
	"agent-overflow/internal/testutil/reporoot"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

// sharedDriveGraph is driveGraph built once for every test that walks it.
var sharedDriveGraph = sync.OnceValues(buildDriveGraph)

// longStep is a bounded step a method's budget may have to cover. Its entry
// points are the functions of the package in dir whose bodies name
// marker: a package-level identifier, a selector such as
// "commitmsg.Timeout", or "runsHooks:", a commandSpec field set in a
// composite literal.
type longStep struct {
	dir, marker string
	bound       time.Duration
}

// longSteps are the bounded long steps methodServerBounds adds up, found
// where they are applied, so a new caller of one is found too.
var longSteps = []longStep{
	{"internal/git", "runsHooks:", hooked},
	{"internal/git", "ForgeAttachmentTimeout", gitops.ForgeAttachmentTimeout},
	{"internal/dictation", "ErrTranscribeBudget", dictation.TranscribeBudget},
	{"internal/providerupdate", "CheckTimeout", providerupdate.CheckTimeout},
	{"internal/providerupdate", "NotesTimeout", providerupdate.NotesTimeout},
	{"internal/appupdate", "UpdaterCheckTimeout", appupdate.UpdaterCheckTimeout},
	{"internal/mcpapp", "AuthRoundTripTimeout", mcpapp.AuthRoundTripTimeout},
	{"internal/app", "commitmsg.Timeout", commitmsg.Timeout},
	{"internal/app", "codexForkTimeout", codexForkTimeout},
	{"internal/provider/claude", "DefaultControlRequestCeiling", claude.DefaultControlRequestCeiling},
	{"internal/threadtools", "MaxForwardedWaitSeconds", time.Duration(threadtools.MaxForwardedWaitSeconds) * time.Second},
}

// budgetReachBoundaries are where the budget walk stops, because the work
// past them does not hold the calling method's answer. Each names a node of
// driveGraph; a "param:" entry stops calls through that parameter.
var budgetReachBoundaries = map[string]string{
	"result:" + appMethodKey + "sessionEventHandler":                "handles provider output on the thread's event worker",
	"field:agent-overflow/internal/provider/claude.Session.onEvent": "handles provider output on the thread's event worker",
	"field:agent-overflow/internal/provider/codex.Session.onEvent":  "handles provider output on the thread's event worker",
	"param:" + appMethodKey + "subscribeGlobalTurnObserver#0":       "runs when a provider turn ends",
	appMethodKey + "enqueueFlushDispatch":                           "queues rows for the thread's flush dispatch worker",
	"(*agent-overflow/internal/providerupdate.Service).runUpdate":   "runs on its own goroutine; Run answers once the run has started",
	"param:(*agent-overflow/internal/app.workerPrompts).run#0":      "runs on the worker prompts' serial queue",
	appMethodKey + "queueThreadAgentEmit":                           "batches agent-row frames for a drain that only emits them",
	"param:" + appMethodKey + "goAgentNoticeWork#1":                 "runs on joined notice work; a message is accepted once queued",
}

// budgetReachMethodBoundaries stop one method's walk where the graph joins
// it to work the method never runs: a branch its own arguments rule out.
var budgetReachMethodBoundaries = map[string]map[string]string{
	"WorkflowDiscardItem": {
		"(*agent-overflow/internal/workflowapp.Service).landDisposition": "lands a merge or a PR; a discard removes the worktree instead",
	},
	"ThreadToolQuery": {
		"(*agent-overflow/internal/threadtools.session).spawn":        "a write tool; the method refuses every tool but the reads",
		"(*agent-overflow/internal/threadtools.session).newAssistant": "a write tool; the method refuses every tool but the reads",
	},
	"ThreadToolWatch": {
		"(*agent-overflow/internal/threadtools.session).spawn":        "a write tool; the method refuses every tool but a thread_status wait",
		"(*agent-overflow/internal/threadtools.session).newAssistant": "a write tool; the method refuses every tool but a thread_status wait",
	},
	"CreateAssistant": {
		"(agent-overflow/internal/threadapp.Workspace).CreateWorktree": "an assistant's thread is made in its home with no worktree cut (createThreadRow)",
	},
	"StartNextAssistantThread": {
		"(agent-overflow/internal/threadapp.Workspace).CreateWorktree": "an assistant's next thread is made in its home with no worktree cut (createThreadRow)",
	},
	"StartTrackerDashboardThread": {
		"(agent-overflow/internal/threadapp.Workspace).CreateWorktree": "a dashboard Ask's thread runs in the project's checkout; its options name no worktree",
	},
}

// TestEveryMethodReachingALongStepDeclaresItsBudget walks driveGraph from
// every bound method and fails when one can reach a long step whose bound
// is past its budget: its clients would give up while the step still runs.
// methodServerBounds says what each budget adds up; this finds the methods
// that need one, across packages, interfaces and function values. The graph
// over-reports (a value it cannot follow counts as called), so an alarm is
// resolved by a budget or by a boundary with the reason the work does not
// hold the method's answer. It checks the longest single step a method
// reaches, not the sum of the steps it runs one after another; that sum is
// methodServerBounds' to state.
func TestEveryMethodReachingALongStepDeclaresItsBudget(t *testing.T) {
	t.Parallel()
	g, err := sharedDriveGraph()
	if err != nil {
		t.Fatal(err)
	}
	steps := map[string]longStep{}
	for _, step := range longSteps {
		keys := longStepEntries(t, step)
		if len(keys) == 0 {
			t.Errorf("no function in %s names %s; the step moved", step.dir, step.marker)
		}
		for _, key := range keys {
			if !g.known(key) {
				t.Errorf("%s names %s but is not in the graph; the key no longer matches the code", key, step.marker)
				continue
			}
			if prior, ok := steps[key]; !ok || prior.bound < step.bound {
				steps[key] = step
			}
		}
	}
	checkBoundary := func(key string) {
		if !g.known(strings.TrimPrefix(key, "param:")) && !g.known(key) {
			t.Errorf("boundary %s is not in the graph; rename or remove it", key)
		}
	}
	for key := range budgetReachBoundaries {
		checkBoundary(key)
	}
	bound := map[string]bool{}
	for _, method := range g.bound {
		bound[method.name] = true
	}
	for name, stops := range budgetReachMethodBoundaries {
		if !bound[name] {
			t.Errorf("budgetReachMethodBoundaries names %s, which is not a bound method", name)
		}
		for key := range stops {
			checkBoundary(key)
		}
	}
	for _, method := range g.bound {
		own := budgetReachMethodBoundaries[method.name]
		stop := func(node string) bool {
			for _, set := range []map[string]string{budgetReachBoundaries, own} {
				if _, at := set[node]; at {
					return true
				}
				if _, through := set[viaParam(node)]; through {
					return true
				}
			}
			return false
		}
		worst, path := longestReachedStep(g, method.key, steps, stop)
		if worst == "" {
			continue
		}
		step := steps[worst]
		if budget := transport.MethodBudget(method.name); budget < step.bound {
			t.Errorf("%s can reach %s (%s, %s) but its budget is %s: declare //ao:budget covering it and its bound in methodServerBounds, or stop the walk at a boundary with a reason:\n  %s",
				method.name, step.marker, worst, step.bound, budget, strings.Join(path, "\n  > "))
		}
	}
}

// longestReachedStep is the step with the longest bound that from reaches
// without entering a node stop accepts, and the path to it.
func longestReachedStep(g *driveGraph, from string, steps map[string]longStep, stop func(string) bool) (string, []string) {
	parent := map[string]string{from: ""}
	queue := []string{from}
	worst := ""
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		if step, ok := steps[node]; ok && (worst == "" || step.bound > steps[worst].bound) {
			worst = node
		}
		for _, next := range g.edges[node] {
			if _, seen := parent[next]; seen || stop(next) {
				continue
			}
			parent[next] = node
			queue = append(queue, next)
		}
	}
	if worst == "" {
		return "", nil
	}
	var path []string
	for at := worst; at != from; at = parent[at] {
		path = append([]string{at}, path...)
	}
	return worst, path
}

// longStepEntries are the graph keys of the functions in step.dir whose
// bodies name step.marker.
func longStepEntries(t *testing.T, step longStep) []string {
	t.Helper()
	entries, err := os.ReadDir(reporoot.Path(t, step.dir))
	if err != nil {
		t.Fatal(err)
	}
	pkgPath := "agent-overflow/" + step.dir
	field, isField := strings.CutSuffix(step.marker, ":")
	qualifier, name, qualified := strings.Cut(step.marker, ".")
	var keys []string
	fset := token.NewFileSet()
	for _, entry := range entries {
		file := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := reporoot.ParseGoFile(t, fset, step.dir+"/"+file, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			names := false
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				switch node := node.(type) {
				case *ast.KeyValueExpr:
					if key, ok := node.Key.(*ast.Ident); ok && isField && key.Name == field {
						if value, ok := node.Value.(*ast.Ident); !ok || value.Name != "false" {
							names = true
						}
					}
				case *ast.SelectorExpr:
					if x, ok := node.X.(*ast.Ident); ok && qualified && x.Name == qualifier && node.Sel.Name == name {
						names = true
					}
				case *ast.Ident:
					if !isField && !qualified && node.Name == step.marker {
						names = true
					}
				}
				return !names
			})
			if names {
				keys = append(keys, funcDeclKey(pkgPath, fn))
			}
		}
	}
	sort.Strings(keys)
	return keys
}

// funcDeclKey is fn's driveGraph key (types.Func.FullName) from its syntax.
func funcDeclKey(pkgPath string, fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return pkgPath + "." + fn.Name.Name
	}
	recv := fn.Recv.List[0].Type
	pointer := ""
	if star, ok := recv.(*ast.StarExpr); ok {
		pointer, recv = "*", star.X
	}
	typeName := ""
	switch recv := recv.(type) {
	case *ast.Ident:
		typeName = recv.Name
	case *ast.IndexExpr:
		typeName = recv.X.(*ast.Ident).Name + "[" + recv.Index.(*ast.Ident).Name + "]"
	case *ast.IndexListExpr:
		params := make([]string, len(recv.Indices))
		for i, index := range recv.Indices {
			params[i] = index.(*ast.Ident).Name
		}
		typeName = recv.X.(*ast.Ident).Name + "[" + strings.Join(params, ",") + "]"
	}
	return "(" + pointer + pkgPath + "." + typeName + ")." + fn.Name.Name
}
