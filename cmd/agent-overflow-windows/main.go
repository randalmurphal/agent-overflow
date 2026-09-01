//go:build windows

// agent-overflow-windows is the Windows entry point for the WSL-backed
// build of Agent Overflow. The binary:
//
//  1. Detects WSL distros via wsl.exe -l -v.
//  2. On first launch, shows a picker window so the user chooses
//     which distro hosts the backend. The choice is persisted to
//     %APPDATA%\agent-overflow\wsl.json.
//  3. Drops the embedded Linux ELF backend into the chosen distro
//     under ~/.local/bin/agent-overflow if the version on disk
//     doesn't match what's bundled.
//  4. Spawns wsl.exe -d <distro> -- ~/.local/bin/agent-overflow
//     --listen 127.0.0.1:0 --print-url-fd 0 and pins the child to a
//     Win32 Job Object so closing this process kills the WSL-side one.
//  5. Reads the bootstrap line { port, token } back from the child.
//  6. Points the Wails WebView2 window at a BARE
//     http://localhost:<port>/?host=webview page URL and injects the
//     one-time page ticket into the document it just loaded, so the
//     ticket never rides a copyable URL (internal/pagehost).
//
// WSL2 forwards 127.0.0.1:<port> from inside the distro to the Windows
// host's localhost via vEthernet. localhostForwarding=true must be
// set in the user's /etc/wsl.conf or %USERPROFILE%/.wslconfig — it's
// the WSL2 default but a user can disable it. A port Windows has
// reserved (Hyper-V/WSL2 excluded ranges, re-seeded every reboot)
// breaks the same hop while the backend binds it happily inside the
// distro; since the backend pins its listen port per install, that
// would repeat on every launch, so launchAndProbe relaunches once with
// --reset-transport-port before giving up. The picker shows a clear
// error only if the retry fails too.
//
// The launcher is split across this file and three siblings:
//
//   - config.go  — wsl.json read/write under %APPDATA%.
//   - payload.go — embedded ELF install + WSL HOME resolution.
//   - picker.go  — picker / loading / connectivity-error HTML serving.
//
// This file owns the entry point and the launcherApp method surface
// (PickDistro, validateDistroName, launchAndShow, run).
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/appidentity"
	"agent-overflow/internal/diagenv"
	"agent-overflow/internal/harness/governor"
	"agent-overflow/internal/harness/instanceinfo"
	"agent-overflow/internal/notify"
	"agent-overflow/internal/observability/pprofserve"
	"agent-overflow/internal/pagehost"
	"agent-overflow/internal/serialqueue"
	"agent-overflow/internal/uikeys"
	"agent-overflow/internal/uiwindow"
	"agent-overflow/internal/webview2host"
	"agent-overflow/internal/wsldistro"
	"agent-overflow/internal/wsllauncher"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/updater"
)

//go:embed picker.html
var pickerHTML string

// linuxPayload is embedded as a string, not []byte: string embeds land
// in the read-only .rdata section (file-backed, shareable, no commit
// charge), while []byte embeds land in the writable .data section,
// which Windows maps copy-on-write and charges the full ~47 MiB
// against the process's private commit at load time — measured as the
// bulk of the launcher's memory footprint before this was a string.
//
//go:embed payload/agent-overflow-linux
var linuxPayload string

// payloadVersion identifies the embedded payload. The launcher writes
// this into wsl.json so a future upgrade can compare against the
// freshly-embedded version and decide whether to reinstall. We use
// a build-time-injectable variable so the Taskfile can stamp it via
// `-ldflags="-X main.payloadVersion=..."`.
var payloadVersion = "dev"

// launcherMode is stamped by the WSL build task as "dev" for
// make dev-wsl and "prod" for distribution builds. It deliberately
// does not infer from payloadVersion: production builds may omit a
// version string, but they still need the production single-instance ID.
var launcherMode = "prod"

// activeProfile is the runtime launch profile (--profile / the
// AGENT_OVERFLOW_PROFILE env var), set once in main() right after flag
// parsing and read-only afterwards. Empty is the normal instance;
// appidentity.ProfileHarness is the isolated instance an agent or a
// developer drives, appidentity.ProfileSoak is that same instance with the
// soak autopilot armed, and appidentity.ProfilePerf is the isolated target
// reserved for destructive renderer benchmarks.
//
// It is a package variable for the same reason launcherMode is: every
// per-instance name (single-instance id, title, WebView2 profile, CDP
// port, log file, window state) is derived from the folded mode, and
// those derivations are reached from Wails callbacks that take no
// arguments of ours.
var activeProfile = ""

// launcherRuntimeMode folds the build stamp with the runtime profile.
// This is THE function every per-instance name goes through — a soak run
// launched from the dev build is "soak", never "dev".
func launcherRuntimeMode() string {
	return appidentity.LauncherMode(launcherMode, activeProfile)
}

// soakWindowSize is the SOAK instance's initial window. Small on
// purpose: a soak sits visible on a real monitor for hours next to the
// developer's actual work, and it only has to be big enough to keep the
// renderer compositing a live thread. The harness profile deliberately
// does NOT share it — that instance is a working surface somebody reads
// and clicks, so it takes the ordinary default window size.
const (
	soakWindowWidth  = 800
	soakWindowHeight = 600
)

// webviewLogEnv opts in to Chromium's chrome_debug.log (see browserArgs).
// dev-wsl forwards it across the WSL→Windows interop hop via WSLENV.
const webviewLogEnv = "AGENT_OVERFLOW_WEBVIEW_LOG"

// webviewSoftwareEnv opts the webview out of GPU acceleration (see
// browserArgs). Diagnostic knob for isolating whether the app's GPU
// submissions are implicated in system-level compositor stalls — the
// standard WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS env var does not work
// here because Wails' Go webviewloader never reads it (that is a
// Microsoft WebView2Loader.dll feature). dev-wsl forwards this var
// across the WSL→Windows interop hop via WSLENV.
const webviewSoftwareEnv = "AGENT_OVERFLOW_WEBVIEW_SOFTWARE"

// webviewExtraArgsEnv appends raw Chromium switches (space-separated) to
// browserArgs. Diagnostic/experiment knob — memory-policy A/Bs like
// --force-gpu-mem-available-mb run on the soak rig through this without a
// launcher rebuild per flag. Not for production configuration: anything
// proven out here graduates into browserArgs with its own rationale.
// dev-wsl forwards it across the WSL→Windows interop hop via WSLENV.
const webviewExtraArgsEnv = "AGENT_OVERFLOW_WEBVIEW_EXTRA_ARGS"

// shutdownTimeout caps how long the launcher waits for the WSL-side
// backend to drain before tearing the Job Object down.
const shutdownTimeout = 5 * time.Second

const (
	launcherPrivateDirPerm os.FileMode = 0o700
	launcherLogFilePerm    os.FileMode = 0o600
)

// bootstrapProbeAttemptTimeout caps a single HTTP attempt against the
// WSL-side /bootstrap.json. RST / connection-refused arrives in
// milliseconds; a real timeout means the request reached the kernel
// but the server didn't respond, which is rare and recoverable. 1 s
// is long enough for either to surface and short enough that we burn
// budget on retrying, not waiting.
const bootstrapProbeAttemptTimeout = 1 * time.Second

// bootstrapProbeDeadline is the total time we'll spend polling. WSL2
// in NAT mode (the default — see %USERPROFILE%/.wslconfig with no
// `networkingMode` line) installs a Windows-side forward rule for the
// WSL listener AFTER the listener binds; the rule shows up sub-second,
// but the launcher's first probe lands inside that window and gets
// "actively refused" by Windows even though the backend is healthy.
// Polling past the install bumps every cold boot through the race
// without flapping. The probe covers both localhost-forwarding setup
// and the backend's readiness-gated ServiceStartup window.
const bootstrapProbeDeadline = 30 * time.Second

// bootstrapProbePollInterval is the gap between failed-probe retries.
// 250 ms is fast enough that the WSL2 forwarder install almost never
// costs us more than one extra hop, slow enough that we don't melt the
// CPU when the backend genuinely never comes up.
const bootstrapProbePollInterval = 250 * time.Millisecond

