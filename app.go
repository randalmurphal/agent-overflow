package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
	mu        sync.Mutex
	sessions  map[string]session // threadID → active session
	// threadID → in-flight session start. Concurrent callers wait for the
	// first start attempt instead of spawning duplicate provider runtimes.
	startingSessions map[string]*sessionStart
	// threadID → persisted in-process system prompt overrides used for
	// discussion participants and other non-default session starts.
	threadSystemPrompts map[string]string
	// channelID → active deliberation state
	deliberations map[string]*discussion.Deliberation
	// Test-only injection points for binding helpers that need to observe start/stop.
	startSessionFn        func(string) error
	stopSessionFn         func(string) error
	sendMessageFn         func(string, string) error
	generateBranchNameFn  func(store.Thread, string) (string, error)
	generateThreadTitleFn func(store.Thread, string) (string, error)
	emitProviderEventFn   func(provider.ProviderEvent)
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
	a.reactor = design.NewReactor(a.artifacts, func(eventName string, data any) {
		a.app.Event.Emit(eventName, data)
	})
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

	return nil
}

// ServiceShutdown is called by Wails v3 when the service is torn down.
func (a *App) ServiceShutdown() error {
	a.mu.Lock()
	sessions := make(map[string]session, len(a.sessions))
	for threadID, sess := range a.sessions {
		sessions[threadID] = sess
	}
	a.sessions = make(map[string]session)
	a.mu.Unlock()

	var errs []error
	for threadID, s := range sessions {
		a.teardownDesignThread(threadID)
		if s.claude != nil {
			errs = appendError(errs, wrapLifecycleError(
				fmt.Sprintf("close claude session for thread %s", threadID),
				s.claude.Close(),
			))
		}
		if s.codex != nil {
			errs = appendError(errs, wrapLifecycleError(
				fmt.Sprintf("close codex session for thread %s", threadID),
				s.codex.Close(),
			))
		}
	}
	if a.terminals != nil {
		errs = appendError(errs, wrapLifecycleError("close terminal sessions", a.terminals.Shutdown()))
	}
	if a.replay != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		errs = appendError(errs, wrapLifecycleError("close replay manager", a.replay.Shutdown(shutdownCtx)))
		cancel()
	}
	if a.telemetry != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		errs = appendError(errs, wrapLifecycleError("shutdown telemetry", a.telemetry.Shutdown(shutdownCtx)))
		cancel()
	}
	if a.store != nil {
		errs = appendError(errs, wrapLifecycleError("close store", a.store.Close()))
	}
	if a.logger != nil {
		errs = appendError(errs, wrapLifecycleError("close logger", a.logger.Close()))
	}
	if a.designMCP != nil {
		errs = appendError(errs, wrapLifecycleError("close design MCP server", a.designMCP.Close()))
	}
	return errors.Join(errs...)
}

// emitWithReplay returns an event emitter that both pushes to the Wails
// frontend and mirrors the event into the per-thread replay log when the
// event is thread-scoped. We inspect the payload for a `threadId` field so
// we don't introduce a hard dependency on any single event shape.
func (a *App) emitWithReplay() func(string, any) {
	return func(eventName string, data any) {
		a.app.Event.Emit(eventName, data)
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

// --- Thread operations ---

func (a *App) CreateThread(providerName string, workspacePath string, model string) (store.Thread, error) {
	now := time.Now().UnixMilli()
	projectPath := a.detectProjectPath(workspacePath)
	t := store.Thread{
		ID:              uuid.New().String(),
		Title:           "New Thread",
		Provider:        providerName,
		WorkspacePath:   workspacePath,
		ProjectPath:     projectPath,
		Model:           model,
		InteractionMode: "default",
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
		cfg := claude.Config{
			Binary:       a.providerBinaryPath(t.Provider),
			Model:        t.Model,
			WorkDir:      t.WorkspacePath,
			Resume:       resumeRef,
			ForkSession:  forkSession,
			SystemPrompt: systemPrompt,
			EventLogger:  a.logger,
		}
		sess, err := claude.NewSession(context.Background(), threadID, cfg, onEvent)
		if err != nil {
			a.teardownDesignThread(threadID)
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
		cfg := codex.Config{
			Binary:         a.providerBinaryPath(t.Provider),
			Model:          t.Model,
			WorkDir:        t.WorkspacePath,
			ResumeThreadID: t.SessionRef,
			SystemPrompt:   systemPrompt,
			MCPServers:     designCfg.MCPServers,
			EventLogger:    a.logger,
		}
		sess, err := codex.NewSession(context.Background(), threadID, cfg, onEvent)
		if err != nil {
			a.teardownDesignThread(threadID)
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
