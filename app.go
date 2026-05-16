package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/checkpoint"
	"agent-overflow/internal/codexmodels"
	"agent-overflow/internal/design"
	"agent-overflow/internal/discussion"
	"agent-overflow/internal/errorsx"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitwatch"
	"agent-overflow/internal/keybindings"
	"agent-overflow/internal/logging"
	obsotel "agent-overflow/internal/observability/otel"
	"agent-overflow/internal/observability/replay"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/screenshot"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
	"agent-overflow/internal/terminal"
	"agent-overflow/internal/textgen"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/uitrace"
	"agent-overflow/internal/workspacefiles"

	"github.com/wailsapp/wails/v3/pkg/application"
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

// App is the primary Wails-bound struct, registered as a v3 service.
type App struct {
	app         *application.App
	store       *store.Store
	git         *gitops.Core
	gitWatch    *gitwatch.Manager
	settings    *settings.Service
	triage      *triage.Router
	checkpoints *checkpoint.Store
	registry    *discussion.Registry
	channels    *discussion.ChannelService
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
	// uiTracer is the dev-only JSONL render-trace appender. It's lazily
	// constructed from configDir the first time AppendUIRenderTraceBatch
	// runs so tests that build a bare App{configDir: t.TempDir()} stay
	// cheap, and so production wiring doesn't need an explicit init step.
	// uiTraceOnce serializes construction; uiTraceErr captures a failed
	// New so subsequent calls fail loudly instead of silently no-op'ing.
	uiTraceOnce sync.Once
	uiTracer    *uitrace.Tracer
	uiTraceErr  error
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
	// transportServer is the Phase C HTTP+WS transport. Set by main.go
	// via SetTransportServer before app.Run() so Shutdown can drain
	// in-flight RPCs BEFORE App subsystems (store, telemetry, sessions)
	// close. atomic.Pointer matches eventBus — wiring can land any time
	// without racing Shutdown's reader.
	transportServer atomic.Pointer[transport.Server]
	// shuttingDown is flipped to true once Shutdown begins. Binding entry
	// points that spin up new work (StartSession, SendMessage, ReconnectSession)
	// check it and fail fast with ErrShuttingDown so late RPCs can't race
	// with subsystem teardown.
	shuttingDown atomic.Bool
	mu           sync.Mutex
	sessions     map[string]session // threadID → active session
	// threadActionLocks serializes per-thread workflows that must observe a
	// stable thread timeline or workspace while they run.
	threadActionLocksOnce sync.Once
	threadActionLocks     *threadActionLockRegistry
	// flushDispatchQueues serializes queued-message flush batches per
	// thread. Triage decides whether the drain is boundary or immediate;
	// App owns the asynchronous provider writes so sequence allocation and
	// Send/Steer locking stay in the same layer.
	flushDispatchMu            sync.Mutex
	flushDispatchQueues        map[string][]flushDispatchBatch
	flushDispatchRunning       map[string]bool
	flushDispatchInflightItems map[string]int
	flushDispatchWG            sync.WaitGroup
	// threadID → in-flight session start. Concurrent callers wait for the
	// first start attempt instead of spawning duplicate provider runtimes.
	startingSessions map[string]*sessionStart
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
	// codexModelCatalog caches Codex's live app-server model/list response by
	// binary path. The catalog is provider-owned state, but fetching it spawns
	// a local CLI subprocess; cache and coalesce calls so settings/model menus
	// do not create process fan-out during normal rendering. See
	// internal/codexmodels. Lazy-init through codexModels() so tests that
	// build an *App via &App{...} don't have to pre-wire it.
	codexModelCatalogOnce sync.Once
	codexModelCatalog     *codexmodels.Cache
	// idleReaperStop signals the idle-session reaper goroutine to exit.
	// Set by startIdleSessionReaper during ServiceStartup; closed exactly
	// once by Shutdown before the parallel session close so the reaper
	// can't fire mid-teardown and race the session snapshot.
	idleReaperStop chan struct{}
	idleReaperWG   sync.WaitGroup
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
	emitEventFn            func(eventName string, data any)
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
	// idleReaperNowFn is a test-only clock injection for the reaper.
	// Production leaves it nil and reaperNow reads time.Now directly.
	idleReaperNowFn func() time.Time
}

