package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/checkpoint"
	"agent-overflow/internal/design"
	"agent-overflow/internal/discussion"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/logging"
	obsotel "agent-overflow/internal/observability/otel"
	"agent-overflow/internal/observability/replay"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
	"agent-overflow/internal/terminal"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/workspacefiles"

	"github.com/google/uuid"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// ErrShuttingDown is returned from binding entry points once Shutdown has
// started. Callers should surface this as a terminal state — no retry will
// succeed because the app is tearing down.
var ErrShuttingDown = errors.New("app: shutting down")

// App is the primary Wails-bound struct, registered as a v3 service.
type App struct {
	app            *application.App
	store          *store.Store
	git            *gitops.Core
	settings       *settings.Service
	triage         *triage.Router
	checkpoints    *checkpoint.Store
	registry       *discussion.Registry
	channels       *discussion.ChannelService
	artifacts      *design.ArtifactStore
	reactor        *design.Reactor
	designMCP      *codex.DesignMCPServer
	terminals      *terminal.Manager
	attachments    *attachment.Store
	workspaceFiles *workspacefiles.Searcher
	logger         *logging.Logger
	telemetry      *obsotel.Provider
	replay         *replay.Manager
	configDir      string
	// seq is a monotonic counter stamped on every event emitted through
	// a.emit. Frontend subscribers use it to log gaps; the counter is
	// scaffolding for a future remote-access transport where gap recovery
	// matters, but today it is purely observability.
	seq atomic.Uint64
	// shuttingDown is flipped to true once Shutdown begins. Binding entry
	// points that spin up new work (StartSession, SendMessage, ReconnectSession)
	// check it and fail fast with ErrShuttingDown so late RPCs can't race
	// with subsystem teardown.
	shuttingDown atomic.Bool
	mu        sync.Mutex
	sessions  map[string]session // threadID → active session
	// threadID → in-flight session start. Concurrent callers wait for the
	// first start attempt instead of spawning duplicate provider runtimes.
	startingSessions map[string]*sessionStart
	// threadID → persisted in-process system prompt overrides used for
	// discussion participants and other non-default session starts.
	threadSystemPrompts map[string]string
	// threadID → last-seen Claude slash-command list from system.init.
	// Claude-only; Codex sessions leave the entry absent. Populated in
	// sessionEventHandler as a side effect of EventInit; drained via the
	// GetThreadSlashCommands binding so the frontend composer can surface
	// an autocomplete popover. Guarded by a.mu with the rest of the
	// per-thread in-process state.
	threadSlashCommands map[string][]string
	// channelID → active deliberation state
	deliberations map[string]*discussion.Deliberation
	// Test-only injection points for binding helpers that need to observe start/stop.
	startSessionFn        func(string) error
	stopSessionFn         func(string) error
	sendMessageFn         func(string, string) error
	generateBranchNameFn  func(store.Thread, string) (string, error)
	generateThreadTitleFn func(store.Thread, string) (string, error)
	emitProviderEventFn   func(provider.ProviderEvent)
	emitEventFn           func(eventName string, data any)
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
	// leaves it nil and a.emit reaches for a.app. Tests that want to
	// observe the envelope shape without wiring up a full Wails
	// application install a hook here; when set, a.emit ALSO calls
	// the hook with the (name, envelope) it would have sent through
	// the Wails event bus.
	testEmitHook func(name string, data any)
}

// SeqEnvelope is the shape every event takes on the Wails wire. The
// Go→frontend boundary in a.emit wraps the caller's payload into this
// envelope so the frontend can log seq gaps. The replay log takes the
// un-enveloped payload because its format records provider events, not
// wire envelopes.
//
// Keeping the nesting (rather than mutating arbitrary payload structs
// in place) lets every caller keep using its existing Go type and
// lets the frontend deserialise a stable `{seq, data}` shape. Wails'
// CustomEvent runs `json.Marshal(&envelope)` which produces
// `{"seq": N, "data": <payload>}`.
type SeqEnvelope struct {
	Seq  uint64 `json:"seq"`
	Data any    `json:"data"`
}

// session wraps a provider session regardless of type.
type session struct {
	provider string
	token    string
	// Exactly one of these is non-nil.
	claude *claude.Session
	codex  *codex.Session
}

func NewApp() *App {
	return &App{
		sessions:            make(map[string]session),
		startingSessions:    make(map[string]*sessionStart),
		threadSystemPrompts: make(map[string]string),
		threadSlashCommands: make(map[string][]string),
		deliberations:       make(map[string]*discussion.Deliberation),
	}
}

