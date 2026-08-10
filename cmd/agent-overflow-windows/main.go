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
//  6. Points the Wails WebView2 window at
//     http://localhost:<port>/?t=<token>.
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
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/appidentity"
	"agent-overflow/internal/diagenv"
	"agent-overflow/internal/notify"
	"agent-overflow/internal/observability/pprofserve"
	"agent-overflow/internal/uikeys"
	"agent-overflow/internal/uiwindow"
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
	logFile, err := openLog()
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

	flags, err := parseLauncherFlags(os.Args[1:])
	if err != nil {
		// flag.ErrHelp means the user passed -h/-help and the flag
		// package already wrote the usage to stderr. Exit cleanly so
		// the launcher doesn't log a phantom "flags: flag: help requested"
		// to launcher.log on every help invocation.
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		log.Fatalf("flags: %v", err)
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

// openLog opens %APPDATA%\agent-overflow\launcher.log for append. The
// file is best-effort; if AppData isn't writable we fall back to
// stderr only. Logging is essential here because the Windows binary
// has no console UI for surfacing errors before the WebView opens.
func openLog() (*os.File, error) {
	dir, ok := wsldistro.WSLConfigDir()
	if !ok {
		return nil, fmt.Errorf("openLog: AppData unresolvable")
	}
	if err := os.MkdirAll(dir, launcherPrivateDirPerm); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, launcherPrivateDirPerm); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(
		filepath.Join(dir, "launcher.log"),
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

	mu       sync.Mutex
	launcher *wsllauncher.Launcher
	// notificationService presents Windows toasts. notificationClient is the
	// narrow WS bridge back to the WSL backend; notificationCancel stops its
	// reconnect loop on backend exit or launcher shutdown. All are guarded by
	// mu because activation callbacks run independently of launchAndShow.
	notificationService     *launcherNotificationService
	notificationClient      *wsllauncher.NotificationClient
	notificationContext     context.Context
	notificationCancel      context.CancelFunc
	notificationActivations *wsllauncher.NotificationActivationQueue

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

	// backendURL holds the full http://127.0.0.1:<port>/?t=<token> URL
	// once launchAndShow points the WebView at the WSL backend. Read by
	// the reload keybinding (uikeys.BrowserWithReload) so Ctrl+R
	// re-navigates with the token instead of reloading the SPA's
	// scrubbed URL. atomic.Pointer because the writer is launchAndShow
	// (goroutine) and the reader is the Wails event loop.
	backendURL atomic.Pointer[string]
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
			w.SetURL("/picker")
			return err
		case errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusInternalServerError:
			w.SetURL("/startup-error")
			return fmt.Errorf("backend failed during startup: %w", err)
		default:
			w.SetURL("/connectivity-error")
			return fmt.Errorf("backend booted but unreachable from Windows: %w", err)
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

	// 127.0.0.1, not "localhost": Windows resolves "localhost" to both
	// ::1 and 127.0.0.1, and which the OS hands the dialer first is
	// non-deterministic. WSL2's localhostForwarding only proxies IPv4,
	// so a ::1 attempt hits Windows-loopback directly and is refused.
	// Hard-coding the IPv4 literal makes the WebView navigation
	// race-free against the OS resolver and the dual-stack hosts file.
	// Durable UI-state client identity minted by the WSL backend;
	// threading it onto the page URL keeps the frontend's per-client
	// ui_state bucket stable across the per-launch origin change.
	// Escaped before the `url` local below shadows the net/url package.
	cidParam := ""
	if bs.ClientID != "" {
		cidParam = "&cid=" + url.QueryEscape(bs.ClientID)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/?t=%s%s", bs.Port, bs.Token, cidParam)
	// Same redaction shape probeBootstrap uses. The token is
	// the per-launch credential; leaking it through launcher.log (which
	// persists in %APPDATA% across runs and is a likely artifact in user
	// bug reports) would let an attacker with file-system read replay
	// the session for as long as the backend is up.
	log.Printf("backend ready at http://127.0.0.1:%d/ (token=%d bytes)", bs.Port, len(bs.Token))
	// Publish the URL before SetURL so a reload that lands between
	// SetURL and the SPA's first bootstrap fetch still finds the token.
	a.backendURL.Store(&url)
	phaseStarted = time.Now()
	w.SetURL(url)
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
		return nil, nil, probeErr
	}

	log.Printf("launcher: port %d is unreachable from Windows; relaunching the backend on a fresh transport port", bs.Port)
	// Retire the unreachable backend first. It is healthy inside the
	// distro and holds the app's SQLite store; two backends on one data
	// dir would fight over the writer.
	a.stopLaunchedBackend(l)

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

// launchBackend spawns the WSL child and records it as the live
// launcher. extraArgs are appended to the backend's own argv.
func (a *launcherApp) launchBackend(ctx context.Context, distro, binPath string, extraArgs []string) (*wsllauncher.Launcher, *wsllauncher.Bootstrap, error) {
	phaseStarted := time.Now()
	l, bs, err := wsllauncher.Launch(ctx, wsllauncher.LaunchOptions{
		Distro:         distro,
		BinaryPath:     binPath,
		ExtraArgs:      extraArgs,
		PassthroughEnv: diagenv.Passthrough(),
	})
	logBootPhase("launcher.wsl_launch", phaseStarted)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", errLaunchFailed, err)
	}

	a.mu.Lock()
	a.launcher = l
	a.mu.Unlock()
	return l, bs, nil
}

// stopLaunchedBackend tears down a backend we are abandoning and clears
// it from the launcher state, so OnShutdown can't later Stop a child
// that a newer launch has already replaced.
func (a *launcherApp) stopLaunchedBackend(l *wsllauncher.Launcher) {
	a.mu.Lock()
	if a.launcher == l {
		a.launcher = nil
	}
	a.mu.Unlock()
	if err := l.Stop(); err != nil {
		log.Printf("launcher: stop unreachable backend: %v", err)
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

// currentBackendURL returns the URL the WebView2 is currently pointed
// at (after launchAndShow's SetURL), or "" before the WSL backend has
// finished booting. Used by the reload keybinding so Ctrl+R re-navigates
// with the bootstrap token instead of reloading the SPA's scrubbed URL.
// "" before launch tells uikeys.BrowserWithReload to fall through to
// the default window.Reload() — picker / loading / connectivity-error
// pages are static and reload-safe.
func (a *launcherApp) currentBackendURL() string {
	if p := a.backendURL.Load(); p != nil {
		return *p
	}
	return ""
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
// unreachable class qualifies — a startup failure, a token mismatch, or
// a host-guard rejection all prove the port is reachable, and moving it
// would cost the user their origin-scoped browser state for nothing.
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
	url := fmt.Sprintf("http://127.0.0.1:%d/bootstrap.json?t=%s", port, token)
	// Errors include the host:path but redact the token. The token is
	// the launch credential — leaking it through a log line that gets
	// surfaced anywhere (launcher.log, bug-report scrape, screenshot)
	// is a credential leak. The host:port is what an operator needs to
	// debug "is the WSL backend actually listening?".
	redacted := fmt.Sprintf("http://127.0.0.1:%d/bootstrap.json", port)
	log.Printf("probe: GET %s (token=%d bytes)", redacted, len(token))

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
		resp, err := client.Get(url)
		if err == nil {
			sawHTTPResponse = true
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				if err := validateBootstrapResponse(body, port, token); err != nil {
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
			log.Printf("probe: status=%d host-resp=%q", resp.StatusCode, string(body))
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

func validateBootstrapResponse(body []byte, port int, token string) error {
	var bootstrap struct {
		WSURL string `json:"wsUrl"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &bootstrap); err != nil {
		return fmt.Errorf("decode bootstrap response: %w", err)
	}
	if bootstrap.Token != token {
		return errors.New("bootstrap response token mismatch")
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
	enableDevBrowserArgs := mode == "dev"

	// Preserve the previous browser session's Chromium log before this
	// session's WebView2 environment truncates it — see rotateChromeDebugLog.
	rotateChromeDebugLog(webviewDataDir(mode))

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
		Windows: application.WindowsOptions{
			AdditionalBrowserArgs: browserArgs(enableDevBrowserArgs),
			DisabledFeatures:      browserDisabledFeatures(),
			EnabledFeatures:       browserEnabledFeatures(),
			// Stable per-mode WebView2 profile. Without this the profile
			// path defaults to %APPDATA%\<exe name>, and dev builds carry a
			// unique timestamped exe name — every `make dev-wsl` minted a
			// fresh EBWebView profile dir (cold caches, orphaned junk under
			// Roaming). A stable path also pins where WebView2 diagnostics
			// land (EBWebView\chrome_debug.log, EBWebView\Crashpad\) so the
			// mixed-DPI blank-window investigation has one place to look.
			// Empty string (unresolvable %APPDATA%) falls back to the Wails
			// default rather than failing the launch.
			WebviewUserDataPath: webviewDataDir(mode),
		},
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
			cancelNotifications := a.notificationCancel
			a.mu.Unlock()
			if cancelNotifications != nil {
				cancelNotifications()
			}
			if flush != nil {
				flush()
			}
			if l != nil {
				_ = l.Stop()
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
	opts := application.WebviewWindowOptions{
		Title:            title,
		Width:            1280,
		Height:           800,
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
	go func() {
		err := launcher.Wait()
		unexpectedExit := ctx.Err() == nil
		a.notificationCancel()
		if err != nil && unexpectedExit {
			log.Printf("notifications: WSL backend exited; stopping launcher bridge: %v", err)
		}
	}()
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

func wslSingleInstanceMode() string {
	if launcherMode == "dev" {
		return "dev"
	}
	return "prod"
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
// browserDisabledFeatures / browserEnabledFeatures, which Wails merges
// into its single switch.
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
// enableDevArgs adds the loopback CDP attach point so a developer can
// talk Chrome DevTools / wsjson to the WebView2 from inside WSL. The
// protocol is unauthenticated, so it never ships.
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
func browserArgs(enableDevArgs bool) []string {
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
		// Force grayscale text AA in every layer state. The scroll
		// controller demotes each parked pane's content layer to shed
		// its tile memory (the promotion lease in
		// frontend/src/lib/utils/scroll/index.svelte.ts); without this
		// flag, demoted text picks up ClearType subpixel AA while
		// composited text renders grayscale, so panes visibly snap
		// between the two styles at lease boundaries. With it, chat
		// text is pixel-identical to the permanently-promoted rendering
		// this replaced, and root-layer text (sidebar/topbar) matches
		// instead of being the app's odd subpixel surface. Glyph-edge
		// blending only — text color is untouched. MANDATORY COMPANION:
		// PreferNonCompositedScrolling in browserEnabledFeatures, or
		// every scroller gets eagerly composited (see that comment).
		"--disable-lcd-text",
	}
	if enableDevArgs {
		args = append(args,
			"--remote-debugging-port=9223",
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
	return args
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

// browserEnabledFeatures returns the Chromium base::Feature names
// switched on in the WebView2, shipped via Wails'
// WindowsOptions.EnabledFeatures (same single-switch rule as
// browserDisabledFeatures).
func browserEnabledFeatures() []string {
	return []string{
		// MANDATORY COMPANION to --disable-lcd-text (browserArgs). Blink
		// composites a scroller eagerly whenever doing so cannot hurt
		// LCD text; with LCD text globally off, that guard always
		// passes, so EVERY scroller (each pane timeline, the pane strip)
		// got promoted to composited scrolling with content-sized raster
		// layers — measured 2026-07-21: renderer cc/tile_memory 165.5MB
		// vs the 89.9MB the lease work started from. This feature
		// (verified present in WebView2 150.0.4078.83) restores the
		// prefer-non-composited default so scrollers stay unlayerized;
		// scrolling still runs on the compositor via raster-inducing
		// scroll, and the lease's explicit will-change promotion — which
		// bypasses the preference — keeps actively-scrolling panes
		// composited exactly as designed. If a future WebView2 drops the
		// feature the symptom to watch for is this same eager-compositing
		// regression, visible as full-scroll-height layers in the CDP
		// LayerTree dump for panes whose contentEl carries no
		// will-change.
		"PreferNonCompositedScrolling",
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
// info in dev (WebView2 recovery narration, window lifecycle), warn+ in
// production so user logs only carry actionable problems.
func wailsLogLevel(mode string) slog.Level {
	if mode == "dev" {
		return slog.LevelInfo
	}
	return slog.LevelWarn
}

// webviewDataDir returns the stable WebView2 user-data folder for this
// launcher mode, or "" when %APPDATA% is unresolvable (Wails then falls back
// to its default). Dev and prod use separate profiles so a dev session never
// pollutes the production cache/cookies, mirroring the single-instance and
// window-title mode split.
func webviewDataDir(mode string) string {
	dir, ok := wsldistro.WSLConfigDir()
	if !ok {
		return ""
	}
	if mode == "dev" {
		return filepath.Join(dir, "webview2-dev")
	}
	return filepath.Join(dir, "webview2")
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
	a.mu.Unlock()
	if l != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		go func() {
			_ = l.Stop()
		}()
		select {
		case <-shutCtx.Done():
		case <-time.After(shutdownTimeout):
		}
	}
}

func logBootPhase(phase string, started time.Time) {
	log.Printf("boot: phase=%s duration=%s", phase, time.Since(started).Round(time.Millisecond))
}
