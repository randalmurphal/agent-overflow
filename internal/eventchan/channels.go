package eventchan

// Channel is the exact name a frame's `channel` field carries on the
// wire. Its underlying type is string, so converting to and from the
// wire representation is free.
type Channel string

// String returns the wire spelling.
func (c Channel) String() string { return string(c) }

// discussion:* — multi-agent deliberation channels.
const (
	DiscussionMessage Channel = "discussion:message"
	DiscussionState   Channel = "discussion:state"
)

// git:* — per-workspace git status streams, keyed by canonical absolute
// workspace path.
const (
	GitStatus Channel = "git:status"
)

// harness:* — the agent test harness's own progress channels. Note the
// harness ALSO publishes onto arbitrary caller-named channels through an
// explicit Channel(...) conversion at the call site; a name that matches
// a registered channel inherits that row's audience (the harness is the
// intended forger, gated by --harness/--soak + LocalOnly), and an
// unrecognized name lands on transport's fail-closed default.
const (
	HarnessMock    Channel = "harness:mock"
	HarnessPerf    Channel = "harness:perf"
	HarnessReplay  Channel = "harness:replay"
	HarnessUIQuery Channel = "harness:ui-query"
)

// highlight:* — syntax-span cache warmers pushed alongside streaming
// text and persisted diffs.
const (
	HighlightDiffSeed Channel = "highlight:diff_seed"
	HighlightSeed     Channel = "highlight:seed"
)

// mcp:* — MCP server status and OAuth completion.
const (
	MCPOAuthCompleted Channel = "mcp:oauth-completed"
	MCPStatus         Channel = "mcp:status"
)

// notification:* — the OS-notification pipe between this backend and a
// host-side presenter (the Windows launcher). Both spellings are also
// exported from internal/notify as plain strings, defined AS these
// constants, because the launcher subscribes by string.
const (
	NotificationActivated Channel = "notification:activated"
	NotificationSend      Channel = "notification:send"
)

// power:* — host power-management directives. Like updater:install and
// webview:trim these are imperative commands the Windows launcher acts
// on, not notifications. PowerKeepAwake carries the LEVEL the OS sleep
// inhibitor should sit at ("off" | "system" | "display"), not an edge:
// the launcher must converge on the latest frame after any reconnect,
// which is why its policy row is latest-only rather than ephemeral.
const (
	PowerKeepAwake Channel = "power:keepawake"
)

// pr:* — pull-request detail and review-thread pushes.
const (
	PRUpdated Channel = "pr:updated"
)

// provider:* — everything a provider session produces: the transcript
// stream, turn lifecycle, approvals, queue state, account identity.
const (
	ProviderAccount                Channel = "provider:account"
	ProviderAccountUsageError      Channel = "provider:account_usage_error"
	ProviderApproval               Channel = "provider:approval"
	ProviderBackgroundTaskState    Channel = "provider:background_task_state"
	ProviderBackgroundTasksChanged Channel = "provider:background_tasks_changed"
	ProviderCommandLifecycle       Channel = "provider:command_lifecycle"
	ProviderCommands               Channel = "provider:commands"
	ProviderCompacting             Channel = "provider:compacting"
	ProviderFastMode               Channel = "provider:fast_mode"
	ProviderItemEvent              Channel = "provider:item_event"
	ProviderModelFallback          Channel = "provider:model_fallback"
	ProviderQueueFlushed           Channel = "provider:queue_flushed"
	ProviderQueueRestored          Channel = "provider:queue_restored"
	ProviderQueueStateChanged      Channel = "provider:queue_state_changed"
	ProviderSessionAccount         Channel = "provider:session_account"
	ProviderSessionDied            Channel = "provider:session_died"
	ProviderStatus                 Channel = "provider:status"
	ProviderSubagentProgress       Channel = "provider:subagent_progress"
	ProviderTerminalOutput         Channel = "provider:terminal_output"
	ProviderTodoUpdate             Channel = "provider:todo_update"
	ProviderTurnCompleted          Channel = "provider:turn_completed"
	ProviderTurnStarted            Channel = "provider:turn_started"
	ProviderUsage                  Channel = "provider:usage"
	ProviderUserInput              Channel = "provider:user_input"
)

