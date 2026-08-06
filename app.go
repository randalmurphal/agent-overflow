package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/updater"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/claudeconfig"
	"agent-overflow/internal/codexconfig"
	"agent-overflow/internal/codexmodels"
	"agent-overflow/internal/codexskills"
	"agent-overflow/internal/codexusage"
	"agent-overflow/internal/design"
	"agent-overflow/internal/devserverprobe"
	"agent-overflow/internal/discussion"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitwatch"
	"agent-overflow/internal/highlight"
	"agent-overflow/internal/keybindings"
	"agent-overflow/internal/logging"
	"agent-overflow/internal/mcpstatus"
	obsotel "agent-overflow/internal/observability/otel"
	"agent-overflow/internal/observability/replay"
	"agent-overflow/internal/orphanreaper"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/claudetui"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/screenshot"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
	"agent-overflow/internal/terminal"
	"agent-overflow/internal/textgen"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/uitrace"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/scheduler"
	"agent-overflow/internal/workspacefiles"
)

// DesignServer returns the http.Handler that serves the per-thread
// design working directories. main.go wires this into the transport
// Config so /design/ requests reach it. Returns nil when the design
// workdir base couldn't be initialised (rare; shows up in tests that
// skip the design subsystem).
//
//wails:ignore
func (a *App) DesignServer() http.Handler {
	return a.designServer
}

// ErrShuttingDown is returned from binding entry points once Shutdown has
// started. Callers should surface this as a terminal state — no retry will
// succeed because the app is tearing down.
var ErrShuttingDown = errors.New("app: shutting down")

const (
	appPrivateDirPerm    os.FileMode = 0o700
	appSensitiveFilePerm os.FileMode = 0o600
)