// ServiceStartup is called by Wails v3 when the service is initialised.
func (a *App) ServiceStartup(ctx context.Context, options application.ServiceOptions) error {
	a.app = application.Get()

	// Open SQLite database in the app data directory.
	dataDir, err := os.UserConfigDir()
	if err != nil {
		// Fall back to ~/.agent-overflow/ which persists across reboots,
		// unlike os.TempDir() which is cleaned on reboot and would lose data.
		homeDir, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return fmt.Errorf("cannot determine config directory: %w (home dir also unavailable: %v)", err, homeErr)
		}
		dataDir = homeDir
	}
	dbDir := filepath.Join(dataDir, "agent-overflow")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		return fmt.Errorf("failed to create data directory %s: %w", dbDir, err)
	}
	dbPath := filepath.Join(dbDir, "agent-overflow.db")

	st, err := store.New(dbPath)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}

	a.store = st
	a.git = gitops.NewCore()
	a.settings = settings.NewService(dbDir)
	a.logger, err = newProviderEventLogger(dbDir)
	if err != nil {
		closeErr := st.Close()
		return errors.Join(
			fmt.Errorf("failed to initialize provider event logger: %w", err),
			wrapLifecycleError("close store after logger initialization failure", closeErr),
		)
	}

	// Observability: opt-in telemetry + per-thread event replay log. Both
	// read from settings; when disabled they are no-ops with zero runtime
	// cost. See internal/observability/* for details.
	settingsSnapshot := a.settings.Get()
	a.telemetry, err = obsotel.NewProvider(ctx, obsotel.ConfigFromFlags(
		settingsSnapshot.ObservabilityTracingEnabled,
		settingsSnapshot.ObservabilityOtlpEndpoint,
	))
	if err != nil {
		// Telemetry failure is non-fatal: we log it and proceed with a
		// no-op provider so the rest of the app still boots. Users will
		// see the failure via the app log; the settings toggle remains on
		// so they can fix the endpoint and restart.
		fmt.Printf("observability: tracing setup failed, proceeding without telemetry: %v\n", err)
		a.telemetry, _ = obsotel.NewProvider(ctx, obsotel.Config{Enabled: false})
	}

	telemetryMetrics := a.telemetry.Metrics()
	a.replay = replay.NewManager(replay.ManagerConfig{
		RootDir: filepath.Join(dbDir, "replay"),
		Enabled: settingsSnapshot.ObservabilityEventLogEnabled,
		DropHook: func() {
			telemetryMetrics.ReplayEventsDropped.Add(context.Background(), 1)
		},
	})

	a.triage = triage.NewRouter(st, a.emitWithReplay())
	a.triage.SetTelemetry(a.telemetry.Tracer(), triage.TurnMetrics{
		TurnsStarted:      telemetryMetrics.TurnsStarted,
		TurnsCompleted:    telemetryMetrics.TurnsCompleted,
		TurnsErrored:      telemetryMetrics.TurnsErrored,
		ItemsPersisted:    telemetryMetrics.ItemsPersisted,
		PayloadsPersisted: telemetryMetrics.PayloadsPersisted,
	})
	a.checkpoints = checkpoint.NewStore()
	a.triage.SetCheckpointStore(a.checkpoints)
	a.registry = discussion.NewRegistry(st)
	a.channels = discussion.NewChannelService(st)
	a.artifacts = design.NewArtifactStore(filepath.Join(dbDir, "design-artifacts"), st)
	a.reactor = design.NewReactor(a.artifacts, a.emit)
	a.designMCP = codex.NewDesignMCPServer(a.reactor)
	a.terminals = terminal.NewManager(a.terminalOutputCallback, a.terminalExitCallback)
	attachmentStore, err := attachment.NewStore(attachment.Config{
		RootDir: filepath.Join(dbDir, "attachments"),
	}, st)
	if err != nil {
		closeErr := st.Close()
		return errors.Join(
			fmt.Errorf("failed to initialise attachment store: %w", err),
			wrapLifecycleError("close store after attachment init failure", closeErr),
		)
	}
	a.attachments = attachmentStore
	a.workspaceFiles = workspacefiles.NewSearcher(workspacefiles.Config{})
	a.configDir = dbDir

	// Probe provider binaries once on boot so the thread-level banner can
	// surface "claude not found" / "codex too old" before the user opens
	// settings. Runs in a goroutine because DetectProvider spawns subprocesses
	// (up to 5s per provider) and we never want that blocking app startup.
	go a.probeStartupProviderStatuses()

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
		errs = appendError(errs, wrapLifecycleError(step, err))
		if a.shutdownStepFn != nil {
			a.shutdownStepFn(step, err)
		}
	}

	// Step 2: drain the triage reactor. Any Handle() calls currently
	// running (dispatched from provider sessionEventHandlers) get to
	// finish. We use a short timeout so a stuck goroutine can't block
	// the rest of teardown.
	record("drain triage", a.drainTriage(ctx, reactorDrainTimeout))

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

	// Step 4: stop provider sessions. Each session's Close tears down
	// its own design-thread state as part of the same parallel closer,
	// so a slow design teardown can't serialize behind an unrelated
	// session close. Session closers aggregate their own errors via
	// closeSessionsParallel — we surface them under a single
	// "close provider sessions" step so the order spy sees one entry.
	a.mu.Lock()
	sessions := make(map[string]session, len(a.sessions))
	for threadID, sess := range a.sessions {
		sessions[threadID] = sess
	}
	a.sessions = make(map[string]session)
	a.mu.Unlock()
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

	// Step 6: close PTYs. Must happen after provider sessions because
	// a provider close might emit terminal output events; terminating
	// the terminal manager first would drop those final frames.
	if a.terminals != nil {
		record("close terminal sessions", a.terminals.Shutdown())
	}

	// Step 7: close the design MCP server. Safe to close once no
	// provider session holds a reference (step 4 guarantees that).
	if a.designMCP != nil {
		record("close design MCP server", a.designMCP.Close())
	}

	// Step 8: close the provider event logger. After providers are
	// gone, nothing else writes to it — close it before SQLite so its
	// final flush isn't racing any other persistence sink.
	if a.logger != nil {
		record("close logger", a.logger.Close())
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

// emit stamps a monotonic seq on every Wails event and forwards the
// wrapped envelope through the Wails event bus. The frontend's
// wrapEventOn helper peels the envelope back off — subscribers see the
// same Go payload shape they would have without the envelope.
//
// When a.app is nil AND no test hook is installed, the helper is a
// silent no-op — this matches the pre-Shutdown boot path (Wails may
// not be initialised) and keeps tests free to construct an App with
// just the fields they need. Tests that want to observe the envelope
// install testEmitHook.
func (a *App) emit(name string, data any) {
	if a.app == nil && a.testEmitHook == nil {
		return
	}
	seq := a.seq.Add(1)
	env := SeqEnvelope{Seq: seq, Data: data}
	if a.app != nil {
		a.app.Event.Emit(name, env)
	}
	if a.testEmitHook != nil {
		a.testEmitHook(name, env)
	}
}

// emitWithReplay returns an event emitter that both pushes to the Wails
// frontend and mirrors the event into the per-thread replay log when the
// event is thread-scoped. We inspect the payload for a `threadId` field so
// we don't introduce a hard dependency on any single event shape.
//
// The emission goes through a.emit so every event gets the seq envelope;
// the replay log receives the raw (un-enveloped) payload because the
// replay format records provider events, not Wails wire envelopes.
func (a *App) emitWithReplay() func(string, any) {
	return func(eventName string, data any) {
		a.emit(eventName, data)
		if a.replay == nil || !a.replay.Enabled() {
			return
		}
		threadID := threadIDFromEvent(data)
		if threadID == "" {
			return
		}
		rec, err := replay.NewRecord(time.Now(), threadID, eventName, data)
		if err != nil {
			return
		}
		if a.replay.Enqueue(rec) {
			if telemetryMetrics := a.telemetry.Metrics(); telemetryMetrics.ReplayEventsQueued != nil {
				telemetryMetrics.ReplayEventsQueued.Add(context.Background(), 1)
			}
		}
	}
}

// closeSessionsParallel closes every session concurrently, bounded by the
// given timeout. Any session whose Close does not return in time is
// abandoned — the teardown emits a timeout error for it and moves on.
// Design-thread teardown runs synchronously in the goroutine that closed
// the session so each thread's state is cleaned up independently.
func closeSessionsParallel(a *App, sessions map[string]session, timeout time.Duration) []error {
	if len(sessions) == 0 {
		return nil
	}
	closers := make([]threadCloser, 0, len(sessions))
	for threadID, s := range sessions {
		closers = append(closers, sessionThreadCloser(a, threadID, s))
	}
	return runParallelClosers(closers, timeout)
}

// threadCloser is a single Close operation that runParallelClosers fires
// off in its own goroutine. The label is used to build a meaningful
// error message if Close fails or times out.
type threadCloser struct {
	label string
	close func() error
}

// sessionThreadCloser bundles the design teardown + provider Close for
// a single thread into one threadCloser so both run under the same
// parallel timeout.
func sessionThreadCloser(a *App, threadID string, s session) threadCloser {
	label := fmt.Sprintf("session for thread %s", threadID)
	return threadCloser{
		label: label,
		close: func() error {
			a.teardownDesignThread(threadID)
			if s.claude != nil {
				if err := s.claude.Close(); err != nil {
					return fmt.Errorf("close claude: %w", err)
				}
			}
			if s.codex != nil {
				if err := s.codex.Close(); err != nil {
					return fmt.Errorf("close codex: %w", err)
				}
			}
			return nil
		},
	}
}

// runParallelClosers invokes every closer concurrently and collects their
// errors, enforcing a single wall-clock timeout across the whole set.
// Closers that do not finish in time are abandoned and reported as
// timeout errors.
func runParallelClosers(closers []threadCloser, timeout time.Duration) []error {
	if len(closers) == 0 {
		return nil
	}
	type result struct {
		label string
		err   error
	}
	results := make(chan result, len(closers))
	for _, c := range closers {
		go func(c threadCloser) {
			results <- result{c.label, c.close()}
		}(c)
	}

	var errs []error
	remaining := len(closers)
	deadline := time.After(timeout)
	pending := make(map[string]struct{}, len(closers))
	for _, c := range closers {
		pending[c.label] = struct{}{}
	}
	for remaining > 0 {
		select {
		case r := <-results:
			remaining--
			delete(pending, r.label)
			if r.err != nil {
				errs = appendError(errs, wrapLifecycleError("close "+r.label, r.err))
			}
		case <-deadline:
			for label := range pending {
				errs = appendError(errs, fmt.Errorf("close %s: did not finish within %s", label, timeout))
			}
			return errs
		}
	}
	return errs
}

// --- Thread operations ---

// CreateThread persists a new thread for the given provider + workspace combo.
// `interactionMode` may be empty (normalized to "default") or one of
// "default" / "plan" / "design". "discussion" is reserved for threads created
// via StartDiscussion and is rejected here to prevent UI code from accidentally
// spawning orphan discussion threads without a deliberation channel.
func (a *App) CreateThread(providerName string, workspacePath string, model string, interactionMode string) (store.Thread, error) {
	mode, err := validateCreateThreadMode(interactionMode)
	if err != nil {
		return store.Thread{}, err
	}
	now := time.Now().UnixMilli()
	projectPath := a.detectProjectPath(workspacePath)
	t := store.Thread{
		ID:              uuid.New().String(),
		Title:           "New Thread",
		Provider:        providerName,
		WorkspacePath:   workspacePath,
		ProjectPath:     projectPath,
		Model:           model,
		InteractionMode: mode,
		RuntimeMode:     a.defaultRuntimeModeForNewThread(),
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := a.store.CreateThread(t); err != nil {
		return store.Thread{}, err
	}
	if a.settings != nil {
		a.settings.AddRecentWorkspace(workspacePath)
	}
	return t, nil
}

func (a *App) ListThreads() ([]store.Thread, error) {
	return a.store.ListThreads()
}

func (a *App) GetThread(id string) (store.Thread, error) {
	return a.store.GetThread(id)
}

func (a *App) DeleteThread(id string) error {
	return a.deleteThreadTree(id)
}

func (a *App) ArchiveThread(id string) error {
	return a.store.ArchiveThread(id)
}

// UnarchiveThread flips archived back to false so the thread reappears in the
// active sidebar. Returns the refreshed row so the caller can re-render
// without a follow-up GetThread round-trip.
func (a *App) UnarchiveThread(id string) (store.Thread, error) {
	if err := a.store.UnarchiveThread(id); err != nil {
		return store.Thread{}, err
	}
	return a.store.GetThread(id)
}

func (a *App) RenameThread(id string, title string) error {
	t, err := a.store.GetThread(id)
	if err != nil {
		return err
	}
	t.Title = title
	t.UpdatedAt = time.Now().UnixMilli()
	return a.store.UpdateThread(t)
}

// --- Item operations ---

func (a *App) ListItems(threadID string) ([]store.Item, error) {
	return a.store.ListItems(threadID)
}

// --- Payload operations ---

func (a *App) GetPayloadData(payloadID string) (string, error) {
	data, err := a.store.GetPayloadData(payloadID)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (a *App) ListPayloadMetas(threadID string) ([]store.PayloadMeta, error) {
	return a.store.ListPayloadMetas(threadID)
}

// --- Session operations ---

func (a *App) StartSession(threadID string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	return a.runSessionStart(threadID, func() error {
		return a.startSessionNow(threadID)
	})
}

func (a *App) startSessionNow(threadID string) error {
	t, err := a.store.GetThread(threadID)
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}

	sessionToken := uuid.NewString()
	onEvent := a.sessionEventHandler(threadID, sessionToken)
	threadPrompt := a.threadSystemPrompt(threadID)

	a.mu.Lock()
	existing, ok := a.sessions[threadID]
	if ok {
		delete(a.sessions, threadID)
	}
	a.mu.Unlock()

	// Stop the old runtime before starting a replacement so thread-scoped
	// design state does not leak across reconnects or restart attempts.
	if ok {
		a.teardownDesignThread(threadID)
		if err := closeProviderSession(threadID, existing); err != nil {
			return fmt.Errorf("start session: %w", err)
		}
	}

	switch t.Provider {
	case string(provider.Claude):
		designCfg, err := a.designSessionConfig(t)
		if err != nil {
			return fmt.Errorf("start session: %w", err)
		}
		systemPrompt := joinSystemPrompts(designCfg.Prompt, threadPrompt)
		resumeRef := t.SessionRef
		forkSession := false
		if t.PendingForkRef != "" {
			resumeRef = t.PendingForkRef
			forkSession = true
		}
		runtimeMode := provider.NormalizeRuntimeMode(t.RuntimeMode)
		cfg := claude.Config{
			Binary:         a.providerBinaryPath(t.Provider),
			Model:          t.Model,
			WorkDir:        t.WorkspacePath,
			Resume:         resumeRef,
			ForkSession:    forkSession,
			SystemPrompt:   systemPrompt,
			PermissionMode: provider.ClaudePermissionMode(runtimeMode),
			EventLogger:    a.logger,
		}
		sess, err := claude.NewSession(context.Background(), threadID, cfg, onEvent)
		if err != nil {
			a.teardownDesignThread(threadID)
			// Surface "binary missing" / "version too old" as a provider:status
			// banner before the error bubbles up as a toast. If the detect
			// path finds nothing wrong the helper is a no-op.
			a.emitProviderStatusOnSessionStartError(string(provider.Claude))
			return fmt.Errorf("start session: %w", err)
		}
		a.mu.Lock()
		a.sessions[threadID] = session{
			provider: string(provider.Claude),
			token:    sessionToken,
			claude:   sess,
		}
		a.mu.Unlock()

	case string(provider.Codex):
		designCfg, err := a.designSessionConfig(t)
		if err != nil {
			return fmt.Errorf("start session: %w", err)
		}
		systemPrompt := joinSystemPrompts(designCfg.Prompt, threadPrompt)
		runtimeMode := provider.NormalizeRuntimeMode(t.RuntimeMode)
		cfg := codex.Config{
			Binary:         a.providerBinaryPath(t.Provider),
			Model:          t.Model,
			WorkDir:        t.WorkspacePath,
			ApprovalPolicy: provider.CodexApprovalPolicy(runtimeMode),
			Sandbox:        provider.CodexSandbox(runtimeMode),
			ResumeThreadID: t.SessionRef,
			SystemPrompt:   systemPrompt,
			MCPServers:     designCfg.MCPServers,
			EventLogger:    a.logger,
		}
		sess, err := codex.NewSession(context.Background(), threadID, cfg, onEvent)
		if err != nil {
			a.teardownDesignThread(threadID)
			a.emitProviderStatusOnSessionStartError(string(provider.Codex))
			return fmt.Errorf("start session: %w", err)
		}
		a.mu.Lock()
		a.sessions[threadID] = session{
			provider: string(provider.Codex),
			token:    sessionToken,
			codex:    sess,
		}
		a.mu.Unlock()

	default:
		return fmt.Errorf("unknown provider: %s", t.Provider)
	}

	return nil
}

func (a *App) SendMessage(threadID string, content string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	return a.sendMessage(threadID, content)
}

func (a *App) InterruptTurn(threadID string) error {
	a.mu.Lock()
	sess, ok := a.sessions[threadID]
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("no active session for thread %s", threadID)
	}

	switch {
	case sess.claude != nil:
		return sess.claude.Interrupt(context.Background())
	case sess.codex != nil:
		return sess.codex.Interrupt(context.Background())
	default:
		return fmt.Errorf("session has no provider")
	}
}

func (a *App) StopSession(threadID string) error {
	a.mu.Lock()
	sess, ok := a.sessions[threadID]
	if ok {
		delete(a.sessions, threadID)
	}
	a.mu.Unlock()

	if !ok {
		a.teardownDesignThread(threadID)
		if a.triage != nil {
			a.triage.CleanupThread(threadID)
		}
		return nil
	}

	a.teardownDesignThread(threadID)
	if a.triage != nil {
		a.triage.CleanupThread(threadID)
	}
	return closeProviderSession(threadID, sess)
}
