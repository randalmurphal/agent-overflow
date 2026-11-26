// main_harness.go owns the --harness boot path: the agent test harness
// that runs the real backend + real SPA headless on an isolated data
// directory, with both provider binaries pointed at ao-mockprovider so
// no real Claude/Codex process (or account) is ever touched. The
// Harness RPC receiver (internal/harnessrpc) is registered on the
// transport only by the two isolated boots, this one and --soak
// (main_soak.go); every other boot leaves the harness surface off the
// wire entirely.
//
// See docs/architecture/agent-harness.md for the full guide.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unsafe"

	appservice "agent-overflow/internal/app"
	"agent-overflow/internal/appdirs"
	"agent-overflow/internal/diagenv"
	"agent-overflow/internal/harness/instanceinfo"
	"agent-overflow/internal/harnessrpc"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/transport"
)

// harnessStdoutPrefix marks the harness bootstrap line on stdout. Kept
// distinct from bootstrapStdoutPrefix so a launcher scanning for the
// headless sentinel can never confuse the two payload shapes.
const harnessStdoutPrefix = "__AO_HARNESS__:"

// harnessKeepHomeEnv opts out of the HOME redirect for callers that
// deliberately want the harness to see the real ~/.claude / ~/.codex
// trees (e.g. replaying against real provider session files).
const harnessKeepHomeEnv = "AO_HARNESS_KEEP_HOME"

// harnessPaths carries every filesystem decision prepareHarness made so
// runHarness and the Harness receiver report the same values.
type harnessPaths struct {
	// DataRoot is the absolute --data-dir value.
	DataRoot string
	// DataDir is <DataRoot>/agent-overflow — where the DB, settings,
	// replay logs, and attachments actually live (initStores appends
	// the app subdirectory to the root override).
	DataDir string
	// HomeDir is the redirected $HOME (empty when AO_HARNESS_KEEP_HOME
	// was set). Provider-session fixtures (~/.claude/projects/...) are
	// seeded under it.
	HomeDir string
	// CredentialHome is the home the credential surface is pinned under
	// (App.credentialHomeOverride). Always <dataRoot>/home — identical to
	// HomeDir on a normal run, and still harness-owned when
	// AO_HARNESS_KEEP_HOME leaves $HOME real.
	CredentialHome string
	// MockProvider is the resolved ao-mockprovider binary path that
	// both provider binary settings point at.
	MockProvider string
	// MockForge is the resolved ao-mockforge binary every git.Core runs
	// in place of gh and glab. Empty when none was found beside this
	// executable: forge CLI calls then fail, and never reach PATH.
	MockForge string
	// AssetsFreshness is the embedded-bundle verdict from
	// checkEmbeddedDistFreshness: "match", "stale", "unknown", or
	// "dev-server". Computed once at boot; HarnessInfo exposes it and
	// `ao-harness health` flags "stale".
	AssetsFreshness string
	// AssetsDigest identifies the exact bundle served by this harness.
	AssetsDigest string
}