// App is the primary Wails-bound struct, registered as a v3 service
// in desktop mode and driven directly via Start() in the headless WSL
// backend mode. The App itself does not import the Wails application
// package; the desktop-mode bindings live in app_desktop.go behind the
// !nogui build tag so the WSL payload can compile without libwebkit2gtk
// and other GTK runtime dependencies pulled in by Wails' cgo bindings.
type App struct {
	// saveDialog opens the native save-file dialog. Wired by
	// ServiceStartup in desktop mode (app_desktop.go); left nil in the
	// headless WSL backend, where there is no native window to attach a
	// dialog to and the frontend uses a download fallback instead.
	saveDialog savePayloadPicker
	// osNotifications is the single platform-routing seam behind notifyOS.
	// Desktop boot installs the in-process Wails service adapter; the WSL
	// headless boot installs the transport bridge; harness mode installs an
	// explicit unavailable sender. Tests may leave it nil to exercise the
	// same visible degraded error without pulling in a platform service.
	osNotifications            osNotificationSender
	store                      *store.Store
	git                        *gitops.Core
	gitWatch                   *gitwatch.Manager
	settings                   *settings.Service
	triage                     *triage.Router
	workflowEngine             *engine.Engine
	workflowRunner             *workflowAppRunner
	workflowScheduler          *scheduler.Scheduler
	workflowDefinitionsWatcher *workflowDefinitionsWatcher
	// workflowDispositionMu serializes local git/forge disposition actions.
	// They are rare, mutate shared repository metadata, and must not race an
	// automatic policy against a manual click.
	workflowDispositionMu sync.Mutex
	// workflowAutoDisposition, workflowWake, and workflowSchedulerQueue serialize
	// the app-side reactions to workflow lifecycle events. Each runs off the
	// engine's command-loop goroutine (which emits them) and in submission order,
	// so two transitions of one run cannot race each other's follow-up. They are
	// separate queues because they are independent consumers: a slow wake
	// composition must not delay the automation a finished run chains into.
	workflowAutoDisposition serialQueue
	workflowWake            serialQueue
	workflowSchedulerQueue  serialQueue
	// workflowDigestMu guards the lazily allocated digest-generator slots.
	workflowDigestMu         sync.Mutex
	workflowDigestSlots      chan struct{}
	generateWorkflowDigestFn func(context.Context, store.WorkItem, WorkflowDigest) (WorkflowDigest, error)
	// turnObservers fan provider events out to internal App features after
	// triage handling has been attempted. Each registration lives until its
	// returned unsubscribe function runs; the built-in discussion observer
	// lives for the App lifetime. turnObserversMu is deliberately independent
	// of mu so callbacks can safely enter other App coordination paths.
	turnObserversMu            sync.RWMutex
	turnObservers              map[string]map[uint64]turnObserver
	nextTurnObserverID         uint64
	discussionTurnObserverOnce sync.Once
	registry                   *discussion.Registry
	channels                   *discussion.ChannelService
	// designWorkdir owns each thread's per-thread {main,options}
	// directory layout. The base directory is the HTTP file server's
	// StripPrefix target — designServer below mounts it at /design/
	// on the existing transport.
	designWorkdir     *design.WorkDirManager
	designDiagnostics *design.DiagnosticBuffer
	designServer      http.Handler
	// screenshotManager drives a long-lived headless Chromium that
	// renders the design preview URL for the agent's read_screenshot
	// tool. Lazily started on first capture; closed on app shutdown.
	// nil during early boot or in tests that don't exercise the
	// design screenshot path — callers tolerate that explicitly.
	screenshotManager *screenshot.Manager
	// designWatchers is the per-thread fs watcher map. Keyed by thread
	// ID; entries land on session start and Stop() on session teardown.
	// designWatchersMu guards both insertion and removal.
	designWatchersMu sync.Mutex
	designWatchers   map[string]*design.Watcher
	reactor          *design.Reactor
	designMCP        *design.MCPServer
	terminals        *terminal.Manager
	attachments      *attachment.Store
	workspaceFiles   *workspacefiles.Searcher
	logger           *logging.Logger
	telemetry        *obsotel.Provider
	replay           *replay.Manager
	configDir        string
	// providerAccounts persists account labels, the active selection, and
	// last-known quota snapshots. providerCredentials keeps credential bytes
	// in provider-native homes and treats them as opaque. Both are initialized
	// with the store in ServiceStartup; tests that do not exercise accounts may
	// leave them nil.
	providerAccounts    *provideraccounts.Store
	providerCredentials *provideraccounts.Credentials
	// providerAccountMu serializes login and activation so two settings
	// clients cannot interleave active-credential snapshots and overwrite a
	// freshly selected account.
	providerAccountMu sync.RWMutex
	// providerCredentialFingerprints tracks the provider-native active
	// credential value last reconciled with providerAccounts. Digests are
	// process-local only: they are never persisted, emitted, or logged. A
	// changed digest is the cheap trigger for the zero-token identity probe
	// that recognizes logins completed in another Claude/Codex process.
	providerCredentialFingerprints map[string][32]byte
	// Provider-specific reconciliation locks coalesce concurrent sends and
	// polling ticks after one native credential change. They stay separate so
	// a slow Claude identity probe never delays an unrelated Codex send.
	claudeCredentialReconcileMu sync.Mutex
	codexCredentialReconcileMu  sync.Mutex
	// uiTracer is the dev-only JSONL render-trace appender. It's lazily
	// constructed from configDir the first time AppendUIRenderTraceBatch
	// runs so tests that build a bare App{configDir: t.TempDir()} stay
	// cheap, and so production wiring doesn't need an explicit init step.
	// uiTraceOnce serializes construction; uiTraceErr captures a failed
	// New so subsequent calls fail loudly instead of silently no-op'ing.
	uiTraceOnce sync.Once
	uiTracer    *uitrace.Tracer
	uiTraceErr  error
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
	// eventBus is the Phase C transport that owns per-channel seq stamping
	// and fan-out to connected webview / remote clients. main.go wires it
	// in via SetEventBus; the atomic.Pointer means SetEventBus and
	// concurrent a.emit readers don't race even if wiring lands after
	// background goroutines have started. Production leaves this set;
	// tests that don't need a real bus leave it nil and observe emissions
	// via testEmitHook instead.
	eventBus atomic.Pointer[transport.EventBus]
	// rateLimitsByProvider retains the latest account-scoped snapshot so a
	// freshly-mounted or reconnected frontend can hydrate explicitly instead
	// of depending on having observed an earlier provider:usage event. The
	// event bus is only a bounded replay buffer and a first connection has no
	// prior channel sequence to request, so event-only ownership could leave
	// the 5h/7d rings blank even after a successful startup probe.
	rateLimitsMu sync.RWMutex
	// Keys are provider + account ID. The empty account ID is retained for
	// unmanaged/legacy sessions.
	rateLimitsByProvider map[string]provider.RateLimitsSnapshot
	// transportServer is the Phase C HTTP+WS transport. Set by main.go
	// via SetTransportServer before app.Run() so Shutdown can drain
	// in-flight RPCs BEFORE App subsystems (store, telemetry, sessions)
	// close. atomic.Pointer matches eventBus — wiring can land any time
	// without racing Shutdown's reader.
	transportServer atomic.Pointer[transport.Server]
	// updater drives in-app self-update (check / download / restart) via the
	// Wails app.Updater singleton. Set once at desktop boot by initUpdater
	// (app_updater_desktop.go) before the transport server starts, so the
	// updater RPC handlers observe it without a race. Stays nil in the
	// headless WSL backend (no Wails application) and in tests; every updater
	// RPC method guards the nil case and reports the feature unsupported
	// rather than panicking.
	updater *updater.Updater
	// updaterProvider is the targetable GitHub provider behind a.updater,
	// retained so the version-selection RPCs (ListReleases, DownloadUpdate)
	// can enumerate releases and aim a download at a specific tag. Set
	// alongside a.updater by initUpdater; nil on the headless backend.
	updaterProvider *targetableProvider
	// updaterMu serializes the provider-retarget-then-resolve sequences that
	// CheckForUpdate (SetTarget("")+Check) and DownloadUpdate (SetTarget(tag)+
	// Check) perform. Without it, a concurrent passive CheckForUpdate from a
	// second --connect client could reset the target between a by-tag
	// download's SetTarget and its Check, resolving "latest" instead of the
	// picked tag. updaterBusy (guarded by this mutex) marks an in-flight
	// download so a racing CheckForUpdate skips its network probe rather than
	// clobbering the pending release the installer is about to use.
	updaterMu   sync.Mutex
	updaterBusy bool
	// restartExitFn is the process-exit call RestartToUpdate's watchdog
	// fires when graceful shutdown wedges (see armRestartExitWatchdog).
	// nil means os.Exit; tests inject a recorder.
	restartExitFn func(code int)
	// shuttingDown is flipped to true once Shutdown begins. Binding entry
	// points that spin up new work (StartSession, SendMessage, ReconnectSession)
	// check it and fail fast with ErrShuttingDown so late RPCs can't race
	// with subsystem teardown. Pairs with appCtx: binding entry points
	// read this for fast-fail; in-flight goroutines derive from appCtx
	// via lifeCtx() so cancellation propagates through downstream I/O.
	// New code: pick by call site (entry point vs goroutine), not by
	// preference.
	shuttingDown atomic.Bool
	// appCtx is the App-lifetime context shared by every fire-and-forget
	// goroutine that has no narrower scope (rate-limit probe loop, Claude
	// OAuth-completion poller, MCP live-reconcile callbacks, etc).
	// appCancel is invoked inside Shutdown between the shuttingDown flip
	// and drainTriage so in-flight goroutines unblock their I/O and exit
	// before the drain barrier instead of after. Initialised in
	// ServiceStartup; tests build it directly via newTestApp.
	appCtx    context.Context
	appCancel context.CancelFunc
	mu        sync.Mutex
	sessions  map[string]session // threadID → active session
	// aoTokens is the `ao` CLI credential registry: scoped token → the
	// authority it carries. It is mutated only from the session-map
	// mutators in app_session_manager.go (under mu), so an entry exists
	// exactly while its session does. See app_ao_session.go.
	aoTokens map[string]transport.CallerScope
	// orphanReaper is the macOS sidecar process that kills provider
	// process groups if this app dies ungracefully; orphanRegistry is the
	// durable backstop the next launch sweeps. Both stay nil on
	// Linux/Windows (which rely on Pdeathsig / a Job Object) and in tests
	// — the watch/release helpers and Client methods are nil-safe. See
	// app_orphan_reaper.go.
	orphanReaper   *orphanreaper.Client
	orphanRegistry *orphanreaper.Registry
	// threadActionLocks serializes per-thread workflows that must observe a
	// stable thread timeline or workspace while they run.
	threadActionLocksOnce sync.Once
	threadActionLocks     *keyedLockRegistry
	// flushDispatchQueues serializes queued-message flush batches per
	// thread. Triage decides whether the drain is boundary or immediate;
	// App owns the asynchronous provider writes so sequence allocation and
	// Send/Steer locking stay in the same layer.
	flushDispatchMu            sync.Mutex
	flushDispatchQueues        map[string][]flushDispatchBatch
	flushDispatchCurrent       map[string]flushDispatchBatch
	flushDispatchRunning       map[string]bool
	flushDispatchInflightItems map[string]int
	flushDispatchGeneration    map[string]uint64
	flushDispatchWG            sync.WaitGroup
	// flushHandoffMu serializes RegisterQueueItem's enqueue→flush handoff
	// against the revert-on-interrupt predicate's read of the queued /
	// in-flight counters. tryFlushQueue deletes a batch from the triage queue
	// before the dispatcher records it as in-flight; in that window the item
	// is invisible to every counter the predicate consults. Holding this mutex
	// across both the handoff (RegisterQueueItem) and the counter read
	// (pendingFlushWorkCount) makes the queued message observable to a
	// concurrent Stop click as either still-queued or already-in-flight, never
	// neither. Deliberately NOT the per-thread action lock: that lock is held
	// for seconds by git / worktree ops, and queueing a message
	// must stay responsive while those run. See RegisterQueueItem for the full
	// lock hierarchy and deadlock-freedom argument.
	flushHandoffMu sync.Mutex
	// threadID → in-flight session start. Concurrent callers wait for the
	// first start attempt instead of spawning duplicate provider runtimes.
	startingSessions map[string]*sessionStart
	// threadID → in-flight ReconnectSession. Single-flight gate around the
	// stop-then-start pair so a rapid double-click (or manual click racing
	// the auto-reconnect path) cannot interleave a second stopSession with
	// the first startSession and yank the new session before it registers.
	reconnectingThreads map[string]bool
	// threadID → an auto-reconnect attempt has already fired since the last
	// observed turn_started for this thread. Stops the recovery path from
	// hammering a provider that dies cleanly on every spawn (broken binary,
	// missing creds at OS level). Cleared by the EventTurnStart observer in
	// sessionEventHandler so a session that genuinely came back online and
	// then dies later gets a fresh attempt.
	autoReconnectAttempted map[string]bool
	// threadID → a config change needs a session restart but the thread is
	// busy (active turn / running background tasks); a watcher goroutine is
	// waiting to fire it once the thread goes quiet. Guarded by mu; see
	// app_session_config.go.
	pendingConfigReconnects map[string]bool
	// Test overrides for the deferred config-reconnect watcher cadence.
	// Zero means the defaults in app_session_config.go.
	configReconnectPollIntervalOverride time.Duration
	configReconnectQuietWindowOverride  time.Duration
	// threadID → persisted in-process system prompt overrides used for
	// discussion participants and other non-default session starts.
	threadSystemPrompts map[string]string
	// channelID → active deliberation state
	deliberations map[string]*discussion.Deliberation
	// gitWatchPumpsMu / gitWatchPumps index every active
	// GitStatusSubscribe by its wire-visible subscription ID.
	// gitwatch.Manager itself refcounts the underlying watchers per
	// cwd; this map is the App's view of "which id maps to which
	// pump goroutine + Subscription handle". gitWatchPumpWG tracks
	// pump goroutines so Shutdown drains them before returning.
	gitWatchPumpsMu sync.Mutex
	gitWatchPumps   map[string]*gitWatchPump
	gitWatchPumpWG  sync.WaitGroup
	// prUpdatePumps index active PR-scope review-pane polling
	// subscriptions. Each subscription owns one low-cadence poller and
	// emits only when the normalized snapshot changes.
	prUpdatePumpsMu  sync.Mutex
	prUpdatePumps    map[string]*prUpdatePump
	prUpdatePumpWG   sync.WaitGroup
	prUpdateInterval time.Duration
	prUpdateFetchFn  func(gitops.PRReference) (prUpdateSnapshot, error)
	// codexModelCatalog caches Codex's live app-server model/list response by
	// binary path. The catalog is provider-owned state, but fetching it spawns
	// a local CLI subprocess; cache and coalesce calls so settings/model menus
	// do not create process fan-out during normal rendering. See
	// internal/codexmodels. Lazy-init through codexModels() so tests that
	// build an *App via &App{...} don't have to pre-wire it.
	codexModelCatalogOnce sync.Once
	codexModelCatalog     *codexmodels.Cache
	// codexAccountUsageCache caches Codex's `account/usage/read` report per
	// (binary, active account). The read leaves the machine — the app-server
	// forwards it to the ChatGPT backend — and costs a whole subprocess when
	// no Codex session is live, so the usage overlay must not call it per
	// render. See internal/codexusage. Lazy-init through codexAccountUsage()
	// for the same reason as the model catalog.
	codexAccountUsageOnce  sync.Once
	codexAccountUsageCache *codexusage.Cache
	// codexSkillsCache caches Codex's `skills/list` answer per
	// (binary, cwd). Skills are directory-scoped, so a composer menu asks
	// once per workspace, and every miss is either a round trip on a live
	// session or a whole subprocess. Live sessions push invalidation into
	// it from `skills/changed`. See internal/codexskills. Lazy-init through
	// codexSkills() for the same reason as the model catalog.
	codexSkillsOnce  sync.Once
	codexSkillsCache *codexskills.Cache
	// mcpStatusCache is the provider-derived MCP status cache. Live
	// thread sessions push into it from their init/notification
	// events; the popup/settings pull from it and refresh via the
	// ephemeral fetchers (`claude mcp list` / Codex
	// `mcpServerStatus/list`) when no live session can feed it.
	// Lazy-init through mcpStatus() so tests building a bare App{}
	// don't have to wire it; explicit Invalidate happens on CRUD
	// edits and on OAuth completion.
	mcpStatusCacheOnce sync.Once
	mcpStatusCache     *mcpstatus.Cache
	// devServerProber dials loopback ports to gate the dev-server chip:
	// triage's textual detection only proves command output mentioned a
	// URL, so ProbeDevServerURL checks a listener actually exists before
	// the chip renders. Verdicts are TTL-bounded (internal/devserverprobe).
	// Lazy-init through devServerProbe() so tests building a bare App{}
	// don't have to wire it.
	devServerProbeOnce sync.Once
	devServerProber    *devserverprobe.Prober
	// highlightSpanCache is the content-addressed syntax-highlight span
	// cache (internal/highlight). Keys hash the full input, so entries
	// never go stale; lazy-init through highlightCache() so tests
	// building a bare App{} don't have to wire it.
	highlightCacheOnce sync.Once
	highlightSpanCache *highlight.Cache
	// highlightSeeder tracks per-streaming-item fence-scan state for
	// the remote-only highlight seed push (app_highlight_seed.go).
	// Zero value ready; internal locking.
	highlightSeeder highlightSeeder
	// diffSeedWorkers counts in-flight preview-push goroutines
	// (app_highlight_diff_seed.go); bursts past diffSeedMaxWorkers drop
	// their seeds instead of queueing behind the parse semaphore.
	diffSeedWorkers atomic.Int32
	// claudeConfigStore / codexConfigStore are the file-backed MCP
	// library adapters. AO is a 1:1 sync UI over Claude's
	// ~/.claude.json `mcpServers` and Codex's ~/.codex/config.toml
	// `[mcp_servers.*]` blocks — no SQLite library, no per-thread
	// snapshot. Tests inject path-scoped instances directly onto the
	// struct; production wires through the lazy claudeConfig() /
	// codexConfig() helpers in app_mcp.go.
	claudeConfigOnce  sync.Once
	claudeConfigStore *claudeconfig.Store
	claudeConfigErr   error
	codexConfigOnce   sync.Once
	codexConfigStore  *codexconfig.Store
	codexConfigErr    error
	// claudeMCPOAuthPolls dedups in-flight OAuth-completion pollers
	// per server name. Re-triggering OAuth for the same server
	// cancels the prior poll so only the most recent click drives
	// status updates and emits a single mcp:oauth-completed event.
	// Claude-only because Codex receives a native
	// `mcpServer/oauthLogin/completed` notification and doesn't
	// poll. Tracks the poll's identity (not the cancel func directly)
	// so a stale defer can compare pointers and avoid wiping a newer
	// poller's entry on its way out.
	claudeMCPOAuthPollsMu sync.Mutex
	claudeMCPOAuthPolls   map[string]*claudeMCPOAuthPoll
	// idleReaperStop signals the idle-session reaper goroutine to exit.
	// Set by startIdleSessionReaper during ServiceStartup; closed exactly
	// once by Shutdown before the parallel session close so the reaper
	// can't fire mid-teardown and race the session snapshot.
	idleReaperStop chan struct{}
	idleReaperWG   sync.WaitGroup
	// retentionCleanupStop signals the retention TTL sweep goroutine to
	// exit. Set by startRetentionCleanup during ServiceStartup; closed
	// exactly once by Shutdown between the idle reaper stop and the
	// parallel session close. The sweep writes to SQLite and calls
	// deleteThreadTreeLocked (which mutates a.sessions via stopSession)
	// — both must finish before subsequent shutdown steps tear down the
	// store and snapshot the session map.
	retentionCleanupStop chan struct{}
	retentionCleanupWG   sync.WaitGroup
	// backgroundFetchStop signals the background `git fetch` cadence to
	// exit. Set by startBackgroundGitFetch during ServiceStartup; closed
	// exactly once by Shutdown before the store closes, because each
	// pass reads the project list out of SQLite.
	backgroundFetchStop chan struct{}
	// backgroundFetchCancel cancels the context the cadence's git
	// subprocesses run under, so stopping the loop kills a `git fetch`
	// hanging on a dead network instead of waiting out its timeout.
	// Set and cleared alongside backgroundFetchStop, under a.mu.
	backgroundFetchCancel context.CancelFunc
	backgroundFetchWG     sync.WaitGroup
	// backgroundFetchErrors remembers the last background-fetch failure
	// per repository so an unreachable remote logs once rather than
	// every tick. See app_git_background_fetch.go.
	backgroundFetchErrors backgroundFetchErrorMemo
	// Chat-thread worktree setup runs, keyed by thread id: at most one
	// per thread, and only while it is running or after it FAILED — a
	// success drops its record. Guarded by its own mutex rather than
	// a.mu because a run settles from its own goroutine and must not
	// contend with session bookkeeping. See app_worktree_setup.go.
	worktreeSetupMu   sync.Mutex
	worktreeSetupRuns map[string]*worktreeSetupRun
	// worktreeSetupStopped is set by stopThreadWorktreeSetups in the same
	// critical section it snapshots the runs from, so no kickoff can join the
	// WaitGroup after the wait below has begun.
	worktreeSetupStopped bool
	// worktreeSetupWG joins every run goroutine in Shutdown before the
	// store closes, because settling a run writes the thread's durable
	// setup state.
	worktreeSetupWG sync.WaitGroup
	// Per-provider usage-probe gates (app_usage_probe_gate.go): every
	// automatic rate-limit refresh trigger funnels through one so bursts
	// coalesce and a cooldown bounds request rate. Lazily built via
	// claudeUsageGate() / codexUsageGate().
	claudeUsageGateOnce sync.Once
	claudeUsageGateVal  *usageProbeGate
	codexUsageGateOnce  sync.Once
	codexUsageGateVal   *usageProbeGate
	// usageBackoff holds per-account usage-endpoint 429 backoffs
	// (app_usage_backoff.go); the refresh paths consult it before sending
	// anything. Zero value ready.
	usageBackoff usageBackoffLedger
	// Test-only injection points for binding helpers that need to observe start/stop.
	startSessionFn        func(string) error
	stopSessionFn         func(string) error
	sendMessageFn         func(string, string, []string) error
	generateBranchNameFn  func(store.Thread, string) (string, error)
	generateThreadTitleFn func(store.Thread, string, []store.Attachment) (string, error)
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
	lookPathFn  func(string) error
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
	// downstream code emitted.
	testEmitHook func(name string, data any)
	// remoteClientProbeFn is a test-only override for hasRemoteClient
	// (production reads the transport server's connection counter).
	remoteClientProbeFn func() bool

	// savePayloadPickerFn is a test-only override for the save-file
	// dialog used by SavePayloadToFile. Production leaves it nil and
	// the real Wails dialog runs; tests install a stub that returns a
	// canned path (or empty for "cancelled").
	savePayloadPickerFn savePayloadPicker
	// rateLimitProbeClientOverride is a test-only injection seam for
	// the Claude rate-limit probe's HTTP client. Production leaves it
	// nil and the probe uses the package-level singleton; tests assign
	// a client pointing at a local httptest server.
	rateLimitProbeClientOverride *http.Client
	// dataDirOverride overrides the data directory root that initStores
	// otherwise resolves via os.UserConfigDir(). The app's data lives in
	// <root>/agent-overflow. Set by the --data-dir CLI flag (harness mode
	// requires it) and by tests, which use a t.TempDir() so the path is
	// deterministic across OSes — os.UserConfigDir() ignores XDG on macOS
	// (it returns $HOME/Library/Application Support), which env overrides
	// can't redirect.
	dataDirOverride string
	// providerExtraEnv is merged into every provider spawn's environment.
	// Harness mode uses it to hand ao-mockprovider its control-channel
	// address + token without exporting those credentials process-wide
	// (terminals, git hooks, and other children must not inherit them).
	// Set once before Start; never mutated afterwards.
	providerExtraEnv map[string]string
	// cliBinDir is the directory holding the canonical-name link to this
	// executable (D30, app_cli_path.go). sessionProcessEnv prepends it to
	// every provider session's PATH so an agent can type `agent-overflow`.
	// Empty means boot could not publish the link and sessions run without
	// the command — the `/workflow` composer block says so. Written once by
	// Start, read-only afterwards.
	cliBinDir string
	// providerBinaryOverride, when non-empty, wins over the settings-
	// backed provider binary paths in providerBinaryPath. Harness mode
	// points it at ao-mockprovider so the "providers are always mocked"
	// guarantee holds structurally — a settings update (RPC or UI) can
	// no longer repoint a spawn at a real claude/codex binary. Set once
	// before Start; never mutated afterwards.
	providerBinaryOverride string
	// fileKeychainOverride makes initStores build Credentials with the
	// file-backed Keychain stand-in instead of security(1). Harness mode
	// sets it because a redirected $HOME isolates every file store but
	// NOT the macOS Keychain — the active Claude slot's service name
	// ignores the home, so a security(1)-backed harness run would touch
	// the developer's real Claude Code login. Set once before Start;
	// never mutated afterwards.
	fileKeychainOverride bool
	// credentialHomeOverride, when non-empty, replaces os.UserHomeDir()
	// as the home that provideraccounts.Credentials operates under —
	// slot storage, canonical credential, ephemeral probe homes, and the
	// orphan prune all resolve beneath it. Harness mode always sets it
	// to the harness-owned home so that even AO_HARNESS_KEEP_HOME (which
	// keeps the real $HOME for provider session-file replay) can never
	// point the credential surface at the developer's real ~/.claude /
	// ~/.codex. Set once before Start; never mutated afterwards.
	credentialHomeOverride string
	// accountAuditPath is the durable append-only file auditAccountEvent
	// writes to (<dataDir>/account-audit.log). Written once by initStores.
	accountAuditPath string
	// backgroundFetchDisabled suppresses the background `git fetch`
	// cadence entirely. Harness mode sets it: e2e runs must be
	// deterministic and offline, and the harness's fixture repositories
	// exist to be asserted against, not fetched over. Unit tests never
	// reach it — they build *App directly and never call Start. Set once
	// before Start; never mutated afterwards.
	backgroundFetchDisabled bool
	// idleReaperNowFn is a test-only clock injection for the reaper.
	// Production leaves it nil and reaperNow reads time.Now directly.
	idleReaperNowFn func() time.Time
	// retentionNowFn is a test-only clock injection for the retention
	// sweep. Production leaves it nil and retentionNow reads time.Now
	// directly. Mirrors idleReaperNowFn.
	retentionNowFn func() time.Time
}

