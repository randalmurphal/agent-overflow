package transport

import (
	"sort"
	"testing"
)

// preScopeTableLocalOnly is the local-only partition as it stood on
// 2026-08-31, the day before LocalOnlyMethods became derived from the
// scope table. It is FROZEN: it records what was reachable from the LAN
// then, so the annotation wave could be checked for having changed
// nothing.
//
// It is not a second classification and nothing consults it at runtime.
// A deliberate reachability change edits this list in the same commit as
// the annotation or override that causes it, which is what makes such a
// change a visible line in a diff rather than a side effect of
// re-reading a method's scope.
var preScopeTableLocalOnly = []string{
	"AddRemoteEndpoint", "AppendUIRenderTraceBatch", "AttachThreadWorktree",
	"AutoResumeThread", "BackgroundClaudeTask", "BookmarkUIRenderTrace",
	"BrowseDirectory", "BrowserCompanionDo", "BrowserCompanionInput",
	"BrowserCompanionNextFrame", "BrowserCompanionResize",
	"BrowserCompanionSubscribe", "BrowserCompanionUnsubscribe",
	"CancelDevicePairing", "CancelSessionImport", "CheckForUpdate",
	"CheckThreadImportUpdates", "CleanCodexBackgroundTerminals",
	"ClearBrowserSiteData", "ClearDraft", "CloseTerminal",
	"CloseThreadTerminals", "CompactCodexThread", "ConcludeDiscussion",
	"ConfirmDevicePairing", "CreateDiffReviewComment", "CreateProject",
	"CreateThread", "CreateThreadFromPR", "DeleteAttachment",
	"DeleteDiffReviewComment", "DeleteEmptyDraftThread",
	"DeleteProviderCustomEnvVar", "DeleteRemoteEndpoint",
	"DevicePairingStatus", "DownloadUpdate", "ForkThread",
	"ForkThreadFromMessage", "GenerateCommitMessage", "GetAccessOverview",
	"GetAttachmentData", "GetAttachmentThumbnail", "GetBranchBaseDiff",
	"GetClaudeSkills", "GetCodexAccountUsage", "GetCodexSkills",
	"GetCommitDiff", "GetDiffContextLines", "GetDraft", "GetGitStatus",
	"GetGitStatusFastForProject", "GetLocalImageData", "GetMcpServerStatus",
	"GetMergeConflictFile", "GetModelsForProvider", "GetNetworkSettings",
	"GetPRCIJobLog", "GetPRCIJobs", "GetPRCommitDiff", "GetPRDetail",
	"GetPRDiff", "GetPRMergeConflicts", "GetProjectWorktreeSetup",
	"GetProviderStatuses", "GetQueueState", "GetRemoteEndpointToken",
	"GetTerminalReplay", "GetThreadContextUsage", "GetThreadDefaults",
	"GetThreadLiveState", "GetThreadWorktreeSetup", "GetUIRenderTracePath",
	"GetWorkingTreeDiff", "GetWorkspaceActivity", "GetWorkspaceCurrentDiff",
	"GetWSLDistroPreference", "GitCheckout", "GitCheckoutForProject",
	"GitCommit", "GitCreateBranch", "GitCreateBranchFrom", "GitCreatePR",
	"GitCreateWorktree", "GitListBranches", "GitListBranchesForProject",
	"GitListBranchPruneCandidates", "GitListWorktrees",
	"GitListWorktreesForProject", "GitMaybeFetchRemotes",
	"GitMaybeFetchRemotesForProject", "GitPruneBranches", "GitPull",
	"GitPush", "GitRemoveWorktree", "GitStageAll", "GitStatusSubscribe",
	"GitStatusUnsubscribe", "GitSyncBranch", "GitSyncBranchForProject",
	"GitWorktreeStatus", "GitWorktreeStatusForProject",
	"HighlightPatchWithContext", "ImportSessions", "ImportThreadUpdates",
	"InterruptAndRevertIfClean", "InterruptTurn", "ListAvailableEditors",
	"ListBranchCommits", "ListDiffReviewComments", "ListImportableSessions",
	"ListMcpServerStatuses", "ListPendingInteractiveRequests",
	"ListPRCommits", "ListProviderAccounts", "ListPRReviewThreads",
	"ListRecentCommits", "ListReleases", "ListRemoteEndpoints",
	"ListTerminals", "ListThreadMcpServers", "ListWorkspaceMcpServers",
	"ListWSLDistros", "LoginProviderAccount", "MarkDiffReviewCommentsSent",
	"MintDevicePairing", "MoveThreadTerminals", "NotificationActivated",
	"OpenExternalURL", "OpenInEditor", "OpenTerminal", "PostChannelMessage",
	"PrepareThreadWorktree", "ProbeClaudeAccount", "ProbeCodexAccount",
	"ProbeDevServerURL", "ProjectDeletionPreview", "ProviderTerminalAttach",
	"ProviderTerminalDetach", "ProviderTerminalInput",
	"ProviderTerminalRefresh", "ProviderTerminalReplay",
	"ProviderTerminalResize", "ProviderTerminalSetControl",
	"RecheckClaudeAccount", "RecheckCodexAccount", "ReconfigureObservability",
	"ReconnectMcpServer", "ReconnectSession", "RefreshMcpServerStatus",
	"RefreshProviderAccountUsage", "RefreshTerminal", "RegenerateThreadTitle",
	"RegisterQueueItem", "RemoveOtherWorktree",
	"RemoveOtherWorktreeForProject", "RemoveProviderAccount",
	"ReplyToPRThread", "ReportFrontendErrorBatch",
	"ReportUpdateInstallStatus", "RequestWebviewMemoryTrim",
	"ResetKeybindings", "ResizeTerminal", "RespondToApproval",
	"RespondToUserInput", "RestartTerminal", "RestartToUpdate",
	"RestoreAccessDevice", "RetryThreadWorktreeSetup",
	"RevertConversationAndResendMessage", "RevokeAccessDevice",
	"RevokeAccessSession", "SaveDraft", "SavePayloadToFile", "SavePRCIJobLog",
	"SearchWorkspaceFiles", "SendDiffReviewComments", "SendMessage",
	"SendMessageWithOptions", "SendPlanRevisionComments", "SetAppearance",
	"SetChatBarFavorite", "SetEditorSettings", "SetNetworkSettings",
	"SetProjectWorktreeSetup", "SetProviderCustomEnvVar",
	"SetPRUpdatesActive", "SetThreadMcpServerEnabled",
	"SetWindowBackgroundColor", "SetWorkspaceMcpServerEnabled",
	"SetWSLDistroPreference", "StartCodexReview", "StartDiscussion",
	"StartDiscussionByID", "StartSession", "StartTerminal",
	"SteerMessageWithOptions", "StopClaudeTask", "StopCodexSubagent",
	"StopSession", "StopThreadBackgroundWork", "SubmitPRReview",
	"SubscribePRUpdates", "SwitchProviderAccount",
	"TerminateCodexBackgroundTerminal", "TouchRemoteEndpoint",
	"TriggerMcpAuth", "TriggerWorkspaceMcpAuth", "UnsubscribePRUpdates",
	"UpdateContextSettingsProfile", "UpdateDiffReviewComment",
	"UpdateKeybindings", "UpdateNewThreadDefaults", "UpdateRemoteEndpoint",
	"UpdateSettings", "UpdateThreadBranch", "UpdateThreadContextSettings",
	"UpdateThreadContextWindow", "UpdateThreadFastMode", "UpdateThreadMode",
	"UpdateThreadModel", "UpdateThreadModelSelection", "UpdateThreadProvider",
	"UpdateThreadReasoningEffort", "UpdateThreadRuntimeMode",
	"UpdateThreadWorkspace", "UploadAttachment", "VerifyEditDiffs",
	"WorkflowAgentAddMemory", "WorkflowAgentAmendSeeds",
	"WorkflowAgentGetNotes", "WorkflowAgentGuideRun",
	"WorkflowAgentInspectRun", "WorkflowAgentListMemory",
	"WorkflowAgentListRuns", "WorkflowAgentRunNarrative",
	"WorkflowAgentRunOutput", "WorkflowAgentRunStatus",
	"WorkflowAgentSchedule", "WorkflowAgentSetNotes", "WorkflowAgentStartRun",
	"WorkflowAgentWatchRun", "WorkflowAnswerQuestion", "WorkflowBindThread",
	"WorkflowCancelItem", "WorkflowCompleteTakeover",
	"WorkflowCreateAutomation", "WorkflowCreateItemPR",
	"WorkflowDeleteAutomation", "WorkflowDiscardItem",
	"WorkflowDiscardPreview", "WorkflowDiscussPR", "WorkflowDropUnit",
	"WorkflowFetchPRReviewComments", "WorkflowMergeItem", "WorkflowPauseItem",
	"WorkflowRequestSoftStop", "WorkflowRerunItem", "WorkflowResolveGate",
	"WorkflowResumeItem", "WorkflowRetryFailedUnits", "WorkflowRetryUnit",
	"WorkflowRunAutomationNow", "WorkflowScheduleResume",
	"WorkflowSendPRReviewCommentsToThread", "WorkflowSetAutomationEnabled",
	"WorkflowSetGlobalPause", "WorkflowSetJobNotes", "WorkflowStartRun",
	"WorkflowTakeOverUnit", "WorkflowUnbindThread",
	"WorkflowUpdateAutomation", "WriteTerminal", "WriteThreadWorkspaceFile",
}