// runHarness boots the agent test harness. The shape mirrors
// runHeadless — transport first, then App.Start, then MarkReady — with
// three harness-specific steps around it: environment preparation
// (prepareHarness), Harness receiver registration (via
// bootTransportOptions.HarnessReceiver), and the harness bootstrap line
// that hands agents the URL, token, and data paths in one place.
func runHarness(flags cliFlags) {
	// First: a worker that died while this backend booted must still end it.
	if flags.exitWithParent > 0 {
		if err := watchParentExit(flags.exitWithParent); err != nil {
			fatalf("harness: %v", err)
		}
	}
	if flags.window {
		// Before any work: a nogui payload can never open a window, and
		// finding that out after seeding a data directory would be a worse
		// error message for the same mistake.
		requireWindowedBuild()
	}
	timing, err := parseHarnessTiming(os.Getenv(diagenv.HarnessTiming))
	if err != nil {
		fatalf("harness: %v", err)
	}
	paths, err := prepareHarness(flags)
	if err != nil {
		fatalf("harness: %v", err)
	}
	paths.AssetsFreshness = warnIfEmbeddedDistStale()
	paths.AssetsDigest = embeddedAssetDigest()
	if flags.window {
		// After prepareHarness (its refusals compare against the real
		// config root) and before the first GLib call. See
		// isolateWebviewStorage.
		if err := isolateWebviewStorage(paths.DataRoot); err != nil {
			fatalf("harness: %v", err)
		}
	}

	isolation := isolationOptionsFor(flags)
	isolation.Timing = timing
	isolation.OlderComputer = envTruthy(os.Getenv(diagenv.HarnessOlderComputer))
	// The fake trackers, served once a spec seeds them; closed after the
	// App has shut down.
	trackers := harnessrpc.NewTrackerFake()
	defer trackers.Close()
	isolation.TrackerOrigin = trackers.Origin
	appService, nativeWindow := newIsolatedProviderApp(paths, isolation)
	h := newHarness(appService, paths, nativeWindow, trackers)
	// The control server must listen before App.Start: it publishes its
	// address/token through App.providerExtraEnv (write-once before
	// Start), and the first session start could spawn a mock that needs
	// them.
	controlServer, providerEnv, err := harnessrpc.StartControl(h)
	if err != nil {
		fatalf("harness: start mock control server: %v", err)
	}
	// Install the mock-control credentials before App.Start. Provider env is a
	// write-once boot input; the first restored/started session must not race it.
	appservice.SetProviderExtraEnv(appService.App, providerEnv)
	defer controlServer.Shutdown()
	// The fake forge's HTTP listener, beside the control server and for
	// the same reason before App.Start: the git core Start builds takes
	// its address.
	forgeAPIServer, forgeAPI, err := harnessrpc.StartForgeAPI(h)
	if err != nil {
		fatalf("harness: start fake forge API: %v", err)
	}
	appservice.SetForgeAPI(appService.App, appservice.ForgeAPI{BaseURL: forgeAPI.BaseURL, Token: forgeAPI.Token})
	defer forgeAPIServer.Shutdown()
	srv := bootTransport(appService, flags.listenAddr, bootTransportOptions{
		IgnorePersistedNetwork: true,
		HarnessReceiver:        h,
		HarnessPageMarker:      harnessrpc.PageMarker(h),
		HarnessMethodsSink:     func(names []string) { harnessrpc.SetWireMethods(h, names) },
		AllowDevServerAssets:   true,
		HarnessTiming:          timing,
		HarnessOlderComputer:   isolation.OlderComputer,
	})
	log.Printf("transport: harness mode (data dir %s)", paths.DataDir)

	// Route all subsequent output to stderr so stdout carries exactly
	// one parseable line (the harness bootstrap), mirroring the
	// headless path's stdout-hygiene contract. The real stdout is kept
	// aside for that single write.
	bootstrapOut := routeStdoutToStderr()

	bootCtx, bootCancel := context.WithCancel(context.Background())
	defer bootCancel()
	identity := instanceIdentityFor(paths, instanceinfo.ModeHarness, flags.window, 0, "", "", "", "")
	bootstrapWritten := false
	if release := os.Getenv(diagenv.HarnessHoldStartup); release != "" {
		// A held boot is driven while it reports starting, so its address
		// is published before readiness instead of after.
		if err := writeHarnessBootstrap(bootstrapOut, srv, paths, nil, identity); err != nil {
			fatalf("harness: write bootstrap: %v", err)
		}
		bootstrapWritten = true
		// A hold that cannot watch its release file boots now; the test
		// that asked for it fails on its own assertions.
		begin := func(phase, detail string) func() { return appservice.BeginBootPhase(appService.App, phase, detail) }
		if err := holdHarnessStartup(bootCtx, begin, release, harnessHoldPoll); err != nil {
			log.Printf("harness: %v", err)
		}
	}
	if err := appService.Start(bootCtx); err != nil {
		log.Printf("app: service startup: %v", err)
		srv.MarkStartupFailed()
		if err := writeHarnessBootstrap(bootstrapOut, srv, paths, err, identity); err != nil {
			log.Printf("harness: write bootstrap: %v", err)
		}
		waitForHeadlessShutdown(appService, srv, nil)
		return
	}
	srv.MarkReady()

	// After Start, because Start is what reads any configured credential:
	// the recorder is installed only where there is none, so a harness
	// instance can never lose a real sender to it (app_push_harness.go).
	appservice.InstallHarnessPushSender(appService.App)

	if !bootstrapWritten {
		if err := writeHarnessBootstrap(bootstrapOut, srv, paths, nil, identity); err != nil {
			shutdownHeadless(appService, srv)
			fatalf("harness: write bootstrap: %v", err)
		}
	}

	// Discovery files last: they advertise an instance that is ready to
	// attach, which is true only now.
	// No launcher pid: --harness is never spawned by the Windows launcher
	// (that shell is --soak), so nobody but this process hosts a window.
	instance := publishInstance(srv, paths, instanceinfo.ModeHarness, flags.window, 0, "", "", "", "")
	harnessrpc.SetInstanceRemoval(h, instance.remove)
	defer instance.remove()

	if flags.window {
		if err := runWindowedShell(appService, srv, isolatedWindowTitle(instanceinfo.ModeHarness, instance.id), nativeWindow); err != nil {
			instance.remove()
			controlServer.Shutdown()
			forgeAPIServer.Shutdown()
			fatalf("harness: %v", err)
		}
		return
	}
	waitForHeadlessShutdown(appService, srv, nil)
}

// harnessHoldPoll is how often a held harness boot looks for its release
// file.
const harnessHoldPoll = 50 * time.Millisecond