func main() {
	// FIRST, before flags, config, logging, or anything else. When this
	// process was spawned as an updater swap helper (WAILS_UPDATER_HELPER=1),
	// HandleHelperMode performs the swap and never returns; without the
	// sentinel it returns immediately and costs one getenv. Wails calls this
	// itself from application.New, but that is far too late here: the helper
	// child is this same launcher binary, so it would first run distro
	// detection, the picker, and the payload install, and trip the
	// single-instance machinery against the app it is trying to replace.
	updater.HandleHelperMode()

	bootStarted := time.Now()

	// Flags first, before logging: the profile decides WHICH log file
	// this process appends to, and a soak run's evidence has to be
	// attributable to that run. A parse failure still gets a log — the
	// default one — so the reason is not lost to a GUI subsystem's
	// discarded stderr.
	flags, flagErr := parseLauncherFlags(os.Args[1:])
	activeProfile = flags.Profile

	logFile, err := openLog(activeProfile)
	if err == nil {
		// io.MultiWriter aborts the whole chain on the first writer's
		// error, and stderr in a `-H windowsgui` app is connected to
		// nothing — its first Write returns an error and the file
		// writer never sees the bytes. tolerantMultiWriter swallows
		// per-writer errors so launcher.log captures every log line
		// regardless of stderr's state. Stderr is still tried first
		// so a `make dev-wsl` invocation (which DOES have a connected
		// stderr) gets live output as before.
		log.SetOutput(tolerantMultiWriter(os.Stderr, logFile))
		log.Printf("launcher: started, log=%s", logFile.Name())
		defer logFile.Close()
	}

	// Same opt-in env var as the WSL backend (WSLENV forwards it across
	// the boundary), but a different loopback: this binds Windows-side
	// 127.0.0.1, the backend binds WSL-side 127.0.0.1 — same default
	// port, zero conflict. Lets the launcher's own heap be profiled;
	// query it from PowerShell, not a WSL shell.
	if pprofAddr, _, pprofErr := pprofserve.StartIfEnabled(); pprofErr != nil {
		log.Printf("launcher: pprof: %v", pprofErr)
	} else if pprofAddr != "" {
		log.Printf("launcher: pprof listening on %s (Windows loopback)", pprofAddr)
	}

	if flagErr != nil {
		// flag.ErrHelp means the user passed -h/-help and the flag
		// package already wrote the usage to stderr. Exit cleanly so
		// the launcher doesn't log a phantom "flags: flag: help requested"
		// to launcher.log on every help invocation.
		if errors.Is(flagErr, flag.ErrHelp) {
			os.Exit(0)
		}
		log.Fatalf("flags: %v", flagErr)
	}
	if err := installHarnessBoundary(governor.DefaultCeilingBytes); err != nil {
		log.Fatalf("harness containment: %v", err)
	}
	// Before ANY WebView2 environment exists in this process, including the
	// one Wails builds for the SPA. An inherited WEBVIEW2_USER_DATA_FOLDER
	// overrides the folder we just pinned, and a SET BUT EMPTY one does it
	// silently: every environment collapses onto the default profile, the
	// pane would read the app's cookies, and nothing reports an error.
	if removed := webview2host.ScrubEnvOverrides(); len(removed) > 0 {
		log.Printf("launcher: cleared inherited WebView2 env overrides: %s", webview2host.FormatScrub(removed))
	}
	if err := prepareWebviewStorage(launcherRuntimeMode()); err != nil {
		log.Fatalf("webview2 storage: %v", err)
	}
	if activeProfile != "" {
		log.Printf("launcher: profile=%s (isolated instance: id/title/webview/log/window-state/data-dir)", activeProfile)
	}

	cfg, err := loadConfig()
	if err != nil {
		log.Printf("warning: load config: %v", err)
	}

	// Plumb the Windows-side AppData path into the WSL backend so the
	// Settings UI can read / mutate the same wsl.json the launcher
	// uses. WSLENV's /p flag translates the Windows path to its
	// /mnt/c form on the Linux side, so the backend reads the env var
	// as a Linux path it can stat directly. Best-effort: a missing
	// APPDATA just means the WSL Settings UI hides itself (the
	// backend's WSLConfigDir() returns ok=false), which is preferable
	// to silently writing into a phantom path.
	exportAppDataToWSL()

	// Forward AGENT_OVERFLOW_DEBUG (if set) to the WSL backend so raw
	// provider stdio capture works for `make dev-wsl PROVIDER_DEBUG=1`
	// and for end users who set the env var in their Windows shell.
	forwardDebugEnvToWSL()

	phaseStarted := time.Now()
	distros, err := wsllauncher.ListDistros(context.Background())
	logBootPhase("launcher.list_distros", phaseStarted)
	if err != nil {
		log.Printf("list distros: %v", err)
	}

	// Pick precedence: --distro override beats saved config beats
	// single-distro auto-pick. A matched override is "transient" — we
	// use it for this run but don't persist to wsl.json, so the
	// developer's `make dev-wsl --distro Foo` doesn't overwrite the
	// user's "real" pick from double-clicking the .exe earlier.
	chosen, transient := resolveChosenDistro(flags, cfg, distros)

	// initialURL controls the page the WebView2 shows during the
	// brief gap between window creation and the WSL backend coming
	// online. Three cases:
	//   - no distros installed → dedicated /wsl-not-installed page
	//     with the `wsl --install` mitigation. Trumps everything;
	//     there's nothing to launch and no picker to render.
	//   - chosen distro (override / saved / single auto-pick) →
	//     /loading interstitial; SetURL flips to the WSL backend
	//     once Launch returns.
	//   - otherwise → /picker so the user can choose.
	initialURL := "/picker"
	switch {
	case len(distros) == 0:
		initialURL = "/wsl-not-installed"
	case chosen != "":
		initialURL = "/loading"
	}

	// buildApp creates the window (and, when chosen != "", kicks off the
	// backend launch) on ApplicationStarted — see its doc for why creation is
	// deferred into the running app loop.
	app := buildApp(distros, initialURL, chosen, transient)
	logBootPhase("launcher.before_run", bootStarted)

	app.run()
}

// exportAppDataToWSL sets AGENT_OVERFLOW_WIN_APPDATA + WSLENV in the
// launcher's process environment so the wsl.exe child (and the Linux
// backend it spawns) sees the Windows AppData directory translated to
// its /mnt/c form. WSLENV is the documented mechanism for this; the
// /p flag is the per-variable rule for path translation.
//
// We append AGENT_OVERFLOW_WIN_APPDATA/p to any existing WSLENV the
// user already has set rather than overwriting — a developer with
// PYTHONPATH/p:GOPATH/p in WSLENV shouldn't lose those just because
// they launched the Windows app.
//
// A missing APPDATA is logged and ignored; the WSL backend's
// WSLConfigDir() returns ok=false in that case, the Settings UI
// hides the WSL section, and the launcher falls back to the saved
// config in %APPDATA%\agent-overflow\wsl.json on next launch (or the
// picker, if nothing is saved). This is a defense-in-depth path —
// %APPDATA% is set by Windows for every interactive user.
func exportAppDataToWSL() {
	appdata := os.Getenv("APPDATA")
	if appdata == "" {
		log.Printf("APPDATA unset; WSL Settings UI distro switcher will be unavailable")
		return
	}
	if err := os.Setenv(wsldistro.AppDataEnv, appdata); err != nil {
		log.Printf("set %s: %v", wsldistro.AppDataEnv, err)
		return
	}
	if err := prependWSLENVRule(wsldistro.AppDataEnv + "/p"); err != nil {
		log.Printf("set WSLENV: %v", err)
	}
}

// forwardDebugEnvToWSL forwards AGENT_OVERFLOW_DEBUG, when set in the
// launcher's environment, to the WSL-side backend by adding it to
// WSLENV. This lets `make dev-wsl PROVIDER_DEBUG=1` enable raw provider
// stdio capture inside the WSL backend the same way `make dev` does for
// the non-WSL path. End users can also set the env var in their Windows
// shell (or System Properties) and it will reach the backend.
//
// No-op when the var is unset, which is the production default. The
// var is forwarded as a plain string (no /p flag) — its value is a
// comma-separated topic list, not a path.
func forwardDebugEnvToWSL() {
	if os.Getenv("AGENT_OVERFLOW_DEBUG") == "" {
		return
	}
	if err := prependWSLENVRule("AGENT_OVERFLOW_DEBUG"); err != nil {
		log.Printf("forward AGENT_OVERFLOW_DEBUG via WSLENV: %v", err)
	}
}

// prependWSLENVRule prepends rule to WSLENV, preserving any rules a
// developer or another tool has already set. WSLENV uses ":" as the
// separator; wsl.exe processes entries left-to-right, so prepending
// keeps our entries deterministic without dropping prior ones.
func prependWSLENVRule(rule string) error {
	existing := os.Getenv("WSLENV")
	merged := rule
	if existing != "" {
		merged = rule + ":" + existing
	}
	return os.Setenv("WSLENV", merged)
}

// resolveChosenDistro picks the distro to launch in based on (in
// order) the --distro override, then the saved wsl.json config, then
// a single-distro auto-pick. Returns ("", false) when none matched
// and the picker (or the WSL-missing page, if zero distros) should
// run instead.
//
// The returned bool is the "transient" flag — true when an override
// supplied the choice and the launcher should NOT persist it after a
// successful boot. Picker selections, saved-config rehydrations, and
// single-distro auto-picks are non-transient (the auto-pick is
// effectively a "first-time setup" pick and should persist so the
// next launch isn't asked again if the user later installs more
// distros).
func resolveChosenDistro(flags launcherFlags, cfg *wsldistro.Config, distros []wsllauncher.Distro) (string, bool) {
	if flags.Distro != "" {
		for _, d := range distros {
			if d.Name == flags.Distro {
				return d.Name, true
			}
		}
		// --distro pointed at a distro that wsl.exe doesn't know
		// about (typo, distro uninstalled since the env var was set).
		// Fall through to the picker so the user sees the real list
		// rather than silently dropping back to a saved choice that
		// might be the wrong dev environment entirely.
		names := make([]string, 0, len(distros))
		for _, d := range distros {
			names = append(names, d.Name)
		}
		log.Printf(
			"warning: --distro %q not found among installed distros (%v); showing picker",
			flags.Distro, names,
		)
		return "", false
	}
	if cfg != nil && cfg.Distro != "" {
		for _, d := range distros {
			if d.Name == cfg.Distro {
				return d.Name, false
			}
		}
	}
	// Single-distro auto-pick: nothing to choose between, so don't
	// make the user click. Persist on success so a later install of
	// a second distro doesn't surprise the user with a picker on
	// next launch — they explicitly own their pick now.
	if len(distros) == 1 {
		return distros[0].Name, false
	}
	return "", false
}

// openLog opens %APPDATA%\agent-overflow\launcher.log for append (or
// launcher-soak.log under the soak profile — see
// appidentity.StateFileName). The file is best-effort; if AppData isn't
// writable we fall back to stderr only. Logging is essential here
// because the Windows binary has no console UI for surfacing errors
// before the WebView opens.
//
// The soak profile splitting off its own file is what makes watchdog
// evidence attributable: `grep 'render recovery' launcher-soak.log`
// answers a question about one soak run, not about every dev launch
// interleaved with it.
func openLog(profile string) (*os.File, error) {
	dir, ok := wsldistro.WSLConfigDir()
	if !ok {
		return nil, fmt.Errorf("openLog: AppData unresolvable")
	}
	name := appidentity.StateFileName("launcher.log", appidentity.LauncherMode(launcherMode, profile))
	if err := os.MkdirAll(dir, launcherPrivateDirPerm); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, launcherPrivateDirPerm); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(
		filepath.Join(dir, name),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY,
		launcherLogFilePerm,
	)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(f.Name(), launcherLogFilePerm); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// launcherApp is the Wails service exposing PickDistro to the picker