// TestLocalOnlyMethodsMatchTheFrozenPartition is the reachability-
// preservation gate. LocalOnlyMethods is now computed — from every
// method's scope, its step-up flag, and the transitional overrides — so
// the question this answers is whether that computation lands on the
// same set a human maintained by hand.
//
// A failure names the direction, because the two are not equally bad: a
// method that GAINED local-only is a remote client silently losing a
// surface, and one that LOST it is a privileged call answered over the
// LAN.
func TestLocalOnlyMethodsMatchTheFrozenPartition(t *testing.T) {
	frozen := make(map[string]bool, len(preScopeTableLocalOnly))
	for _, name := range preScopeTableLocalOnly {
		if frozen[name] {
			t.Errorf("%q is listed twice in preScopeTableLocalOnly", name)
		}
		frozen[name] = true
	}

	var gained, lost []string
	for name := range LocalOnlyMethods {
		if !frozen[name] {
			gained = append(gained, name)
		}
	}
	for name := range frozen {
		if !LocalOnlyMethods[name] {
			lost = append(lost, name)
		}
	}
	sort.Strings(gained)
	sort.Strings(lost)

	if len(gained) > 0 {
		t.Errorf("%d method(s) became local-only that were not: %v\n\n"+
			"A remote client loses these surfaces. Either the scope is wrong, or the change is "+
			"deliberate — in which case update preScopeTableLocalOnly in this commit.", len(gained), gained)
	}
	if len(lost) > 0 {
		t.Errorf("%d method(s) stopped being local-only: %v\n\n"+
			"These are answered over the LAN now. Either the scope is wrong, or the change is "+
			"deliberate — in which case update preScopeTableLocalOnly in this commit.", len(lost), lost)
	}
}