// holdHarnessStartup reports the harness.hold_startup boot phase through
// begin until the release file exists (diagenv.HarnessHoldStartup) or ctx
// ends.
func holdHarnessStartup(ctx context.Context, begin func(phase, detail string) (end func()), release string, poll time.Duration) error {
	end := begin("harness.hold_startup", "Holding startup for a test")
	defer end()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		_, err := os.Stat(release)
		if err == nil {
			return nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("hold startup: read release file: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("hold startup: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// harnessTiming is a harness boot's diagenv.HarnessTiming: product
// intervals shortened so a test does not wait out a cadence it is not
// about. A zero duration keeps the product interval. Every entry is a row
// of harnessTimers, which says where its value goes.
type harnessTiming struct {
	// PairingProbe is how often a pending pairing asks whether the owner
	// confirmed it (deviceclient.WithProbeInterval).
	PairingProbe time.Duration
	// Watermark is the transport's watermark tick
	// (transport.Config.WatermarkInterval).
	Watermark time.Duration
	// Isolation holds the App's timer overrides, each documented on its
	// appservice.IsolationConfig field; isolationConfig starts from it.
	Isolation appservice.IsolationConfig
	// Frontend is the page's timer overrides in milliseconds by entry name,
	// served in the bootstrap manifest (transport.Config.FrontendTiming).
	// Nil when none is set.
	Frontend map[string]int64
}

// harnessTimer is one diagenv.HarnessTiming entry: its name and the field
// its value replaces, or for a page timer none, since the page reads it by
// name from the manifest (frontend/src/lib/transport/harnessMode.ts
// FRONTEND_TIMERS names the same set; TestFrontendTimersMatchThePage).
type harnessTimer struct {
	name  string
	field func(*harnessTiming) *time.Duration
	// max is the longest value the timer's destination takes, when its
	// arithmetic or its representation bounds it; zero is any Duration.
	max time.Duration
	// unit is what the value must be a whole number of, when its
	// destination holds it in a coarser unit than a Duration; zero is any.
	unit time.Duration
}

// pageTimerMax is the longest page timer: the page arms it with setTimeout
// (transport.MaxFrontendTimingMs).
const pageTimerMax = transport.MaxFrontendTimingMs * time.Millisecond

// harnessTimers is the one table of harness timers: parseHarnessTiming,
// isolationConfig and the bootstrap manifest all read it, so a timer is
// added here and nowhere else in the boot.
var harnessTimers = []harnessTimer{
	{name: "pairing-probe", field: func(t *harnessTiming) *time.Duration { return &t.PairingProbe }},
	{name: "watermark", field: func(t *harnessTiming) *time.Duration { return &t.Watermark }},
	{name: "thread-poll", field: func(t *harnessTiming) *time.Duration { return &t.Isolation.ThreadRequestPoll }, max: appservice.MaxThreadRequestPoll},
	{name: "transfer-retry", field: func(t *harnessTiming) *time.Duration { return &t.Isolation.TransferPendingRetry }},
	{name: "pr-update", field: func(t *harnessTiming) *time.Duration { return &t.Isolation.PRUpdateInterval }, max: appservice.MaxPRUpdateInterval},
	{name: "pr-update-retry", field: func(t *harnessTiming) *time.Duration { return &t.Isolation.PRUpdateRetryBase }},
	{name: "pr-ci-live", field: func(t *harnessTiming) *time.Duration { return &t.Isolation.PRCILiveInterval }, max: appservice.MaxPRCILiveInterval},
	{name: "pr-ci-follow", field: func(t *harnessTiming) *time.Duration { return &t.Isolation.PRCIFollowInterval }},
	{name: "pr-ci-log-wait", field: func(t *harnessTiming) *time.Duration { return &t.Isolation.PRCILogWaitInterval }},
	{name: "merged-pr-poll", field: func(t *harnessTiming) *time.Duration { return &t.Isolation.MergedPRPollInterval }, max: appservice.MaxMergedPRPollInterval},
	{name: "pr-lookup-retry", field: func(t *harnessTiming) *time.Duration { return &t.Isolation.PRLookupRetryBase }},
	{name: "idle-reap", field: func(t *harnessTiming) *time.Duration { return &t.Isolation.IdleReapThreshold }},
	{name: "idle-reap-sweep", field: func(t *harnessTiming) *time.Duration { return &t.Isolation.IdleReapInterval }},
	{name: "wakeup-grace", field: func(t *harnessTiming) *time.Duration { return &t.Isolation.WakeupReapGrace }},
	{name: "wake-floor", field: func(t *harnessTiming) *time.Duration { return &t.Isolation.AgentWakeFloor }, unit: appservice.AgentWakeFloorUnit},
	{name: "watch-failure-window", field: func(t *harnessTiming) *time.Duration { return &t.Isolation.AgentWatchFailureWindow }},
	{name: "remote-watch-poll", field: func(t *harnessTiming) *time.Duration { return &t.Isolation.RemoteWatchPoll }},
	{name: "drain-stall", field: func(t *harnessTiming) *time.Duration { return &t.Isolation.ProviderEventDrainStall }},
	{name: "usage-probe-gap", field: func(t *harnessTiming) *time.Duration { return &t.Isolation.UsageProbeGap }},
	{name: "usage-poll", field: func(t *harnessTiming) *time.Duration { return &t.Isolation.UsagePollInterval }},
	{name: "wake-retry", field: func(t *harnessTiming) *time.Duration { return &t.Isolation.ThreadWakeRetryBase }, max: appservice.MaxThreadWakeRetryBase},
	{name: "agent-settled-window", field: func(t *harnessTiming) *time.Duration { return &t.Isolation.AgentSettledWindow }},
	{name: "tracker-poll", field: func(t *harnessTiming) *time.Duration { return &t.Isolation.TrackerPollInterval }},
	// Page timers: how long a computer stays unreachable before the screen
	// says so, and how long a settled Agents row stays.
	{name: "unreachable-settle", max: pageTimerMax, unit: time.Millisecond},
	{name: "agent-row-linger", max: pageTimerMax, unit: time.Millisecond},
}

// parseHarnessTiming reads a diagenv.HarnessTiming value. Empty is the
// product timing. A malformed value is an error rather than ignored, so a
// test cannot pass while running at the cadence it meant to replace.
func parseHarnessTiming(value string) (harnessTiming, error) {
	var timing harnessTiming
	seen := map[string]bool{}
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		raw, value, ok := strings.Cut(entry, "=")
		name := strings.TrimSpace(raw)
		row := slices.IndexFunc(harnessTimers, func(t harnessTimer) bool { return t.name == name })
		if !ok || row < 0 {
			return harnessTiming{}, fmt.Errorf("%s: unknown entry %q", diagenv.HarnessTiming, entry)
		}
		if seen[name] {
			return harnessTiming{}, fmt.Errorf("%s: %s is given twice", diagenv.HarnessTiming, name)
		}
		seen[name] = true
		timer := harnessTimers[row]
		interval, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || interval <= 0 {
			return harnessTiming{}, fmt.Errorf("%s: %s needs a positive duration, got %q", diagenv.HarnessTiming, name, strings.TrimSpace(value))
		}
		if timer.unit > 0 && interval%timer.unit != 0 {
			return harnessTiming{}, fmt.Errorf("%s: %s must be a whole number of %s, got %s", diagenv.HarnessTiming, name, timer.unit, interval)
		}
		if timer.max > 0 && interval > timer.max {
			return harnessTiming{}, fmt.Errorf("%s: %s must be at most %s, got %s", diagenv.HarnessTiming, name, timer.max, interval)
		}
		if field := timer.field; field != nil {
			*field(&timing) = interval
			continue
		}
		if timing.Frontend == nil {
			timing.Frontend = map[string]int64{}
		}
		timing.Frontend[name] = interval.Milliseconds()
	}
	return timing, nil
}

// isolationOptions carries the isolation decisions a mocked boot mode is
// allowed to make for itself. The provider-safety pins are deliberately
// absent: they are unconditional in every mode, which is what
// TestMockedBootModesShareOneIsolationHelper proves. Anything added here
// must be defaulted SAFE by its zero value, so a caller that forgets to
// fill it in gets the pinned behaviour.
type isolationOptions struct {
	// RealBrowserEngine lifts the fake-engine pin so this instance selects
	// whatever real engine its deployment has. Zero value keeps the pin.
	// Only realBrowserEngineRequested may set it.
	RealBrowserEngine bool
	// ScanScopePIDs lets dev-server discovery also look at these
	// processes' trees (--scan-scope-pid). Zero value scans only this
	// backend's own tree, which is what every launcher-hosted --soak
	// instance gets.
	ScanScopePIDs []int
	// Timing shortens product intervals for a test. Only a harness boot
	// sets it (diagenv.HarnessTiming); the zero value keeps every product
	// interval.
	Timing harnessTiming
	// TrackerOrigin is where the App's tracker clients connect: a harness
	// boot's fake trackers (harnessrpc.TrackerFake). Nil refuses every
	// tracker.
	TrackerOrigin func(kind, site string) (string, error)
	// OlderComputer makes this computer answer as one on v0.0.19: its
	// window's calls (olderComputerSurface) and a paired spawner's archive
	// or delete of a worker here. Only a harness boot sets it
	// (diagenv.HarnessOlderComputer); the zero value is this build.
	OlderComputer bool
}

// olderComputerSurface is how v0.0.19 answers this build's window: the
// App methods added since are absent, so a call answers method_not_found;
// ArchiveThread and DeleteThread, which returned only an error then,
// answer a success with null; and a settings snapshot names none of the
// keys added since. From the generated bindings and internal/settings at
// the tag and at head. It reproduces the answers, not the release's write
// path: a save of a key the release lacks is taken here.
func olderComputerSurface() (absent map[string]bool, reshape map[string]func(json.RawMessage) (json.RawMessage, error)) {
	absent = map[string]bool{}
	for _, name := range []string{
		"AgentRosterSeeds", "CancelThreadWake", "CheckAgentProfiles", "CreateAssistant", "DeleteThreads",
		"FinishProviderAccountReset", "GetDeleteDependents", "GetGitBlobImage", "GetNativeMemoryStatus", "GetStandingAgent",
		"GetThreadAgentRows", "GetThreadRequestStates", "GetThreadWakeDetail",
		"GetThreadWorkerSummary", "ListAssistantPastThreads", "ListStandingAgentWakes",
		"ListThreadAgentRows", "ListThreadPastAgentRows", "ListThreadRecurringJobs",
		"ListThreadWakes", "ListThreadWorkerSummaries", "MarkThreadsRead", "ResolveForgeFacts",
		"SaveGitBlobImage", "SendExternalThreadMessage", "SetAssistantProjects", "SetThreadPlacement",
		"SpendProviderAccountReset", "StartNextAssistantThread", "StopThreadAgentIncoming", "StopThreadAgentRows",
		"StopThreadAgentRow", "ThreadToolWatch", "WakeThreadWakeNow", "WorkflowPauseItemAndEndSessions",
		"TrackerConnections", "AddTrackerConnection", "RenameTrackerConnection", "SetTrackerConnectionEnabled",
		"SetTrackerConnectionTokenExpiry", "RemoveTrackerConnection", "VerifyTrackerConnection",
		"EditTrackerKeyFile", "OpenTrackerTokenPage", "TrackerProjects", "TrackerBoards",
		"ProjectTrackerBoards", "SetProjectTrackerBoards", "TrackerBoardChoices", "ProjectTrackerSetup", "SetProjectTrackerSetup",
		"SubscribeTrackerBoard", "TrackerBoard", "TrackerSectionRows", "TrackerBoardPatch",
		"SetTrackerSubscriptionVisible", "RefreshTrackerSubscription", "ReleaseTrackerSubscription",
		"SubscribeTrackerTicket", "TrackerTicket", "TrackerComments", "TrackerAssignableUsers", "TrackerChoices",
		"EditTrackerIssue", "MoveTrackerIssue", "SetTrackerEstimate", "AddTrackerComment", "EditTrackerComment",
		"DeleteTrackerComment", "TrackerMoves", "RankTrackerIssues", "MoveTrackerIssuesToSprint",
		"CreateTrackerIssue", "TrackerIssueTypes", "TrackerCreateFields", "CreateTrackerSprint", "StartTrackerSprint", "CompleteTrackerSprint",
		"EditTrackerSprint", "EditTrackerWorklog", "DeleteTrackerWorklog", "TrackerWorklogs", "RefreshTrackerWorklogs",
		"MintTrackerAttachmentUploadTicket", "MintTrackerAttachmentDownloadTicket",
		"SaveTrackerAttachment", "TrackerLinkTypes", "LinkTrackerIssues", "UnlinkTrackerIssues", "TrackerPokerSupport",
		"FollowTrackerPoker", "TrackerPoker", "VoteTrackerPoker", "RevealTrackerPoker", "ClearTrackerPokerVotes",
		"SetThreadTicket", "ClearThreadTicket", "ThreadsForTicket",
	} {
		absent[name] = true
	}
	answerNull := func(json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`null`), nil }
	reshape = map[string]func(json.RawMessage) (json.RawMessage, error){
		"ArchiveThread": answerNull, "DeleteThread": answerNull,
	}
	for _, name := range []string{"GetSettings", "UpdateSettings", "SetProviderCustomEnvVar", "DeleteProviderCustomEnvVar"} {
		reshape[name] = settingsWithoutNewerKeys
	}
	return absent, reshape
}

// settingsAfterV0019 are the Settings keys v0.0.19's Settings does not have.
var settingsAfterV0019 = []string{
	"agentNotes", "agentProfiles", "assistantsEnabled", "claudeOwnMemoryOff",
	"codexOwnMemoryOff", "notifyThreadAttention", "sharedMemoryEnabled", "subThreadLevels",
}

// settingsWithoutNewerKeys is a settings snapshot as v0.0.19 encodes it.
func settingsWithoutNewerKeys(answer json.RawMessage) (json.RawMessage, error) {
	var snapshot map[string]json.RawMessage
	if err := json.Unmarshal(answer, &snapshot); err != nil {
		return nil, fmt.Errorf("older computer: read settings answer: %w", err)
	}
	for _, key := range settingsAfterV0019 {
		delete(snapshot, key)
	}
	return json.Marshal(snapshot)
}

// isolationOptionsFor is the one mapping from boot flags to isolation
// options, shared by --harness and --soak.
func isolationOptionsFor(flags cliFlags) isolationOptions {
	return isolationOptions{
		RealBrowserEngine: realBrowserEngineRequested(flags),
		ScanScopePIDs:     flags.scanScopePIDs,
	}
}

// newIsolatedProviderApp builds the App for a boot mode whose providers
// are mocked: the agent test harness (--harness) and the soak rig
// (--soak). It is the ONE place these pins are applied, so a second
// mocked boot mode cannot ship with only some of them; the pins are
// enforced by
// TestMockedBootModesShareOneIsolationHelper.
//
// Every pin here is structural, not advisory: settings stay editable at
// runtime in both modes, and only the spawn-time override makes "this
// process can never reach a real provider binary" true regardless of
// what a later UpdateSettings writes.
//
// It also returns the window cell an isolated WINDOWED boot fills in
// later (isolatedNativeWindow). Both live here for the same reason: this
// helper is the first thing both boot modes do, so "the browser engine's
// window getter was installed before App.Start" is true by construction
// rather than by remembering to call a second function.
func newIsolatedProviderApp(paths harnessPaths, opts isolationOptions) (*App, *isolatedNativeWindow) {
	appService := newApp()
	appservice.ConfigureIsolation(appService.App, isolationConfig(paths, opts))
	window := &isolatedNativeWindow{}
	appservice.SetBrowserNativeWindow(appService.App, window.pointer)
	return appService, window
}

// isolationConfig maps a mocked boot's paths and options to the isolation
// the App applies (newIsolatedProviderApp).
func isolationConfig(paths harnessPaths, opts isolationOptions) appservice.IsolationConfig {
	config := opts.Timing.Isolation
	config.ProviderBinary = paths.MockProvider
	config.CredentialHome = paths.CredentialHome
	config.UseFileKeychain = true
	config.DisableBackgroundFetch = true
	config.DisableRetentionSweep = true
	// Default-ON, and lifted only by the manual gate below. A mocked boot
	// is normally headless and unattended, so the fake engine is what
	// keeps the companion pane's chrome, tab strip and host rect
	// renderable (spec §10) with no browser behind them — and what keeps
	// `make go-test` / `make e2e` display-free.
	config.MockBrowserEngine = !opts.RealBrowserEngine
	// Workspace boundary; see internal/app/app_isolated_workspace.go.
	config.WorkspaceRoot = paths.DataRoot
	// gh and glab; see internal/git/forge_cli.go.
	config.ForgeCLI = paths.MockForge
	// Dev-server discovery; see internal/app/app_preview.go.
	config.ScanScopePIDs = opts.ScanScopePIDs
	config.OmitOwnerOps = opts.OlderComputer
	config.TrackerOrigin = opts.TrackerOrigin
	// v0.0.19 groups agent messages by the absorbed rule and never
	// reads agentMessageMembers; bootTransport leaves the capability
	// out of its hello for the same boot.
	config.OmitAgentMessageRunMembers = opts.OlderComputer
	return config
}

// realBrowserEngineRequested answers whether this isolated boot may host a
// REAL browser engine instead of the fake one — the manual gate spec §10
// promised (`AO_HARNESS_REAL_BROWSER=1 make harness-window`, or the same
// variable forwarded across the WSL boundary for `make harness-wsl`).
//
// Two conditions, both required:
//
//   - The opt-in is truthy. DEFAULT-OFF is the entire safety story: every
//     unattended run — `make go-test`, `make e2e`, a Playwright leg, a CI
//     boot — keeps the fake engine and opens no browser anybody has to
//     notice or close.
//   - The soak autopilot is NOT armed. `--autopilot` is exactly what makes
//     an isolated instance a SOAK rather than a harness (main_soak.go): a
//     rig left streaming for hours with nobody at the keyboard must not
//     grow a browser engine, so the pin stays unconditional there. Note the
//     axis is the autopilot, not the flag spelling: the Windows harness
//     (`make harness-wsl`) rides the launcher-owned `--soak` wire flag
//     WITHOUT it, and is as attended as `--harness` is.
//
// Every other pin ConfigureIsolation applies stays unconditional in both
// modes; this is the only liftable one, and this is the only place it lifts.
func realBrowserEngineRequested(flags cliFlags) bool {
	if !envTruthy(os.Getenv(diagenv.HarnessRealBrowser)) {
		return false
	}
	if flags.autopilot {
		log.Printf("%s is set, but --autopilot arms the soak: keeping the fake browser engine (an unattended rig never hosts a real one)", diagenv.HarnessRealBrowser)
		return false
	}
	log.Printf("%s: fake browser engine pin lifted — this instance selects the real engine for its deployment (manual gate, docs/specs/embedded-browser.md §10)", diagenv.HarnessRealBrowser)
	return true
}

// isolatedNativeWindow is the desktop window an isolated WINDOWED boot hands
// both the in-process browser engine and the harness's native-window RPCs.
//
// It exists because an isolated boot inverts the desktop boot's order: the
// backend is started and marked ready BEFORE any window is created, while
// SetBrowserNativeWindow must be called before Start. That works only
// because the two facts are needed at different times — engine SELECTION
// reads whether a getter exists (webkit_engine_linux.go / the darwin twin
// check `opts.NativeWindow == nil`), and the window POINTER is resolved
// lazily, when the first browser tool call starts the engine. So the
// getter/controller cell is installed empty here and filled by
// runWindowedShell, which cannot run until long after Start returned.
type isolatedNativeWindow struct {
	mu      sync.Mutex
	handle  func() unsafe.Pointer
	state   func() (harnessrpc.WindowState, error)
	command func(harnessrpc.WindowCommand) error
}

// publish installs the live window accessor. Called once, by the windowed
// shell, before the app loop starts.
func (w *isolatedNativeWindow) publish(
	handle func() unsafe.Pointer,
	state func() (harnessrpc.WindowState, error),
	command func(harnessrpc.WindowCommand) error,
) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.handle = handle
	w.state = state
	w.command = command
}

