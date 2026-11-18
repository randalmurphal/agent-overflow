package app

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/accountswitch"
	"agent-overflow/internal/agenthome"
	"agent-overflow/internal/appidentity"
	"agent-overflow/internal/appupdate"
	"agent-overflow/internal/assetwatch"
	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/attachment"
	"agent-overflow/internal/cilog"
	"agent-overflow/internal/claudeapp"
	"agent-overflow/internal/codexapp"
	"agent-overflow/internal/codexthread"
	"agent-overflow/internal/dictation"
	"agent-overflow/internal/forgeattach"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitapp"
	"agent-overflow/internal/gitwatch"
	"agent-overflow/internal/highlightapp"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/keybindings"
	"agent-overflow/internal/keyedlock"
	"agent-overflow/internal/localimage"
	"agent-overflow/internal/logging"
	"agent-overflow/internal/mcpapp"
	"agent-overflow/internal/network"
	obsotel "agent-overflow/internal/observability/otel"
	"agent-overflow/internal/observability/replay"
	"agent-overflow/internal/orphanreaper"
	"agent-overflow/internal/power"
	"agent-overflow/internal/projectapp"
	"agent-overflow/internal/provider/claude/claudefake"
	"agent-overflow/internal/provideraccountapp"
	"agent-overflow/internal/providerdiscoveryapp"
	"agent-overflow/internal/providerlifecycleapp"
	"agent-overflow/internal/providerupdate"
	"agent-overflow/internal/providerupdate/updatefake"
	"agent-overflow/internal/readaloud"
	"agent-overflow/internal/remotejobs"
	"agent-overflow/internal/reviewimage"
	"agent-overflow/internal/sessionruntime"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/shellrun"
	"agent-overflow/internal/soundlib"
	"agent-overflow/internal/spinner"
	"agent-overflow/internal/store"
	"agent-overflow/internal/terminal"
	"agent-overflow/internal/textgen"
	"agent-overflow/internal/theme"
	"agent-overflow/internal/threadapp"
	"agent-overflow/internal/threadtitleapp"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/uitrace"
	"agent-overflow/internal/workflowapp"
	"agent-overflow/internal/workspacefiles"
	"agent-overflow/internal/worktreeapp"
	"agent-overflow/internal/worktreesetupapp"
	"agent-overflow/internal/worktreewatch"
)

// ErrShuttingDown is returned from binding entry points once Shutdown has
// started. Callers should surface this as a terminal state — no retry will
// succeed because the app is tearing down.
var ErrShuttingDown = errors.New("app: shutting down")

// updateRestartWatchdogDelay leaves the App's bounded graceful-shutdown steps
// room to finish while staying below the swap helper's 30-second parent-exit
// timeout. This is root lifecycle policy, passed explicitly to appupdate.
const updateRestartWatchdogDelay = 25 * time.Second

