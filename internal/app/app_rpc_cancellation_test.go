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

// A bound method's leading context is cancelled when its caller's
// connection ends (internal/app/AGENTS.md § Authorization and caller
// state). Every bound method that takes one declares here what that
// cancellation can do to it, so a new one cannot reach the wire without
// someone deciding. The class is a property of the method body, which no
// parameter type can carry: the dispatcher injects one context.Context
// for every method, and a narrower type would still be read the same way
// inside.
type ctxClass string

const (
	// ctxPrincipal reads the caller's identity from the context and never
	// its cancellation.
	ctxPrincipal ctxClass = "principal"
	// ctxRead abandons reads or waits that have no side effect.
	ctxRead ctxClass = "read"
	// ctxConnection arms or uses a resource the caller's connection owns,
	// which ends with that connection.
	ctxConnection ctxClass = "connection"
	// ctxEphemeral changes only in-process state the next read shows as
	// it is.
	ctxEphemeral ctxClass = "ephemeral"
	// ctxPreCommit bounds only the waits before its first durable,
	// provider-visible or remote effect; from there it runs on a context
	// the caller's cancellation does not reach.
	ctxPreCommit ctxClass = "precommit"
	// ctxRollback undoes what it did before returning a cancellation.
	ctxRollback ctxClass = "rollback"
	// ctxRepeatable has effects that are idempotent or keyed by an id the
	// caller supplies, so repeating the call completes an interrupted one.
	ctxRepeatable ctxClass = "repeatable"
)