// session wraps a provider session regardless of type. Exactly one of
// the `claude` or `codex` pointers is non-nil; the provider string
// mirrors which. Use the `providerSession` accessor for the common
// methods (Send / Interrupt / RespondToApproval / Close) so call sites
// don't have to keep branching on the two typed fields.
type session struct {
	provider string
	token    string
	// credentialGeneration is the provider account generation this process
	// started against. Claude can adopt a new generation without restart;
	// Codex is safely reconnected before the next send.
	credentialGeneration uint64
	// credentialAccountID attributes provider-pushed quota events to the
	// account that authenticated this process. This matters after a wholesale
	// switch while older Codex turns are still finishing.
	credentialAccountID string
	// credentialAccount retains the verified, non-secret identity attached at
	// process start so a removed-but-still-running Codex session remains
	// attributable after its provider-wide metadata card is deleted.
	credentialAccount provider.AccountInfo
	// Exactly one of these is non-nil.
	claude    *claude.Session
	codex     *codex.Session
	claudetui *claudetui.Session
	// launchOpts is the SessionOptions bundle this session was spawned
	// with, replaced (token-guarded, see sessionManager.updateLaunchOpts)
	// when a config change is live-applied without a restart. The config
	// reconciler diffs it against the thread row's current options to
	// decide between live apply and restart (app_session_config.go).
	launchOpts provider.SessionOptions
	// aoToken / aoScope / aoEnv are the `ao` CLI credential this session's
	// process was spawned with (app_ao_session.go). The token is registered
	// when the session enters a.sessions and revoked when it leaves, so a
	// credential can never outlive the process holding it. Empty for sessions
	// the CLI cannot be scoped to (no transport server, no project).
	aoToken string
	aoScope transport.CallerScope
	aoEnv   map[string]string
	// liveness is the heap-allocated sibling that carries activity-tracking
	// atomics. Never nil for registered sessions — spawnProviderSession sets
	// it on construction. Stored behind a pointer so the value-type session
	// can still be copied through the a.sessions map without tripping vet's
	// atomic-copy check.
	liveness *sessionLiveness
}