// HTML. It also owns the Launcher / window references so we can flip
// the window URL once the backend boots.
type launcherApp struct {
	wails *application.App
	// window is created on the ApplicationStarted handler goroutine and read by
	// the single-instance callback / launchAndShow; access it via win() (under
	// mu), never the field directly.
	window  *application.WebviewWindow
	distros []wsllauncher.Distro

	mu                sync.Mutex
	launcher          *wsllauncher.Launcher
	launcherWait      *launcherExit
	memoryWatchCancel context.CancelFunc
	memoryGovernor    *governor.Manager
	memoryLease       *governor.Lease
	memoryLeaseCancel context.CancelFunc
	backendBootstrap  *wsllauncher.Bootstrap
	// notificationService presents Windows toasts. notificationClient is the
	// narrow WS bridge back to the WSL backend; notificationCancel stops its
	// reconnect loop on backend exit or launcher shutdown. All are guarded by
	// mu because activation callbacks run independently of launchAndShow.
	notificationService     *launcherNotificationService
	notificationClient      *wsllauncher.NotificationClient
	notificationContext     context.Context
	notificationCancel      context.CancelFunc
	notificationActivations *wsllauncher.NotificationActivationQueue

	// browserHost is the embedded browser pane's WebView2 environment,
	// built on the first browser:host directive and nil until then
	// (browserhost.go). Guarded by mu like the bridge above, because
	// directives arrive on their own goroutine.
	browserHost *webview2host.Host
	// browserReports serializes the host's answers back to the backend.
	// It is deliberately NOT under mu: its jobs take mu themselves, and
	// submission happens from a WebView2 completion handler on the UI
	// thread, which must never block. Zero value ready.
	browserReports serialqueue.Queue

	// flushGeometry persists the window placement immediately. Set on the
	// ApplicationStarted handler (with window) and read on shutdown as a
	// backstop to the WindowClosing flush; both under mu. nil until that
	// handler runs.
	flushGeometry func()

	// updateInstalling serializes install directives: one swap in flight at a
	// time, and a directive that arrives while one is running is dropped (see
	// handleUpdateInstall). Not under mu — it is claimed from the notification
	// bridge's dispatch goroutine and held across the whole install, which
	// would otherwise block every lifecycle path that takes the mutex.
	updateInstalling atomic.Bool

	// backendURL holds the page URL launchAndShow pointed the WebView at.
	// Read by the reload keybinding (uikeys.BrowserWithReload) so Ctrl+R
	// re-navigates with a credential instead of reloading the SPA's
	// scrubbed URL. atomic.Pointer because the writer is launchAndShow
	// (goroutine) and the reader is the Wails event loop.
	backendURL atomic.Pointer[string]
}

type launcherExit struct {
	done chan struct{}
	mu   sync.Mutex
	err  error
}

func (e *launcherExit) set(err error) {
	e.mu.Lock()
	e.err = err
	close(e.done)
	e.mu.Unlock()
}

func (e *launcherExit) error() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

// PickDistro is bound to the picker HTML. It's invoked once when the
// user clicks a distro row. Subsequent invocations are silently
// ignored — the launcher is single-shot for a process lifetime.
//
// The chosen distro is intentionally NOT persisted here: if install or
// Launch fails downstream, persisting up front would trap the user on
// the next boot (the saved distro short-circuits the picker, the
// retry hits the same failure, no escape). launchAndShow persists the
// final {distro, version} pair only after a successful launch.
func (a *launcherApp) PickDistro(name string) error {
	a.mu.Lock()
	already := a.launcher != nil
	a.mu.Unlock()
	if already {
		return errors.New("backend already launched")
	}

	if err := a.validateDistroName(name); err != nil {
		return err
	}

	// Picker selections are user intent — persist on success.
	// launchAndShow owns the WebView URL on every exit path (picker,
	// connectivity-error, or backend URL), so we don't override it here.
	if err := a.launchAndShow(name, false); err != nil {
		return err
	}
	return nil
}

// validateDistroName ensures `name` matches one of the distros wsl.exe
// reported when the launcher started. Without this check, a malicious
// or corrupted picker page could pass arbitrary strings into the WSL
// invocation. The list is fixed for the launcher's lifetime, so a
// linear scan is fine.
func (a *launcherApp) validateDistroName(name string) error {
	if name == "" {
		return errors.New("distro name is required")
	}
	for _, d := range a.distros {
		if d.Name == name {
			return nil
		}
	}
	return fmt.Errorf("unknown distro %q", name)
}

// launchAndShow runs the install + launch pipeline and then points
// the visible window at the WSL backend.
//
// transient suppresses persisting the chosen distro to wsl.json on
// success. Used by the --distro override path so a dev-mode run
// doesn't overwrite the user's saved pick from double-clicking the
// .exe earlier.
//
// Outer timeout removed — cold WSL2 boot can exceed any single budget
// (the WSL VM itself can take 20+ seconds, then 9P startup, then SQLite
// migrations). Inner phase timeouts (install, bootstrap) bound the
// user-visible wait per step. If the user wants to abort they close
// the window; the Wails OnShutdown hook tears the WSL child down.
func (a *launcherApp) launchAndShow(distro string, transient bool) error {
	ctx := context.Background()

	// The window is created on the ApplicationStarted handler, which kicks off
	// this call (or PickDistro does, after the picker has rendered), so it's set
	// by now; capture it under the mutex once and drive all SetURL calls through
	// it rather than re-reading the shared field.
	w := a.win()
	if w == nil {
		return errors.New("launchAndShow: window not created yet")
	}

	started := time.Now()
	defer logBootPhase("launcher.launch_and_show.total", started)

	phaseStarted := time.Now()
	binPath, err := a.ensurePayloadInstalled(ctx, distro)
	logBootPhase("launcher.ensure_payload", phaseStarted)
	if err != nil {
		w.SetURL("/picker")
		return fmt.Errorf("install payload: %w", err)
	}

	l, bs, err := a.launchAndProbe(ctx, distro, binPath)
	if err != nil {
		var httpErr bootstrapHTTPError
		switch {
		case errors.Is(err, errLaunchFailed):
			// The spawn itself died (child exited before the bootstrap
			// line). The distro choice is fine — re-showing the picker
			// reads as "pick again" and hides that anything failed
			// (observed 2026-08-30: a broken backend spawn presented as
			// the first-run distro picker). The actionable artifact is
			// launcher.log, which is what /startup-error points at.
			w.SetURL("/startup-error")
			return err
		case errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusInternalServerError:
			w.SetURL("/startup-error")
			return fmt.Errorf("backend failed during startup: %w", err)
		default:
			w.SetURL("/connectivity-error")
			return fmt.Errorf("backend booted but unreachable from Windows: %w", err)
		}
	}
	a.mu.Lock()
	a.backendBootstrap = bs
	a.mu.Unlock()
	if activeProfile != "" {
		if err := writeWSLContainmentEvidence(context.Background(), distro, binPath, bs); err != nil {
			if stopErr := a.stopLaunchedBackend(l, bs); stopErr != nil {
				return errors.Join(fmt.Errorf("write WSL containment evidence: %w", err), stopErr)
			}
			return fmt.Errorf("write WSL containment evidence: %w", err)
		}
	}

	if err := a.startNotificationBridge(bs, l); err != nil {
		log.Printf("notifications: start launcher bridge: %v", err)
	}

	// Persist the chosen distro + installed version only after the
	// backend has booted successfully. This pairs with PickDistro's
	// deliberate non-persistence on failure: a saved distro short-
	// circuits the picker on next launch, so we only commit it after
	// we've proven the install + launch path works end-to-end.
	//
	// transient runs (the --distro override) skip persistence so a dev
	// invocation doesn't redirect the user's saved pick. The install
	// step still runs and the next non-override launch reuses the
	// freshly-installed binary in that distro — we just don't change
	// which distro the launcher boots into by default.
	if !transient {
		if err := a.persistSuccessfulLaunch(distro); err != nil {
			log.Printf("save config after launch: %v", err)
		}
	}

	// The backend assembles the page URL (main.go webviewPageURL): the
	// client id and page marker that ride on it are its own. It carries
	// no credential — this window's ticket arrives by injection — and it
	// names 127.0.0.1, not "localhost", because Windows resolves
	// "localhost" to both ::1 and 127.0.0.1 and WSL2's
	// localhostForwarding only proxies IPv4, so a ::1 attempt hits
	// Windows-loopback directly and is refused.
	pageURL := bs.PageURL
	// Redacted logging, same shape the probe uses. The URL no longer
	// carries a credential, but launcher.log persists in %APPDATA% across
	// runs and is a likely artifact in user bug reports, so the byte
	// counts stay the whole record.
	log.Printf("backend ready at http://127.0.0.1:%d/ (page url=%d bytes, token=%d bytes)", bs.Port, len(pageURL), len(bs.Token))
	// Publish the URL before SetURL so a reload that lands between
	// SetURL and the SPA's first bootstrap fetch still finds a credential.
	a.backendURL.Store(&pageURL)
	phaseStarted = time.Now()
	w.SetURL(pageURL)
	logBootPhase("launcher.window_set_url", phaseStarted)
	return nil
}

// errLaunchFailed marks "we never got a backend at all" — wsl.exe
// refused, the child died before its bootstrap line, the distro is
// broken. launchAndShow routes this class back to the picker, since
// there is nothing to connect to and possibly another distro to try.
var errLaunchFailed = errors.New("launch backend")

