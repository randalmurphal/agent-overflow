package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"agent-overflow/internal/transport"
)

// callerBoundMethods is every bound method marked //ao:callerbound, with
// what its context reaches. Such a context ends when its caller's
// connection does, so it may reach only store reads, in-memory state,
// local file reads and resources the connection's cleanup releases: never a
// subprocess, a network request, a shared operation or a write. Every other
// method runs until it returns or the server stops (internal/app/AGENTS.md
// § Authorization and caller state).
var callerBoundMethods = map[string]string{
	"ListActivityRunMembers":    "store read",
	"GetPRReviewSubmitOutcome":  "in-memory attempt record",
	"ListItemsAfterCursor":      "store read",
	"ListItemsBeforeCursor":     "store read",
	"ListThreadSliceAround":     "store read",
	"SyncThreadWindow":          "store read",
	"SearchThreadItems":         "store read",
	"SearchTimelineItems":       "store read",
	"ListThreadsOnPR":           "store read and in-memory live state",
	"ThreadToolResolve":         "store read",
	"WorkflowAgentInspectRun":   "store read",
	"WorkflowAgentListMemory":   "store read",
	"WorkflowAgentListRuns":     "store read",
	"WorkflowAgentRunNarrative": "store read",
	"WorkflowAgentRunOutput":    "store read",
	"WorkflowAgentRunStatus":    "store read",
	"WorkflowGetRunMap":         "store read",
	"WorkflowAgentWatchRun":     "in-memory wait for a store change",
	"RemoteCommandArtifact":     "in-memory snapshot and local file read",
	"RemoteCommandLogArtifact":  "local file read",
	"ThreadToolExportChunk":     "local file read",
	"SearchPRCILog":             "local file read",
	"MintDictationChunkTicket":  "in-memory snapshot",
	"ReadReviewDiff":            "stream released by cleanup",
	"OpenEditDiff":              "store read; stream released by cleanup",
	"OpenTurnEditsDiff":         "store read; stream released by cleanup",
	"SubscribePRList":           "store read; subscription released by cleanup",
	"WatchBackgroundTaskOutput": "store read; local file tail released by cleanup",
	"ProviderTerminalAttach":    "in-memory attachment released by cleanup",
}

func TestCallerBoundMethodsAreReviewed(t *testing.T) {
	t.Parallel()
	takesContext := map[string]bool{}
	entries, err := os.ReadDir("internal/app")
	if err != nil {
		t.Fatalf("read internal/app: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join("internal/app", name)
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range parsed.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && isAppMethod(fn) && takesLeadingContext(fn) {
				takesContext[fn.Name.Name] = true
			}
		}
	}
	marked := map[string]bool{}
	for _, meta := range transport.GeneratedMethods {
		if !meta.CallerBound {
			continue
		}
		marked[meta.Name] = true
		if _, reviewed := callerBoundMethods[meta.Name]; !reviewed {
			t.Errorf("%s is //ao:callerbound but not in callerBoundMethods. A disconnect cuts it anywhere: "+
				"mark it only if it has no durable, provider-visible or remote effect, and add the reason", meta.Name)
		}
		if !takesContext[meta.Name] {
			t.Errorf("%s is //ao:callerbound but takes no leading context, so nothing can end it", meta.Name)
		}
	}
	var stale []string
	for name := range callerBoundMethods {
		if !marked[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	for _, name := range stale {
		t.Errorf("callerBoundMethods names %s, which is not an //ao:callerbound bound method", name)
	}
}

func takesLeadingContext(fn *ast.FuncDecl) bool {
	params := fn.Type.Params.List
	if len(params) == 0 {
		return false
	}
	selector, ok := params[0].Type.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && pkg.Name == "context" && selector.Sel.Name == "Context"
}
