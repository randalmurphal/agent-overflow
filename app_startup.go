package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/checkpoint"
	"agent-overflow/internal/design"
	"agent-overflow/internal/discussion"
	"agent-overflow/internal/errorsx"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitwatch"
	"agent-overflow/internal/logging"
	obsotel "agent-overflow/internal/observability/otel"
	"agent-overflow/internal/observability/replay"
	"agent-overflow/internal/screenshot"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
	"agent-overflow/internal/terminal"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/uitrace"
	"agent-overflow/internal/workspacefiles"
)

// Start runs the App's startup phases. Called by ServiceStartup in
// desktop mode (app_desktop.go, behind the !nogui build tag) and
// directly by runHeadless in the WSL backend mode. Splitting the body
// out of ServiceStartup keeps the Wails dependency confined to the
// desktop build — see App struct doc.
//
// The body is split into three phases (initStores → initObservability →
// initSubsystems) so the dependency order is obvious: stores boot first
// because every other subsystem either embeds the store or reads from it,
// observability boots next because the triage router installs metrics
// before it can accept events, and the remaining subsystems (triage,
// checkpoints, discussion, design, terminals, attachments, workspace
// search) boot last once their inputs are ready.
//
//wails:ignore
func (a *App) Start(ctx context.Context) error {
	started := time.Now()
	defer logBootPhase("app.service_startup.total", started)

	// Initialise the App-lifetime ctx before any goroutine spawn so the
	// probe loops, OAuth poller, MCP reconcile callbacks, and any future
	// fire-and-forget worker can derive from a single cancellable parent
	// instead of context.Background.
	a.appCtx, a.appCancel = context.WithCancel(context.Background())

	phaseStarted := time.Now()
	dbDir, st, err := a.initStores()
	logBootPhase("app.init_stores", phaseStarted)
	if err != nil {
		return err
	}
	phaseStarted = time.Now()
	if err := a.initObservability(ctx, dbDir); err != nil {
		logBootPhase("app.init_observability", phaseStarted)
		return err
	}
	logBootPhase("app.init_observability", phaseStarted)
	phaseStarted = time.Now()
	if err := a.initSubsystems(dbDir, st); err != nil {
		logBootPhase("app.init_subsystems", phaseStarted)
		return err
	}
	logBootPhase("app.init_subsystems", phaseStarted)

	// Guard against provider subprocesses outliving an ungraceful app
	// death (macOS only — Linux has Pdeathsig, Windows a Job Object).
	// Runs after subsystems so the data dir is ready; before the probe
	// goroutines and any session RPC so the startup sweep can't race a
	// fresh registry Add. See app_orphan_reaper.go.
	a.startOrphanReaper(dbDir)

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

	// Start the Codex rate-limit probe loop. Startup hydration is already
	// covered by probeStartupAccountInfo; this loop keeps the rings fresh
	// while Codex sessions are active.
	a.startCodexRateLimitProbeLoop()

	// Start the idle-session reaper. Walks a.sessions every
	// IdleReapInterval and closes provider subprocesses that have been
	// idle past IdleReapThreshold so leaked subprocesses can't pile up
	// across long-running app sessions. See app_session_reaper.go.
	a.startIdleSessionReaper()

	// Start the retention TTL sweep. Reads Settings.Retention.Days
	// every tick so toggling retention on/off (or changing the window)
	// doesn't require a restart. See app_retention_cleanup.go.
	a.startRetentionCleanup()

	// Start the sidebar's host CPU/memory sampler. Emits a
	// `system:stats` event every ~2s. See app_sysstat.go.
	a.startSystemStatsSampler()

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
	dataDir := a.userConfigDirOverride
	if dataDir == "" {
		var err error
		dataDir, err = os.UserConfigDir()
		if err != nil {
			// Fall back to ~/.agent-overflow/ which persists across reboots,
			// unlike os.TempDir() which is cleaned on reboot and would lose data.
			homeDir, homeErr := os.UserHomeDir()
			if homeErr != nil {
				return "", nil, fmt.Errorf("cannot determine config directory: %w (home dir also unavailable: %v)", err, homeErr)
			}
			dataDir = homeDir
		}
	}
	dbDir := filepath.Join(dataDir, "agent-overflow")
	if err := ensureAppPrivateDir(dbDir); err != nil {
		return "", nil, fmt.Errorf("failed to create data directory %s: %w", dbDir, err)
	}
	if err := repairStartupOwnedPaths(dbDir); err != nil {
		return "", nil, err
	}
	dbPath := filepath.Join(dbDir, "agent-overflow.db")
	if err := prepareAppSensitiveFile(dbPath); err != nil {
		return "", nil, fmt.Errorf("failed to prepare database file %s: %w", dbPath, err)
	}

	st, err := store.New(dbPath)
	if err != nil {
		return "", nil, fmt.Errorf("failed to open database: %w", err)
	}
	if err := repairSQLiteSidecarPermissions(dbPath); err != nil {
		closeErr := st.Close()
		return "", nil, errors.Join(
			fmt.Errorf("failed to repair database file permissions: %w", err),
			errorsx.WrapLifecycle("close store after database permission repair failure", closeErr),
		)
	}

	a.store = st
	a.git = gitops.NewCore()
	a.gitWatch = gitwatch.NewManager(gitwatch.ManagerConfig{
		StatusFn:     a.git.Status,
		FastStatusFn: a.git.StatusFast,
		WatchRootsFn: a.git.WatchRoots,
	})
	a.settings = settings.NewService(dbDir)
	// Seed the git Core's GitLab self-hosted host snapshot from the
	// persisted settings before any Status / DetectForge call sees a
	// stale empty list. The settings service is lazy-loaded on first
	// Get; reading here forces the load and warms the cache too.
	a.git.SetGitLabHosts(a.settings.Get().GitLabSelfHostedHosts)
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

func repairStartupOwnedPaths(dbDir string) error {
	for _, dir := range []string{
		filepath.Join(dbDir, "logs"),
		filepath.Join(dbDir, "attachments"),
		filepath.Join(dbDir, "replay"),
		filepath.Join(dbDir, uitrace.DirName),
		filepath.Join(dbDir, uitrace.DirName, uitrace.BookmarkSubdir),
	} {
		if err := repairAppOwnedTreeIfExists(dir); err != nil {
			return fmt.Errorf("failed to repair permissions for %s: %w", dir, err)
		}
	}
	return nil
}

func ensureAppPrivateDir(path string) error {
	if err := os.MkdirAll(path, appPrivateDirPerm); err != nil {
		return err
	}
	return os.Chmod(path, appPrivateDirPerm)
}

func repairAppOwnedTreeIfExists(root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			return chmodIfModeDiffers(path, appPrivateDirPerm)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			return chmodIfModeDiffers(path, appSensitiveFilePerm)
		}
		return nil
	})
}

func prepareAppSensitiveFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to use symlinked sensitive file")
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing to use non-regular sensitive file")
		}
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, appSensitiveFilePerm)
	if err != nil {
		return err
	}
	if closeErr := file.Close(); closeErr != nil {
		return closeErr
	}
	return chmodAppSensitiveFileIfExists(path)
}

func repairSQLiteSidecarPermissions(dbPath string) error {
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := chmodAppSensitiveFileIfExists(path); err != nil {
			return err
		}
	}
	return nil
}

func chmodAppSensitiveFileIfExists(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil
	}
	return chmodIfModeDiffers(path, appSensitiveFilePerm)
}

func chmodIfModeDiffers(path string, want os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm() == want {
		return nil
	}
	return os.Chmod(path, want)
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
		log.Printf("observability: tracing setup failed, proceeding without telemetry: %v", err)
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
	// Synthesize session_died terminals for backgrounded launches whose
	// owning Claude session did not survive the previous app instance.
	// Without this sweep the launches would render as "running" forever
	// in the chat and the tray, since no live agent will ever observe
	// their completion. See docs/architecture/turn-lifecycle.md
	// §Crash recovery.
	recoverStarted := time.Now()
	if recovered, err := a.triage.RecoverOrphanedBackgroundTasks(); err != nil {
		logBootPhase("app.recover_orphaned_background_tasks", recoverStarted)
		log.Printf("app: recover Claude background launches: %v", err)
	} else if recovered > 0 {
		logBootPhase("app.recover_orphaned_background_tasks", recoverStarted)
		log.Printf("app: recovered %d Claude background launches as session_died", recovered)
	} else {
		logBootPhase("app.recover_orphaned_background_tasks", recoverStarted)
	}
	a.checkpoints = checkpoint.NewStore()
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