// resetTransportPortArg is the backend flag that discards this
// install's pinned listen port before binding (the backend declares the
// same name from wsllauncher and acts on it in main_transport_port.go).
// It rides the child's argv rather than an env var deliberately: WSLENV
// passthrough is for diagnostics, and anything load-bearing across the
// WSL boundary belongs in explicit launch args
// (internal/wsllauncher/AGENTS.md).
const resetTransportPortArg = "--" + wsllauncher.ResetTransportPortFlag

// launchAndProbe spawns the WSL backend and proves the Windows host can
// reach its listener, which is the whole reason for the probe: WSL2
// forwards 127.0.0.1:<port> out of the distro over vEthernet, and when
// that forward is missing the backend serves happily inside WSL while
// the WebView2 blank-screens with no feedback.
//
// It retries EXACTLY ONCE, on a fresh transport port, when the first
// attempt was unreachable at the transport layer. That case is not
// hypothetical: the backend pins its listen port per install so the
// webview origin (and every origin-scoped browser store behind it) is
// stable, and the pin is adopted from the ephemeral range — the same
// range Hyper-V/WSL2 excluded port ranges cover, re-seeded on every
// Windows reboot. Nothing on the WSL side can see that: the bind
// SUCCEEDS, so the backend's own pin-clearing never fires, and without
// this retry the app would show /connectivity-error identically on
// every launch, forever, with the mitigation on that page (turn
// localhostForwarding on) already true.
//
// One retry, not a loop: a fresh port costs the user their origin-scoped
// browser state, and if a second port is unreachable too the problem is
// the forwarding path itself, which is exactly what the error page
// describes.
func (a *launcherApp) launchAndProbe(ctx context.Context, distro, binPath string) (*wsllauncher.Launcher, *wsllauncher.Bootstrap, error) {
	l, bs, err := a.launchBackend(ctx, distro, binPath, nil)
	if err != nil {
		return nil, nil, err
	}

	probeErr := probeLaunchedBackend(bs)
	if probeErr == nil {
		return l, bs, nil
	}
	if !retryWithFreshTransportPort(probeErr) {
		if stopErr := a.stopLaunchedBackend(l, bs); stopErr != nil {
			return nil, nil, errors.Join(probeErr, stopErr)
		}
		return nil, nil, probeErr
	}

	log.Printf("launcher: port %d is unreachable from Windows; relaunching the backend on a fresh transport port", bs.Port)
	// Retire the unreachable backend first. It is healthy inside the
	// distro and holds the app's SQLite store; two backends on one data
	// dir would fight over the writer.
	if err := a.stopLaunchedBackend(l, bs); err != nil {
		return nil, nil, err
	}

	l, bs, err = a.launchBackend(ctx, distro, binPath, []string{resetTransportPortArg})
	if err != nil {
		return nil, nil, err
	}
	if retryErr := probeLaunchedBackend(bs); retryErr != nil {
		// Wrapped, so the caller's bootstrapHTTPError classification
		// still reads the retry's own failure; the first attempt rides
		// along as context for launcher.log.
		return nil, nil, fmt.Errorf("%w (also unreachable on the previous port: %v)", retryErr, probeErr)
	}
	return l, bs, nil
}

// profileBackendArgs returns the backend flags this launch profile
// implies.
//
// `--soak` is the internal launcher↔backend WIRE FLAG meaning "isolated
// mocked instance speaking the launcher bootstrap contract": it makes
// the WSL backend rewrite both provider binaries to ao-mockprovider,
// redirect HOME, and open its own data dir, while still printing the
// ordinary headless {port, token} line this launcher parses. The name is
// historical — the soak rig was the first thing to need it — and it is
// launcher-owned, never typed by a user. Both isolated profiles ride it;
// what separates them is `--autopilot`, which arms the soak preset (seed
// two threads, start a never-ending streaming turn). The harness profile
// boots the same instance and then waits for whoever is driving it.
//
// `--launcher-pid` hands the backend this launcher's own Windows pid so
// a deliberate teardown (`ao-harness down`) can close the window too: the
// launcher survives its child's death on purpose, to preserve the
// evidence of a crash.
//
// These ride the child's argv rather than an env var deliberately:
// WSLENV passthrough is for diagnostics, and anything load-bearing
// across the WSL boundary belongs in explicit launch args
// (internal/wsllauncher/AGENTS.md). --data-dir is deliberately NOT
// spelled here: the launcher runs on the Windows side and has no Linux
// path to offer, so the backend resolves its own default.
func profileBackendArgs() ([]string, error) {
	if activeProfile == "" {
		return nil, nil
	}
	identity, err := instanceinfo.CaptureProcessIdentity(os.Getpid())
	if err != nil {
		return nil, fmt.Errorf("capture launcher process identity: %w", err)
	}
	mode := launcherRuntimeMode()
	args := []string{
		"--launcher-pid", strconv.Itoa(os.Getpid()),
		"--launcher-start-time", identity.StartTime,
		"--launcher-executable", identity.Executable,
		"--launcher-profile", mode,
		"--launcher-webview-profile", webviewDataDir(mode),
	}
	switch activeProfile {
	case appidentity.ProfileHarness:
		return append([]string{"--soak"}, args...), nil
	case appidentity.ProfileSoak:
		return append([]string{"--soak", "--autopilot"}, args...), nil
	case appidentity.ProfilePerf:
		return append([]string{"--soak", "--isolated-profile", "perf"}, args...), nil
	default:
		return nil, fmt.Errorf("unknown launcher profile %q", activeProfile)
	}
}

// launchBackend spawns the WSL child and records it as the live
// launcher. extraArgs are appended to the backend's own argv, after the
// profile's own flags.
func (a *launcherApp) launchBackend(ctx context.Context, distro, binPath string, extraArgs []string) (*wsllauncher.Launcher, *wsllauncher.Bootstrap, error) {
	phaseStarted := time.Now()
	if err := a.acquireHarnessReservation(distro); err != nil {
		return nil, nil, err
	}
	profileArgs, err := profileBackendArgs()
	if err != nil {
		_ = a.releaseHarnessReservation()
		return nil, nil, err
	}
	args := append(profileArgs, extraArgs...)
	l, bs, err := wsllauncher.Launch(ctx, wsllauncher.LaunchOptions{
		Distro:         distro,
		BinaryPath:     binPath,
		ExtraArgs:      args,
		PassthroughEnv: diagenv.Passthrough(),
		MemoryLimitBytes: func() uint64 {
			if activeProfile != "" {
				return governor.DefaultCeilingBytes
			}
			return 0
		}(),
		UseParentJob: activeProfile != "",
	})
	logBootPhase("launcher.wsl_launch", phaseStarted)
	if err != nil {
		_ = a.releaseHarnessReservation()
		return nil, nil, fmt.Errorf("%w: %w", errLaunchFailed, err)
	}

	a.mu.Lock()
	a.launcher = l
	waitDone := &launcherExit{done: make(chan struct{})}
	a.launcherWait = waitDone
	if activeProfile != "" {
		a.memoryWatchCancel = startWSLMemoryWatchdog(context.Background(), distro, binPath, bs, governor.DefaultCeilingBytes, func() {
			log.Printf("harness memory watchdog: stopping isolated launcher after WSL safety failure")
			if err := l.Stop(); err != nil {
				log.Printf("harness memory watchdog: stop WSL launcher: %v", err)
			}
			a.mu.Lock()
			app := a.wails
			a.mu.Unlock()
			if app != nil {
				app.Quit()
			}
		})
	}
	a.mu.Unlock()
	go func() { waitDone.set(l.Wait()) }()
	return l, bs, nil
}

// stopLaunchedBackend tears down a backend we are abandoning and clears
// it from the launcher state, so OnShutdown can't later Stop a child
// that a newer launch has already replaced.
func (a *launcherApp) stopLaunchedBackend(l *wsllauncher.Launcher, bs *wsllauncher.Bootstrap) error {
	a.mu.Lock()
	var waitDone *launcherExit
	var watchCancel context.CancelFunc
	if a.launcher == l {
		a.launcher = nil
		waitDone = a.launcherWait
		a.launcherWait = nil
		watchCancel = a.memoryWatchCancel
		a.memoryWatchCancel = nil
		a.backendBootstrap = nil
	}
	a.mu.Unlock()
	if watchCancel != nil {
		watchCancel()
	}
	backendGone := false
	if bs != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := a.shutdownBackend(ctx, bs); err == nil {
			if err := waitBackendGone(ctx, bs); err != nil {
				log.Printf("launcher: backend acknowledged shutdown but stayed reachable: %v", err)
			} else {
				backendGone = true
			}
		} else {
			log.Printf("launcher: authenticated backend shutdown unavailable: %v; using Job Object fallback", err)
		}
	}
	stopErr := l.Stop()
	if stopErr != nil {
		log.Printf("launcher: stop backend fallback: %v", stopErr)
	}
	if waitDone != nil {
		select {
		case <-waitDone.done:
			waitErr := waitDone.error()
			if waitErr != nil {
				log.Printf("launcher: backend wrapper exited with error: %v", waitErr)
			}
		case <-time.After(shutdownTimeout):
			return errors.New("launcher backend wrapper did not exit after stop")
		}
	}
	if stopErr != nil {
		return stopErr
	}
	if bs == nil {
		return nil
	}
	if backendGone {
		return a.releaseHarnessReservation()
	}
	return errors.New("backend teardown was not authenticated or confirmed; refusing to reuse its data root")
}

// shutdownBackend uses the token from the just-validated bootstrap. The Job
// Object only owns the Windows wsl.exe process and WSL may transfer Linux
// children to wslhost.exe, so closing it is a fallback, never proof that the
// backend or its providers have stopped.
func (a *launcherApp) shutdownBackend(ctx context.Context, bs *wsllauncher.Bootstrap) error {
	client, err := wsllauncher.NewNotificationClient(wsllauncher.NotificationClientConfig{
		WSURL:      fmt.Sprintf("ws://127.0.0.1:%d/ws", bs.Port),
		Token:      bs.Token,
		Present:    func(notify.Send) error { return nil },
		MinBackoff: 20 * time.Millisecond,
		MaxBackoff: 100 * time.Millisecond,
		Logf:       log.Printf,
	})
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go client.Run(runCtx)
	return client.Shutdown(ctx)
}