// App is the primary Wails-bound struct, registered as a v3 service
// in desktop mode and driven directly via Start() in the headless WSL
// backend mode. The App itself does not import the Wails application
// package; the desktop-mode bindings live in app_desktop.go behind the
// !nogui build tag so the WSL payload can compile without libwebkit2gtk
// and other GTK runtime dependencies pulled in by Wails' cgo bindings.
type App struct {
	deviceName        *appidentity.DeviceName
	deviceNameWatcher *assetwatch.DeviceNameWatcher
	// version is the build-stamped release supplied by the executable shell.
	// Keeping it on the service makes the integration package importable while
	// the root main package remains the ldflags target.
	version string
	// saveDialog opens the native save-file dialog. Wired by
	// ServiceStartup in desktop mode (app_desktop.go); left nil in the
	// headless WSL backend, where there is no native window to attach a
	// dialog to and the frontend uses a download fallback instead.
	saveDialog savePayloadPicker
	// setWindowBackground paints the native window chrome (the color
	// visible while a resize outruns the webview). Wired by
	// ServiceStartup in desktop mode (app_desktop.go); left nil in the
	// headless WSL backend, whose window lives in the Windows launcher
	// process, so SetWindowBackgroundColor there is a validated no-op.
	setWindowBackground func(red, green, blue uint8)
	// osNotifications is the single platform-routing seam behind notifyOS.
	// Desktop boot installs the in-process Wails service adapter; the WSL
	// headless and harness boots install the transport bridge. Tests may
	// leave it nil to exercise the same visible degraded error without
	// pulling in a platform service.
	osNotifications osNotificationSender
	// notifications is the event→notification mapping's coordination: the
	// ordered queue its moments dispatch on, and the small edge state the
	// pure mapping is handed. See app_notification_mapping.go.
	notifications notificationDispatch
	// workerPrompts routes the prompts of a spawned thread to its spawner
	// (app_worker_prompts.go).
	workerPrompts workerPrompts
	// resting holds the threads being brought to rest, which take no
	// app-written message and start no work meanwhile (app_thread_rest_fence.go).
	resting threadRestFence
	// push is the PHONE half of the same path: its own ordered queue, the
	// sender this backend wakes phones through, and the standing fault the
	// owner reads. Its own queue because a send to Google can hang for its
	// whole timeout and the desktop's toast must not wait behind it. See
	// app_push.go.
	push pushDispatch
	// harnessPush is the recorder a harness boot installs in place of the
	// FCM sender, and the only harness-shaped field on this struct. It is
	// nil in every shipping boot. See app_push_harness.go.
	harnessPush atomic.Pointer[harnessPushSender]
	store       *store.Store
	git         *gitops.Core
	gitWatch    *gitwatch.Manager
	// worktreeWatch observes every project's worktree registry so a checkout
	// removed outside this process reattaches its threads. Nil in fixtures
	// that never ran startup; every use is nil-safe. See app_worktree_watch.go.
	worktreeWatch *worktreewatch.Manager
	// worktreeWatchArmed opens once the activation gate has: a reattach sweep
	// acts on rows and sessions, so no project is watched before it.
	worktreeWatchArmed atomic.Bool
	// providerWorktreeExits maps a thread id to the worktree its own CLI
	// reported removing (`ExitWorktree remove`) until the app has followed
	// the move. The registry watch can see that removal first and must not
	// treat the thread that performed it as an outside removal's victim.
	// See app_worktree_follow.go.
	providerWorktreeExits sync.Map
	// pendingWorktreeExits covers the same thread earlier: from the
	// `ExitWorktree` tool_use until its result. See app_worktree_follow.go.
	pendingWorktreeExits pendingWorktreeExits
	// appWorktreeRemovals names the worktrees the app is removing without
	// the in-app removal's gates (workflow cleanup). See app_worktree.go.
	appWorktreeRemovals appWorktreeRemovals
	// gitApp owns gitwatch wire fan-out and the unattended background-fetch
	// lifecycle. This shell retains the stable Wails façades and event projection.
	gitAppOnce sync.Once
	gitApp     *gitapp.Service
	// worktreeApp owns store-backed worktree membership and safety queries.
	// This shell retains destructive git operations and their stable Wails façades.
	worktreeAppOnce sync.Once
	worktreeApp     *worktreeapp.Service
	// projectApp owns project persistence and workspace-membership policy.
	// This shell retains live workflow coordination and destructive git execution.
	projectAppOnce sync.Once
	projectApp     *projectapp.Service
	settings       *settings.Service
	triage         *triage.Router
	// workflowApp owns persisted agent-facing workflow reads, wake/event
	// coordination, and the engine/autoresume application runtime.
	workflowAppOnce sync.Once
	workflowApp     *workflowapp.Service
	// themeWatcher watches <configDir>/themes so an agent (or a text
	// editor) rewriting a theme file reaches the UI without a restart.
	// Nil when the watcher could not start — live reload is a
	// convenience on top of GetThemeFiles, never a requirement.
	themeWatcher *assetwatch.ThemeWatcher
	// spinnerWatcher watches <configDir>/spinners so a sprite dropped
	// into the directory reaches the composer's working indicator
	// without a restart. Nil on the same terms as themeWatcher — live
	// reload is a convenience on top of GetSpinnerFiles.
	spinnerWatcher *assetwatch.SpinnerWatcher
	// soundWatcher watches <configDir>/sounds so a custom notification
	// cue added, replaced or removed reaches every attached screen
	// without a restart. Nil on the same terms as themeWatcher; unlike
	// spinnerWatcher it also suppresses this process's own cue writes,
	// which are RPCs rather than hand edits.
	soundWatcher *assetwatch.SoundWatcher
	// promptWatcher watches <configDir>/prompts so a system prompt override
	// edited as a file reaches attached screens and live Claude sessions
	// without a save in the app. Nil on the same terms as themeWatcher.
	promptWatcher *assetwatch.PromptWatcher
	// turnObservers fans provider events out to internal App features after
	// triage handling has been attempted.
	turnObservers appTurnObserverState
	// providerEvents carries every provider event from the session's
	// onEvent callback to its handling, one FIFO per thread, so triage
	// never runs on a provider read loop (app_provider_event_queue.go).
	providerEvents providerEventQueues
	// browser owns the built-in provider-neutral browser and its MCP bridge.
	browser appBrowserState
	// backends is the set of OTHER machines this installation has
	// attached, carried on this backend's own listener
	// (internal/attachedbackends). Nil in every boot that has no device
	// profile directory to keep pairings in — a test fixture, and the
	// harness — and every attached-backend method is a plain refusal
	// then, never a panic.
	backends   *attachedbackends.Manager
	sshSetup   appSSHSetup
	transfers  appThreadTransfers
	terminals  *terminal.Manager
	remoteJobs *remotejobs.Manager
	// shellCommands owns the composer shell commands in flight.
	shellCommands *shellrun.Service
	remoteMCP     appRemoteMCP
	// threadMCP owns the ao-thread-tools server and the threadtools
	// contract bound to this app. Zero value ready; built on first use.
	threadMCP appThreadMCP
	// workflowMCP is ao-workflow-tools (app_workflow_tools_mcp.go).
	workflowMCP appWorkflowMCP
	// threadRequests holds the agent thread-request ledger's in-process
	// state: the parked waits, the per-token settle lock, and which
	// threads own a receipt that a turn end could settle. Zero value
	// ready. See app_thread_tools_wait.go.
	threadRequests threadRequestState
	// peerAdmissions is the tokens a paired computer's spawn, send or ask
	// has a handler in flight for here (app_thread_tools_admissions.go).
	// Zero value ready.
	peerAdmissions peerAdmissions
	// threadAgentEmits coalesces the Agents rows' change frames
	// (app_thread_agent_rows_emit.go).
	threadAgentEmits threadAgentEmits
	// liveWorkers is the threads last found not at rest, which keep the
	// local spawn that made each open (app_thread_agent_live.go).
	liveWorkers liveWorkers
	// restRevisions is the revision of what memory holds of a thread's
	// rest, for the decisions on rest that cannot be taken back
	// (app_thread_rest_assess.go).
	restRevisions restRevisions
	// threadAgentPeers is the last agent_rows answer for each spawn on a
	// paired computer (app_thread_agent_rows_remote.go).
	threadAgentPeers threadAgentPeerCache
	ownerActivity    ownerActivity
	ownerUpdates     ownerUpdates
	ownerReads       ownerReads
	// agentRoles keeps each standing agent's role line until its
	// instructions file changes, for the sends, spawns, settlements and
	// thread reads that name it. Read it through assistantRoles, which
	// makes the store's deletion of an assistant record forget its home.
	agentRoles     agenthome.RoleCache
	agentRolesOnce sync.Once
	// threadRequestsWG joins the request sweep goroutine at shutdown, the
	// way remoteWatchWG joins the remote completion watcher, with the
	// settlements, wake runs and emits it counts. They start from any
	// goroutine while it may be waited on, so it is a workGroup.
	threadRequestsWG workGroup
	// threadRequestSettling counts, inside threadRequestsWG, the
	// settlements and wake deliveries in flight (goThreadRequestSettling),
	// so a caller can wait for what a settlement sets off without waiting
	// out the sweep.
	threadRequestSettling workGroup
	// agentWakes is the wake supervisor's in-process state: the runs and
	// long-running watch processes in flight. Zero value ready. See
	// app_agent_wakes.go.
	agentWakes agentWakeState
	// agentNoticeWork joins what notices run after they are accepted.
	agentNoticeWork appAgentNoticeWorkState
	// memoryReports joins the memory reads behind warning rows.
	memoryReports appMemoryReportsState
	// assistantHandOffs holds each assistant's running handoff and the
	// messages it carries (StartNextAssistantThread,
	// recoverAssistantSessionLoss).
	assistantHandOffs assistantHandOffState
	// assistantsSwitch converges this computer on its Assistants setting
	// (app_assistants_switch.go).
	assistantsSwitch assistantsSwitchState
	// projectIdentityWG joins the boot pass that re-derives project
	// repository identity (startProjectIdentityRefresh).
	projectIdentityWG sync.WaitGroup
	// startupProbeWG joins the startup provider status probe
	// (probeStartupProviderStatuses), the startup account probes and the
	// Claude probe's retry loop (probeStartupAccountInfo).
	startupProbeWG sync.WaitGroup
	// unattendedLoops joins the goroutines the boot's unattended work leaves
	// running that no narrower owner joins: the worktree watch's first read,
	// the provider binary watcher, the domain certificate loop and the
	// tailnet loop. Each ends with the app context.
	unattendedLoops workGroup
	// permissionModeFollowWG joins the goroutines that move a thread onto
	// the permission mode Claude moved to (followClaudePermissionMode).
	permissionModeFollowWG sync.WaitGroup
	// threadSearchIndex is the boot-time search index build.
	threadSearchIndex threadSearchIndexBuild
	// pendingThreadDeletes completes interrupted thread deletes at boot.
	pendingThreadDeletes pendingThreadDeletes
	// archiveWalks counts the archive walks under way
	// (app_archive_walks.go).
	archiveWalks archiveWalks
	// ownerOps delivers what archives and deletes owe the paired computers
	// their threads spawned on (app_thread_owner_ops.go).
	ownerOps ownerOpDelivery
	// deleteDecisions are the running deletes' decision gates, which the
	// app's own starts wait at (app_thread_delete.go).
	deleteDecisions deleteDecisions
	// terminalThreads ends each terminal thread when its last shell exits
	// (app_terminal_threads.go).
	terminalThreads terminalThreads
	// firstReads holds heavy post-boot work until a client has read its
	// catalogs. See app_first_reads.go.
	firstReads    firstReadsGate
	remoteWatchWG sync.WaitGroup
	// remoteWatchNudges wakes the remote command watcher's loops
	// (app_remote_watch_loop.go).
	remoteWatchNudges remoteWatchNudges
	// peerExports records the exports rendered for paired computers
	// (app_thread_tools_export.go).
	peerExports      peerExports
	remoteStartsOnce sync.Once
	remoteStarts     *keyedlock.Registry
	// reviewSubmits, postedReviewDrafts and reviewAttempts are
	// SubmitPRReview's per-PR submit lock, its posted-but-unmarked drafts
	// and its attempts' outcomes (app_pr_review_submit.go). Zero value
	// ready.
	reviewSubmitsOnce  sync.Once
	reviewSubmits      *keyedlock.Registry
	postedReviewDrafts postedReviewDrafts
	reviewAttempts     writeAttempts[SubmitPRReviewResult]
	// gitActionAttempts are the outcomes of the git writes a client can
	// lose the answer of: commit, push, pull, create PR (app_git_attempts.go).
	// Zero value is ready.
	gitActionAttempts writeAttempts[gitops.GitActionResult]
	// providerTerminals is the per-connection take-control bookkeeping for
	// claude-tui PTYs: which caller armed which attachment, so a dead socket
	// releases exactly its own claim and its input lease. Zero value ready.
	// See app_claudetui_terminal.go.
	providerTerminals providerTerminalAttachments
	attachments       *attachment.Store
	workspaceFiles    *workspacefiles.Searcher
	logger            *logging.Logger
	// engineLogger is the workflow run-lifecycle log. Separate from `logger`
	// because it is always on: the provider-event log is a debugging opt-in,
	// while a park's diagnosis has to be readable without having enabled
	// anything before the run parked.
	engineLogger *logging.Logger
	telemetry    *obsotel.Provider
	replay       *replay.Manager
	configDir    string
	// providerAccounts owns managed-account metadata, native credentials, and
	// their complete lock/fingerprint/reconcile transaction boundary. Root
	// retains only provider sessions and the stable Wails façades.
	providerAccountsOnce sync.Once
	providerAccounts     *provideraccountapp.Manager
	// uiTracer is the dev-only JSONL render-trace appender. It's lazily
	// constructed from configDir the first time AppendUIRenderTraceBatch
	// runs so tests that build a bare App{configDir: t.TempDir()} stay
	// cheap, and so production wiring doesn't need an explicit init step.
	// uiTraceOnce serializes construction; uiTraceErr captures a failed
	// New so subsequent calls fail loudly instead of silently no-op'ing.
	uiTraceOnce sync.Once
	uiTracer    *uitrace.Tracer
	uiTraceErr  error
	// Settings that apply on the next start, as this session started with
	// them (app_launch_setting.go): the launcher's GPU vsync preference
	// (app_wsl.go) and OTLP tracing (app_observability_launch.go).
	launchGPUVsync launchValue
	launchTracing  launchValue
	// frontendErrors is the always-on JSONL appender for window-level
	// frontend runtime errors (onerror / unhandledrejection). Same lazy
	// construction contract as uiTracer.
	frontendErrorsOnce sync.Once
	frontendErrors     *uitrace.Tracer
	frontendErrorsErr  error
	// keybindings is the lazy-init persisted-config service backing the
	// three Keybindings bindings. Constructed from configDir on first
	// use; falls back to ~/.agent-overflow when configDir is empty so
	// early-boot RPCs still resolve to a writable path.
	keybindingsOnce sync.Once
	keybindings     *keybindings.Service
	keybindingsErr  error
	// theme is the lazy-init themes-directory service backing the theme
	// bindings. Same construction contract as keybindings above: one
	// Service per App, configDir-rooted, with the ~/.agent-overflow
	// fallback for an early-boot RPC.
	themeOnce sync.Once
	theme     *theme.Service
	themeErr  error
	// spinner is the lazy-init spinners-directory service backing
	// GetSpinnerFiles. Same construction contract as theme above: one
	// Service per App, configDir-rooted, with the ~/.agent-overflow
	// fallback for an early-boot RPC.
	spinnerOnce sync.Once
	spinner     *spinner.Service
	spinnerErr  error
	// sound is the lazy-init sounds-directory service backing the custom
	// notification-cue bindings. Same construction contract as spinner
	// above.
	soundOnce sync.Once
	sound     *soundlib.Service
	soundErr  error
	// eventBus is the Phase C transport that owns per-channel seq stamping
	// and fan-out to connected webview / remote clients. main.go wires it
	// in via SetEventBus; the atomic.Pointer means SetEventBus and
	// concurrent a.emit readers don't race even if wiring lands after
	// background goroutines have started. Production leaves this set;
	// tests that don't need a real bus leave it nil and observe emissions
	// via testEmitHook instead.
	eventBus atomic.Pointer[transport.EventBus]
	// providerLifecycle owns quota cache/poll/backoff state and session-account
	// event attribution. This shell retains provider-event triage ordering and exact
	// Wails/event façades.
	providerLifecycleOnce sync.Once
	providerLifecycle     *providerlifecycleapp.Service
	// accountSwitcher decides account auto-switches and continues after a
	// usage limit (app_account_autoswitch.go).
	accountSwitcherOnce    sync.Once
	accountSwitcher        *accountswitch.Coordinator
	accountSwitchStartOnce sync.Once
	// accountSwitchCheckInterval shortens the waiting-thread check for a
	// test. Zero keeps the product interval.
	accountSwitchCheckInterval time.Duration
	// switchedAccountProbeFn replaces the identity probe that follows a
	// Claude account switch in tests. Nil runs ProbeClaudeAccount.
	switchedAccountProbeFn func() error
	// forgeAttachCache holds the bytes of forge-hosted PR/MR attachments
	// between the bound method that fetched them and the ticketed byte
	// route that serves them (app_forge_attachments.go). Lazily
	// constructed under forgeAttachOnce, the same contract keybindings
	// and theme use: a bare App{} in a test is usable, and a boot that
	// never opens a review pane allocates nothing.
	forgeAttachOnce  sync.Once
	forgeAttachCache *forgeattach.Cache
	// localImageService resolves markdown-referenced image files and holds
	// them for the same byte route family (app_local_images.go). Lazy for
	// the reason forgeAttachCache is.
	localImageOnce    sync.Once
	localImageService *localimage.Service
	// reviewDiffImageStore holds the image bytes of review diff files for
	// the same byte route (app_review_diff_files.go). Lazy for the reason
	// forgeAttachCache is.
	reviewDiffImageOnce  sync.Once
	reviewDiffImageStore *reviewimage.Store
	// reviewDiffFiles remembers what each git-backed review diff open
	// pinned, for its file reads (app_review_diff_records.go).
	reviewDiffFiles reviewDiffRecords
	// reviewDiffPinTimeout overrides reviewDiffPinTimeout in tests.
	reviewDiffPinTimeout time.Duration
	// reviewDiffPinHook, set only by tests, runs at the start of each pin
	// with the pin's context.
	reviewDiffPinHook func(context.Context)
	// reviewDiffReadHook, set only by tests, runs in a file read after its
	// record is found and before it waits for the pin.
	reviewDiffReadHook func()
	// ciLogStore is the CI job log cache (app_ci_logs.go), opened on
	// first use for the reason forgeAttachCache is lazy.
	ciLogOnce  sync.Once
	ciLogStore *cilog.Store
	// claudeRetentionLogged is the last Claude transcript-retention failure
	// logged per working directory, so a repeated one is logged once
	// (app_claude_retention.go).
	claudeRetentionLogMu  sync.Mutex
	claudeRetentionLogged map[string]string
	// dictation holds recordings clients are uploading for transcription
	// (app_dictation.go), created on first use.
	dictationOnce sync.Once
	dictation     *dictation.Recordings
	// liveDictation holds clients' live recordings streaming to OpenAI
	// (app_live_dictation.go), created on first use.
	liveDictationOnce sync.Once
	liveDictation     *dictation.Live
	// readAloud holds sentences admitted to be read aloud in OpenAI voices
	// (app_read_aloud.go), created on first use.
	readAloudOnce sync.Once
	readAloud     *readaloud.Sentences
	// transportServer is the Phase C HTTP+WS transport. Set by main.go
	// via SetTransportServer before app.Run() so Shutdown can drain
	// in-flight RPCs BEFORE App subsystems (store, telemetry, sessions)
	// close. atomic.Pointer matches eventBus — wiring can land any time
	// without racing Shutdown's reader.
	transportServer atomic.Pointer[transport.Server]
	// storeIdentity is the store's backend id + replica generation
	// (migration v55), captured when the store opens so the transport's
	// bootstrap manifest can carry them without the transport package
	// learning about SQLite. atomic.Pointer for the same reason as
	// eventBus: the manifest handler can be serving before initStores
	// has run (a webview that connects during ServiceStartup), and an
	// empty identity there is a correct "not yet known" rather than a
	// race. See docs/specs/thread-replica-sync.md §3.3.
	storeIdentity atomic.Pointer[store.Identity]
	// identity is the session core plus the local page channel's current
	// credential (app_identity.go). atomic.Pointer for the reason
	// storeIdentity is one: the transport's hooks can be serving before
	// initIdentity has run, and nil there means "identity is not wired",
	// which every accessor answers honestly rather than panicking.
	identity identitySlot
	// liveConns parks the transport's live-connection registry for the
	// AttachSessionConns ordering handshake: the transport is constructed
	// before initIdentity runs, so the registry must survive until the
	// session core exists to receive it (app_identity.go).
	liveConns atomic.Pointer[identity.LiveConns]
	// updater owns the complete in-app update state machine. This shell retains
	// only the stable App-bound wire adapters in app_updater.go.
	updater *appupdate.Service
	// providerUpdates owns provider CLI update checks, release notes and
	// update runs. Built on first use by providerUpdateService; the
	// App-bound adapters are in app_provider_update.go.
	providerUpdates     *providerupdate.Service
	providerUpdatesOnce sync.Once
	// providerUpdateRunner replaces the provider update service's command
	// runner. Production leaves it nil and runs textgen.ExecCLI; a test
	// binary is refused without one (providerUpdateCommand).
	providerUpdateRunner providerupdate.Runner
	// providerUpdateClientOverride answers the provider release hosts.
	// ConfigureIsolation sets one that providerUpdateFeed answers in
	// process; nil uses the default client outside a test binary.
	providerUpdateClientOverride *http.Client
	// providerUpdateFeed is the isolated boot's fake release hosts
	// (HarnessSetProviderUpdateFeed). Nil outside isolation.
	providerUpdateFeed *updatefake.Feed
	// webviewTrimLastUnixNano stamps the last accepted webview:trim request,
	// the backend-side floor between forced renderer GCs
	// (app_webview_trim.go). Atomic: read-CAS on the RPC path, no lock.
	webviewTrimLastUnixNano atomic.Int64
	// turnActivityUnixNano stamps provider turn lifecycle changes. The trim
	// policy uses it to skip no-work forced GCs without counting every delta.
	turnActivityUnixNano atomic.Int64
	// keepAwakeApply is the OS sleep-inhibitor seam (app_power.go). nil
	// means power.Apply, which is what production always uses; fixtures
	// install a recorder so no test binary can move the developer's
	// machine's power state. internal/power refuses inside a test binary
	// on its own too — this seam is what lets a test ASSERT the mode
	// instead of just being protected from it.
	keepAwakeApply func(power.Mode) error
	// shuttingDown is flipped to true once Shutdown begins. Binding entry
	// points that spin up new work (StartSession, SendMessage, ReconnectSession)
	// check it and fail fast with ErrShuttingDown so late RPCs can't race
	// with subsystem teardown. Pairs with appCtx: binding entry points
	// read this for fast-fail; in-flight goroutines derive from appCtx
	// via lifeCtx() so cancellation propagates through downstream I/O.
	// New code: pick by call site (entry point vs goroutine), not by
	// preference.
	shuttingDown atomic.Bool
	// backendShutdown ends this process on ShutdownBackend's behalf. Only
	// the boots whose shell is a signal wait install one (runHeadless and
	// runSoak, the two the Windows launcher spawns); every other boot owns
	// its own quit path and leaves this nil, which is what makes the RPC
	// refuse rather than half-tear-down an app whose shell is still up.
	// Installed before Start by ConfigureBackendShutdown.
	backendShutdown func() error
	// bootProgress receives Start's phases for the readiness report. Nil
	// reports nothing. A boot input installed before Start by
	// SetBootProgress.
	bootProgress BootProgress
	// startDone receives the result of the desktop Start that
	// ServiceStartup runs on its own goroutine. A boot input installed by
	// SetStartDone before the Wails application runs.
	startDone func(error)
	// asyncStart is that desktop Start while it runs, so ServiceShutdown
	// can cancel it and wait for it (app_start_async.go).
	asyncStart atomic.Pointer[asyncStart]
	// settingsAttached is set once the settings service and its tier store
	// are in place during Start. A caller outside Start, the desktop
	// window's geometry tracker, checks it before touching a.settings.
	settingsAttached atomic.Bool
	// appCtx is the App-lifetime context shared by every fire-and-forget
	// goroutine that has no narrower scope (rate-limit probe loop, Claude
	// OAuth-completion poller, MCP live-reconcile callbacks, etc).
	// appCancel is invoked inside Shutdown between the shuttingDown flip
	// and drainTriage so in-flight goroutines unblock their I/O and exit
	// before the drain barrier instead of after. Initialised in
	// ServiceStartup; tests build it directly via newTestApp.
	appCtx    context.Context
	appCancel context.CancelFunc
	// sessionRuntime is the single owner of all process-local provider session
	// state and the atomic session/start/token/live-config transitions. Lazy
	// construction keeps focused bare-App tests valid.
	sessionRuntimeOnce sync.Once
	sessionRuntime     *sessionruntime.Manager
	// orphanReaper is the sidecar process that kills the watched process
	// groups if this app dies ungracefully; orphanRegistry is the durable
	// backstop the next launch sweeps. Both stay nil on Windows and in tests
	// that do not start them; the watch/release helpers and Client methods
	// are nil-safe. See app_orphan_reaper.go.
	orphanReaper   *orphanreaper.Client
	orphanRegistry *orphanreaper.Registry
	// threadApp owns store-backed thread application policy and the keyed
	// action-lock registry shared by application-shell thread workflows. Provider
	// sessions, git subprocesses, and destructive cleanup stay behind shell adapters.
	threadAppOnce sync.Once
	threadApp     *threadapp.Service
	// Serializes user profile read/modify/write operations across threads.
	chatModelProfileMu sync.Mutex
	// Client send identities serialize across RPC shapes before action locks.
	// Entries disappear when their last holder/waiter releases them.
	sendAdmissionsOnce sync.Once
	sendAdmissions     *keyedlock.Registry
	// sessionConfigApplyLocks serializes the live-apply section of the
	// per-thread config reconciler (app_session_config.go); see
	// App.configApplyLocks for the lock-order rules.
	sessionConfigApplyLocksOnce sync.Once
	// threadRowAnnouncer builds every thread:updated row frame
	// (app_thread_broadcast.go).
	threadAnnouncerOnce     sync.Once
	threadRowAnnouncer      *triage.ThreadAnnouncer
	sessionConfigApplyLocks *keyedlock.Registry
	// flushDispatch is the queued-message flush concern
	// (`app_flush_queue*.go`), including both of its mutexes.
	flushDispatch appFlushDispatchState
	// sendsPending publishes provider:sends_pending
	// (`app_sends_pending.go`).
	sendsPending appSendsPendingState
	// sessionLive publishes provider:session_live
	// (`app_session_live.go`).
	sessionLive appSessionLiveState
	// prUpdates is the PR-scope review-pane polling concern
	// (`app_pr_updates.go`).
	prUpdates appPRUpdateState
	// prManage runs one PR's management writes one at a time
	// (app_pr_manage.go).
	prManage prManageLocks
	// mergedPRs removes a thread's worktree once its pull request merges
	// (`app_worktree_merge_cleanup.go`).
	mergedPRs mergedPRCleanupState
	// reviewDiffs holds the review diffs clients are reading
	// (`app_review_diff_handles.go`).
	reviewDiffs appReviewDiffState
	// providerDiscovery owns bounded provider probe/model caches and the
	// separate Claude, Codex, status, and custom-environment coordinators.
	// This shell retains the stable Wails façades and account/event adapters.
	providerDiscoveryOnce sync.Once
	providerDiscovery     *providerdiscoveryapp.Service
	// providerDiscoveryCaches is a test-only cache injection. Production leaves
	// it nil and providerDiscoveryService uses the bounded process-wide set.
	providerDiscoveryCaches *providerdiscoveryapp.Caches
	// providerBinaries is the provider-binary upgrade watcher's concern
	// (`app_provider_binary_watch.go`): the last version each binary
	// reported and the live threads flagged stale against it. Zero value
	// ready; the watcher goroutine only runs from Start.
	providerBinaries appProviderBinaryWatchState
	// promptStale is the set of live threads whose settings-level system
	// prompt would differ at their next session start
	// (app_prompt_stale.go).
	promptStale appPromptStaleState
	// unsavedRows is the per-thread list of error and notice rows the store
	// refused, delivered live instead (app_unsaved_rows.go).
	unsavedRows appUnsavedRowState
	// taskOutput follows the output files of running background commands
	// for the tray rows clients expanded (app_background_task_output.go).
	taskOutput appBackgroundTaskOutputState
	// claudeApp owns application-facing reads and controls for live Claude
	// sessions. Account and process lifecycle stay with their own services.
	claudeAppOnce sync.Once
	claudeApp     *claudeapp.Service
	// codexApp owns Codex's application-facing leaf controls and cached global
	// reads. Lazy construction keeps focused tests that build a bare App cheap.
	codexAppOnce sync.Once
	codexApp     *codexapp.Service
	// highlightApp owns cached parsing, live code highlighting, and persisted
	// span workers.
	// Lazy construction keeps bare App test fixtures cheap.
	highlightAppOnce sync.Once
	highlightApp     *highlightapp.Service
	// mcpApp owns MCP config, status, OAuth, reload coordination, and live
	// provider-session application. This shell keeps only the lazy service seam.
	mcpAppOnce sync.Once
	mcpApp     *mcpapp.Service
	// worktreeSetupApp owns the complete asynchronous chat-worktree setup
	// lifecycle. This shell retains only stable Wails and project/workspace adapters.
	worktreeSetupAppOnce sync.Once
	worktreeSetupApp     *worktreesetupapp.Service
	// sessionImport is the session-import concern
	// (`app_session_import*.go`): the scan cache and the one live run.
	sessionImport appSessionImportState
	// markThreadRead joins the background thread read-state stamps
	// (`app_session_bindings.go`).
	markThreadRead appMarkThreadReadState
	// threadTitleApp owns title-generation singleflight, store coordination,
	// and automatic/heal/regeneration policy. This shell injects the provider CLI
	// adapter and projects callbacks onto the stable Wails event shapes.
	threadTitleAppOnce sync.Once
	threadTitleApp     *threadtitleapp.Service
	// Test-only injection points for binding helpers that need to observe start/stop.
	startSessionFn func(string) error
	stopSessionFn  func(string) error
	// closeBrowserPagesFn and closeProviderSessionFn, when set, answer
	// closeThreadBrowserPages and a teardown's provider close in place of the
	// real ones, which the fakes and mock CLIs cannot make fail.
	closeBrowserPagesFn    func(threadID string) error
	closeProviderSessionFn func(threadID string) error
	sendMessageFn          func(string, string, []string) error
	// sendStageFn runs at two points of a direct send where a turn the
	// provider starts on its own can land: "placed" (between placement and
	// the row's persist) and "persisted" (after the allocation lock is
	// released).
	sendStageFn          func(threadID, stage string)
	generateBranchNameFn func(store.Thread, string) (string, error)
	// threadTitleGenerator is the test seam over the one provider-capable
	// dependency injected into threadtitleapp. Production leaves it nil.
	threadTitleGenerator threadtitleapp.Generator
	// Legacy-shaped test seams keep root App integration tests focused on
	// Wails/store/provider wiring while policy tests live in threadtitleapp.
	generateThreadTitleFn   func(store.Thread, string, []store.Attachment) (string, error)
	regenerateThreadTitleFn func(store.Thread, string) (string, error)
	// textGenerationExecutor stubs the provider-CLI invocation used by short
	// text-generation helpers. Production leaves it nil — call sites use
	// textgen.ExecCLI directly. Tests install a fake that returns a
	// canned stdout/stderr/exitCode triple so the test suite runs without
	// `codex` / `claude` binaries on PATH.
	textGenerationExecutor textgen.CLIExecutor
	// lookPathFn stubs exec.LookPath for provider-availability detection
	// in resolveTextGenerationConfig and the Layer 2 retry path. Production
	// leaves it nil; resolveLookPath() falls through to exec.LookPath.
	// Tests install a fake so they can simulate "only claude installed"
	// without touching $PATH.
	lookPathFn func(string) error
	// codexLookPathFn replaces exec.LookPath when the default `codex`
	// setting is resolved to the binary it runs (defaultCodexBinary).
	// Production leaves it nil; test fixtures install one so the default
	// never resolves against the developer's PATH.
	codexLookPathFn func(string) (string, error)
	// defaultCodexBinaryCache is the default `codex` setting's resolution
	// (app_codex_binary.go).
	defaultCodexBinaryCache defaultCodexBinaryCache
	// emitEventFn is a test-only override for a.emitEvent. It takes the
	// channel in its WIRE spelling rather than as an eventchan.Channel —
	// it is an observation seam, not an emit site, so it has nothing to
	// keep honest and typing it would churn every test that installs one.
	// Same reasoning as testEmitHook below.
	emitEventFn func(eventName string, data any)
	// shutdownStepFn is a test-only hook fired after every step of
	// Shutdown. Production leaves this nil. Order tests install it to
	// record the step sequence and observe per-step errors without
	// wrapping every subsystem in its own spy type.
	shutdownStepFn func(step string, err error)
	// shutdownInjectErrFn is a test-only hook that, when set, takes the
	// step name + the subsystem's own error and returns the error
	// Shutdown will record for that step. Production leaves it nil;
	// shutdown error-aggregation tests install it to force controlled
	// failures across specific steps.
	shutdownInjectErrFn func(step string, err error) error
	// testEmitHook is a test-only observer for a.emit. Production
	// leaves it nil and a.emit forwards to the loaded eventBus pointer.
	// Tests that want to observe emissions without wiring up a real
	// transport bus install a hook here; when set, a.emit ALSO calls
	// the hook with the (name, data) pair it would have published. data
	// is the raw payload — the transport bus assigns its own per-channel
	// seq when it emits, so test observers see the same shape the
	// downstream code emitted. name is the channel's WIRE spelling, for
	// the reason given on emitEventFn above.
	testEmitHook func(name string, data any)
	// heldReturnedForTest, when set, runs after answers and completions
	// have been given back to their owners (giveBackToOwners), so a test can
	// land a watcher pass at that moment.
	heldReturnedForTest func(threadID string)
	// queueDropReadForTest, when set, runs in a wholesale drop between its
	// read of the durable queue and its delete (dropDurableFlushQueue), so
	// a test can queue a message at that moment.
	queueDropReadForTest func(threadID string)
	// queueDroppedForTest, when set, runs in a wholesale drop after its
	// delete of the durable queue (dropDurableFlushQueue), so a test can
	// queue a message before the in-memory queue is cleared.
	queueDroppedForTest func(threadID string)
	// remoteCompletionReadForTest, when set, runs in a watcher pass between
	// its read of the watch and the queueing of the completion
	// (checkRemoteWatch), so a test can hand the thread off at that moment.
	remoteCompletionReadForTest func(w store.RemoteWatch)
	// sessionLossHandOffForTest, when set, runs as a session-loss handoff
	// begins, before it reads anything (recoverAssistantSessionLoss), so a
	// test can send again or make the handoff fail at that moment.
	sessionLossHandOffForTest func(threadID string)
	// handOffCommittedForTest, when set, runs in a handoff after its store
	// transaction committed and before the handoff ends
	// (nextAssistantThread), so a test can act while it still runs.
	handOffCommittedForTest func(oldThreadID, successorID string)
	// handOffSuccessorMadeForTest, when set, runs in a handoff after it
	// created the successor and before it takes the old thread's locks
	// (nextAssistantThread), so a test can rename or archive meanwhile.
	handOffSuccessorMadeForTest func(oldThreadID, successorID string)
	// remoteReplySettledForTest, when set, runs in a parked remote call's
	// after-response work between the dismissal and the end of its wait.
	remoteReplySettledForTest func()
	// agentWakeClaimedForTest, when set, runs in a fire between its claim
	// and its run's registration (fireAgentWake), under the wake lifecycle
	// lock, so a test can cancel the row at that moment.
	agentWakeClaimedForTest func(row store.AgentWake)
	// agentWakeRetiringForTest, when set, runs as an interval run publishes
	// the row it leaves behind (retireAgentWakeRun): before the write, and
	// after it under the wake lifecycle lock (rearmed), so a test can hold
	// the run on either side of the write.
	agentWakeRetiringForTest func(row store.AgentWake, rearmed bool)
	// deleteDecisionReadForTest, when set, runs in a start that met a
	// delete's claim once it read the claim, before it looks for the
	// delete's gate (waitDeleteDecision), so a test can decide the delete
	// at that moment.
	deleteDecisionReadForTest func(threadID string)
	// deleteDecisionMadeForTest, when set, runs in a start that waited for
	// a delete's decision once it is made, before the start goes on
	// (waitDeleteDecision), so a test can finish the delete meanwhile.
	deleteDecisionMadeForTest func(threadID string)
	// flushHandoffForTest, when set, runs as triage hands a batch to the
	// dispatch (enqueueFlushDispatch), before the dispatch records it, so a
	// test can withdraw a message in the handoff.
	flushHandoffForTest func(threadID string)
	// requeueRowsReadForTest, when set, runs in a requeue between its read
	// of which rows stand and its queueing of the messages again
	// (requeueUnconfirmedFlushItemsLocked), so a test can withdraw one then.
	requeueRowsReadForTest func(threadID string)
	// flushDispatchReturnedForTest, when set, runs in a dispatch worker once
	// a batch's dispatch returned and released the thread's action lock,
	// before the worker retires the batch, so a test can recover the
	// session in between.
	flushDispatchReturnedForTest func(threadID string)
	// flushGroupCommitForTest, when set, runs in a dispatch under the
	// thread's action lock right before a group's commit point (committed
	// false) and right after it (committed true), so a test can withdraw a
	// message on either side of it.
	flushGroupCommitForTest func(threadID string, committed bool)
	// agentWatchRefusedForTest, when set, runs in a long-running watch's
	// supervisor after the pause refused its process start and before the
	// supervisor returns (superviseAgentWatch).
	agentWatchRefusedForTest func(wakeID string)
	// cancelReadForTest, when set, runs in a request cancel between its read
	// of the receipt and its settlement (stopThreadRequestWorkFor), so a
	// test can settle the receipt at that moment.
	cancelReadForTest func(token string)
	// agentStopCapturedForTest, when set, runs in a Stop All and in each
	// rest it makes once it has read what it stops under threadID and
	// before it stops any of it (threadAgentStopRows,
	// stopRequestsOfRestingThread), so a test can make a request at that
	// moment.
	agentStopCapturedForTest func(threadID string)
	// restPausedReadForTest, when set, runs in a rest pass once it has read
	// a held request's thread under a paused assistant (settleRestedReceipts),
	// so a test can turn assistants off and on again at that moment.
	restPausedReadForTest func(threadID string)
	// threadAgentDrainForTest, when set, runs in the Agents rows' drain
	// before it takes each batch (drainThreadAgentEmits), so a test can hold
	// what liveWorkers records behind a change.
	threadAgentDrainForTest     func()
	beforeWorkerReassessForTest func(string)
	// beforeOwnerWaitForTest holds the boundary between applying a cursor and checking out its next connection.
	beforeOwnerWaitForTest func(context.Context, OwnerCursor)
	// restReadForTest, when set, runs in a read of rest a decision acts on
	// once the threads reached from roots have been read, memory included,
	// and before the decision is written (readRest), so a test can admit
	// work there.
	restReadForTest func(roots []string)
	// sweepSleepForTest, when set, is the request sweep's clock: the sweep
	// sleeps until the channel it returns for the delay asked sends
	// (sweepSleep).
	sweepSleepForTest func(d time.Duration) <-chan time.Time
	// admitWorkForTest, when set, runs as a call reaches admitWork, after
	// its authentication and before its admission, so a test can hold it
	// there while the thread is brought to rest.
	admitWorkForTest func(threadID string)
	// asyncQuestionsReadForTest, when set, runs in a reconcile of threadID's
	// async questions between its store read and what it registers
	// (reconcileAsyncQuestions), so a test can change the thread at that
	// moment.
	asyncQuestionsReadForTest func(threadID string)
	// spawnNamingForTest, when set, runs in a thread_spawn of
	// callerThreadID's, here or on a paired computer, once its thread is
	// made and before the spawn names it (acceptRequest), so a test can hold
	// an admitted spawn there.
	spawnNamingForTest func(callerThreadID string)
	// peerAdmissionForTest, when set, runs in a paired computer's spawn,
	// send or ask once its handler holds the token and before anything can
	// admit it (runThreadPeerRequest), so a test can hold it there.
	peerAdmissionForTest func(token string)
	// peerStatusReadForTest, when set, runs in a ThreadToolRequestStatus
	// between its two reads of a token, whether a handler holds it and its
	// receipt, so a test can let a handler admit and exit there.
	peerStatusReadForTest func(token string)
	// deleteClaimedForTest, when set, runs once a delete's claim committed
	// and before it verifies anything (familyDelete.claim), with the
	// threads it claimed.
	deleteClaimedForTest func(threadIDs []string)
	// deleteFamilyReadForTest, when set, runs each time a delete reads and
	// claims its family (store.ClaimDeleteFamily), so a test can count the
	// reads.
	deleteFamilyReadForTest func()
	// deleteVerifiedForTest, when set, runs once a delete verified
	// threadID and released its action lock (familyDelete.verify), so a
	// test can act between two threads' checks.
	deleteVerifiedForTest func(threadID string)
	// deleteCommittedForTest, when set, runs once a delete committed and
	// before it destroys anything (familyDelete.commit).
	deleteCommittedForTest func(threadIDs []string)
	// deleteDestroyedForTest, when set, runs once a delete destroyed
	// threadID (familyDelete.destroy), so a test can stop it between two
	// threads.
	deleteDestroyedForTest func(threadID string)
	// deletePortsForTest, when set, edits the ports a delete destroys each
	// thread through, so a test can fail one.
	deletePortsForTest func(ports *threadapp.DeletePorts)
	// restRunsListedForTest, when set, runs once a rest has listed the
	// workflow runs that report to the thread and before it acts on any
	// (pauseRunsOfRestingThread), so a test can move a run at that moment.
	restRunsListedForTest func(runs []store.WorkItem)
	// remoteStatusReadForTest, when set, runs in a watch check once the
	// destination answered its status call and before the answer is
	// recorded (checkRemoteWatch), so a test can stop the thread then.
	remoteStatusReadForTest func(w store.RemoteWatch)
	// workerStopObservedForTest, when set, runs in a person's Stop of a
	// worker that outlived its spawn once it read the send id of its message
	// to the spawner and before it brings the worker to rest
	// (workerStoppedNotice), so a test can race a second Stop there.
	workerStopObservedForTest func(token string)
	// turnEndSettledForTest, when set, runs in a turn-end settlement pass
	// after it settled its receipts and before it retires the stops it read
	// (settleReceiptsForEndedTurn), so a test can stop or end a turn then.
	turnEndSettledForTest func(threadID string)
	// threadWakeRenderedForTest, when set, runs in a wake's delivery after
	// it rendered the message and before it is queued
	// (deliverThreadWakeMessage), so a test can hand the caller off then.
	threadWakeRenderedForTest func(carrier string)
	// archiveWalkEndingForTest, when set, runs once the last walk of
	// threadID's archive decided to end it and before the store ends it
	// (endArchiveWalk), so a test can archive the thread again then.
	archiveWalkEndingForTest func(threadID string)
	// workerPromptAnsweringForTest, when set, runs once an answer claimed
	// a routed prompt and before it is written to the worker
	// (answerRoutedPrompt); an error it returns is that write's failure.
	workerPromptAnsweringForTest func(token string) error
	// savePayloadPickerFn is a test-only override for the save-file
	// dialog used by SavePayloadToFile. Production leaves it nil and
	// the real Wails dialog runs; tests install a stub that returns a
	// canned path (or empty for "cancelled").
	savePayloadPickerFn savePayloadPicker
	// rateLimitProbeClientOverride replaces the Claude rate-limit probe's
	// HTTP client. ConfigureIsolation sets one that answers in process only
	// what claudeUsageFake holds; tests assign a client pointing at a local
	// httptest server. Nil uses the package-level singleton.
	rateLimitProbeClientOverride *http.Client
	// claudeUsageFake holds the usage answers an isolated boot's transport
	// gives, per identity (HarnessSetClaudeUsage). Nil outside isolation.
	claudeUsageFake *claudefake.Usage
	// openAIClientOverride replaces the client keyed OpenAI requests use
	// (app_live_dictation.go). Only an isolated boot sets it, to answer
	// them in process.
	openAIClientOverride *http.Client
	// externalURLsIsolated makes every system-browser open answer
	// externalurl.ErrNoOpener, as a host with no browser does. Only an
	// isolated boot sets it: a test run must not open the developer's browser.
	externalURLsIsolated bool
	// netReach is where a LAN bind listens and which address it publishes.
	// ConfigureIsolation confines an isolated boot to loopback unless it
	// runs inside the test network namespace (network.IsolatedReach). The
	// zero value is the host's network.
	netReach network.Reach
	// threadPollOverride and transferPendingRetry shorten the agent thread
	// request poll and the pending conversation transfer retry for an
	// end-to-end test (IsolationConfig). Zero keeps the product intervals.
	threadPollOverride   time.Duration
	transferPendingRetry time.Duration
	// workerPromptLeaseOverride shortens the lease of a prompt routed to a
	// paired computer (workerPromptLease) for a test. Zero keeps the
	// product lease.
	workerPromptLeaseOverride time.Duration
	// ownerOpsOmitted answers agent_owner_op as a build before owner
	// operations does (IsolationConfig.OmitOwnerOps). False serves them.
	ownerOpsOmitted bool
	// remoteWatchPoll replaces the remote-job watcher's cadence for a
	// running job (IsolationConfig.RemoteWatchPoll). Zero keeps the
	// product cadence.
	remoteWatchPoll time.Duration
	// idleReap holds an isolated test boot's idle-session reaper timing
	// (IsolationConfig). Zero fields keep the product values.
	idleReap idleReapTiming
	// usageProbeGap shortens the shortest gap between two usage probes of
	// one provider for an end-to-end test (IsolationConfig). Zero keeps the
	// product gap.
	usageProbeGap time.Duration
	// dataDirOverride overrides the data directory root that initStores
	// otherwise resolves via os.UserConfigDir(). The app's data lives in
	// <root>/agent-overflow. Set by the --data-dir CLI flag (harness mode
	// requires it) and by tests, which use a t.TempDir() so the path is
	// deterministic across OSes — os.UserConfigDir() ignores XDG on macOS
	// (it returns $HOME/Library/Application Support), which env overrides
	// can't redirect.
	dataDirOverride string
	// refusePendingMigrations fails Start with a store.MigrationsPendingError
	// instead of migrating the database (RefusePendingMigrations). A boot
	// input like dataDirOverride.
	refusePendingMigrations bool
	// certFingerprint is the fingerprint of the TLS certificate the
	// transport listener presents (internal/servercert), carried on every
	// pairing link this backend mints so a client that owns its own TLS
	// configuration can pin it. Empty when the boot resolved no
	// certificate, which mints links exactly as before: the payload's
	// field is omitted and the device pairs on proof-of-possession plus
	// the verification number, never on channel secrecy.
	//
	// A boot input like dataDirOverride, written before the transport
	// serves and never after, so the RPC goroutines that read it cannot
	// race the write.
	certFingerprint string
	// boundPortRecorder persists the port this listener is on, so the
	// executable's transport-port cache keeps naming the previous bind
	// after a settings-driven rebind moved it. Nil in every fixture and on
	// every boot that resolved no config directory, and calling it is
	// best-effort by construction: the file is a cache, and losing a write
	// to it costs one churned origin on some later launch, never this call.
	//
	// A boot input like the two above, installed before the transport
	// serves (bootstrap.SetBoundPortRecorder) and never written after.
	boundPortRecorder func(port int)
	// domainCert owns the canonical domain's certificate: what is loaded,
	// what failed, and the one goroutine that renews it. Zero value is an
	// App with no reconciler running, which is every fixture that never
	// calls Start. See app_domaincert.go.
	domainCert domainCertState
	// tailnet owns whether this backend is a node on the owner's tailnet:
	// the live node, the listeners it feeds the transport, and the one
	// goroutine that reconciles them against the persisted preference.
	// Zero value is an App with no reconciler and no node, which is every
	// fixture that never calls Start and every install that leaves the
	// feature off. See app_tailnet.go.
	tailnet                 tailnetState
	computerPairing         computerPairingState
	nativeNetwork           nativeNetworkState
	localListener           localListenerState
	computerRoutesPublished computerRoutesPublication
	// networkApply serializes every apply of the network preferences (the
	// settings write, the rebind and the reconciler kicks are one act) across
	// the settings screen and the own-devices worker. See app_network.go.
	networkApply sync.Mutex
	// preview owns this machine's dev-server list: the scanner, the one
	// goroutine that polls it while somebody off-machine is watching, and
	// the platform refusal once a platform has given one. Zero value is
	// an App with no loop and no scanner, which is every fixture that
	// never calls Start. See app_preview.go.
	preview previewState
	// providerExtraEnv is merged into the environment of every provider
	// spawn something outside the process has to STEER: sessions
	// (sessionProcessEnv) and account sign-ins (Deps.LoginSpawnEnv on the
	// provider-account manager). Harness mode uses it to hand
	// ao-mockprovider its control-channel address + token without exporting
	// those credentials process-wide (terminals, git hooks, and other
	// children must not inherit them). Account PROBES are deliberately out:
	// a probe is one invocation with no behaviour to command, so a control
	// registration for one is a mock nothing addresses.
	// Set once before Start; never mutated afterwards.
	providerExtraEnv map[string]string
	// cliBinDir is the directory holding the canonical-name link to this
	// executable (D30, app_cli_path.go). sessionProcessEnv prepends it to
	// every provider session's PATH so an agent can type `agent-overflow`.
	// Empty means boot could not publish the link and sessions run without
	// the command — the `/workflow` composer block says so. Written once by
	// Start, read-only afterwards.
	cliBinDir string
	// forgeCLIs is the isolated boot's forge CLI pin: every git.Core this
	// App builds (newGitCore) runs the fake in place of gh and glab and
	// never resolves them on PATH. The zero value is the desktop
	// behavior. Set once before Start; never mutated afterwards.
	forgeCLIs isolatedForgeCLIs
	// prLookupRetryBase replaces how long the git.Core caches a first
	// failed open-PR lookup (git.WithPRLookupErrorBase); zero keeps the
	// product value. Only a harness boot sets it. Set once before Start.
	prLookupRetryBase time.Duration
	// providerBinaryOverride, when non-empty, wins over the settings-
	// backed provider binary paths in providerBinaryPath. Harness mode
	// points it at ao-mockprovider so the "providers are always mocked"
	// guarantee holds structurally — a settings update (RPC or UI) can
	// no longer repoint a spawn at a real claude/codex binary. Set once
	// before Start; never mutated afterwards.
	providerBinaryOverride string
	// defaultProviderBinaryOverride, when non-empty, is what a provider
	// binary setting left at its default runs, in place of the bare name
	// resolved on PATH. Test fixtures point it at their poisoned binary
	// (isolateE2EProviderSpawns), so a default read after the fixture's
	// settings directory is gone cannot reach an installed CLI. Production
	// leaves it empty. Set before the App is used.
	defaultProviderBinaryOverride string
	// fileKeychainOverride makes initStores build Credentials with the
	// file-backed Keychain stand-in instead of security(1). Harness mode
	// sets it because a redirected $HOME isolates every file store but
	// NOT the macOS Keychain — the active Claude slot's service name
	// ignores the home, so a security(1)-backed harness run would touch
	// the developer's real Claude Code login. Set once before Start;
	// never mutated afterwards.
	fileKeychainOverride bool
	// activation is the ONE gate every subsystem that can take an action of
	// its own waits behind, so a supervisor trial can boot fully — store,
	// migrations, transport bind, RPCs answered — without doing anything a
	// rollback could not undo. The zero value is OPEN: every boot except a
	// supervisor trial never touches it. See app_activation.go.
	activation activation
	// workflowStartsHeldForBoot is true while the workflow engine runs paused
	// only so its rebuild starts nothing before startBeforeClients. Written by
	// Start before the gate's goroutine exists and cleared by that step.
	workflowStartsHeldForBoot bool
	// serviceUpdate holds this backend's ability to replace itself: the call
	// that asks its supervisor to run an already-staged version, and the
	// release source, layout and live flow the remote trigger drives. Both
	// halves are installed by a supervised `serve` boot and absent everywhere
	// else, which is what makes "this install has no supervisor" an answer
	// rather than a nil dereference. See app_service_update.go.
	serviceUpdate serviceUpdateState
	// restartUpdate is the in-app restart to update while it waits for
	// running work and hands off. See app_update_restart.go.
	restartUpdate restartUpdateState
	workAdmission workAdmission
	// credentialHomeOverride, when non-empty, replaces os.UserHomeDir()
	// as the home that provideraccounts.Credentials operates under —
	// slot storage, canonical credential, ephemeral probe homes, and the
	// orphan prune all resolve beneath it. Harness mode always sets it
	// to the harness-owned home so that even AO_HARNESS_KEEP_HOME (which
	// keeps the real $HOME for provider session-file replay) can never
	// point the credential surface at the developer's real ~/.claude /
	// ~/.codex. Set once before Start; never mutated afterwards.
	credentialHomeOverride string
	// backgroundFetchDisabled suppresses the background `git fetch`
	// cadence entirely. Harness mode sets it: e2e runs must be
	// deterministic and offline, and the harness's fixture repositories
	// exist to be asserted against, not fetched over. Unit tests never
	// reach it — they build *App directly and never call Start. Set once
	// before Start; never mutated afterwards.
	backgroundFetchDisabled bool
	// retentionSweepDisabled keeps the retention sweep from starting
	// (IsolationConfig.DisableRetentionSweep). Set once before Start;
	// never mutated afterwards.
	retentionSweepDisabled bool
	// isolatedWorkspaceRoot, when non-empty, bounds every project path and
	// session working directory (app_isolated_workspace.go). Empty disables
	// the check, which only unit tests do. Set once before Start; never
	// mutated afterwards.
	isolatedWorkspaceRoot string
	// downloadsIsolated keeps saved files under the app data directory
	// instead of the user's Downloads folder (downloadsDir). A mocked boot
	// inherits the developer's HOME and the launcher's environment, so
	// without it a harness save would write into their real Downloads.
	// Set once before Start; never mutated afterwards.
	downloadsIsolated bool
	// idleReaperNowFn is a test-only clock injection for the reaper.
	// Production leaves it nil and reaperNow reads time.Now directly.
	idleReaperNowFn func() time.Time
	// retentionNowFn is a test-only clock injection for the retention
	// sweep. Production leaves it nil and retentionNow reads time.Now
	// directly. Mirrors idleReaperNowFn.
	retentionNowFn func() time.Time
	// reclaimFreeSpaceFn is a test-only override for the sweep's
	// free-space step. Production leaves it nil and the sweep calls
	// Store.ReclaimFreeSpace; the production thresholds need a
	// multi-gigabyte freelist to trip, which a unit test cannot build.
	reclaimFreeSpaceFn func(context.Context, time.Duration) (int64, error)
	// maintenance shortens the background maintenance timings for
	// tests. Zero fields mean production values.
	maintenance maintenanceTuning
	// deferredMigrations owns the stop gate of the run that finishes the
	// store's deferred migration phases. See app_store_maintenance.go.
	deferredMigrations backgroundLoop
	// codexThread owns provider-thread reconcile and cumulative-cost reads.
	codexThreadOnce sync.Once
	codexThread     *codexthread.Service
}