// sessionLiveness tracks the inputs the idle-session reaper uses.
// lastActivityUnixNano is bumped on every provider event and every user
// send; activeTurns is incremented on EventTurnStart and decremented on
// EventTurnComplete so the reaper can skip sessions mid-turn. Both
// counters are atomic so the reaper's sweep can read them without
// taking a.mu beyond the map walk itself.
type sessionLiveness struct {
	lastActivityUnixNano atomic.Int64
	activeTurns          atomic.Int32
}

func newSessionLiveness(now time.Time) *sessionLiveness {
	l := &sessionLiveness{}
	l.lastActivityUnixNano.Store(now.UnixNano())
	return l
}

// bumpActivity stamps the current time as the last activity timestamp.
// Safe to call from any goroutine. now() is injected so tests can pin
// the clock; production callers pass time.Now. The stored value is
// monotonic even when two activity events observe the same wall-clock
// nanosecond; the idle reaper only needs ordering, and identical stamps
// make rapid event bursts look like no activity happened.
func (l *sessionLiveness) bumpActivity(now time.Time) {
	if l == nil {
		return
	}
	next := now.UnixNano()
	for {
		prev := l.lastActivityUnixNano.Load()
		if next <= prev {
			next = prev + 1
		}
		if l.lastActivityUnixNano.CompareAndSwap(prev, next) {
			return
		}
	}
}

