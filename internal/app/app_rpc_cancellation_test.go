package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"

	"agent-overflow/internal/testutil/reporoot"
	"agent-overflow/internal/transport"
)

// callerBoundMethods is every bound method marked //ao:callerbound, with
// what its context reaches. Such a context ends when its caller's
// connection does, so it may reach only store reads, in-memory state,
// local file reads and resources the connection's cleanup releases: never a
// subprocess, a network request, a shared operation or a write, unless the
// package that owns the operation defines and tests what a cut at each
// point leaves. Every other
// method runs until it returns or the server stops (internal/app/AGENTS.md
// § Authorization and caller state).
var callerBoundMethods = map[string]string{
	"TrackerDashboards":         "store read and a private in-memory database",
	"EvaluateTrackerDashboard":  "store read into a private in-memory database",
	"TrackerDashboardDrill":     "store read into a private in-memory database",
	"ListActivityRunMembers":    "store read",
	"GetPRReviewSubmitOutcome":  "in-memory attempt record",
	"GetGitActionOutcome":       "in-memory attempt record",
	"ListItemsAfterCursor":      "store read",
	"ListItemsBeforeCursor":     "store read",
	"ListThreadSliceAround":     "store read",
	"SyncThreadWindow":          "store read",
	"SearchThreadItems":         "store read",
	"SearchTimelineItems":       "store read",
	"ListThreadsOnPR":           "store read and in-memory live state",
	"ThreadToolResolve":         "store read",
	"ThreadOwnerRecovery":       "store reads and in-memory prompt and thread listings",
	"ThreadOwnerChanges":        "store reads and an owner wait released by connection cleanup",
	"ThreadToolWatch":           "store reads, live state and an in-memory wait registration",
	"WorkflowGetRunMap":         "store read",
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
	"SubscribeTrackerBoard":     "store read; board subscription released by cleanup",
	"SubscribeTrackerTicket":    "store read; ticket subscription released by cleanup",
	// A cut before the save leaves nothing stored, and the far side revokes
	// the session it issued at its next mint; once the save has begun the
	// cut changes only the caller's answer (attachedbackends enroll).
	"AcceptOwnDeviceIntroduction": "own-device introduction's wait and redemption, which its owner ends cleanly",
}

func TestCallerBoundMethodsAreReviewed(t *testing.T) {
	t.Parallel()
	takesContext := map[string]bool{}
	for _, path := range reporoot.Files(t, "internal/app", "*.go") {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		parsed, err := reporoot.ParseGoFile(t, token.NewFileSet(), path, parser.SkipObjectResolution)
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
				"mark it only if it has no durable, provider-visible or remote effect, or its owner defines and tests every cut, and add the reason", meta.Name)
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
