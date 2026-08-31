package main

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/updater"

	appbrowser "agent-overflow/internal/browser"
	"agent-overflow/internal/claudeconfig"
	"agent-overflow/internal/codexconfig"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/mcpstatus"
	"agent-overflow/internal/sessionimport"
	"agent-overflow/internal/usagebackoff"

	"agent-overflow/internal/workflowhost"
)

// The App struct's concern groups.
//
// Most of App's fields are single-owner: 70% are referenced by exactly one
// `app_*.go` besides `app.go` (docs/architecture/root-decomposition.md §(a)).
// Collapsing each such run into a named struct is what lets the struct be read
// as a list of concerns instead of a wall of 200 names, and it makes the cut
// line visible if a concern is ever promoted to its own package.
//
// Rules these groups follow, and any new one must:
//
//   - Plain named struct types stored as NAMED fields (`updater
//     appUpdaterState`), never embedded — an embedded group would promote its
//     field names back onto App and undo the whole point.
//   - A mutex moves into a group only WITH EVERY FIELD IT GUARDS. A group is
//     never a reason to separate a lock from its wards.
//   - `mu` (session lifecycle) and its wards stay on App itself, along with the
//     ambient dependencies (store, settings, configDir, triage, logger,
//     transportServer, telemetry, shuttingDown, ...) that every cluster reads.
//   - Only genuinely single-cluster runs group. A field two clusters touch
//     stays top-level.

// appUpdaterState is the in-app self-update concern (`app_updater*.go`):
// the Wails updater handle, the provider behind it, and the resolve/download/
// install bookkeeping its own mutex guards.
type appUpdaterState struct {
	// handle drives in-app self-update (check / download / restart) via the
	// Wails app.Updater singleton. Set once at desktop boot by initUpdater
	// (app_updater_desktop.go) before the transport server starts, so the
	// updater RPC handlers observe it without a race. Stays nil in the
	// headless WSL backend (no Wails application) and in tests; every updater
	// RPC method guards the nil case and reports the feature unsupported
	// rather than panicking.
	handle *updater.Updater
	// provider is the targetable GitHub provider behind handle, retained so
	// the version-selection RPCs (ListReleases, DownloadUpdate) can enumerate
	// releases and aim a download at a specific tag. Set alongside handle by
	// initUpdater; nil on the headless backend.
	provider *targetableProvider
	// mu serializes the provider-retarget-then-resolve sequences that
	// CheckForUpdate (SetTarget("")+Check) and DownloadUpdate (SetTarget(tag)+
	// Check) perform. Without it, a concurrent passive CheckForUpdate from a
	// second --connect client could reset the target between a by-tag
	// download's SetTarget and its Check, resolving "latest" instead of the
	// picked tag. busy (guarded by this mutex) marks an in-flight
	// download so a racing CheckForUpdate skips its network probe rather than
	// clobbering the pending release the installer is about to use.
	mu   sync.Mutex
	busy bool
	// pending mirrors the release handle would install right now:
	// stashed by every successful resolve (CheckForUpdate and DownloadUpdate's
	// by-tag path), cleared when a resolve finds nothing or fails. Only
	// meaningful while handle.State() == StateAvailable. It exists for the
	// WSL staging step, which needs the release's asset filename and digest
	// AFTER DownloadAndInstall — the Updater exposes neither, only the staged
	// file's path. Guarded by mu.
	pending *updater.Release
	// staged is the release copied into the Windows-side staging
	// directory and waiting for RestartToUpdate to hand it to the launcher.
	// WSL mode only; nil until a download stages successfully. Guarded by mu.
	staged *updater.Release
	// install is the install RestartToUpdate handed the launcher and has
	// not yet settled; nil at rest. installAcked distinguishes its two
	// phases — awaiting the launcher's acknowledgement, then awaiting the
	// process death that acknowledgement promised — and installTimer is
	// the single deadline whichever phase is currently under (see
	// armWSLInstallDeadlineLocked; one field means a phase change can never
	// leave the previous phase's timer armed). installGen rises on every
	// armed deadline so a callback whose deadline was replaced or settled —
	// even one already fired and parked on mu — cannot unwind an
	// install it no longer speaks for. All guarded by mu.
	install      *updater.Release
	installAcked bool
	installGen   uint64
	installTimer *time.Timer
	// applyFailure is the boot-detected "the launcher never applied the
	// staged update" notice, surfaced to the UI on
	// UpdateAvailability.LastApplyFailure. Process-lifetime only: it is
	// recomputed from the on-disk marker at every boot and never persisted, so
	// a boot that finds no marker (or one matching the running version) starts
	// with it empty. Guarded by mu.
	applyFailure string
	// wsl is non-nil only on the headless WSL backend spawned by the
	// Windows launcher (see initWSLUpdater). It is the mode switch every WSL
	// branch of the updater RPCs keys off, and carries the two directories that
	// path needs. Immutable after init — set before the transport server
	// starts, read without a lock afterwards.
	wsl *wslUpdateMode
	// restartExitFn is the process-exit call RestartToUpdate's watchdog
	// fires when graceful shutdown wedges (see armRestartExitWatchdog).
	// nil means os.Exit; tests inject a recorder.
	restartExitFn func(code int)
}