// TestTransitionalOverridesAreLoadBearing keeps the override map
// shrinking. An entry earns its place only by CONTRADICTING the
// derivation: once a scope changes so the two agree, the override says
// nothing and the reader has to re-derive that fact to find out. Stale
// scaffolding reads exactly like live scaffolding, so it fails here.
func TestTransitionalOverridesAreLoadBearing(t *testing.T) {
	known := make(map[string]MethodMeta, len(GeneratedMethods))
	for _, method := range GeneratedMethods {
		known[method.Name] = method
	}

	for name, override := range transitionalReachability {
		method, ok := known[name]
		if !ok {
			t.Errorf("transitionalReachability[%q] matches no generated method — typo or stale entry", name)
			continue
		}
		if override.Reason == "" {
			t.Errorf("transitionalReachability[%q] carries no reason; an override nobody argued for is one nobody can retire", name)
		}
		if derived := derivedLocalOnly(method); derived == override.LocalOnly {
			t.Errorf("transitionalReachability[%q] pins localOnly=%v, which is what scope %q already derives; delete the entry",
				name, override.LocalOnly, method.Scope)
		}
	}
}

// TestEveryGeneratedMethodCarriesADeclaredScope is the runtime-side
// half of the generator's completeness gate. methodgen refuses to emit
// an unannotated method, so this cannot fail from a missing annotation
// — what it catches is a scope that the tier table does not place,
// which would derive as local-only for a reason nobody wrote down.
func TestEveryGeneratedMethodCarriesADeclaredScope(t *testing.T) {
	for _, method := range GeneratedMethods {
		if !method.Scope.Valid() {
			t.Errorf("%s carries scope %q, which internal/transport/scopes.go does not declare", method.Name, method.Scope)
		}
	}
}

// TestStepUpMethodsAreTheSpecSet pins the step-up annotations to §4's
// list. Step-up is mandatory-or-nothing there: these are the calls that
// re-key the system or re-route every prompt, so one dropped //ao:stepup
// turns a required per-call proof into an ambient standing grant, and
// nothing else in the tree would notice.
func TestStepUpMethodsAreTheSpecSet(t *testing.T) {
	want := map[string]string{
		"MintDevicePairing":            "minting a pairing link",
		"SetNetworkSettings":           "network bind / exposure change",
		"SetProviderCustomEnvVar":      "provider custom-env write",
		"DeleteProviderCustomEnvVar":   "provider custom-env write",
		"SetThreadMcpServerEnabled":    "MCP config write",
		"SetWorkspaceMcpServerEnabled": "MCP config write",
		"SetWSLDistroPreference":       "WSL distro preference",
		"SetProjectWorktreeSetup":      "worktree-setup recipe write: stores argv that runs unattended on every worktree cut",
	}
	got := map[string]bool{}
	for _, method := range GeneratedMethods {
		if method.StepUp {
			got[method.Name] = true
		}
	}
	for name, why := range want {
		if !got[name] {
			t.Errorf("%s lost its //ao:stepup annotation (%s, docs/specs/remote-access.md §4)", name, why)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s is annotated //ao:stepup and §4 does not list it; widen the spec set here deliberately or drop the directive", name)
		}
	}
}