// session wraps a provider session regardless of type. Exactly one of
// the `claude` or `codex` pointers is non-nil; the provider string
// mirrors which. Use the `providerSession` accessor for the common
// methods (Send / Interrupt / RespondToApproval / Close) so call sites
// don't have to keep branching on the two typed fields.
type session struct {
	provider string
	token    string
	// Exactly one of these is non-nil.
	claude *claude.Session
	codex  *codex.Session
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
	default:
		return nil
	}
}

func NewApp() *App {
	return &App{
		sessions:            make(map[string]session),
		threadActionLocks:   newThreadActionLocks(),
		startingSessions:    make(map[string]*sessionStart),
		threadSystemPrompts: make(map[string]string),
		deliberations:       make(map[string]*discussion.Deliberation),
		gitWatchPumps:       make(map[string]*gitWatchPump),
	}
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

// ServiceStartup is called by Wails v3 when the service is initialised.
// The body is split into three phases (initStores → initObservability →
// initSubsystems) so the dependency order is obvious: stores boot first
// because every other subsystem either embeds the store or reads from it,
// observability boots next because the triage router installs metrics
// before it can accept events, and the remaining subsystems (triage,
// checkpoints, discussion, design, terminals, attachments, workspace
// search) boot last once their inputs are ready.
func (a *App) ServiceStartup(ctx context.Context, options application.ServiceOptions) error {
	a.app = application.Get()

	dbDir, st, err := a.initStores()
	if err != nil {
		return err
	}
	if err := a.initObservability(ctx, dbDir); err != nil {
		return err
	}
	if err := a.initSubsystems(dbDir, st); err != nil {
		return err
	}

	// Probe provider binaries once on boot so the thread-level banner can
	// surface "claude not found" / "codex too old" before the user opens
	// settings. Runs in a goroutine because DetectProvider spawns subprocesses
	// (up to 5s per provider) and we never want that blocking app startup.
	go a.probeStartupProviderStatuses()

	// Probe authenticated account info (planType, subscription) for both
	// providers. Separate goroutine because the account probe spawns a
	// short-lived provider process and runs the wire handshake — slower
	// than DetectProvider's version check. Results land on the frontend
	// via the `provider:account` event.
	go a.probeStartupAccountInfo()

	// Start the Claude rate-limit probe loop. Fires once at startup,
	// then every 2 mins while at least one Claude session is alive;
	// turn-complete is wired separately in sessionEventHandler. The
	// probe reads `anthropic-ratelimit-unified-*` response headers from
	// a minimal Messages API call — see internal/provider/claude/
	// ratelimits_probe.go for the rationale.
	a.startClaudeRateLimitProbeLoop()

	// Start the idle-session reaper. Walks a.sessions every
	// IdleReapInterval and closes provider subprocesses that have been
	// idle past IdleReapThreshold so leaked subprocesses can't pile up
	// across long-running app sessions. See app_session_reaper.go.
	a.startIdleSessionReaper()

	return nil
}

// initStores resolves the on-disk data directory, opens SQLite, wires the
// git/settings helpers, and installs the provider-event logger. Returns
// (dbDir, store) so later phases can attach their own subdirectories
// (replay/, attachments/, design-artifacts/) without re-computing the
// root path.
//
// A logger init failure closes the store before returning so we don't
// leak an open DB file on startup error; the close error is joined onto
// the logger error so tests see both causes.
func (a *App) initStores() (string, *store.Store, error) {
	dataDir, err := os.UserConfigDir()
	if err != nil {
		// Fall back to ~/.agent-overflow/ which persists across reboots,
		// unlike os.TempDir() which is cleaned on reboot and would lose data.
		homeDir, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return "", nil, fmt.Errorf("cannot determine config directory: %w (home dir also unavailable: %v)", err, homeErr)
		}
		dataDir = homeDir
	}
	dbDir := filepath.Join(dataDir, "agent-overflow")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		return "", nil, fmt.Errorf("failed to create data directory %s: %w", dbDir, err)
	}
	dbPath := filepath.Join(dbDir, "agent-overflow.db")

	st, err := store.New(dbPath)
	if err != nil {
		return "", nil, fmt.Errorf("failed to open database: %w", err)
	}

	a.store = st
	a.git = gitops.NewCore()
	a.gitWatch = gitwatch.NewManager(a.git.Status)
	a.settings = settings.NewService(dbDir)
	a.logger, err = logging.NewProviderEventLogger(dbDir)
	if err != nil {
		closeErr := st.Close()
		return "", nil, errors.Join(
			fmt.Errorf("failed to initialize provider event logger: %w", err),
			errorsx.WrapLifecycle("close store after logger initialization failure", closeErr),
		)
	}
	return dbDir, st, nil
}