type session = sessionruntime.Entry
type sessionLiveness = sessionruntime.Liveness

func newSessionLiveness(now time.Time) *sessionLiveness { return sessionruntime.NewLiveness(now) }

func NewApp() *App { return NewAppWithVersion("dev") }

// NewAppWithVersion constructs the application service with the executable's
// build-stamped version. Root is the sole production caller; tests use NewApp
// and therefore retain the unstamped "dev" behavior.
func NewAppWithVersion(buildVersion string) *App {
	if buildVersion == "" {
		buildVersion = "dev"
	}
	app := &App{
		version:                 buildVersion,
		sessionConfigApplyLocks: keyedlock.New(),
		turnObservers: appTurnObserverState{
			byThread: make(map[string]map[uint64]turnObserver),
		},
		prUpdates: appPRUpdateState{
			pumps:   make(map[string]*prUpdatePump),
			handles: make(map[string]*prUpdateHandle),
		},
	}
	app.providerAccounts = newProviderAccountManager(app)
	app.updater = appupdate.New(app.version, appupdate.Deps{
		Context: app.lifeCtx,
		IsShuttingDown: func() bool {
			return app.shuttingDown.Load()
		},
		Emit:                 app.emit,
		RestartWatchdogDelay: updateRestartWatchdogDelay,
	})
	return app
}

