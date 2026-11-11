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
// why a disconnect may cut it. Such a method's context ends when its
// caller's connection does, so it may only read, wait or arm a resource
// its connection releases. Every other method runs until it returns or
// the server stops (internal/app/AGENTS.md § Authorization and caller
// state).
var callerBoundMethods = map[string]string{
	"AgentRemoteComputers":          "read",
	"DiscoverComputers":             "read",
	"FetchForgeAttachment":          "read",
	"GetCodexSkills":                "read",
	"GetDevServers":                 "read",
	"GetGitStatus":                  "read",
	"GetPRDetail":                   "read",
	"GitListBranchPruneCandidates":  "read",
	"InspectProjectFolder":          "read",
	"ListActivityRunMembers":        "read",
	"ListCommitFiles":               "read",
	"ListForgeProjects":             "read",
	"ListItemsAfterCursor":          "read",
	"ListItemsBeforeCursor":         "read",
	"ListPRReviewThreads":           "read",
	"ListProjectPipelines":          "read",
	"ListThreadSliceAround":         "read",
	"ListThreadsOnPR":               "read",
	"ReadPipelineStages":            "read",
	"ReadThreadRemoteLog":           "read",
	"RemoteCommandArtifact":         "read",
	"RemoteCommandLogArtifact":      "read",
	"RemoteCommandProjects":         "read",
	"SearchThreadItems":             "read",
	"SearchTimelineItems":           "read",
	"SyncThreadWindow":              "read",
	"ThreadToolExportChunk":         "read",
	"ThreadToolQuery":               "read",
	"ThreadToolResolve":             "read",
	"WorkflowAgentInspectRun":       "read",
	"WorkflowAgentListMemory":       "read",
	"WorkflowAgentListRuns":         "read",
	"WorkflowAgentRunNarrative":     "read",
	"WorkflowAgentRunOutput":        "read",
	"WorkflowAgentRunStatus":        "read",
	"WorkflowAgentWatchRun":         "read that waits for a change",
	"WorkflowFetchPRReviewComments": "read",
	"WorkflowGetRunMap":             "read",
	"ReadReviewDiff":                "read of the connection's diff stream",
	"MintDictationChunkTicket":      "ticket for the connection's recording",
	"GitStatusSubscribe":            "subscription released with the connection",
	"SubscribePipeline":             "subscription released with the connection",
	"SubscribePRList":               "subscription released with the connection",
	"SubscribePRUpdates":            "subscription released with the connection",
	"WatchBackgroundTaskOutput":     "subscription released with the connection",
	"ProviderTerminalAttach":        "output attachment released with the connection",
	"OpenBranchBaseDiff":            "diff stream released with the connection",
	"OpenCommitDiff":                "diff stream released with the connection",
	"OpenCommitFileDiff":            "diff stream released with the connection",
	"OpenEditDiff":                  "diff stream released with the connection",
	"OpenPRCommitDiff":              "diff stream released with the connection",
	"OpenPRDiff":                    "diff stream released with the connection",
	"OpenTurnEditsDiff":             "diff stream released with the connection",
	"OpenWorkspaceDiff":             "diff stream released with the connection",
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