// initObservability wires the opt-in OTEL provider and the per-thread
// replay log. Both read from the settings snapshot; when disabled they
// install noop-backed providers so callers never have to nil-check.
// A telemetry init failure is non-fatal — we print it and fall back to a
// disabled provider so the rest of the app still boots.
func (a *App) initObservability(ctx context.Context, dbDir string) error {
	settingsSnapshot := a.settings.Get()
	telemetry, err := obsotel.NewProvider(ctx, obsotel.ConfigFromFlags(
		settingsSnapshot.ObservabilityTracingEnabled,
		settingsSnapshot.ObservabilityOtlpEndpoint,
	))
	if err != nil {
		// Telemetry failure is non-fatal: we log it and proceed with a
		// no-op provider so the rest of the app still boots. Users will
		// see the failure via the app log; the settings toggle remains on
		// so they can fix the endpoint and restart.
		fmt.Printf("observability: tracing setup failed, proceeding without telemetry: %v\n", err)
		telemetry, _ = obsotel.NewProvider(ctx, obsotel.Config{Enabled: false})
	}
	a.telemetry = telemetry

	telemetryMetrics := a.telemetry.Metrics()
	a.replay = replay.NewManager(replay.ManagerConfig{
		RootDir: filepath.Join(dbDir, "replay"),
		Enabled: settingsSnapshot.ObservabilityEventLogEnabled,
		DropHook: func() {
			telemetryMetrics.ReplayEventsDropped.Add(context.Background(), 1)
		},
	})
	return nil
}