// appFlushDispatchState is the queued-message flush concern
// (`app_flush_queue*.go`): the per-thread dispatch batches and the two mutexes
// whose hierarchy RegisterQueueItem documents.
type appFlushDispatchState struct {
	// mu guards the per-thread dispatch bookkeeping below. Triage decides
	// whether a drain is boundary or immediate; App owns the asynchronous
	// provider writes so sequence allocation and Send/Steer locking stay in
	// the same layer.
	mu            sync.Mutex
	queues        map[string][]flushDispatchBatch
	current       map[string]flushDispatchBatch
	running       map[string]bool
	inflightItems map[string]int
	generation    map[string]uint64
	wg            sync.WaitGroup
	// handoffMu serializes RegisterQueueItem's enqueue→flush handoff
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
	handoffMu sync.Mutex
}

// appGitStatusPumpState is the "git:status" fan-out concern
// (`app_gitwatch.go`), distinct from the gitwatch.Manager it subscribes to.
type appGitStatusPumpState struct {
	// pumps holds one pump per canonical cwd — one gitwatch.Subscription and
	// one goroutine forwarding it to the "git:status" channel, shared by every
	// caller of that workspace via the pump's refcount. handles maps each
	// caller's wire-visible GitStatusSubscribe id to the PUMP it holds a
	// reference on, which is what GitStatusUnsubscribe and the per-connection
	// cleanup release. The pump, not its cwd: a dying pump can be replaced
	// under the same cwd, and a handle naming the cwd would then release a
	// reference it never took. Both maps are guarded by mu; wg tracks pump
	// goroutines so Shutdown drains them before returning.
	mu      sync.Mutex
	pumps   map[string]*gitWatchPump
	handles map[string]*gitWatchPump
	wg      sync.WaitGroup
}

// appPRUpdateState is the PR-scope review-pane polling concern
// (`app_pr_updates.go`).
type appPRUpdateState struct {
	// pumps index active PR-scope polling by PR key
	// ("<forge>:<namespace>/<repo>:<number>"). Each PR owns ONE
	// low-cadence poller and one change-detection state however many
	// callers watch it, and emits only when the normalized snapshot (or
	// its failure) changes. handles maps each caller's wire-visible
	// SubscribePRUpdates id to its reference on that pump, which is what
	// UnsubscribePRUpdates, SetPRUpdatesActive, and the per-connection
	// cleanup act on. Both maps are guarded by mu; wg tracks pump goroutines
	// so Shutdown drains them before returning. seq stamps every stored pump
	// state so a subscriber can order the frames it sees against the state
	// its subscribe returned; it is GLOBAL rather than per-pump because a
	// pump can be replaced under its key (a dead pump's successor), and a
	// per-pump counter would restart at zero — letting the dead one's late
	// frames outrank the replacement's fresh state.
	mu       sync.Mutex
	pumps    map[string]*prUpdatePump
	handles  map[string]*prUpdateHandle
	seq      uint64
	wg       sync.WaitGroup
	interval time.Duration
	fetchFn  func(gitops.PRReference) (prUpdateSnapshot, error)
}