// SetEventBus wires the Phase C transport event bus into the App so
// a.emit forwards every event through it. The atomic.Pointer storage
// means SetEventBus and concurrent a.emit readers do not race — boot
// ordering is no longer load-bearing, so callers may wire the bus at
// any point in startup. Tests that don't exercise the wire path leave
// the bus nil and rely on testEmitHook for observation.
//
//wails:ignore
func (a *App) SetEventBus(b *transport.EventBus) {
	a.eventBus.Store(b)
}

// SetTransportServer hands the Phase C HTTP+WS server to the App so
// Shutdown can drain in-flight RPCs before tearing down subsystems.
// main.go calls this once after constructing the server and before
// app.Run(). The atomic.Pointer means SetTransportServer and the
// Shutdown reader cannot race even if a Wails-driven shutdown beats
// the wiring goroutine.
//
//wails:ignore
func (a *App) SetTransportServer(s *transport.Server) {
	a.transportServer.Store(s)
}

// backendIdentity reports the store's identity for the transport
// bootstrap manifest. Zero values mean the store is not open yet; the
// client treats an empty backend id as "no replica keying available"
// and refetches the manifest on its next connect.
func (a *App) backendIdentity() (backendID, replicaGeneration string) {
	if id := a.storeIdentity.Load(); id != nil {
		return id.BackendID, id.ReplicaGeneration
	}
	return "", ""
}

// --- Item operations ---

// ListItems returns every item persisted for a thread in chronological
// order. Unwindowed, so it carries the byte backstop the same way the
// paged loads do; active panes use the bounded slice surface instead.
//
//ao:scope threads:read
func (a *App) ListItems(threadID string, inlinePreviews bool) ([]store.Item, error) {
	if err := a.readyThreadHistory(threadID); err != nil {
		return nil, err
	}
	items, err := a.store.ListItems(threadID)
	if err != nil {
		return nil, err
	}
	return projectItemSlice(items, inlinePreviews), nil
}

// --- Payload operations ---