func waitBackendGone(ctx context.Context, bs *wsllauncher.Bootstrap) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := probeBootstrapWithConfig(bs.Port, bs.Token, bootstrapProbeConfig{
			AttemptTimeout: 200 * time.Millisecond,
			Deadline:       300 * time.Millisecond,
			PollInterval:   50 * time.Millisecond,
		}); err != nil && errors.Is(err, errBackendUnreachable) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("backend remained reachable: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// probeLaunchedBackend runs the connectivity probe and logs its verdict.
func probeLaunchedBackend(bs *wsllauncher.Bootstrap) error {
	phaseStarted := time.Now()
	err := probeBootstrap(bs.Port, bs.Token)
	logBootPhase("launcher.probe_bootstrap", phaseStarted)
	if err != nil {
		log.Printf("connectivity probe failed: %v", err)
	}
	return err
}

// currentBackendURL returns a page URL for the reload keybinding, or ""
// before the WSL backend has finished booting. "" tells
// uikeys.BrowserWithReload to fall through to the default
// window.Reload() — picker / loading / connectivity-error pages are
// static and reload-safe.
//
// It asks the backend for a FRESH page URL first, because the boot URL
// is a string the backend assembled for one navigation and a rebind (the
// LAN toggle) moves the origin out from under it. The credential is not
// on either URL — the reloaded document is ticketed by injection like
// every other one — so the stored URL is a perfectly good fallback and a
// blip costs a reload nothing. The request is one loopback GET against a
// backend that is by definition serving.
func (a *launcherApp) currentBackendURL() string {
	stored := ""
	if p := a.backendURL.Load(); p != nil {
		stored = *p
	}
	a.mu.Lock()
	bs := a.backendBootstrap
	a.mu.Unlock()
	if bs == nil {
		return stored
	}
	fresh, err := fetchPageURL(bs.Port, bs.Token)
	if err != nil {
		log.Printf("reload: fresh page url unavailable (%v); reusing the launch URL", err)
		return stored
	}
	return fresh
}

// pageTicket mints one one-time page ticket for the SPA document the
// WebView2 has just loaded, or fails before the WSL backend exists.
//
// The launcher owns this window, so the ticket is injected into the
// document rather than written into the URL that loaded it
// (internal/uiwindow.DeliverPageTicket). The failure before a backend is
// what keeps the launcher's own picker / loading / error pages out of the
// delivery path even if one of them ever announced itself as a host page.
func (a *launcherApp) pageTicket() (string, error) {
	a.mu.Lock()
	bs := a.backendBootstrap
	a.mu.Unlock()
	if bs == nil {
		return "", errors.New("page ticket requested before the backend booted")
	}
	answer, err := fetchWebviewPageURL(bs.Port, bs.Token)
	if err != nil {
		return "", err
	}
	return answer.Ticket, nil
}

// pageURLTimeout bounds the reload path's page-URL request. The
// keybinding runs on the WebView2 UI thread, so this is a stall budget
// as much as a network one: the backend is local and answering the
// route from memory.
const pageURLTimeout = 2 * time.Second

// fetchPageURL asks the backend's transport for a bare page URL to
// navigate to. The session token goes in a header — the query slot on
// this backend's routes belongs to the page ticket, and a header keeps
// the credential out of any URL a proxy or log would see.
func fetchPageURL(port int, token string) (string, error) {
	answer, err := fetchWebviewPageURL(port, token)
	if err != nil {
		return "", err
	}
	return answer.URL, nil
}

// fetchWebviewPageURL is the one request behind both halves: the bare
// URL a navigation needs and the ticket the document it produces needs.
// Asking with the webview marker is what splits them, so neither half
// ever appears in the other's string.
func fetchWebviewPageURL(port int, token string) (pagehost.Answer, error) {
	endpoint := fmt.Sprintf("http://127.0.0.1:%d%s%s", port, wsllauncher.PageURLPath, wsllauncher.PageURLWebviewQuery)
	resp, err := getWithToken(&http.Client{Timeout: pageURLTimeout}, endpoint, token)
	if err != nil {
		return pagehost.Answer{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return pagehost.Answer{}, fmt.Errorf("GET %s: status %d", endpoint, resp.StatusCode)
	}
	// One small JSON object; 8 KiB is far past any real page URL and
	// bounds a misbehaving responder.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil {
		return pagehost.Answer{}, err
	}
	var answer pagehost.Answer
	if err := json.Unmarshal(body, &answer); err != nil {
		return pagehost.Answer{}, fmt.Errorf("decode page url answer: %w", err)
	}
	answer.URL = strings.TrimSpace(answer.URL)
	answer.Ticket = strings.TrimSpace(answer.Ticket)
	if answer.URL == "" || answer.Ticket == "" {
		return pagehost.Answer{}, errors.New("page url answer was missing a half")
	}
	return answer, nil
}

// tolerantMultiWriter is io.MultiWriter without abort-on-first-error.
// Per-writer Write errors are dropped so a closed/disconnected writer
// (notably os.Stderr in a `windowsgui` app) doesn't suppress writes
// to the remaining writers. Always reports len(p) bytes written so
// the log package doesn't see a short-write error and stop logging.
func tolerantMultiWriter(writers ...io.Writer) io.Writer {
	return tolerantWriters(writers)
}

type tolerantWriters []io.Writer

func (t tolerantWriters) Write(p []byte) (int, error) {
	for _, w := range t {
		_, _ = w.Write(p)
	}
	return len(p), nil
}

// probeBootstrap performs a deadline-bounded HTTP GET against the WSL
// backend's /bootstrap.json over localhost. A successful response
// proves the WSL2 localhostForwarding path is functional; a timeout or
// connection refused indicates the backend is reachable from inside
// the distro but the Windows WebView won't be able to reach it.
//
// We probe /bootstrap.json rather than just opening a socket because a
// stale TIME_WAIT socket from a prior boot can satisfy a TCP connect
// while the new server isn't actually accepting requests. A successful
// HTTP response with the expected status confirms the right server is
// listening. Network errors (timeout / connection refused) are the
// localhostForwarding signal; HTTP-level errors (4xx/5xx) mean the
// path works but something else is wrong server-side, which we still
// surface as failure so the user sees actionable feedback rather than
// a blank WebView.
func probeBootstrap(port int, token string) error {
	return probeBootstrapWithConfig(port, token, bootstrapProbeConfig{
		AttemptTimeout: bootstrapProbeAttemptTimeout,
		Deadline:       bootstrapProbeDeadline,
		PollInterval:   bootstrapProbePollInterval,
	})
}

type bootstrapProbeConfig struct {
	AttemptTimeout time.Duration
	Deadline       time.Duration
	PollInterval   time.Duration
}

type bootstrapHTTPError struct {
	StatusCode int
	URL        string
}

func (e bootstrapHTTPError) Error() string {
	return fmt.Sprintf("GET %s: status %d", e.URL, e.StatusCode)
}

// errBackendUnreachable marks a probe that never received a single HTTP
// response from the WSL backend: every attempt inside the deadline was
// refused, reset, or timed out at the transport layer. That is the
// signature of the Windows→WSL localhost path not carrying this port —
// either localhostForwarding is off, or (the case the retry below
// exists for) the port sits inside a Hyper-V/WSL2 excluded range, which
// Windows re-seeds on every reboot and which routinely covers the
// ephemeral range our pinned port is adopted from.
//
// A probe that got ANY HTTP response never carries this: the localhost
// path demonstrably works, and the failure is server-side.
var errBackendUnreachable = errors.New("no HTTP response from the WSL backend over Windows localhost")

// retryWithFreshTransportPort decides whether a failed connectivity
// probe is worth one relaunch on a fresh transport port. Only the
// unreachable class qualifies — a startup failure, a refused
// credential, or a host-guard rejection all prove the port is
// reachable, and moving it would cost the user their origin-scoped
// browser state for nothing.
func retryWithFreshTransportPort(err error) bool {
	return errors.Is(err, errBackendUnreachable)
}

func probeBootstrapWithConfig(port int, token string, cfg bootstrapProbeConfig) error {
	if cfg.AttemptTimeout <= 0 {
		cfg.AttemptTimeout = bootstrapProbeAttemptTimeout
	}
	if cfg.Deadline <= 0 {
		cfg.Deadline = bootstrapProbeDeadline
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = bootstrapProbePollInterval
	}

	// 127.0.0.1, not "localhost": Windows resolves "localhost" to both
	// ::1 and 127.0.0.1, and Go's dialer races them. WSL2's
	// localhostForwarding only proxies IPv4, so a ::1 attempt hits
	// Windows-loopback directly and is refused — surfacing a misleading
	// connectivity-error page even though the backend is fine on
	// 127.0.0.1. Pinning the IPv4 literal removes the race.
	url := fmt.Sprintf("http://127.0.0.1:%d/bootstrap.json", port)
	// The session token rides an Authorization header, not the query
	// string: the query slot on this route belongs to the one-time page
	// ticket a browser presents, and this probe is not a browser. The
	// header also keeps the credential out of URLs that get logged.
	log.Printf("probe: GET %s (token=%d bytes)", url, len(token))
	redacted := url

	// Poll, don't one-shot. WSL2 NAT mode installs the Windows-side
	// forward rule for a fresh WSL listener AFTER the listener binds,
	// and the launcher's first probe usually lands inside that race —
	// Windows returns RST and we'd surface a misleading
	// connectivity-error page even though the backend is healthy and
	// the forwarder is about to catch up. We use a 250 ms / 30 s probe
	// loop because the backend may now publish its bootstrap port before
	// ServiceStartup has released readiness.
	//
	// Transport errors (refused / timeout / DNS failure) are
	// transient and trigger a retry. An HTTP-level response (any
	// status code) means the request reached our handler and we
	// decide right there: 200 = ready, 503 = backend still booting and
	// worth retrying, anything else (token mismatch, host guard reject)
	// is terminal.
	client := &http.Client{Timeout: cfg.AttemptTimeout}
	deadline := time.Now().Add(cfg.Deadline)
	var lastErr error
	attempt := 0
	// Tracks whether the localhost path ever carried a response at all —
	// the difference between "Windows cannot reach this port" and "the
	// backend answered and we didn't like the answer". See
	// errBackendUnreachable.
	sawHTTPResponse := false
	for {
		attempt++
		resp, err := getWithToken(client, url, token)
		if err == nil {
			sawHTTPResponse = true
			// 64KB bounds a rogue server's response while leaving room
			// for the real document: Bootstrap grew past 256 bytes once
			// harness boots added pageMarker + store identity, and a cap
			// below the document size truncates valid JSON — decode then
			// fails on every boot (observed 2026-08-30, harness profile).
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				if err := validateBootstrapResponse(body, port); err != nil {
					log.Printf("probe: invalid bootstrap response: %v", err)
					return err
				}
				if attempt > 1 {
					log.Printf("probe: ok after %d attempts", attempt)
				}
				return nil
			}
			if resp.StatusCode == http.StatusServiceUnavailable {
				lastErr = fmt.Errorf("GET %s: status %d", redacted, resp.StatusCode)
				if time.Now().After(deadline) {
					break
				}
				time.Sleep(cfg.PollInterval)
				continue
			}
			// Server is reachable but rejected. Surface the response
			// shape (status + first bytes of body) so a future
			// regression in handleBootstrap shows up clearly.
			log.Printf("probe: status=%d host-resp=%q", resp.StatusCode, string(body[:min(len(body), 256)]))
			return bootstrapHTTPError{StatusCode: resp.StatusCode, URL: redacted}
		}
		lastErr = err
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(cfg.PollInterval)
	}
	if !sawHTTPResponse {
		return fmt.Errorf("GET %s: %w after %d attempts: %w", redacted, errBackendUnreachable, attempt, lastErr)
	}
	return fmt.Errorf("GET %s: timed out after %d attempts: %w", redacted, attempt, lastErr)
}