// appMCPState is the MCP-library concern (`app_mcp*.go`): the two file-backed
// config adapters, the status cache, and the three auth/reload coordination
// locks with their own maps.
type appMCPState struct {
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
	// statusCache is the provider-derived MCP status cache. Live
	// thread sessions push into it from their init/notification
	// events; the popup/settings pull from it and refresh via the
	// ephemeral fetchers (`claude mcp list` / Codex
	// `mcpServerStatus/list`) when no live session can feed it.
	// Lazy-init through mcpStatus() so tests building a bare App{}
	// don't have to wire it; explicit Invalidate happens on CRUD
	// edits and on OAuth completion.
	statusCacheOnce sync.Once
	statusCache     *mcpstatus.Cache
	// claudeOAuthPolls dedups in-flight OAuth-completion pollers
	// per server name. Re-triggering OAuth for the same server
	// cancels the prior poll so only the most recent click drives
	// status updates and emits a single mcp:oauth-completed event.
	// Claude-only because Codex receives a native
	// `mcpServer/oauthLogin/completed` notification and doesn't
	// poll. Tracks the poll's identity (not the cancel func directly)
	// so a stale defer can compare pointers and avoid wiping a newer
	// poller's entry on its way out.
	claudeOAuthPollsMu sync.Mutex
	claudeOAuthPolls   map[string]*claudeMCPOAuthPoll
	// workspaceAuthFlows owns provider processes started for OAuth from
	// an unmaterialized draft. The process is keyed by the provider config
	// entity rather than a thread, and stays alive through the browser hop.
	// Concurrent clicks for the same target share one startup and URL.
	workspaceAuthMu      sync.Mutex
	workspaceAuthFlows   map[workspaceMCPAuthKey]*workspaceMCPAuthRun
	workspaceAuthStarter workspaceMCPAuthStarter
	// codexReloads coalesces async `config/mcpServer/reload` requests
	// per thread (requestCodexMCPReload): the RPC is a level trigger, so
	// requests landing while one is in flight collapse into a single
	// follow-up run. Codex-only; Claude reconnects are per-server RPCs
	// with no whole-config semantics to coalesce.
	codexReloadsMu sync.Mutex
	codexReloads   map[string]*codexMCPReloadState
}

// appWorktreeSetupState is the chat-thread worktree setup concern
// (`app_worktree_setup.go`).
type appWorktreeSetupState struct {
	// runs are keyed by thread id: at most one per thread, and only while it
	// is running or after it FAILED — a success drops its record. Guarded by
	// its own mutex rather than a.mu because a run settles from its own
	// goroutine and must not contend with session bookkeeping.
	mu   sync.Mutex
	runs map[string]*worktreeSetupRun
	// stopped is set by stopThreadWorktreeSetups in the same critical section
	// it snapshots the runs from, so no kickoff can join the WaitGroup after
	// the wait below has begun.
	stopped bool
	// wg joins every run goroutine in Shutdown before the store closes,
	// because settling a run writes the thread's durable setup state.
	wg sync.WaitGroup
}

// appSessionImportState is the session-import concern
// (`app_session_import*.go`): the provider-home scan cache and the single
// in-flight import run.
type appSessionImportState struct {
	// scans caches provider-home scans behind the import modal
	// (internal/sessionimport). Lazy-init through sessionImportScanCache()
	// so tests building a bare App{} don't wire it.
	scansOnce sync.Once
	scans     *sessionimport.ScanCache
	// The one in-flight session-import run. Importing writes threads and
	// projects, and two concurrent runs over overlapping ids would race the
	// dedup set that makes "Import All" idempotent — so there is at most one,
	// and a second request is refused rather than queued. Same
	// stopped-flag-plus-WaitGroup discipline as the worktree setups;
	// Shutdown joins it before the store closes. See app_session_import_run.go.
	mu      sync.Mutex
	active  *sessionImportRun
	stopped bool
	wg      sync.WaitGroup
}

// appBrowserState is the provider-neutral arbitrary-web browser concern. The
// MCP listener is cheap and per-session-tokened; Manager owns the lazily
// launched Chrome process and workspace BrowserContexts.
type appBrowserState struct {
	manager            *appbrowser.Manager
	mcp                *appbrowser.MCPServer
	applyMu            sync.Mutex
	applyWG            sync.WaitGroup
	settingsGeneration atomic.Uint64
	liveEnabled        atomic.Bool
}

// appBackgroundFetchState is the background `git fetch` cadence
// (`app_git_background_fetch.go`).
type appBackgroundFetchState struct {
	// mu guards the cadence's start/stop handshake (stop + cancel). Its own
	// mutex, not a.mu: the git-fetch cadence shares nothing with session
	// lifecycle, and the two fields are set and cleared as one unit so
	// they must live under one lock. Nothing inside its critical
	// sections takes another App mutex (lifeCtx is a plain field read),
	// and no a.mu holder touches these fields — the two locks are
	// disjoint.
	mu sync.Mutex
	// stop signals the background `git fetch` cadence to
	// exit. Set by startBackgroundGitFetch during ServiceStartup; closed
	// exactly once by Shutdown before the store closes, because each
	// pass reads the project list out of SQLite.
	stop chan struct{}
	// cancel cancels the context the cadence's git subprocesses run under,
	// so stopping the loop kills a `git fetch` hanging on a dead network
	// instead of waiting out its timeout. Set and cleared alongside stop,
	// under mu.
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// errors remembers the last background-fetch failure per repository so
	// an unreachable remote logs once rather than every tick.
	errors backgroundFetchErrorMemo
}