// initSubsystems wires the remaining services: triage (with metrics and
// checkpoint capture), discussion registry/channels, design artifacts and
// reactor, the design MCP server, terminals, attachments, and workspace
// search. Called after initStores / initObservability so every subsystem
// has its dependencies in place.
//
// An attachment init failure closes the store before returning — matching
// the logger init failure path in initStores — so a boot error never
// leaves an open DB handle dangling.
func (a *App) initSubsystems(dbDir string, st *store.Store) error {
	telemetryMetrics := a.telemetry.Metrics()

	a.triage = triage.NewRouter(st, a.emitWithReplay())
	a.triage.SetTelemetry(a.telemetry.Tracer(), triage.TurnMetrics{
		TurnsStarted:      telemetryMetrics.TurnsStarted,
		TurnsCompleted:    telemetryMetrics.TurnsCompleted,
		TurnsErrored:      telemetryMetrics.TurnsErrored,
		ItemsPersisted:    telemetryMetrics.ItemsPersisted,
		PayloadsPersisted: telemetryMetrics.PayloadsPersisted,
	})
	// Flush-queue callbacks: drain queued user messages at provider
	// boundaries and capture message checkpoints once the provider echo
	// confirms the deferred user row. See app_flush_queue.go.
	a.configureTriageQueueCallbacks()
	if err := st.ReconcileProposedPlanStateFromAcceptedTurns(time.Now().UnixMilli()); err != nil {
		log.Printf("app: reconcile proposed plan state: %v", err)
	}
	// Synthesize session_died terminals for backgrounded launches whose
	// owning provider session did not survive the previous app instance.
	// Without this sweep the launches would render as "running" forever
	// in the chat and the tray, since no live agent will ever observe
	// their completion. See docs/architecture/turn-lifecycle.md
	// §Crash recovery.
	if recovered, err := a.triage.RecoverOrphanedBackgroundTasks(); err != nil {
		log.Printf("app: recover orphaned background tasks: %v", err)
	} else if recovered > 0 {
		log.Printf("app: recovered %d orphaned background launches as session_died", recovered)
	}
	a.checkpoints = checkpoint.NewStore()
	a.cleanupLegacyCheckpointRefs(st)
	a.registry = discussion.NewRegistry(st)
	a.channels = discussion.NewChannelService(st)
	designBase := filepath.Join(dbDir, "design-workdirs")
	a.designWorkdir = design.NewWorkDirManager(designBase)
	a.designDiagnostics = design.NewDiagnosticBuffer(nil)
	a.designServer = design.FileHandler(designBase)
	a.designWatchers = make(map[string]*design.Watcher)
	// Headless Chromium-driven capture for the agent's read_screenshot
	// tool. Lazy: the binary downloads on first capture (~150 MB
	// once), and the browser process boots only when something asks
	// for a screenshot. Threads that never call read_screenshot pay
	// nothing.
	a.screenshotManager = screenshot.NewManager(
		screenshot.NewInstaller(dbDir, a.emit),
	)
	a.reactor = design.NewReactor(a.designDiagnostics, a.newDesignCapturer())
	a.designMCP = design.NewMCPServer(a.reactor)
	a.terminals = terminal.NewManager(a.terminalOutputCallback, a.terminalExitCallback)
	attachmentStore, err := attachment.NewStore(attachment.Config{
		RootDir: filepath.Join(dbDir, "attachments"),
	}, st)
	if err != nil {
		closeErr := st.Close()
		return errors.Join(
			fmt.Errorf("failed to initialise attachment store: %w", err),
			errorsx.WrapLifecycle("close store after attachment init failure", closeErr),
		)
	}
	a.attachments = attachmentStore
	a.workspaceFiles = workspacefiles.NewSearcher(workspacefiles.Config{})
	a.configDir = dbDir
	return nil
}

// Shutdown timeouts. Each subsystem gets its own budget so a slow telemetry
// flush can't eat the replay writer's window. The top-level ServiceShutdown
// wrapper does NOT sum these — it calls Shutdown(context.Background()) and
// lets each subsystem enforce its own deadline below. Callers (tests) that
// want a ceiling on total shutdown latency can pass their own ctx.
const (
	// sessionShutdownTimeout caps how long Shutdown will wait for every
	// provider session to close in parallel before giving up and moving on
	// to the rest of the teardown. Sessions that don't finish in time get
	// abandoned — the Wails process is going away regardless, so the
	// underlying subprocesses will be reaped by the OS.
	sessionShutdownTimeout = 5 * time.Second

	// reactorDrainTimeout caps how long we wait for in-flight triage
	// Handle calls to return before continuing shutdown. Triage work is
	// short-lived in practice (SQLite writes over the local FS), so 2s is
	// generous; the guard is there to stop a stuck goroutine from
	// blocking user-perceived Quit latency.
	reactorDrainTimeout = 2 * time.Second

	// replayShutdownTimeout bounds the replay manager's queue drain.
	replayShutdownTimeout = 2 * time.Second

	// telemetryShutdownTimeout bounds the OTLP exporter flush. The otel
	// package applies its own internal cap on top of this.
	telemetryShutdownTimeout = 5 * time.Second

	// transportShutdownDrainTimeout caps how long Shutdown waits for the
	// embedded HTTP+WS server to drain in-flight RPCs before continuing
	// with subsystem teardown. Short by design — once subsystems start
	// closing, any RPC still running would observe closed-store errors
	// anyway, so we pay a bounded delay to let the polite ones finish.
	transportShutdownDrainTimeout = 3 * time.Second
)

