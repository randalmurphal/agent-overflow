package main

import (
	"sort"

	"agent-overflow/internal/eventchan"
)

// The channel vocabulary `events channels` prints, and the list
// `events tail|await|count` check a --channel against before warning.
//
// Go cannot enumerate a package's constants at runtime, so this is a
// hand-kept roll call of internal/eventchan's — but it names the
// CONSTANTS, never their strings, so a rename fails the compile here.
// The other half of the drift (a channel ADDED there and not listed
// here) is caught by TestKnownChannelsCoversTheEventChannelRegistry,
// which AST-parses that package and diffs it against this slice — the
// same shape internal/transport uses to keep its own policy table in
// step with the same source.
//
// The list is what a CHECK reads, and the check is a warning rather than
// a refusal. The harness publishes onto caller-named channels through an
// explicit escape hatch, so "not in the registry" means "the backend
// does not emit this by itself", not "this cannot carry traffic".
func eventChannelConstants() []eventchan.Channel {
	return []eventchan.Channel{
		eventchan.BackendAttach,
		eventchan.BrowserCompanionState,
		eventchan.BrowserHost,
		eventchan.DevServerList,
		eventchan.DiscussionMessage,
		eventchan.DiscussionState,
		eventchan.DraftUpdated,
		eventchan.GitStatus,
		eventchan.HarnessMock,
		eventchan.HarnessPerf,
		eventchan.HarnessReplay,
		eventchan.HarnessUIQuery,
		eventchan.HighlightDiffSeed,
		eventchan.HighlightSeed,
		eventchan.MCPOAuthCompleted,
		eventchan.MCPStatus,
		eventchan.NotificationActivated,
		eventchan.NotificationSend,
		eventchan.PowerKeepAwake,
		eventchan.PRUpdated,
		eventchan.ProjectUpdated,
		eventchan.ProviderAccount,
		eventchan.ProviderAccountUsageError,
		eventchan.ProviderApproval,
		eventchan.ProviderBackgroundTaskState,
		eventchan.ProviderBackgroundTasksChanged,
		eventchan.ProviderCommandLifecycle,
		eventchan.ProviderCommands,
		eventchan.ProviderCompacting,
		eventchan.ProviderFastMode,
		eventchan.ProviderItemEvent,
		eventchan.ProviderLogin,
		eventchan.ProviderModelFallback,
		eventchan.ProviderQueueFlushed,
		eventchan.ProviderQueueRestored,
		eventchan.ProviderQueueStateChanged,
		eventchan.ProviderSessionAccount,
		eventchan.ProviderSessionDied,
		eventchan.ProviderStatus,
		eventchan.ProviderSubagentProgress,
		eventchan.ProviderTerminalOutput,
		eventchan.ProviderTodoUpdate,
		eventchan.ProviderTurnCompleted,
		eventchan.ProviderTurnStarted,
		eventchan.ProviderUsage,
		eventchan.ProviderUserInput,
		eventchan.SessionImportProgress,
		eventchan.ServiceUpdateOutcome,
		eventchan.ServiceUpdateStatus,
		eventchan.SettingsUpdated,
		eventchan.SpinnerChanged,
		eventchan.ThemeChanged,
		eventchan.SystemStats,
		eventchan.TerminalExit,
		eventchan.TerminalOutput,
		eventchan.ThreadErrorNotice,
		eventchan.ThreadModeChanged,
		eventchan.ThreadRuntimeModeChanged,
		eventchan.ThreadTitleGeneration,
		eventchan.ThreadUpdated,
		eventchan.UpdaterDownloadStarted,
		eventchan.UpdaterError,
		eventchan.UpdaterInstall,
		eventchan.UpdaterInstalling,
		eventchan.UpdaterProgress,
		eventchan.UpdaterReady,
		eventchan.UpdaterVerifying,
		eventchan.WebviewTrim,
		eventchan.UsageThreadCost,
		eventchan.UserMessageReverted,
		eventchan.WorkflowDefinitionsChanged,
		eventchan.WorkflowEngineState,
		eventchan.WorkflowError,
		eventchan.WorkflowGateNotify,
		eventchan.WorkflowItemState,
		eventchan.WorkflowPhaseState,
		eventchan.WorkflowSoftStop,
		eventchan.WorktreeSetup,
	}
}

// knownChannels is the same list as sorted wire spellings.
func knownChannels() []string {
	constants := eventChannelConstants()
	names := make([]string, 0, len(constants))
	for _, channel := range constants {
		names = append(names, string(channel))
	}
	sort.Strings(names)
	return names
}
