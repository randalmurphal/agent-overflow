package app

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/appupdate"
	"agent-overflow/internal/assetwatch"
	"agent-overflow/internal/attachment"
	"agent-overflow/internal/claudeapp"
	"agent-overflow/internal/codexapp"
	"agent-overflow/internal/codexthread"
	"agent-overflow/internal/devserverprobe"
	"agent-overflow/internal/discussionapp"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitapp"
	"agent-overflow/internal/gitwatch"
	"agent-overflow/internal/highlightapp"
	"agent-overflow/internal/keybindings"
	"agent-overflow/internal/keyedlock"
	"agent-overflow/internal/logging"
	"agent-overflow/internal/mcpapp"
	obsotel "agent-overflow/internal/observability/otel"
	"agent-overflow/internal/observability/replay"
	"agent-overflow/internal/orphanreaper"
	"agent-overflow/internal/power"
	"agent-overflow/internal/projectapp"
	"agent-overflow/internal/provideraccountapp"
	"agent-overflow/internal/providerdiscoveryapp"
	"agent-overflow/internal/providerlifecycleapp"
	"agent-overflow/internal/sessionruntime"
	"agent-overflow/internal/settings"
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
	// headless boot installs the transport bridge; harness mode installs an
	// explicit unavailable sender. Tests may leave it nil to exercise the
	// same visible degraded error without pulling in a platform service.
	osNotifications osNotificationSender
	store           *store.Store
	git             *gitops.Core
	gitWatch        *gitwatch.Manager
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
	// coordination, and the engine/scheduler/autoresume application runtime.
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
	// turnObservers fans provider events out to internal App features after
	// triage handling has been attempted.
	turnObservers appTurnObserverState
	// discussionApp owns definition/channel services and every process-local
	// deliberation ward. Session lifecycle remains on App behind its narrow
	// ParticipantRuntime adapter.
	discussionAppOnce sync.Once
	discussionApp     *discussionapp.Service
	// browser owns the built-in provider-neutral browser and its MCP bridge.
	browser        appBrowserState
	terminals      *terminal.Manager
	attachments    *attachment.Store
	workspaceFiles *workspacefiles.Searcher
	logger         *logging.Logger
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
	// updater owns the complete in-app update state machine. This shell retains
	// only the stable App-bound wire adapters in app_updater.go.
	updater *appupdate.Service
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
	// orphanReaper is the macOS sidecar process that kills provider
	// process groups if this app dies ungracefully; orphanRegistry is the
	// durable backstop the next launch sweeps. Both stay nil on
	// Linux/Windows (which rely on Pdeathsig / a Job Object) and in tests
	// — the watch/release helpers and Client methods are nil-safe. See
	// app_orphan_reaper.go.
	orphanReaper   *orphanreaper.Client
	orphanRegistry *orphanreaper.Registry
	// threadApp owns store-backed thread application policy and the keyed
	// action-lock registry shared by application-shell thread workflows. Provider
	// sessions, git subprocesses, and destructive cleanup stay behind shell adapters.
	threadAppOnce sync.Once
	threadApp     *threadapp.Service
	// sessionConfigApplyLocks serializes the live-apply section of the
	// per-thread config reconciler (app_session_config.go); see
	// App.configApplyLocks for the lock-order rules.
	sessionConfigApplyLocksOnce sync.Once
	sessionConfigApplyLocks     *keyedlock.Registry
	// flushDispatch is the queued-message flush concern
	// (`app_flush_queue*.go`), including both of its mutexes.
	flushDispatch appFlushDispatchState
	// prUpdates is the PR-scope review-pane polling concern
	// (`app_pr_updates.go`).
	prUpdates appPRUpdateState
	// providerDiscovery owns bounded provider probe/model caches and the
	// separate Claude, Codex, status, and custom-environment coordinators.
	// This shell retains the stable Wails façades and account/event adapters.
	providerDiscoveryOnce sync.Once
	providerDiscovery     *providerdiscoveryapp.Service
	// providerDiscoveryCaches is a test-only cache injection. Production leaves
	// it nil and providerDiscoveryService uses the bounded process-wide set.
	providerDiscoveryCaches *providerdiscoveryapp.Caches
	// claudeApp owns application-facing reads and controls for live Claude
	// sessions. Account and process lifecycle stay with their own services.
	claudeAppOnce sync.Once
	claudeApp     *claudeapp.Service
	// codexApp owns Codex's application-facing leaf controls and cached global
	// reads. Lazy construction keeps focused tests that build a bare App cheap.
	codexAppOnce sync.Once
	codexApp     *codexapp.Service
	// devServerProber dials loopback ports to gate the dev-server chip:
	// triage's textual detection only proves command output mentioned a
	// URL, so ProbeDevServerURL checks a listener actually exists before
	// the chip renders. Verdicts are TTL-bounded (internal/devserverprobe).
	// Lazy-init through devServerProbe() so tests building a bare App{}
	// don't have to wire it.
	devServerProbeOnce sync.Once
	devServerProber    *devserverprobe.Prober
	// highlightApp owns cached parsing, live seeds, and persisted span workers.
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
	startSessionFn       func(string) error
	stopSessionFn        func(string) error
	sendMessageFn        func(string, string, []string) error
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
func (a *App) ListItems(threadID string, inlinePreviews bool) ([]store.Item, error) {
	items, err := a.store.ListItems(threadID)
	if err != nil {
		return nil, err
	}
	return projectItemSlice(items, inlinePreviews, keepNewest), nil
}

// --- Payload operations ---