// Shutdown tears the App down in a documented order. See the numbered steps
// inline for the rationale. Shutdown is idempotent and safe to call on a
// zero-value App: every guard is nil-safe so tests can wire a subset of
// subsystems without threading nil-checks through the call sites.
//
// Errors from individual subsystems are collected and joined — Shutdown
// never aborts on the first failure because every caller is mid-quit and
// we want every subsystem to flush before the process exits.
//
// `//wails:ignore` keeps this method out of the auto-generated TS bindings.
// Shutdown is a process-local lifecycle hook; the frontend has no business
// calling it.
//
//wails:ignore
func (a *App) Shutdown(ctx context.Context) error {
	// Step 0 (pre-shutdown): drain the transport server while every
	// subsystem is still alive. Without this, a webview WS client that
	// fires an RPC during the window between the App's subsystem-close
	// (Step 9 closes SQLite, Step 6 closes terminals, etc) and the
	// post-Run main.go call to srv.Shutdown would hit closed subsystems
	// and race teardown. transport.Server.Shutdown is idempotent (stopOnce),
	// so a later main.go call lands as a no-op.
	if srv := a.transportServer.Load(); srv != nil {
		drainCtx, cancel := contextWithTimeout(ctx, transportShutdownDrainTimeout)
		// Note: we call this BEFORE flipping shuttingDown so any RPCs
		// already mid-flight finish through the existing handlers; new
		// RPCs that arrive after Shutdown is wired bounce off the
		// rootCtx cancellation that Server.Shutdown triggers.
		if err := srv.Shutdown(drainCtx); err != nil {
			log.Printf("transport: drain on app shutdown: %v", err)
		}
		cancel()
	}

	// Step 1: stop accepting new work. Binding entry points check this
	// atomic on every call, so any concurrent RPC after this line fails
	// fast with ErrShuttingDown instead of starting a session we'd have
	// to tear right back down.
	if !a.shuttingDown.CompareAndSwap(false, true) {
		// Already shut down. Return nil so double-invocation (test
		// harness + Wails both calling us) stays a no-op.
		return nil
	}

	var errs []error
	record := func(step string, err error) {
		if a.shutdownInjectErrFn != nil {
			err = a.shutdownInjectErrFn(step, err)
		}
		errs = errorsx.Append(errs, errorsx.WrapLifecycle(step, err))
		if a.shutdownStepFn != nil {
			a.shutdownStepFn(step, err)
		}
	}

	// Step 2: drain the triage reactor. Any Handle() calls currently
	// running (dispatched from provider sessionEventHandlers) get to
	// finish. We use a short timeout so a stuck goroutine can't block
	// the rest of teardown.
	record("drain triage", a.drainTriage(ctx, reactorDrainTimeout))
	record("drain flush dispatch", a.drainFlushDispatch(ctx, reactorDrainTimeout))

	// Step 3: flush observability writers BEFORE closing provider
	// sessions. Provider close events pass through the replay log; if
	// we close the log first we drop the last few frames of every
	// in-flight session. OTEL flush goes here too because traces
	// attached to in-flight turns should land before the sessions
	// holding those turns go away.
	if a.replay != nil {
		replayCtx, cancel := contextWithTimeout(ctx, replayShutdownTimeout)
		record("close replay manager", a.replay.Shutdown(replayCtx))
		cancel()
	}
	if a.telemetry != nil {
		otelCtx, cancel := contextWithTimeout(ctx, telemetryShutdownTimeout)
		record("shutdown telemetry", a.telemetry.Shutdown(otelCtx))
		cancel()
	}

	// Step 3b: stop the idle-session reaper before snapshotting
	// sessions for Step 4. Otherwise the reaper could fire between the
	// snapshot and the close, see entries the snapshot already moved
	// out, and either close nothing (harmless) or — if a fresh entry
	// is racing in — close a session the Shutdown closer doesn't know
	// about. stopIdleSessionReaper is idempotent and blocks until the
	// goroutine returns.
	a.stopIdleSessionReaper()
	record("stop idle session reaper", nil)

	// Step 4: stop provider sessions. Each session's Close tears down
	// its own design-thread state as part of the same parallel closer,
	// so a slow design teardown can't serialize behind an unrelated
	// session close. Session closers aggregate their own errors via
	// closeSessionsParallel — we surface them under a single
	// "close provider sessions" step so the order spy sees one entry.
	sessions := a.sessionManager().snapshotAndClear()
	sessionErrs := closeSessionsParallel(a, sessions, sessionShutdownTimeout)
	record("close provider sessions", errors.Join(sessionErrs...))

	// Step 5: close the design reactor's pending choice requests. This
	// tears down per-thread design state left dangling when a session
	// never reached a clean Close — matches the teardownDesignThread
	// work the session closers did above but is safe to call again
	// (reactor.TeardownThread is a no-op when nothing is pending).
	if a.reactor != nil {
		// Walk the sessions we snapshotted so TeardownThread fires for
		// each thread even if the session close itself failed before
		// reaching its own teardown.
		for threadID := range sessions {
			a.reactor.TeardownThread(threadID)
		}
		record("close design reactor", nil)
	}

	// Step 5b: stop gitwatch subscriptions. Connection-tied subs were
	// already drained at Step 0 (transport drain runs ConnState
	// cleanups via runConnHandler's defer, which call our internal
	// unsubscribeGitWatch). Manager.Close tears down any leftover
	// watchers — primarily the case when shutdown is initiated
	// without an open WS client (tests, headless quit). Pump
	// goroutines exit when their Subscription channels close
	// (Manager.Close blocks on watcher run-loops exiting, which fires
	// broadcastClose, which closes Updates() channels). The pump
	// WaitGroup adds a hard guarantee that no pump can still emit on
	// the wire after this step returns.
	if a.gitWatch != nil {
		a.gitWatch.Close()
		a.gitWatchPumpWG.Wait()
		record("close gitwatch manager", nil)
	}

	// Step 5c: stop any design watchers that survived session teardown.
	// Per-session teardownDesignThread already fires from the parallel
	// closer in step 4 for sessions that reached close cleanly, but a
	// session that errored out before installing a teardown hook (or a
	// future code path that creates a watcher without a session) would
	// leave the goroutine alive past App lifetime. Walk the map under
	// the dedicated mu and stop each watcher; safe to call after step 4
	// because session closers don't write to designWatchers concurrently
	// (each calls stopDesignWatcher which acquires the same mutex).
	a.designWatchersMu.Lock()
	leftoverWatchers := make([]*design.Watcher, 0, len(a.designWatchers))
	for _, w := range a.designWatchers {
		leftoverWatchers = append(leftoverWatchers, w)
	}
	a.designWatchers = nil
	a.designWatchersMu.Unlock()
	for _, w := range leftoverWatchers {
		w.Stop()
	}
	if len(leftoverWatchers) > 0 {
		record("close leftover design watchers", nil)
	}

	// Step 6: close PTYs. Must happen after provider sessions because
	// a provider close might emit terminal output events; terminating
	// the terminal manager first would drop those final frames.
	if a.terminals != nil {
		record("close terminal sessions", a.terminals.Shutdown())
	}

	// Step 7: close the headless Chromium driving read_screenshot.
	// MUST run before the design MCP server: designMCP.Close() calls
	// http.Server.Shutdown(context.Background()) which blocks until
	// in-flight handlers return, and any in-flight read_screenshot
	// handler is parked inside Manager.Capture waiting on chromedp.
	// Closing the manager first cancels browserCtx, the chromedp
	// run returns, the handler returns, and step 7b's Shutdown
	// finishes promptly. The opposite order deadlocked shutdown
	// against a long-running capture.
	// Safe on a never-started Manager — the package treats Close as a
	// no-op when allocCancel/browserCancel are nil.
	if a.screenshotManager != nil {
		record("close headless screenshot manager", a.screenshotManager.Close())
	}

	// Step 7b: close the design MCP server. Safe to close once no
	// provider session holds a reference (step 4 guarantees that)
	// and the screenshot manager has been torn down (step 7
	// guarantees in-flight read_screenshot handlers can unblock).
	if a.designMCP != nil {
		record("close design MCP server", a.designMCP.Close())
	}

	// Step 8: close the provider event logger. After providers are
	// gone, nothing else writes to it — close it before SQLite so its
	// final flush isn't racing any other persistence sink.
	if a.logger != nil {
		record("close logger", a.logger.Close())
	}

	// Step 8b: final settle-goroutine drain. Provider session close at
	// Step 4 may have emitted session_died / EventTurnComplete events
	// that ran through triage AFTER Step 2's drainTriage barrier. Those
	// post-drain Handle calls can spawn async settle goroutines
	// (settleStreamingTextAsync / settleStreamingThinkingAsync) that
	// would otherwise call into SQLite after Step 9 closes it. The
	// 5-second cap matches the busy_timeout PRAGMA: if a settle is
	// genuinely stuck behind a SQL lock, no amount of extra waiting will
	// help and shutdown should proceed.
	if a.triage != nil {
		settleCtx, settleCancel := contextWithTimeout(ctx, 5*time.Second)
		drained := make(chan struct{})
		go func() {
			a.triage.WaitForPendingSettles()
			close(drained)
		}()
		select {
		case <-drained:
		case <-settleCtx.Done():
			log.Printf("app: drain settles timed out after 5s — proceeding with close")
		}
		settleCancel()
	}

	// Step 9: close SQLite last. Triage, replay, provider sessions,
	// and the logger have all flushed by this point; anyone calling
	// into the store after this will see a closed-db error rather
	// than corrupting a half-shut database.
	if a.store != nil {
		record("close store", a.store.Close())
	}

	return errors.Join(errs...)
}