// getWithToken issues one probe request carrying the session token in
// an Authorization header. Shared by every launcher-side call so the
// carrier stays in one place.
func getWithToken(client *http.Client, url, token string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return client.Do(req)
}

// validateBootstrapResponse checks the manifest the backend answered
// with is the one this launcher booted. The manifest no longer carries
// a credential to compare — the page's credential is an HttpOnly cookie
// the backend sets, and this probe authenticates with a header — so the
// wsUrl's host and port carry the whole check: they prove the responder
// is our backend on our port rather than some other server that
// happened to answer on the forwarded loopback hop.
func validateBootstrapResponse(body []byte, port int) error {
	var bootstrap struct {
		WSURL string `json:"wsUrl"`
	}
	if err := json.Unmarshal(body, &bootstrap); err != nil {
		return fmt.Errorf("decode bootstrap response: %w", err)
	}
	parsed, err := url.Parse(bootstrap.WSURL)
	if err != nil {
		return fmt.Errorf("parse bootstrap wsUrl: %w", err)
	}
	if parsed.Scheme != "ws" || parsed.Path != "/ws" {
		return fmt.Errorf("bootstrap wsUrl has unexpected shape: %q", bootstrap.WSURL)
	}
	host, portString, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return fmt.Errorf("split bootstrap wsUrl host: %w", err)
	}
	if host != "127.0.0.1" {
		return fmt.Errorf("bootstrap wsUrl host = %q, want 127.0.0.1", host)
	}
	if portString != fmt.Sprintf("%d", port) {
		return fmt.Errorf("bootstrap wsUrl port = %q, want %d", portString, port)
	}
	return nil
}

// persistSuccessfulLaunch writes the {distro, installed_version} pair
// to wsl.json once the backend has booted. Called only after a
// successful Launch so a half-broken launcher doesn't trap the user
// on the next boot with a saved-but-broken distro choice.
//
// We re-read wsl.json before mutating so any field the WSL backend
// wrote during the previous session (notably Distro, when the user
// switched via the Settings UI) doesn't get clobbered. The launcher
// owns InstalledVer + InstalledDistro; the backend owns Distro from
// the moment the user picks a different one.
func (a *launcherApp) persistSuccessfulLaunch(distro string) error {
	if activeProfile != "" {
		// wsl.json is shared with the real instance (and co-written by
		// the WSL backend's Settings distro switch). A profiled launch
		// reads it but never writes it: its InstalledVer would make the
		// developer's next real launch skip a payload reinstall it
		// actually needed. This is the choke point rather than the
		// transient flag because the picker path passes transient=false.
		log.Printf("launcher: profile=%s — not persisting the distro choice to wsl.json", activeProfile)
		return nil
	}
	cfg, _ := loadConfig()
	if cfg == nil {
		cfg = &wsldistro.Config{}
	}
	cfg.Distro = distro
	cfg.InstalledVer = payloadVersion
	cfg.InstalledDistro = distro
	return saveConfig(cfg)
}

// buildApp wires the Wails service + window. We pass the distro list
// at construction time so the picker can render it directly. (A
// freshly-running picker can't query Go for the list because Wails
// service bindings are method-based — the listing-as-data pattern
// keeps the picker UI dumb.)
//
// initialURL selects which static page the WebView2 lands on first.
// "/picker" shows the distro picker; "/loading" shows a spinner
// while we wait for SetURL to flip us over to the WSL backend.
//
// make dev-wsl adds the remote-debugging port (so a developer can
// attach Chrome DevTools / talk CDP from inside WSL) plus the staged
// memory-reduction experiments described in browserArgs.
// win returns the window under the mutex. It's created on the ApplicationStarted
// handler goroutine (see buildApp) while the single-instance callback and
// launchAndShow may read it concurrently; nil until that handler runs.
func (a *launcherApp) win() *application.WebviewWindow {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.window
}

// buildApp wires the Wails app and registers the window-creation handler.
// chosen/transient describe the resolved launch target: when chosen is
// non-empty the handler also kicks off the backend launch (the picker is
// skipped).
func buildApp(distros []wsllauncher.Distro, initialURL, chosen string, transient bool) *launcherApp {
	a := &launcherApp{
		distros:                 distros,
		notificationActivations: wsllauncher.NewNotificationActivationQueue(),
	}
	a.notificationContext, a.notificationCancel = context.WithCancel(context.Background())
	notificationService := newLauncherNotificationService(func(target notify.Target) {
		a.queueNotificationActivation(target)
	})
	a.notificationService = notificationService
	mode := wslSingleInstanceMode()
	title := appidentity.AppTitle(mode)

	// Preserve the previous browser session's Chromium log before this
	// session's WebView2 environment truncates it — see rotateChromeDebugLog.
	rotateChromeDebugLog(webviewDataDir(mode))

	forensicsDir := renderForensicsDir(mode)
	if forensicsDir == "" {
		log.Printf("webview2: render-hang forensics disabled: %%APPDATA%% is unresolvable")
	} else {
		// One startup line so launcher.log always names the evidence
		// path — a hang report's first grep is this, then the dir.
		log.Printf("webview2: render-hang forensics dir: %s", forensicsDir)
	}

	app := application.New(application.Options{
		Name:           title,
		SingleInstance: wslSingleInstanceOptions(a.win),
		// Route Wails' internal slog into launcher.log. Its default logger
		// writes to stderr, which a GUI subsystem exe discards — that hid
		// the WebView2 process-failure/recovery lines ("webview2: process
		// failed: kind=...") during the mixed-DPI investigation.
		Logger: slog.New(slog.NewTextHandler(log.Writer(), &slog.HandlerOptions{
			Level: wailsLogLevel(mode),
		})),
		Services: []application.Service{
			application.NewService(notificationService),
			application.NewService(a),
		},
		Assets: application.AssetOptions{
			Handler: pickerAssetHandler(distros),
		},
		Windows: webviewBrowserOptions(mode, webviewDataDir(mode), forensicsDir),
		// Cancel app shutdown until the user explicitly closes the
		// window. Without this, a transient WSL hiccup during launch
		// would crash us silently.
		PanicHandler: func(d *application.PanicDetails) {
			log.Printf("wails panic: %v", d)
		},
		// Kill the WSL child when the launcher is told to quit. We
		// also lean on the Job Object — this hook is the graceful
		// shutdown path; the Job Object is the unconditional one.
		OnShutdown: func() {
			// Backstop the WindowClosing flush so the final placement is
			// persisted even if that event didn't fire. Uses the tracker's
			// in-memory latest, so it's safe after the window is gone.
			a.mu.Lock()
			flush := a.flushGeometry
			l := a.launcher
			bs := a.backendBootstrap
			cancelNotifications := a.notificationCancel
			a.mu.Unlock()
			if flush != nil {
				flush()
			}
			// Before Wails closes the windows below us: a pane controller
			// outliving its parent HWND faults inside WebView2. Safe to
			// call from here even though this hook already runs on the main
			// thread, because Wails' dispatch runs the closure inline when
			// it is already there rather than posting to a pump that is
			// blocked on us.
			a.closeBrowserHost()
			if l != nil {
				a.stopLaunchedBackend(l, bs)
			}
			if cancelNotifications != nil {
				cancelNotifications()
			}
		},
	})
	a.wails = app

	// Without KeyBindings the WebView2 swallows zoom/reload/fullscreen
	// and there's no menu bar to fall back on. `make dev-wsl` makes
	// this the only window the user touches, so the missing bindings
	// were the most user-visible symptom. BrowserWithReload reads
	// a.backendURL on each reload so Ctrl+R re-navigates with the
	// bootstrap token after the SPA scrubs it from window.location —
	// see uikeys.BrowserWithReload.
	//
	// F12 devtools is gated on launcherMode because dev and prod ship
	// the same .exe (only this ldflags string differs) and Wails
	// compiles WebView2 devtools in unconditionally — the binding is
	// the only thing standing between prod users and an inspector.
	keyBindings := uikeys.BrowserWithReload(a.currentBackendURL)
	if launcherMode == "dev" {
		keyBindings = uikeys.WithDevTools(keyBindings)
	}
	// Context-menu policy lives in the frontend guard
	// (browserHistoryGuard.ts): native menu allowed in editable fields
	// and on selected text, suppressed elsewhere. Don't set
	// DefaultContextMenuDisabled — on WebView2 it would hard-disable
	// the allowed menus below the JS layer. (The picker/loading pages
	// keep their native menu; they're plain trusted HTML with nothing
	// to hide.)
	width, height := 1280, 800
	if mode == appidentity.ModeSoak {
		width, height = soakWindowWidth, soakWindowHeight
	}
	opts := application.WebviewWindowOptions{
		Title:            title,
		Width:            width,
		Height:           height,
		MinWidth:         800,
		MinHeight:        600,
		BackgroundColour: application.NewRGBA(22, 22, 30, 255),
		// Picker / loading first; once Launch returns we SetURL to
		// the WSL backend's localhost port. We can't use the WSL URL
		// up front because we don't know the port until after Launch.
		URL:         initialURL,
		KeyBindings: keyBindings,
	}
	// Reopen where we left off last. The window is created here — on
	// ApplicationStarted — rather than before app.Run() so it materializes
	// synchronously against a live app loop. That's what lets RestoreAndTrack
	// maximize/fullscreen a restored window on the monitor it was saved on and
	// reveal it already in that state (Wails' creation-time maximize lands on
	// the primary and flashes). The placement survives the picker → backend
	// SetURL navigation because it's the same window object, and is stored
	// Windows-side in window.json, not the WSL settings.
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		w, flush := uiwindow.RestoreAndTrack(app, opts, loadWindowGeometry(), saveWindowGeometry)
		trimWebviewMemoryOnMinimise(w)
		// Every SPA document this window loads is handed its one-time
		// page ticket here, so the URL the launcher navigates to (and
		// logs, and reports in a window diagnostic) carries no
		// credential at all. Only the SPA announces itself to its host,
		// so the picker / loading / error pages never reach this — and
		// a.pageTicket refuses anyway until a backend exists to mint one.
		uiwindow.DeliverPageTicket(w, a.pageTicket)
		a.mu.Lock()
		a.window = w
		a.flushGeometry = flush
		a.mu.Unlock()

		if chosen == "" {
			return
		}
		// We already know which distro; skip the picker and launch the backend.
		// The launch path takes 5-15s on cold boot, so run it off the event
		// goroutine while the window's /loading state covers the gap. We start
		// it only after the window exists so launchAndShow's SetURL calls land.
		// launchAndShow owns the WebView URL on every exit path: WSL backend on
		// success, /connectivity-error or /picker on failure — the goroutine
		// just logs.
		go func() {
			if err := a.launchAndShow(chosen, transient); err != nil {
				log.Printf("launch backend: %v", err)
			}
		}()
	})

	return a
}