// pointer is the getter the App holds. nil means "no window" — a headless
// isolated boot, or a windowed one whose window has not materialized yet —
// which the engine reports as a bounded refusal, never a crash.
func (w *isolatedNativeWindow) pointer() unsafe.Pointer {
	w.mu.Lock()
	handle := w.handle
	w.mu.Unlock()
	if handle == nil {
		return nil
	}
	return handle()
}

func (w *isolatedNativeWindow) State() (harnessrpc.WindowState, error) {
	w.mu.Lock()
	state := w.state
	w.mu.Unlock()
	if state == nil {
		return harnessrpc.WindowState{}, errors.New("native window unavailable: start the harness with --window")
	}
	return state()
}

func (w *isolatedNativeWindow) Command(command harnessrpc.WindowCommand) error {
	w.mu.Lock()
	drive := w.command
	w.mu.Unlock()
	if drive == nil {
		return errors.New("native window unavailable: start the harness with --window")
	}
	return drive(command)
}

var _ harnessrpc.WindowController = (*isolatedNativeWindow)(nil)

func newHarness(app *App, paths harnessPaths, window harnessrpc.WindowController, trackers *harnessrpc.TrackerFake) *harnessrpc.Harness {
	return appservice.NewHarness(app.App, appservice.HarnessPaths{
		DataRoot:        paths.DataRoot,
		DataDir:         paths.DataDir,
		HomeDir:         paths.HomeDir,
		CredentialHome:  paths.CredentialHome,
		MockProvider:    paths.MockProvider,
		MockForge:       paths.MockForge,
		Trackers:        trackers,
		BuildStamp:      buildStamp(),
		AssetsFreshness: paths.AssetsFreshness,
		AssetsDigest:    paths.AssetsDigest,
		ShutdownTimeout: headlessShutdownTimeout,
		TerminateSelf:   terminateSelf,
		Window:          window,
	})
}