// browser:* — managed browser artifact install progress and the live in-app
// companion surface.
const (
	BrowserCompanionState  Channel = "browser:companion-state"
	BrowserInstallProgress Channel = "browser:install-progress"
)

// session-import:* — one frame per session an import run finishes, plus
// exactly one terminal frame. The frames name provider-home file paths,
// which is why the registry keeps the channel loopback-only.
const (
	SessionImportProgress Channel = "session-import:progress"
)

// settings:* — one frame per tier a persisted settings write moved,
// carrying the tier and the changed KEY NAMES only. No values ride it:
// GetSettings redacts endpoint tokens and sensitive environment values,
// and a push carrying values would be the one path around that.
// Receivers re-read through GetSettings, the same refetch-nudge shape
// usage:thread_cost uses.
const (
	SettingsUpdated Channel = "settings:updated"
)

// spinner:* / theme:* — payload-less refetch nudges from the two
// client-asset directory watchers.
const (
	SpinnerChanged Channel = "spinner:changed"
	ThemeChanged   Channel = "theme:changed"
)

// system:* — host CPU/memory samples for the sidebar footer.
const (
	SystemStats Channel = "system:stats"
)

// terminal:* — local PTY session bytes and lifecycle.
const (
	TerminalExit   Channel = "terminal:exit"
	TerminalOutput Channel = "terminal:output"
)

// thread:* — thread-row state every viewer renders.
const (
	ThreadModeChanged        Channel = "thread:mode_changed"
	ThreadRuntimeModeChanged Channel = "thread:runtime_mode_changed"
	ThreadTitleGeneration    Channel = "thread:title_generation"
	ThreadUpdated            Channel = "thread:updated"
)

// updater:* — the self-update lifecycle. Six of these are bridged from
// Wails updater event names by internal/appupdate's updaterEventBridge;
// UpdaterInstall is the imperative directive the Windows launcher acts
// on, and internal/selfupdate re-exports its spelling as a string.
const (
	UpdaterDownloadStarted Channel = "updater:download-started"
	UpdaterError           Channel = "updater:error"
	UpdaterInstall         Channel = "updater:install"
	UpdaterInstalling      Channel = "updater:installing"
	UpdaterProgress        Channel = "updater:progress"
	UpdaterReady           Channel = "updater:ready"
	UpdaterVerifying       Channel = "updater:verifying"
)

// webview:* — directives to the process that owns the webview. Like
// updater:install these are imperative commands the Windows launcher acts
// on, not notifications; WebviewTrim asks it to run a memory-reducing GC
// in the renderer (CDP HeapProfiler.collectGarbage) while the window
// stays visible — the trigger Blink otherwise only gets from a hidden
// window or OS memory pressure, neither of which ever fires for an
// always-visible desktop app.
const (
	WebviewTrim Channel = "webview:trim"
)

// usage:* — per-thread cost refetch nudge fired after a provider figure
// lands. It carries only the thread id: every usage surface refetches
// through GetUsageStats rather than trusting a pushed number, the same
// rule provider:turn_completed's refresh bump follows.
const (
	UsageThreadCost Channel = "usage:thread_cost"
)

// user_message:* — timeline truncation directive for fork-and-revert.
const (
	UserMessageReverted Channel = "user_message:reverted"
)

// workflow:* — the workflows engine's run/phase/unit state, plus its
// error toasts and gate notifications.
const (
	WorkflowDefinitionsChanged Channel = "workflow:definitions-changed"
	WorkflowEngineState        Channel = "workflow:engine-state"
	WorkflowError              Channel = "workflow:error"
	WorkflowGateNotify         Channel = "workflow:gate-notify"
	WorkflowItemState          Channel = "workflow:item-state"
	WorkflowPhaseState         Channel = "workflow:phase-state"
	WorkflowSoftStop           Channel = "workflow:soft-stop"
)

// worktree:* — per-project worktree setup command output. Its own
// channel rather than a phase discriminator on an existing one: only the
// setup panel subscribes, and the frames carry local command output (the
// registry keeps it loopback-only, like terminal:output).
const (
	WorktreeSetup Channel = "worktree:setup"
)