func (a *launcherApp) startNotificationBridge(bs *wsllauncher.Bootstrap, launcher *wsllauncher.Launcher) error {
	client, err := wsllauncher.NewNotificationClient(wsllauncher.NotificationClientConfig{
		WSURL:   fmt.Sprintf("ws://127.0.0.1:%d/ws", bs.Port),
		Token:   bs.Token,
		Present: a.notificationService.present,
		Logf:    log.Printf,
		// The backend stages the new launcher .exe but cannot swap a running
		// Windows executable; this is the callback that does (see update.go).
		HandleUpdateInstall: a.handleUpdateInstall,
		// The backend decides WHEN the renderer should drop its pooled GC
		// pages (frontend input idle + no provider turn); this process owns
		// the WebView2 and is the only one that can fire the GC
		// (webviewtrim.go).
		HandleWebviewTrim: func(reason string) {
			a.mu.Lock()
			w := a.window
			a.mu.Unlock()
			trimRendererMemory(w, reason)
		},
		// The backend owns the SETTING; this process owns the machine's
		// power state. SetThreadExecutionState is a Win32 call the WSL
		// backend cannot make, so keep awake is asserted here
		// (keepawake.go).
		HandleKeepAwake: applyKeepAwakeDirective,
		// The backend decides what the embedded browser pane shows and
		// where it sits; the controllers that draw it must be child windows
		// of THIS process's HWND, driven from its UI thread (browserhost.go).
		HandleBrowserHost: func(directive webview2host.Directive) {
			a.handleBrowserHostDirective(bs, directive)
		},
	})
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.notificationClient = client
	ctx := a.notificationContext
	startDrain := a.notificationActivations.StartIfPending(true)
	a.mu.Unlock()

	go client.Run(ctx)
	if startDrain {
		go a.drainNotificationActivations()
	}
	a.mu.Lock()
	waitDone := a.launcherWait
	a.mu.Unlock()
	if waitDone != nil {
		go func() {
			<-waitDone.done
			err := waitDone.error()
			unexpectedExit := ctx.Err() == nil
			a.notificationCancel()
			if err != nil && unexpectedExit {
				log.Printf("notifications: WSL backend exited; stopping launcher bridge: %v", err)
			}
		}()
	}
	return nil
}

func (a *launcherApp) queueNotificationActivation(target notify.Target) {
	if window := a.win(); window != nil {
		window.Show()
		window.Restore()
		window.Focus()
	}
	a.mu.Lock()
	dropped, startDrain := a.notificationActivations.Push(target, a.notificationClient != nil)
	a.mu.Unlock()
	if dropped != nil {
		log.Printf("notifications: pending activation queue full; dropped oldest target kind=%s", dropped.Kind)
	}
	if startDrain {
		go a.drainNotificationActivations()
	}
}

func (a *launcherApp) drainNotificationActivations() {
	for {
		a.mu.Lock()
		client := a.notificationClient
		if client == nil {
			a.notificationActivations.Stop()
			a.mu.Unlock()
			return
		}
		target, ok := a.notificationActivations.Next()
		if !ok {
			a.mu.Unlock()
			return
		}
		ctx := a.notificationContext
		a.mu.Unlock()

		err := client.Activate(ctx, target)
		if ctx.Err() != nil {
			a.mu.Lock()
			a.notificationActivations.Stop()
			a.mu.Unlock()
			return
		}
		// Once an RPC write is attempted, a disconnect is ambiguous: the
		// backend may have emitted activation before its response was lost.
		// Do not retry without an idempotency key; one click must emit at most
		// one frontend event.
		if err != nil {
			log.Printf("notifications: post activation to backend: %v", err)
		}
	}
}

func wslSingleInstanceOptions(window func() *application.WebviewWindow) *application.SingleInstanceOptions {
	return &application.SingleInstanceOptions{
		UniqueID: appidentity.SingleInstanceID("wsl", wslSingleInstanceMode()),
		OnSecondInstanceLaunch: func(_ application.SecondInstanceData) {
			if w := window(); w != nil {
				w.Show()
				w.Restore()
				w.Focus()
			}
		},
	}
}

// wslSingleInstanceMode is the folded (build stamp + runtime profile)
// mode every per-instance name is derived from. Named for its oldest
// caller; it is the same string that picks the title, WebView2 profile,
// CDP port, log file, and window-state file.
func wslSingleInstanceMode() string {
	return launcherRuntimeMode()
}