// prepareHarness makes every filesystem decision the harness boot
// needs, in dependency order: validate + create the data root, redirect
// HOME under it, resolve the mock provider binary, and seed the
// harness settings. Fails loudly on the first problem — a harness that
// half-boots against ambiguous state is worse than no harness.
//
// The soak rig (main_soak.go) runs the same preparation: same data-dir
// refusals, same HOME redirect, same mock resolution, same settings
// seed. Only the boot shell around it differs.
func prepareHarness(flags cliFlags) (harnessPaths, error) {
	dataRoot, err := filepath.Abs(flags.dataDir)
	if err != nil {
		return harnessPaths{}, fmt.Errorf("resolve --data-dir: %w", err)
	}
	// Check the spelling before canonicalizing so an owned leaf symlink is
	// refused, then retain the canonical existing ancestors for every child
	// created below it.
	if err := refuseSymlink(dataRoot); err != nil {
		return harnessPaths{}, err
	}
	dataRoot, err = instanceinfo.CanonicalPath(dataRoot)
	if err != nil {
		return harnessPaths{}, fmt.Errorf("canonicalize --data-dir: %w", err)
	}
	if err := refuseRealDataDir(dataRoot); err != nil {
		return harnessPaths{}, err
	}
	// No harness-owned path may be a symlink: these directories are
	// seeded and wiped wholesale, and a planted link would aim those
	// operations at whatever it points to. That covers the root itself
	// (`make harness` derives a predictable /tmp path from the checkout —
	// a link there routes EVERY child path elsewhere; the real-data check
	// above only catches links aimed at the config root specifically),
	// the app data dir, and the redirected home below. Parent components
	// (e.g. macOS's /tmp -> /private/tmp) are deliberately not resolved —
	// only the leaves the harness owns are checked.
	if err := refuseSymlink(dataRoot); err != nil {
		return harnessPaths{}, err
	}
	// The root gets created and vetted BEFORE anything below it, and the
	// order is the point: refuseSymlink rules out a planted LINK at this
	// predictable path, and refuseUnsafeHarnessDir rules out a planted
	// world-writable DIRECTORY at it. Everything created afterwards is
	// created inside a directory nobody else can write, so a pre-existing
	// child cannot have been planted by a stranger.
	if err := ensureHarnessPrivateDir(dataRoot); err != nil {
		return harnessPaths{}, fmt.Errorf("create harness data root: %w", err)
	}
	if err := refuseUnsafeHarnessDir(dataRoot); err != nil {
		return harnessPaths{}, err
	}

	dataDir := filepath.Join(dataRoot, "agent-overflow")
	if err := refuseSymlink(dataDir); err != nil {
		return harnessPaths{}, err
	}
	if err := appservice.EnsurePrivateDir(dataDir); err != nil {
		return harnessPaths{}, fmt.Errorf("create harness data dir: %w", err)
	}
	if err := refuseUnsafeHarnessDir(dataDir); err != nil {
		return harnessPaths{}, err
	}

	// Before ANY mutation of the root's contents: everything below this
	// line (the HOME redirect, the .gitconfig seed, the settings seed) is
	// a write into a tree a live backend may already own. The lock is held
	// for the process's lifetime and released by the kernel on death.
	if _, err := acquireHarnessInstanceLock(dataDir, harnessBootMode(flags)); err != nil {
		return harnessPaths{}, err
	}

	homeDir, err := isolateHarnessHome(dataRoot)
	if err != nil {
		return harnessPaths{}, err
	}
	// Checked whether or not AO_HARNESS_KEEP_HOME left $HOME real: the
	// credential surface is pinned under this directory either way.
	if err := refuseUnsafeHarnessDir(filepath.Join(dataRoot, "home")); err != nil {
		return harnessPaths{}, err
	}

	mockProvider, err := resolveMockProvider(flags.mockProvider)
	if err != nil {
		return harnessPaths{}, err
	}

	mockForge, err := resolveMockForge(flags.mockForge)
	if err != nil {
		return harnessPaths{}, err
	}

	if err := seedHarnessSettings(dataDir, mockProvider); err != nil {
		return harnessPaths{}, err
	}

	return harnessPaths{
		DataRoot:       dataRoot,
		DataDir:        dataDir,
		HomeDir:        homeDir,
		CredentialHome: filepath.Join(dataRoot, "home"),
		MockProvider:   mockProvider,
		MockForge:      mockForge,
	}, nil
}

