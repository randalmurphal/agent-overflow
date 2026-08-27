package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"agent-overflow/internal/appdirs"
	"agent-overflow/internal/attachment"
	appbrowser "agent-overflow/internal/browser"
	"agent-overflow/internal/chromium"
	"agent-overflow/internal/design"
	"agent-overflow/internal/discussion"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/eventchan"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitwatch"
	"agent-overflow/internal/logging"
	obsotel "agent-overflow/internal/observability/otel"
	"agent-overflow/internal/observability/replay"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
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
// discussion, design, terminals, attachments, workspace search) boot
// last once their inputs are ready.
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
	// Publish this executable as the `agent-overflow` command before any
	// session can be started, so the very first session already has it on
	// PATH. Best-effort by design: the helper logs its own failure and
	// returns "", which sessionProcessEnv reads as "nothing to prepend".
	a.cliBinDir = a.ensureCLIBinDir(dbDir)

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

	// Start the Claude rate-limit probe loop. Startup account adoption runs
	// the first usage read; this loop refreshes every 2 mins while at least
	// one Claude session is alive, and turn-complete is wired separately in
	// sessionEventHandler. The probe reads the dynamic OAuth usage endpoint
	// with legacy unified headers as a compatibility fallback.
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

	// Assert the persisted keep-awake state. Synchronous and cheap (one
	// D-Bus round trip at most, nothing at all when the setting is off),
	// and it must run on the boot path rather than lazily: the whole
	// point of persisting the switch is that the machine stays awake
	// across a restart without the user touching the toggle again. The
	// event bus is wired before Start (bootTransport → SetEventBus), so
	// the directive this emits is retained on its latest-only ring and
	// reaches a launcher that connects later. See app_power.go.
	a.applyKeepAwake(a.currentSettings())

	// Start the background `git fetch` cadence so ahead/behind counts
	// track the remote instead of the user's last manual fetch. Reads
	// Settings.BackgroundGitFetch live each tick; skipped entirely in
	// harness mode. See app_git_background_fetch.go.
	a.startBackgroundGitFetch()

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
	dataDir := a.dataDirOverride
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
	// Published for the transport bootstrap manifest the moment the store
	// is open. A failure here is fatal to the store: without a backend id
	// a client cannot key its replica database, and silently serving an
	// empty identity would look like "not ready yet" forever.
	identity, err := st.Identity()
	if err != nil {
		closeErr := st.Close()
		return "", nil, errors.Join(
			fmt.Errorf("failed to read store identity: %w", err),
			errorsx.WrapLifecycle("close store after store identity failure", closeErr),
		)
	}
	a.storeIdentity.Store(&identity)
	a.git = gitops.NewCore()
	a.gitWatch = gitwatch.NewManager(gitwatch.ManagerConfig{
		StatusFn:     a.git.Status,
		FastStatusFn: a.git.StatusFast,
		WatchRootsFn: a.git.WatchRoots,
	})
	a.settings = settings.NewService(dbDir)
	a.providerAccounts, err = provideraccounts.NewStore(dbDir)
	if err != nil {
		closeErr := st.Close()
		return "", nil, errors.Join(
			fmt.Errorf("failed to initialize provider accounts: %w", err),
			errorsx.WrapLifecycle("close store after provider account initialization failure", closeErr),
		)
	}
	userHome, homeErr := a.providerHome()
	if homeErr != nil {
		closeErr := st.Close()
		return "", nil, errors.Join(
			fmt.Errorf("failed to locate provider credential home: %w", homeErr),
			errorsx.WrapLifecycle("close store after provider credential initialization failure", closeErr),
		)
	}
	newCredentials := provideraccounts.NewCredentials
	if a.fileKeychainOverride {
		newCredentials = provideraccounts.NewCredentialsWithFileKeychain
	}
	a.providerCredentials, err = newCredentials(userHome, providerCredentialPolicy())
	if err != nil {
		closeErr := st.Close()
		return "", nil, errors.Join(
			fmt.Errorf("failed to initialize provider credentials: %w", err),
			errorsx.WrapLifecycle("close store after provider credential initialization failure", closeErr),
		)
	}
	a.accountAuditPath = filepath.Join(dbDir, "account-audit.log")
	// The prune deletes credential slots, and a metadata store paired with
	// the WRONG provider home deletes slots that were never its to manage
	// (incident 2026-07-29: a scratch data dir against the real ~/.claude
	// pruned every saved login). The stamp binds store to home on first
	// contact; on mismatch the prune is skipped outright — orphan slots
	// are benign, destroyed logins are not.
	claimedHome, homeMatches, claimErr := a.providerAccounts.ClaimProviderHome(userHome)
	if claimErr != nil {
		closeErr := st.Close()
		return "", nil, errors.Join(
			fmt.Errorf("bind provider account metadata to credential home: %w", claimErr),
			errorsx.WrapLifecycle("close store after provider account home claim failure", closeErr),
		)
	}
	if !homeMatches {
		a.auditAccountEvent(
			"prune skipped: metadata store %s is bound to provider home %s but this process resolves %s",
			filepath.Join(dbDir, "provider-accounts.json"),
			claimedHome,
			userHome,
		)
	} else {
		for _, providerName := range []string{string(provider.Claude), string(provider.Codex)} {
			keep := make(map[string]bool)
			for _, account := range a.providerAccounts.List(providerName, time.Now()) {
				keep[account.ID] = true
			}
			pruned, err := a.providerCredentials.PruneOrphanedAccounts(providerName, keep)
			// A removed slot is an unrecoverable login (Claude refresh tokens are
			// single-use), so every one is announced — silent destruction here
			// once cost days of "sign in again" debugging.
			for _, accountID := range pruned {
				a.auditAccountEvent(
					"removed orphaned %s credential slot %s (no saved account references it)",
					providerName,
					accountID,
				)
			}
			if err != nil {
				closeErr := st.Close()
				return "", nil, errors.Join(
					fmt.Errorf("clean orphaned %s account credentials: %w", providerName, err),
					errorsx.WrapLifecycle("close store after provider credential cleanup failure", closeErr),
				)
			}
		}
	}
	// After the prune (so a deleted account's orphan is discarded, not
	// adopted into a slot the prune just removed), recover anything a
	// crashed ephemeral Claude home left behind — its Keychain item or
	// credential file can hold the only live copy of a rotated
	// single-use chain. Best-effort: an entry the sweep cannot resolve
	// safely is kept for the next boot, never guessed at, so errors are
	// announced rather than fatal.
	sweepResults, sweepErr := a.providerCredentials.SweepEphemeralClaudeCredentials(time.Now())
	for _, result := range sweepResults {
		if result.Action == "skipped" {
			continue
		}
		a.auditAccountEvent(
			"ephemeral claude sweep: %s crash-orphaned home %s (account %q)",
			result.Action,
			result.ConfigHome,
			result.AccountID,
		)
	}
	if sweepErr != nil {
		a.auditAccountEvent("ephemeral claude sweep errors: %v", sweepErr)
	}
	// Server-imposed usage holds are per-bearer and run about an hour, far
	// longer than the interval between app restarts, so they persist.
	a.usageProbe.backoff.Load(filepath.Join(dbDir, "usage-backoff.json"))
	a.hydratePersistedAccountRateLimits()
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
	a.engineLogger, err = logging.NewEngineEventLogger(dbDir)
	if err != nil {
		// The provider-event logger opened above is already holding a file
		// handle; a boot that fails here must not leave it — or the DB —
		// dangling.
		var closeErr error
		if a.logger != nil {
			closeErr = errorsx.WrapLifecycle("close provider event logger", a.logger.Close())
			a.logger = nil
		}
		return "", nil, errors.Join(
			fmt.Errorf("failed to initialize workflow engine logger: %w", err),
			closeErr,
			errorsx.WrapLifecycle("close store after logger initialization failure", st.Close()),
		)
	}
	// One-shot move of pre-appStorage view state (paneLayout,
	// collapsedProjects) out of settings.json into the embedded
	// client's ui_state bucket. Runs before any frontend RPC can
	// arrive, so a settings save can't drop the stale keys first.
	migrateUIStateFromSettings(dbDir, st)
	return dbDir, st, nil
}