// browserArgs returns the Chromium command-line flags forwarded to
// the embedded WebView2 via Wails' AdditionalBrowserArgs.
//
// Together with browserDisabledFeatures, the unconditional set turns
// off browser-grade subsystems that don't apply to a desktop shell
// pointing at a single trusted localhost origin: telemetry, sync,
// translation, autofill, casting, phishing detection, ping beacons,
// BFCache, and prerendering. Each is pure overhead; none affect
// rendering perf or correctness.
//
// base::Feature toggles do NOT belong here. Wails builds its own
// --disable-features switch (seeded with msSmartScreenProtection) and
// appends AdditionalBrowserArgs after it; Chromium keeps only the last
// occurrence of a duplicated switch, so a raw --disable-features here
// silently clobbered Wails' list (msSmartScreenProtection was being
// re-enabled) — and anything Wails seeds would clobber ours if the
// order ever flipped. Feature toggles go through
// browserDisabledFeatures, which Wails merges into its single switch.
//
// 3D APIs are deliberately left ENABLED. The terminal's xterm renderer
// loads the WebGL addon to draw box-drawing and block/quadrant glyphs
// (U+2500–259F — the half-blocks and quadrants TUI sprite art like
// Claude Code's startup banner is built from) through xterm's
// pixel-perfect custom-glyph atlas. That atlas only runs on the
// canvas/WebGL renderers; the DOM fallback defers those glyphs to the
// system font, which tiles them with visible seams. An earlier
// `--disable-3d-apis` here forced the DOM fallback (WebGL2 context
// creation throws when 3D APIs are off), so block-art rendered
// fragmented — keeping WebGL available is the fix. That flag is
// all-or-nothing: it gates WebGL and WebGPU together, so leaving it off
// also re-enables WebGPU, which the SPA never uses. Exposing the unused
// API is acceptable here because the WebView2 only ever renders the
// bundled SPA over loopback — no untrusted origin reaches the GPU
// stack. Revisit only if a real memory measurement justifies switching
// the terminal to the (CPU) canvas renderer instead.
//
// mode adds the loopback CDP attach point so a developer can talk
// Chrome DevTools / wsjson to the WebView2 from inside WSL, on a
// per-mode port (appidentity.DevToolsPort, distinct for every diagnostic
// profile so all can be attached at once). The protocol is unauthenticated, so
// production gets no port at all.
//
// Memory experiments tried and pulled back: --single-process (~290 MB
// savings, but couples all rendering work onto one thread pool and
// removes the renderer-crash recovery boundary), --enable-low-end-
// device-mode (smaller raster workers + image caches; perceptible
// decode-time regression on strong machines), and
// --js-flags=--max-old-space-size=128 (V8 cap; combined with
// single-process turns any future big-memory feature — large diffs,
// terminal log dumps — into a whole-window crash). Revisit if memory
// becomes a real constraint.
func browserArgs(mode string) []string {
	args := []string{
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-default-apps",
		"--disable-sync",
		"--disable-domain-reliability",
		"--disable-client-side-phishing-detection",
		"--no-pings",
		"--no-experiments",
		"--no-default-browser-check",
		// Every byte this webview loads comes from the loopback backend
		// (or the embedded picker page) — the HTTP disk cache can never
		// win anything over a localhost fetch, but Chromium still sizes
		// an index and in-memory bookkeeping for it in the network
		// service. 1 MiB is the practical floor (0 means "default").
		"--disk-cache-size=1048576",
	}
	if port := appidentity.DevToolsPort(mode); port > 0 {
		args = append(args,
			fmt.Sprintf("--remote-debugging-port=%d", port),
			"--remote-debugging-address=127.0.0.1",
		)
	}
	// Opt-in Chromium logging, written to
	// <WebviewUserDataPath>\EBWebView\chrome_debug.log (rotated per session
	// by rotateChromeDebugLog): GPU/compositor errors, process deaths, and
	// renderer CONSOLE(n) lines — this log is what root-caused the mixed-DPI
	// GPU-kill crash (wails #5732). NOT on by default because WebView2 opens
	// a visible console window whenever Chromium logging is enabled — even
	// for file-only destinations like --enable-logging=file
	// (WebView2Feedback #3192, tracked priority-low with no workaround) —
	// and closing that console CTRL_CLOSE-kills the whole app. Available in
	// prod too: it is explicit opt-in, and field debugging is exactly when
	// it is needed.
	if os.Getenv(webviewLogEnv) != "" {
		args = append(args, "--enable-logging", "--v=1")
	}
	// Opt-in software rendering: removes the webview as a GPU client
	// entirely (raster + compositing on CPU). Diagnostic for the
	// 2026-07-04 desktop-stutter investigation — system-wide present
	// stalls correlated with this webview's GPU load transitions;
	// running a session without GPU work discriminates app-caused from
	// environmental. Expect visibly degraded scrolling/streaming
	// smoothness while enabled.
	if os.Getenv(webviewSoftwareEnv) != "" {
		args = append(args, "--disable-gpu")
	}
	// Raw switch pass-through for experiments (see webviewExtraArgsEnv).
	// Appended last so an experiment can override an unconditional arg —
	// Chromium keeps the final occurrence of a duplicated switch. A bare
	// --disable-features would still clobber Wails' merged feature list
	// (see the function comment); feature toggles stay out of this knob.
	args = append(args, strings.Fields(os.Getenv(webviewExtraArgsEnv))...)
	return args
}

// webviewBrowserOptions is the one construction path for process-wide WebView2
// configuration. Keeping the final Wails shape pure lets tests inspect every
// mode, including EnabledFeatures, instead of proving only one input slice.
//
// EnabledFeatures deliberately stays empty. The retired pair
// --disable-lcd-text + PreferNonCompositedScrolling first disabled Blink's
// LCD-text guard against eager scroller promotion, then tried to restore the
// old placement policy with an internal feature. Without the companion every
// scroller became a content-sized composited layer: renderer cc/tile_memory
// measured 165.5MB versus an 89.9MB same-day baseline. Chromium defaults now
// own both text antialiasing and scroller placement; neither half belongs here.
func webviewBrowserOptions(mode, userDataDir, forensicsDir string) application.WindowsOptions {
	return application.WindowsOptions{
		AdditionalBrowserArgs: browserArgs(mode),
		DisabledFeatures:      browserDisabledFeatures(),
		// Stable per-mode WebView2 profile. Without this the profile path
		// defaults to %APPDATA%\<exe name>, and dev builds carry a unique
		// timestamped exe name. A stable path also pins WebView2 diagnostics.
		// Empty (unresolvable %APPDATA%) intentionally takes the Wails default.
		WebviewUserDataPath: userDataDir,
		// Renderer minidumps and breadcrumbs. Empty disables capture.
		RenderForensicsDir: forensicsDir,
	}
}

// browserDisabledFeatures returns the Chromium base::Feature names
// switched off in the WebView2, shipped via Wails'
// WindowsOptions.DisabledFeatures so they merge with Wails' own
// defaults into one --disable-features switch (see browserArgs for why
// a raw flag is unsafe).
func browserDisabledFeatures() []string {
	return []string{
		"Translate",
		"AutofillServerCommunication",
		"MediaRouter",
		"DialMediaRouteProvider",
		"OptimizationHints",
		"IsolateOrigins",
		"site-per-process",
		"BackForwardCache",
		"Prerender2",
		// Chromium's two-finger-trackpad back/forward gesture. Wails
		// already calls PutIsSwipeNavigationEnabled(false), but WebView2
		// ≥ 1.0.2151.40 stopped routing that setting to this
		// Chromium-side touchpad path (WebView2Feedback #4502, open
		// regression) — a horizontal trackpad swipe chaining to the root
		// scroller navigated the window back to the boot-time picker
		// page. browserHistoryGuard.ts can't catch it either: the
		// gesture is handled in the compositor and never surfaces as a
		// cancelable DOM event. Disabling the feature kills it at the
		// source; frontend/src/app.css carries an overscroll-behavior-x
		// backstop in case a future runtime renames the feature.
		"OverscrollHistoryNavigation",
	}
}

// rotateChromeDebugLog renames the previous browser session's
// EBWebView\chrome_debug.log (written when Chromium logging is opted in
// via webviewLogEnv) to chrome_debug.previous.log. Chromium truncates the
// live file at every browser startup, and the session worth autopsying is by
// definition the one that just died — without this rotation, relaunching
// after a blank-window event destroys the only record of why the previous
// browser process shut down. Must run before the WebView2 environment is
// created (i.e. before the window exists). Best-effort: no file or a failed
// rename only costs the post-mortem, never the launch.
func rotateChromeDebugLog(dataDir string) {
	if dataDir == "" {
		return
	}
	src := filepath.Join(dataDir, "EBWebView", "chrome_debug.log")
	if _, err := os.Stat(src); err != nil {
		return
	}
	dst := filepath.Join(dataDir, "EBWebView", "chrome_debug.previous.log")
	if err := os.Rename(src, dst); err != nil {
		log.Printf("launcher: rotate chrome_debug.log: %v", err)
	}
}

// wailsLogLevel picks how much of Wails' internal slog reaches launcher.log:
// debug in the isolated profile instances, info in dev (WebView2 recovery
// narration, window lifecycle), warn+ in production so user logs only carry
// actionable problems.
//
// Both isolated profiles take debug because they are where a render hang
// is reproduced, and half the watchdog narrative — "render watchdog
// armed", "standing down", "render recovery re-navigating" — is logged at
// debug by the pinned wails fork. Losing those leaves an episode with a
// start line and no story. The cost lands only on their own
// launcher-<profile>.log, never on the developer's.
func wailsLogLevel(mode string) slog.Level {
	switch mode {
	case appidentity.ModeSoak, appidentity.ModeHarness, appidentity.ModePerf:
		return slog.LevelDebug
	case appidentity.ModeDev:
		return slog.LevelInfo
	default:
		return slog.LevelWarn
	}
}

// webviewDataDir returns the stable WebView2 user-data folder for this
// launcher mode, or "" when %APPDATA% is unresolvable (Wails then falls back
// to its default). Dev, prod, and soak use separate profiles so a dev session
// never pollutes the production cache/cookies — and so a soak run's
// localStorage, IndexedDB thread replica, and Crashpad/chrome_debug.log
// evidence are its own — mirroring the single-instance and window-title
// mode split.
func webviewDataDir(mode string) string {
	dir, ok := wsldistro.WSLConfigDir()
	if !ok {
		return ""
	}
	return filepath.Join(dir, appidentity.WebviewProfileDir(mode))
}

// renderForensicsDir is where the wails fork's render watchdog drops a
// renderer minidump + breadcrumb the moment it declares a hang — BEFORE
// recovery reaps the wedged process tree, which is the only instant the
// evidence exists (incident 2026-08-18: third renderer-hang episode with
// zero artifact; Crashpad writes nothing for a hang). Always on, in every
// mode: a production user's hang report is only fixable if the dump was
// already taken when they hit it. The fork caps the directory at three
// dumps, so it never needs tending. Empty (unresolvable %APPDATA%)
// disables capture rather than failing the launch, matching
// webviewDataDir.
func renderForensicsDir(mode string) string {
	dir, ok := wsldistro.WSLConfigDir()
	if !ok {
		return ""
	}
	return filepath.Join(dir, appidentity.RenderForensicsDir(mode))
}

// run drives the Wails app loop. Errors are logged but don't take
// down the launcher silently — the user sees the WebView regardless.
func (a *launcherApp) run() {
	if err := a.wails.Run(); err != nil {
		log.Printf("wails run: %v", err)
	}
	// Defensive cleanup: if the user closes the window before the
	// backend finishes booting, OnShutdown still fires, but we run
	// Stop again here as a belt-and-braces. Stop is idempotent.
	a.mu.Lock()
	l := a.launcher
	bs := a.backendBootstrap
	a.mu.Unlock()
	if l != nil {
		if err := a.stopLaunchedBackend(l, bs); err != nil {
			log.Printf("launcher: final backend teardown: %v", err)
		}
	}
}

func logBootPhase(phase string, started time.Time) {
	log.Printf("boot: phase=%s duration=%s", phase, time.Since(started).Round(time.Millisecond))
}
