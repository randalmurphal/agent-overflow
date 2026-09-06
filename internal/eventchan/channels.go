package eventchan

// Channel is the exact name a frame's `channel` field carries on the
// wire. Its underlying type is string, so converting to and from the
// wire representation is free.
type Channel string

// String returns the wire spelling.
func (c Channel) String() string { return string(c) }

// chatbar:* — the composer chat bar's app-wide state, both halves
// persisted in internal/store's chat_bar.go and both written from the
// same toolbar.
//
// ChatBarFavorites carries the whole starred list, exactly as
// ListChatBarFavorites answers it: the list is short, unkeyed and
// replaced wholesale, so the newest frame is the entire answer.
//
// ChatBarNewThreadDefaults carries the seed a "+ New" composer shows
// before any thread row exists, together with the project whose draft
// placeholders adopt it — which is the same set the writing client
// applies it to, so its own echo is a repeat of what it already did.
const (
	ChatBarFavorites         Channel = "chatbar:favorites"
	ChatBarNewThreadDefaults Channel = "chatbar:new-thread-defaults"
)

// devserver:* — this backend's dev-server list: which loopback ports are
// serving pages, which thread owns each, and which of them a preview URL
// can be minted for. Per-backend and whole-state, so the newest frame is
// the only one worth having.
const (
	DevServerList Channel = "devserver:list"
)

// discussion:* — multi-agent deliberation channels, plus the definitions
// those deliberations are built from.
//
// DiscussionDefinitionsChanged is a payload-less refetch nudge for the
// persisted discussion DEFINITIONS (create / update / delete), the same
// shape workflow:definitions-changed carries for workflow definitions.
const (
	DiscussionDefinitionsChanged Channel = "discussion:definitions-changed"
	DiscussionMessage            Channel = "discussion:message"
	DiscussionState              Channel = "discussion:state"
)

// draft:* — one frame per persisted composer-draft write, naming the thread
// and the screen that wrote it. Carries no draft TEXT: receivers re-read
// through GetDraft, which takes `threads:operate` for the disclosure reason
// (in-progress user-typed work) and is the same grant the registry gates
// this channel on.
const (
	DraftUpdated Channel = "draft:updated"
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

// keybindings:* — a payload-less refetch nudge fired after the user
// keybindings file is rewritten or reset. No bindings ride it: every
// receiver re-reads through GetKeybindings, which is also where the
// user-file read error the result carries comes from.
const (
	KeybindingsUpdated Channel = "keybindings:updated"
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

// project:* — one frame per project row a persisted write moved, carrying
// the row and what the receiver must do with it. Same vocabulary and same
// rules as thread:updated (triage.ProjectUpdateEvent).
const (
	ProjectUpdated Channel = "project:updated"
)

// provider:* — everything a provider session produces: the transcript
// stream, turn lifecycle, approvals, queue state, account identity.
const (
	ProviderAccount                Channel = "provider:account"
	ProviderAccountUsageError      Channel = "provider:account_usage_error"
	ProviderAccountsChanged        Channel = "provider:accounts_changed"
	ProviderApproval               Channel = "provider:approval"
	ProviderBackgroundTaskState    Channel = "provider:background_task_state"
	ProviderBackgroundTasksChanged Channel = "provider:background_tasks_changed"
	ProviderCommandLifecycle       Channel = "provider:command_lifecycle"
	ProviderCommands               Channel = "provider:commands"
	ProviderCompacting             Channel = "provider:compacting"
	ProviderFastMode               Channel = "provider:fast_mode"
	ProviderItemEvent              Channel = "provider:item_event"
	ProviderLogin                  Channel = "provider:login"
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

// browser:* — managed browser artifact install progress, the live in-app
// companion surface, and the embedded-pane host directive.
//
// BrowserHost is the odd one out: like updater:install and webview:trim it
// is an imperative command for the Windows launcher, not a notification.
// Its frames create, move, show, hide and destroy real browser windows
// inside the launcher's own window, and the launcher answers on the same
// connection with the BrowserHostReport RPC. Payload shape:
// internal/webview2host.Directive.
const (
	BrowserCompanionState Channel = "browser:companion-state"
	BrowserHost           Channel = "browser:host"
)

// backend:* — the set of OTHER machines this installation drives.
//
// BackendAttach is how one attach ended. The RPC that starts a pairing
// returns the verification number immediately and cannot wait for the
// owner of the far machine to match it: that window is ten minutes. This
// channel is the other half, and carries at most one frame per attach.
//
// BackendSetChanged is every OTHER mutation of that set — a removal, a
// rename — so two pages open on this host do not diverge. Its own channel
// rather than a second meaning on backend:attach: one says how a pairing
// ceremony ended, the other says the list changed.
const (
	AccessDevicesChanged  Channel = "access:devices-changed"
	BackendNameChanged    Channel = "backend:name-changed"
	BackendAttach         Channel = "backend:attach"
	BackendSetChanged     Channel = "backend:set-changed"
	AgentComputersChanged Channel = "agent-computers:changed"
	// ThreadTransfer carries bounded public operation status; never grants,
	// activation secrets or private installation recipes.
	ThreadTransfer Channel = "thread:transfer"
)

// review:* — a payload-less-per-set refetch nudge for the inline review
// comments a thread holds: the proposed-plan set (keyed by plan item) and
// the diff-review set (keyed by scope + source). No comment bodies ride
// it — a delete is a DELETE-OR-RESOLVE depending on whether the comment
// was sent, so only a re-read can say what the set now holds.
const (
	ReviewCommentsChanged Channel = "review:comments-changed"
)

// session-import:* — one frame per session an import run finishes, plus
// exactly one terminal frame. The frames name provider-home file paths,
// which is why the registry gates the channel on `threads:operate` — the
// grant ListImportableSessions and ImportSessions already carry.
const (
	SessionImportProgress Channel = "session-import:progress"
)

// service:* — what a supervised serve host says about updating itself.
//
// update-outcome is the launch half: one frame per boot at most,
// published the moment the activation gate opens, naming the update this
// boot is the outcome of. It is what makes "the update succeeded" mean
// the NEW version answered rather than that the old one stopped — the
// client that asked holds the update id and waits for it to come back.
//
// update-status is the half BEFORE that: the whole ServiceUpdateStatus
// shape on every phase change of a remote update flow (resolving,
// downloading with progress, verifying, staging, requested) and on every
// failure. The two are one story told by two processes: this one runs
// until the supervisor stops it, and the version that comes back
// publishes the outcome.
const (
	ServiceUpdateOutcome Channel = "service:update-outcome"
	ServiceUpdateStatus  Channel = "service:update-status"
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
//
// TerminalOpened is the other half of TerminalExit: one frame per PTY
// this backend starts, carrying the same terminal.SessionSummary
// ListTerminals answers with, so a second client learns a terminal
// exists rather than dropping its output for an id it never saw. Close
// needs no channel of its own — closing a session kills the process, so
// TerminalExit already carries it.
const (
	TerminalExit   Channel = "terminal:exit"
	TerminalOpened Channel = "terminal:opened"
	TerminalOutput Channel = "terminal:output"
)

// thread-group:* — the sidebar thread group (migration v76). Its own
// family rather than a thread:* member because a group is NOT a thread:
// the frame carries a store.ThreadGroup and an action, and no thread id.
const (
	ThreadGroupUpdated Channel = "thread-group:updated"
)

// thread:* — thread-row state every viewer renders.
const (
	ThreadErrorNotice        Channel = "thread:error_notice"
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
// registry gates it on `terminal:operate`, like terminal:output).
const (
	WorktreeSetup Channel = "worktree:setup"
)