// drainTriage blocks until every in-flight triage Handle() call returns
// or the timeout fires. Shutdown runs this before flushing observability
// + SQLite so no event is persisted after the store is closed. The
// timeout caps Quit latency — a stuck goroutine can delay the rest of
// teardown by at most `timeout`.
//
// A DeadlineExceeded error is returned rather than swallowed so the
// caller (Shutdown) records it in the step sequence; the rest of
// teardown continues regardless.
func (a *App) drainTriage(ctx context.Context, timeout time.Duration) error {
	if a.triage == nil {
		return nil
	}
	drainCtx, cancel := contextWithTimeout(ctx, timeout)
	defer cancel()
	if err := a.triage.Wait(drainCtx); err != nil {
		return fmt.Errorf("drain triage (timeout %s): %w", timeout, err)
	}
	return nil
}

// contextWithTimeout derives a child context bounded by whichever is
// tighter: the parent's remaining deadline or the subsystem's own timeout.
// A nil parent behaves like context.Background — this keeps the test path
// (which passes context.Background directly) and the Wails path (which has
// the parent cancelled mid-shutdown) both working.
func contextWithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, timeout)
}

// ServiceShutdown is the Wails v3 lifecycle hook. We keep it as a thin
// wrapper around Shutdown so the Wails runtime drives the same code path
// tests exercise directly.
func (a *App) ServiceShutdown() error {
	return a.Shutdown(context.Background())
}

// --- Item operations ---

// ListItems returns every item persisted for a thread in chronological order.
func (a *App) ListItems(threadID string) ([]store.Item, error) {
	return a.store.ListItems(threadID)
}

// --- Payload operations ---

// ListPayloadMetas returns all payload metadata for a thread without the body.
func (a *App) ListPayloadMetas(threadID string) ([]store.PayloadMeta, error) {
	return a.store.ListPayloadMetas(threadID)
}