// appWorkflowAutoResumeState is the live arming behind parked runs that resume
// themselves (`app_workflow_autoresume.go`).
type appWorkflowAutoResumeState struct {
	// timers holds one timer per parked run that will resume itself. The
	// durable record is `work_items.auto_resume_at`; this is only the live
	// arming, rebuilt at boot and disarmed by every transition out of the
	// park. The engine owns no timers by boundary, which is why the schedule
	// lives here.
	mu     sync.Mutex
	timers map[string]workflowhost.Timer
	// newTimer and nowFn are test-only injections, mirroring idleReaperNowFn.
	// Production leaves both nil.
	newTimer func(time.Duration, func()) workflowhost.Timer
	nowFn    func() time.Time
}

// appTurnObserverState fans provider events out to internal App features after
// triage handling has been attempted.
type appTurnObserverState struct {
	// Each registration lives until its returned unsubscribe function runs;
	// the built-in discussion observer lives for the App lifetime. mu is
	// deliberately independent of a.mu so callbacks can safely enter other
	// App coordination paths.
	mu             sync.RWMutex
	byThread       map[string]map[uint64]turnObserver
	nextID         uint64
	discussionOnce sync.Once
}

// appUsageProbeState is the rate-limit refresh concern
// (`app_usage_probe_gate.go`, `app_ratelimits.go`): what may probe the usage
// endpoint, and when.
type appUsageProbeState struct {
	// Per-provider usage-probe gates: every automatic rate-limit refresh
	// trigger funnels through one so bursts coalesce and a cooldown bounds
	// request rate. Lazily built via claudeUsageGate() / codexUsageGate().
	claudeGateOnce sync.Once
	claudeGate     *usageProbeGate
	codexGateOnce  sync.Once
	codexGate      *usageProbeGate
	// backoff holds per-account usage-endpoint 429 backoffs
	// (internal/usagebackoff); the refresh paths consult it before sending
	// anything. Zero value ready.
	backoff usagebackoff.Ledger
	// turnActivityByProvider records the last turn completion per provider.
	// The periodic rate-limit poll reads it so an idle app — open threads, no
	// turns — sends zero usage-endpoint requests.
	turnActivityMu         sync.Mutex
	turnActivityByProvider map[string]time.Time
}

// appThreadTitleGenState is the in-flight set for thread-title generation
// (`app_thread_title_generation.go`).
type appThreadTitleGenState struct {
	// active is the set of threads with a title generation in flight. Auto,
	// heal, and user-triggered regeneration all claim through it, so N sends
	// on a still-default thread — or N impatient clicks — cost one run of up
	// to two 3-minute CLI attempts instead of N. Lazily built.
	mu     sync.Mutex
	active map[string]struct{}
}

// appMarkThreadReadState joins the background thread read-state stamps
// (`app_session_bindings.go`).
type appMarkThreadReadState struct {
	// SwitchThread registers one stamp per focus so the RPC the UI blocks on
	// doesn't queue behind the store's single writer connection. stopped is
	// set inside the same critical section that the WaitGroup is joined from,
	// so no stamp can register after the wait has begun; Shutdown joins them
	// before the store closes.
	mu      sync.Mutex
	stopped bool
	wg      sync.WaitGroup
}

// appCodexThreadCostState single-flights the post-turn Codex thread-cost read,
// per thread (`app_codex_thread_cost.go`).
type appCodexThreadCostState struct {
	// The read is fired asynchronously from a settled turn and forwarded by
	// the app-server to the ChatGPT backend, so a fast follow-up turn can
	// settle while the previous read is still out; without the gate each
	// settle would add another concurrent backend request for a figure that
	// only gets more accurate by waiting.
	//
	// The value is the slot's whole state, not a bare presence marker: a
	// settle during an in-flight read cannot simply be dropped (that read
	// may have been answered before the turn completed), so it marks the
	// slot dirty, hands over its own session token, and the owner goes
	// around once more against the CURRENT session.
	//
	// There is deliberately no companion "the delete failed" map. A stored
	// row names the provider thread it was read from (store migration v68),
	// and every read compares that against the thread's current SessionRef,
	// so a row a rollback could not delete is already unreadable — no
	// process-lifetime marker is standing between it and the user.
	mu       sync.Mutex
	inflight map[string]*codexThreadCostRead
}