// providerSession returns the underlying provider-agnostic Session, or
// nil when neither typed field is populated. The zero value is
// intentionally left nil so callers can distinguish "no provider" from
// a panic-inducing partial state.
func (s session) providerSession() provider.Session {
	switch {
	case s.claude != nil:
		return s.claude
	case s.codex != nil:
		return s.codex
	case s.claudetui != nil:
		return s.claudetui
	default:
		return nil
	}
}

func NewApp() *App {
	app := &App{
		sessions:                       make(map[string]session),
		aoTokens:                       make(map[string]transport.CallerScope),
		threadActionLocks:              newKeyedLocks(),
		startingSessions:               make(map[string]*sessionStart),
		reconnectingThreads:            make(map[string]bool),
		autoReconnectAttempted:         make(map[string]bool),
		turnObservers:                  make(map[string]map[uint64]turnObserver),
		threadSystemPrompts:            make(map[string]string),
		deliberations:                  make(map[string]*discussion.Deliberation),
		gitWatchPumps:                  make(map[string]*gitWatchPump),
		prUpdatePumps:                  make(map[string]*prUpdatePump),
		providerCredentialFingerprints: make(map[string][32]byte),
		worktreeSetupRuns:              make(map[string]*worktreeSetupRun),
	}
	app.installDiscussionTurnObserver()
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

// --- Item operations ---

// ListItems returns every item persisted for a thread in chronological order.
func (a *App) ListItems(threadID string) ([]store.Item, error) {
	return a.store.ListItems(threadID)
}

// --- Payload operations ---