// boundMethodContextClasses lists every bound *App method whose first
// parameter is a context.Context.
var boundMethodContextClasses = map[string]ctxClass{
	"AcceptOwnDeviceIntroduction":          ctxPreCommit, // redemption on: attachedbackends runs it on its own lifetime
	"AgentRemoteCancel":                    ctxRepeatable,
	"AgentRemoteComputers":                 ctxRead,
	"AgentRemoteFetchArtifact":             ctxRead,
	"AgentRemoteFetchLog":                  ctxRead,
	"AgentRemoteStart":                     ctxRepeatable, // the request id; a lost reply keeps the watch
	"AgentRemoteStatus":                    ctxRepeatable,
	"AllowPreviewPort":                     ctxPreCommit,
	"AppendLiveDictation":                  ctxConnection,
	"ApprovePR":                            ctxPreCommit, // forge write: git.Core detaches it
	"BeginDictation":                       ctxConnection,
	"BeginDraftProjectTransfer":            ctxPreCommit,
	"BeginLiveDictation":                   ctxConnection,
	"BeginPasskeyStepUp":                   ctxPrincipal,
	"BeginThreadTransfer":                  ctxPreCommit, // a move detaches at stopping its remote commands
	"BindThreadTransferDestination":        ctxPreCommit,
	"BrowserCompanionDo":                   ctxEphemeral,
	"BrowserCompanionPaneAttach":           ctxConnection,
	"BrowserCompanionPaneDetach":           ctxConnection,
	"BrowserCompanionRevealPageFile":       ctxRead,
	"CancelDictation":                      ctxConnection,
	"CancelLiveDictation":                  ctxConnection,
	"CancelPRAutoMerge":                    ctxPreCommit,
	"CancelThreadRemoteCommand":            ctxPreCommit, // the cancel call; only the settle wait ends with the caller
	"ClearDraft":                           ctxPreCommit,
	"CompactCodexThread":                   ctxPreCommit,
	"ConfirmSSHConnection":                 ctxRollback, // an abandoned confirmation cancels the setup
	"CreateProjectCheckout":                ctxPreCommit,
	"CreateThread":                         ctxPreCommit,
	"CreateThreadTransferOffer":            ctxPreCommit,
	"DisallowPreviewPort":                  ctxPreCommit,
	"DiscardUnpreparedThreadTransfer":      ctxRepeatable,
	"DiscoverComputers":                    ctxRead,
	"FetchForgeAttachment":                 ctxRead,
	"FinishDictation":                      ctxConnection,
	"FinishLiveDictation":                  ctxConnection,
	"ForgetTailnetNode":                    ctxPrincipal,
	"ForkSideChat":                         ctxRollback,
	"ForkThread":                           ctxRollback,
	"ForkThreadFromMessage":                ctxRollback,
	"GetCodexSkills":                       ctxRead,
	"GetDevServers":                        ctxRead,
	"GetGitStatus":                         ctxRead,
	"GetNativeNetworkConfig":               ctxPrincipal,
	"GetNetworkSettings":                   ctxPrincipal,
	"GetPRDetail":                          ctxRead,
	"GetSettings":                          ctxPrincipal,
	"GetUIState":                           ctxPrincipal,
	"GitCreatePR":                          ctxPreCommit, // gh pr create pushes, then opens
	"GitListBranchPruneCandidates":         ctxRead,
	"GitStatusSubscribe":                   ctxConnection,
	"GitStatusUnsubscribe":                 ctxConnection,
	"InspectProjectFolder":                 ctxRead,
	"IntroduceOwnDevice":                   ctxRepeatable,
	"ListActivityRunMembers":               ctxRead,
	"ListCommitFiles":                      ctxRead,
	"ListForgeProjects":                    ctxRead,
	"ListItemsAfterCursor":                 ctxRead,
	"ListItemsBeforeCursor":                ctxRead,
	"ListOwnDevices":                       ctxPrincipal,
	"ListPRReviewThreads":                  ctxRead,
	"ListProjectPipelines":                 ctxRead,
	"ListThreadSliceAround":                ctxRead,
	"ListThreadsOnPR":                      ctxRead,
	"MarkPRReady":                          ctxPreCommit,
	"MergePR":                              ctxPreCommit,
	"MintDictationChunkTicket":             ctxConnection,
	"MintFilePreviewURL":                   ctxPrincipal,
	"MintOwnDeviceIntroduction":            ctxPrincipal,
	"MintOwnDevicePairing":                 ctxPrincipal,
	"MintOwnDevicePairingOnNetwork":        ctxPrincipal,
	"MintPreviewURL":                       ctxPrincipal,
	"MintReadAloudTicket":                  ctxPrincipal,
	"MoveDraftToThread":                    ctxPreCommit,
	"OpenBranchBaseDiff":                   ctxConnection,
	"OpenCommitDiff":                       ctxConnection,
	"OpenCommitFileDiff":                   ctxConnection,
	"OpenComputerPairing":                  ctxConnection,
	"OpenEditDiff":                         ctxConnection,
	"OpenOwnComputerPairing":               ctxConnection,
	"OpenPRCommitDiff":                     ctxConnection,
	"OpenPRDiff":                           ctxConnection,
	"OpenPRThread":                         ctxRollback,
	"OpenTurnEditsDiff":                    ctxConnection,
	"OpenWorkspaceDiff":                    ctxConnection,
	"PairAgentComputer":                    ctxPreCommit, // the redemption runs on the app lifetime (AddBackend)
	"PromoteScratchThread":                 ctxPreCommit,
	"ProviderTerminalAttach":               ctxConnection,
	"ProviderTerminalDetach":               ctxConnection,
	"ProviderTerminalInput":                ctxConnection,
	"ProviderTerminalSetControl":           ctxConnection,
	"ReadPipelineStages":                   ctxRead,
	"ReadReviewDiff":                       ctxConnection,
	"ReadThreadRemoteLog":                  ctxRead,
	"RefreshPRCI":                          ctxConnection,
	"RefreshPRList":                        ctxConnection,
	"RefreshPipeline":                      ctxConnection,
	"RefreshProjectIdentity":               ctxPreCommit,
	"RegisterOwnDevice":                    ctxPrincipal,
	"RegisterPushToken":                    ctxPrincipal,
	"RegisterQueueItem":                    ctxPreCommit,
	"ReleaseReviewDiff":                    ctxConnection,
	"RemoteCommandArtifact":                ctxRead,
	"RemoteCommandCancel":                  ctxPrincipal,
	"RemoteCommandLogArtifact":             ctxRead,
	"RemoteCommandProjects":                ctxRead,
	"RemoteCommandReadLog":                 ctxPrincipal,
	"RemoteCommandSearchLog":               ctxPrincipal,
	"RemoteCommandStart":                   ctxPreCommit,
	"RemoteCommandStatus":                  ctxPrincipal,
	"RemoveOwnDevice":                      ctxPrincipal,
	"RenewCanonicalDomainCert":             ctxPrincipal,
	"RepairBackendAddress":                 ctxPreCommit,
	"ReplyToPRThread":                      ctxPreCommit,
	"ReportNativeNetworkState":             ctxConnection,
	"RequestServiceUpdate":                 ctxPrincipal, // the update flow runs on the app lifetime
	"RespondToApproval":                    ctxPrincipal,
	"RespondToUserInput":                   ctxPrincipal,
	"RevertConversationAndResendMessage":   ctxPreCommit,
	"RunThreadShellCommand":                ctxPreCommit,
	"SaveDraft":                            ctxPreCommit,
	"SaveForgeAttachment":                  ctxRead,
	"SavePRCIJobLog":                       ctxRead,
	"SearchThreadItems":                    ctxRead,
	"SearchTimelineItems":                  ctxRead,
	"SendMessageWithOptions":               ctxPreCommit,
	"SendPRDiffReviewComments":             ctxPreCommit,
	"SetAgentComputerEnabled":              ctxPreCommit,
	"SetAsyncQuestionDismissed":            ctxPreCommit,
	"SetNetworkSettings":                   ctxPrincipal,
	"SetPRCILogFollows":                    ctxConnection,
	"SetPRListActive":                      ctxConnection,
	"SetPRThreadResolved":                  ctxPreCommit,
	"SetPipelineActive":                    ctxConnection,
	"SetPipelineFollows":                   ctxConnection,
	"StartCodexReview":                     ctxPreCommit,
	"StartSSHComputer":                     ctxRepeatable, // wakes an installed service
	"StartTerminal":                        ctxPrincipal,
	"SteerMessageWithOptions":              ctxPreCommit,
	"StopThreadBackgroundWork":             ctxPreCommit,
	"SubmitAsyncAnswers":                   ctxPreCommit,
	"SubmitPRReview":                       ctxPreCommit, // detached; a GitLab repeat skips pending drafts
	"SubscribePRList":                      ctxConnection,
	"SubscribePRUpdates":                   ctxConnection,
	"SubscribePipeline":                    ctxConnection,
	"SyncOwnDevices":                       ctxPrincipal,
	"SyncThreadWindow":                     ctxRead,
	"ThreadToolCall":                       ctxPreCommit, // a request detaches at its receipt
	"ThreadToolExportChunk":                ctxRead,
	"ThreadToolQuery":                      ctxRead,
	"ThreadToolRequestStatus":              ctxRepeatable,
	"ThreadToolResolve":                    ctxRead,
	"UnapprovePR":                          ctxPreCommit,
	"UnregisterPushToken":                  ctxPrincipal,
	"UnsubscribePRList":                    ctxConnection,
	"UnsubscribePRUpdates":                 ctxConnection,
	"UnsubscribePipeline":                  ctxConnection,
	"UnwatchBackgroundTaskOutput":          ctxConnection,
	"UpdateClientDeviceName":               ctxPrincipal,
	"UpdateNewThreadDefaults":              ctxPrincipal,
	"UpdatePRBranch":                       ctxPreCommit,
	"UpdateSettings":                       ctxPrincipal,
	"UpdateThreadRuntimeMode":              ctxPrincipal,
	"VerifyBrowserUnlock":                  ctxPrincipal,
	"WatchBackgroundTaskOutput":            ctxConnection,
	"WorkflowAgentAddMemory":               ctxPrincipal,
	"WorkflowAgentAmendSeeds":              ctxPrincipal,
	"WorkflowAgentGetNotes":                ctxPrincipal,
	"WorkflowAgentGuideRun":                ctxPrincipal,
	"WorkflowAgentInspectRun":              ctxRead,
	"WorkflowAgentListMemory":              ctxRead,
	"WorkflowAgentListRuns":                ctxRead,
	"WorkflowAgentRunNarrative":            ctxRead,
	"WorkflowAgentRunOutput":               ctxRead,
	"WorkflowAgentRunStatus":               ctxRead,
	"WorkflowAgentSchedule":                ctxPrincipal,
	"WorkflowAgentSetNotes":                ctxPrincipal,
	"WorkflowAgentStartRun":                ctxPrincipal,
	"WorkflowAgentWatchRun":                ctxRead,
	"WorkflowAnswerQuestion":               ctxPrincipal,
	"WorkflowCancelItem":                   ctxPrincipal,
	"WorkflowCreateItemPR":                 ctxPreCommit, // the push; a repeat adopts the branch's open PR
	"WorkflowDiscardItem":                  ctxPreCommit,
	"WorkflowDiscussPR":                    ctxPreCommit,
	"WorkflowFetchPRReviewComments":        ctxRead,
	"WorkflowGetRunMap":                    ctxRead,
	"WorkflowMergeItem":                    ctxPreCommit,
	"WorkflowPauseItem":                    ctxPrincipal,
	"WorkflowRequestSoftStop":              ctxPrincipal,
	"WorkflowRerunItem":                    ctxPrincipal,
	"WorkflowResolveGate":                  ctxPrincipal,
	"WorkflowResumeItem":                   ctxPrincipal,
	"WorkflowRetryFailedUnits":             ctxPrincipal,
	"WorkflowRetryUnit":                    ctxPrincipal,
	"WorkflowScheduleResume":               ctxPrincipal,
	"WorkflowSendPRReviewCommentsToThread": ctxPreCommit,
}