func repairStartupOwnedPaths(dbDir string) error {
	for _, dir := range []string{
		logging.Dir(dbDir),
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
	if err := os.MkdirAll(path, appdirs.PrivateDirPerm); err != nil {
		return err
	}
	return os.Chmod(path, appdirs.PrivateDirPerm)
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
			return chmodIfModeDiffers(path, appdirs.PrivateDirPerm)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			return chmodIfModeDiffers(path, appdirs.SensitiveFilePerm)
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
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, appdirs.SensitiveFilePerm)
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
	return chmodIfModeDiffers(path, appdirs.SensitiveFilePerm)
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

// initSubsystems wires the remaining services: triage (with metrics),
// discussion registry/channels, design artifacts and
// reactor, the design MCP server, terminals, attachments, and workspace
// search. Called after initStores / initObservability so every subsystem
// has its dependencies in place.
//
// An attachment init failure closes the store before returning — matching
// the logger init failure path in initStores — so a boot error never
// leaves an open DB handle dangling.
func (a *App) initSubsystems(dbDir string, st *store.Store) error {
	telemetryMetrics := a.telemetry.Metrics()

	a.triage = a.newTriageRouter(st)
	a.triage.SetTelemetry(a.telemetry.Tracer(), triage.TurnMetrics{
		TurnsStarted:      telemetryMetrics.TurnsStarted,
		TurnsCompleted:    telemetryMetrics.TurnsCompleted,
		TurnsErrored:      telemetryMetrics.TurnsErrored,
		ItemsPersisted:    telemetryMetrics.ItemsPersisted,
		PayloadsPersisted: telemetryMetrics.PayloadsPersisted,
	})
	// Flush-queue callbacks: drain queued user messages at provider
	// boundaries and record message anchors once the provider echo
	// confirms the deferred user row. See app_flush_queue.go.
	a.configureTriageQueueCallbacks()
	// Settle turn rows the previous app instance left in-flight. An
	// in-app session death settles its turn via the synthesized
	// truncated turn-complete, but an app crash leaves completed_at
	// NULL — which GetActiveTurn reads as "turn still active", wedging
	// revert behind an interrupt that has nothing to interrupt, and
	// leaving the turn's streaming items stuck forever. Runs before any
	// provider session can spawn, so every NULL row is provably crash
	// residue. See docs/architecture/turn-lifecycle.md §Crash recovery.
	sweepStarted := time.Now()
	if settled, err := a.triage.RecoverCrashedTurns(); err != nil {
		log.Printf("app: recover crashed turns: %v", err)
	} else if settled > 0 {
		log.Printf("app: settled %d crashed in-flight turns as interrupted", settled)
	}
	logBootPhase("app.recover_crashed_turns", sweepStarted)
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
	// Settle worktree setups the previous instance left mid-recipe. A run
	// lives only inside a live process, so every 'running' row here is crash
	// (or shutdown) residue over a worktree whose provisioning state nobody
	// can vouch for — which is what 'failed' means, and what puts the retry
	// affordance back in reach.
	worktreeSetupSweepStarted := time.Now()
	a.sweepCrashedWorktreeSetups()
	logBootPhase("app.sweep_crashed_worktree_setups", worktreeSetupSweepStarted)
	a.registry = discussion.NewRegistry(st)
	a.channels = discussion.NewChannelService(st)
	designBase := filepath.Join(dbDir, "design-workdirs")
	a.design.workdir = design.NewWorkDirManager(designBase)
	a.design.diagnostics = design.NewDiagnosticBuffer(nil)
	a.design.server = design.FileHandler(designBase)
	a.design.watchers = make(map[string]*design.Watcher)
	// Headless Chromium-driven capture for the agent's read_screenshot
	// tool. Lazy: the binary downloads on first capture (~150 MB
	// once), and the browser process boots only when something asks
	// for a screenshot. Threads that never call read_screenshot pay
	// nothing.
	a.design.screenshots = screenshot.NewManager(
		screenshot.NewInstaller(dbDir, a.emit),
	)
	a.design.reactor = design.NewReactor(a.design.diagnostics, a.newDesignCapturer())
	a.design.mcp = design.NewMCPServer(a.design.reactor)
	browserSettings := a.currentSettings()
	a.browser.manager = appbrowser.NewManager(
		chromium.NewInstaller(dbDir, chromium.ArtifactChrome, eventchan.BrowserInstallProgress, a.emit),
		dbDir,
		appbrowser.Config{
			Enabled:               browserSettings.BrowserEnabled,
			ShowWindow:            browserSettings.BrowserShowWindow,
			PersistSiteData:       browserSettings.BrowserPersistSiteData,
			AllowOutsideWorkspace: browserSettings.BrowserAllowOutsideWorkspace,
		},
	)
	a.browser.mcp = appbrowser.NewMCPServer(a.browser.manager, browserSettings.BrowserEnabled)
	a.browser.liveEnabled.Store(browserSettings.BrowserEnabled)
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
	if err := a.initWorkflowEngine(dbDir); err != nil {
		return err
	}
	a.startWorkflowDefinitionsWatcher(dbDir)
	a.initThemeDirectory()
	a.initSpinnerDirectory()
	return nil
}

// initThemeDirectory materializes <configDir>/themes (dir + generated
// schema/TOKENS.md reference + a seeded appearance.json) and arms the
// live-reload watcher over it.
//
// The legacy settings.theme value is read here and handed to EnsureBoot
// so a user upgrading into the theme system keeps the light/dark choice
// they already made. It is consulted only when appearance.json is
// absent.
//
// The read is RAW (Service.RetiredString) because the field is now
// retired: it is gone from the Settings struct and listed in
// retiredSettingsFieldNames, so neither the typed value nor the
// unknown-field preservation carries it any more. This one-time
// migration is the only legitimate reader left, which is exactly what
// that accessor exists for (docs/specs/theme-system.md §6.2).
//
// Neither half fails boot. A themes directory that cannot be created
// costs live reload and the on-disk reference; GetThemeFiles still
// answers, and the frontend still renders on built-in themes.
func (a *App) initThemeDirectory() {
	service, err := a.themeService()
	if err != nil {
		log.Printf("theme directory unavailable: %v", err)
		return
	}
	legacyMode := ""
	if a.settings != nil {
		legacyMode = a.settings.RetiredString("theme")
	}
	if err := service.EnsureBoot(legacyMode); err != nil {
		log.Printf("theme directory setup: %v", err)
	}
	a.startThemeWatcher(service.Dir())
}

// initSpinnerDirectory materializes <configDir>/spinners (dir + the
// generated SPINNERS.md authoring reference) and arms the live-reload
// watcher over it.
//
// Nothing is migrated here — spinners are a new surface, so unlike
// initThemeDirectory there is no retiring settings key to consume — and
// nothing fails boot for the same reason themes does not: a spinners
// directory that cannot be created costs live reload and the on-disk
// reference, while GetSpinnerFiles still answers and the composer still
// animates on the sprites bundled with the frontend.
func (a *App) initSpinnerDirectory() {
	service, err := a.spinnerService()
	if err != nil {
		log.Printf("spinner directory unavailable: %v", err)
		return
	}
	if err := service.EnsureBoot(); err != nil {
		log.Printf("spinner directory setup: %v", err)
	}
	a.startSpinnerWatcher(service.Dir())
}