// ensureHarnessPrivateDir creates a harness-owned directory at 0700,
// stamping the mode explicitly because MkdirAll applies the umask, and a
// restrictive one (0277, say) would leave the owner unable to write it.
//
// A directory that ALREADY exists is left exactly as found, unlike
// ensureAppPrivateDir: the caller's next move is to refuse a planted one,
// and a chmod here would repair the very evidence that check reads.
func ensureHarnessPrivateDir(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(path, appdirs.PrivateDirPerm); err != nil {
		return err
	}
	return os.Chmod(path, appdirs.PrivateDirPerm)
}

// refuseRealDataDir rejects a --data-dir that resolves to the OS
// default config root — the one place the user's real app data lives.
// The harness seeds, wipes, and restores its data dir wholesale; one
// mistyped flag must not point that at real threads.
func refuseRealDataDir(dataRoot string) error {
	defaultRoot, err := os.UserConfigDir()
	if err != nil {
		// No default config dir resolvable means nothing to protect.
		return nil
	}
	if harnessrpc.SameCanonicalPath(dataRoot, defaultRoot) {
		return fmt.Errorf("--data-dir %s is the OS config root; an isolated boot (--harness / --soak) refuses to run against real app data (pick a scratch dir)", dataRoot)
	}
	if harnessrpc.SameCanonicalPath(dataRoot, filepath.Join(defaultRoot, "agent-overflow")) {
		return fmt.Errorf("--data-dir %s is the real app data dir; an isolated boot (--harness / --soak) refuses to run against real app data (pick a scratch dir)", dataRoot)
	}
	return nil
}