func TestEveryContextTakingBoundMethodDeclaresItsCancellationClass(t *testing.T) {
	t.Parallel()
	bound := map[string]bool{}
	for _, meta := range transport.GeneratedMethods {
		bound[meta.Name] = true
	}
	entries, err := os.ReadDir("internal/app")
	if err != nil {
		t.Fatalf("read internal/app: %v", err)
	}
	seen := map[string]bool{}
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
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !isAppMethod(fn) || !fn.Name.IsExported() || !bound[fn.Name.Name] || !takesLeadingContext(fn) {
				continue
			}
			seen[fn.Name.Name] = true
			class, declared := boundMethodContextClasses[fn.Name.Name]
			switch {
			case !declared:
				t.Errorf("%s: bound method %s takes a context but declares no cancellation class. "+
					"Its context is cancelled when the caller's connection ends: add it to "+
					"boundMethodContextClasses, detaching at its commit point if it mutates",
					path, fn.Name.Name)
			case !validCtxClass(class):
				t.Errorf("%s declares unknown cancellation class %q", fn.Name.Name, class)
			}
		}
	}
	var stale []string
	for name := range boundMethodContextClasses {
		if !seen[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	for _, name := range stale {
		t.Errorf("boundMethodContextClasses names %s, which is no longer a context-taking bound method on *App", name)
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

func validCtxClass(class ctxClass) bool {
	switch class {
	case ctxPrincipal, ctxRead, ctxConnection, ctxEphemeral, ctxPreCommit, ctxRollback, ctxRepeatable:
		return true
	}
	return false
}