// refuseSymlink rejects a path that exists as a symlink. Harness-owned
// directories are created and wiped wholesale; following a planted link
// would aim those operations at whatever it points to.
func refuseSymlink(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; an isolated boot (--harness / --soak) refuses to operate through links (it seeds and wipes this directory wholesale)", path)
	}
	return nil
}

// isolateHarnessHome redirects $HOME (and %USERPROFILE% on Windows) to
// <dataRoot>/home so everything the backend resolves through the home
// directory — ~/.claude session scans, ~/.codex rollout tails, git's
// global config — is harness-local and seedable. A minimal .gitconfig
// is written so git commits made by checkpoint/test flows don't fail
// identity detection under the empty home.
//
// Returns the redirected home path, or "" when AO_HARNESS_KEEP_HOME
// opted out. The harness home directory is created in BOTH cases: even a
// keep-home run pins the credential surface (provideraccounts slots,
// canonical credential, orphan prune) under it via
// App.credentialHomeOverride, so the flag only ever widens what the
// harness can READ (~/.claude session files, ~/.codex rollouts) — never
// what it can destroy: a keep-home run whose reused data dir lists a mock
// account still prunes only the harness home, never the real ~/.claude.
func isolateHarnessHome(dataRoot string) (string, error) {
	homeDir := filepath.Join(dataRoot, "home")
	if err := refuseSymlink(homeDir); err != nil {
		return "", err
	}
	if err := ensureHarnessPrivateDir(homeDir); err != nil {
		return "", fmt.Errorf("create harness home: %w", err)
	}
	if os.Getenv(harnessKeepHomeEnv) != "" {
		log.Printf(
			"harness: %s set — keeping real HOME %s for provider session reads; credential storage stays in %s",
			harnessKeepHomeEnv,
			os.Getenv("HOME"),
			homeDir,
		)
		return "", nil
	}
	gitconfig := filepath.Join(homeDir, ".gitconfig")
	if _, err := os.Stat(gitconfig); errors.Is(err, os.ErrNotExist) {
		identity := "[user]\n\tname = Agent Overflow Harness\n\temail = harness@agent-overflow.invalid\n[init]\n\tdefaultBranch = main\n"
		if err := os.WriteFile(gitconfig, []byte(identity), 0o600); err != nil {
			return "", fmt.Errorf("write harness .gitconfig: %w", err)
		}
	} else if err != nil {
		return "", fmt.Errorf("stat harness .gitconfig: %w", err)
	}
	if err := os.Setenv("HOME", homeDir); err != nil {
		return "", fmt.Errorf("redirect HOME: %w", err)
	}
	// The browser pane saves downloads to the user's Downloads folder, which
	// XDG_CONFIG_HOME's user-dirs file can name outside HOME: both follow the
	// harness home, and its Downloads folder exists as a user's does.
	if err := os.Setenv("XDG_CONFIG_HOME", filepath.Join(homeDir, ".config")); err != nil {
		return "", fmt.Errorf("redirect XDG_CONFIG_HOME: %w", err)
	}
	if err := ensureHarnessPrivateDir(filepath.Join(homeDir, "Downloads")); err != nil {
		return "", fmt.Errorf("create the harness Downloads folder: %w", err)
	}
	if runtime.GOOS == "windows" {
		if err := os.Setenv("USERPROFILE", homeDir); err != nil {
			return "", fmt.Errorf("redirect USERPROFILE: %w", err)
		}
	}
	return homeDir, nil
}

// resolveMockProvider locates the ao-mockprovider binary: the explicit
// --mock-provider flag wins, otherwise it must sit next to the running
// executable (where `make harness` puts it). The path is validated
// eagerly — a missing mock provider would otherwise only surface as a
// session-start failure deep into a test run.
func resolveMockProvider(flagPath string) (string, error) {
	candidate := flagPath
	if candidate == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("locate own executable for mock provider lookup: %w", err)
		}
		name := "ao-mockprovider"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		candidate = filepath.Join(filepath.Dir(exe), name)
	}
	abs, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve mock provider path: %w", err)
	}
	if _, err := exec.LookPath(abs); err != nil {
		return "", fmt.Errorf("mock provider binary not runnable at %s (build it with `make mockprovider`, or pass --mock-provider): %w", abs, err)
	}
	return abs, nil
}

// resolveMockForge locates the ao-mockforge binary. An explicit
// --mock-forge must be runnable. Without it the binary beside this
// executable is used when present; when it is absent the boot still
// comes up, logs that forge CLIs are disabled, and every gh or glab call
// fails with git.ForgeCLIUnavailableError. It never falls back to PATH.
func resolveMockForge(flagPath string) (string, error) {
	if flagPath != "" {
		abs, err := filepath.Abs(flagPath)
		if err != nil {
			return "", fmt.Errorf("resolve mock forge path: %w", err)
		}
		if _, err := exec.LookPath(abs); err != nil {
			return "", fmt.Errorf("mock forge binary not runnable at %s (build it with `make mockforge`): %w", abs, err)
		}
		return abs, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate own executable for mock forge lookup: %w", err)
	}
	name := "ao-mockforge"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	candidate := filepath.Join(filepath.Dir(exe), name)
	if _, err := exec.LookPath(candidate); err != nil {
		log.Printf("harness: no runnable ao-mockforge at %s; gh and glab calls will fail (build it with `make mockforge`, or pass --mock-forge): %v", candidate, err)
		return "", nil
	}
	return candidate, nil
}

// seedHarnessSettings points both provider binary settings at the mock
// provider and switches on the NDJSON event log so every harness
// session is recordable for wire-level replay. Runs through the real
// settings service (validation included) and re-applies on every boot —
// the mock provider path can move between runs (rebuilds, different
// checkouts) and a stale path would fail only at session start.
func seedHarnessSettings(dataDir, mockProvider string) error {
	svc := settings.NewService(dataDir)
	_, err := svc.Update(map[string]any{
		"claudeBinaryPath":             mockProvider,
		"codexBinaryPath":              mockProvider,
		"observabilityEventLogEnabled": true,
	})
	if err != nil {
		return fmt.Errorf("seed harness settings: %w", err)
	}
	return nil
}

// harnessBootstrap is the one-line JSON contract agents parse off
// stdout. Everything needed to attach is in this line: the page URL
// (one-time page ticket included), the session token for direct RPC
// clients, and the data paths where evidence (DB, traces, event logs)
// accumulates.
//
// The page URL opens ONE browser session — its ticket is spent by the
// first page load, and the cookie that load receives carries every later
// request from that browser. A caller that opens a second, cookie-less
// browser context (the e2e rig does, once per test) asks the running
// instance for a fresh URL at transport.PageURLPath rather than reusing
// this string.
type harnessBootstrap struct {
	URL          string `json:"url"`
	Port         int    `json:"port"`
	Token        string `json:"token"`
	DataRoot     string `json:"dataRoot"`
	DataDir      string `json:"dataDir"`
	HomeDir      string `json:"homeDir,omitempty"`
	MockProvider string `json:"mockProvider"`
	MockForge    string `json:"mockForge"`
	PID          int    `json:"pid"`
	Version      string `json:"version"`
	// ClientID is this instance's durable UI-state identity
	// (ensureClientID), already threaded onto URL as `&cid=`. It is
	// reported separately as well because a caller that builds its own
	// page URL — the Windows launcher opening a WebView2 window on a
	// --soak backend, an e2e run pointing Playwright at the instance —
	// must attach the SAME id, or the frontend's per-client ui_state
	// bucket changes identity on every launch and every persisted UI
	// preference reads as unset.
	//
	// Isolated boots resolve it under their own --data-dir
	// (bootSettingsDir honors the override), so it is the harness's own
	// id and never the developer's.
	ClientID   string `json:"clientId,omitempty"`
	PageMarker string `json:"pageMarker,omitempty"`
	// StartupError is set when App.Start failed; the transport still
	// serves (bootstrap returns the terminal failure state) so the
	// caller can read logs, but the harness is not usable.
	StartupError string `json:"startupError,omitempty"`
	instanceinfo.Identity
}

// newHarnessBootstrap assembles the payload. Split from the write so
// the same fields reach the stdout line and <dataDir>/harness-instance.json
// (main_harness_instance.go) — a tool that attaches to a running
// instance must not be reading a second, drifting description of it.
func newHarnessBootstrap(srv *transport.Server, paths harnessPaths, startupErr error) harnessBootstrap {
	clientID := ensureClientID()
	pageMarker := srv.PageMarker()
	bs := harnessBootstrap{
		URL:          fullPageURL(srv),
		Port:         portFromAddr(srv.Addr()),
		Token:        srv.Token(),
		DataRoot:     paths.DataRoot,
		DataDir:      paths.DataDir,
		HomeDir:      paths.HomeDir,
		MockProvider: paths.MockProvider,
		MockForge:    paths.MockForge,
		PID:          os.Getpid(),
		Version:      version,
		ClientID:     clientID,
		PageMarker:   pageMarker,
	}
	if startupErr != nil {
		bs.StartupError = startupErr.Error()
	}
	return bs
}

func writeHarnessBootstrap(out *os.File, srv *transport.Server, paths harnessPaths, startupErr error, identities ...instanceinfo.Identity) error {
	bootstrap := newHarnessBootstrap(srv, paths, startupErr)
	if len(identities) > 0 {
		bootstrap.Identity = identities[0]
	} else {
		bootstrap.Identity = instanceIdentityFor(paths, instanceinfo.ModeHarness, false, 0, "", "", "", "")
	}
	payload, err := json.Marshal(bootstrap)
	if err != nil {
		return fmt.Errorf("marshal harness bootstrap: %w", err)
	}
	if _, err := fmt.Fprintf(out, "\n%s %s\n", harnessStdoutPrefix, payload); err != nil {
		return fmt.Errorf("write harness bootstrap: %w", err)
	}
	return nil
}
