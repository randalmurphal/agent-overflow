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
//     --listen 127.0.0.1:0 --print-url-fd 3 and pins the child to a
//     Win32 Job Object so closing this process kills the WSL-side one.
//  5. Reads the bootstrap line { port, token } back from the child.
//  6. Points the Wails WebView2 window at
//     http://localhost:<port>/?t=<token>.
//
// WSL2 forwards 127.0.0.1:<port> from inside the distro to the Windows
// host's localhost via vEthernet. localhostForwarding=true must be
// set in the user's /etc/wsl.conf or %USERPROFILE%/.wslconfig — it's
// the WSL2 default but a user can disable it. The picker shows a
// clear error if the connection back to the WSL backend fails.
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
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/appidentity"
	"agent-overflow/internal/uikeys"
	"agent-overflow/internal/wsldistro"
	"agent-overflow/internal/wsllauncher"

	"github.com/wailsapp/wails/v3/pkg/application"
)

//go:embed picker.html
var pickerHTML string

//go:embed payload/agent-overflow-linux
var linuxPayload []byte

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

// shutdownTimeout caps how long the launcher waits for the WSL-side
// backend to drain before tearing the Job Object down.
const shutdownTimeout = 5 * time.Second

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
// without flapping. The probe now covers both localhost-forwarding
// setup and the backend's readiness-gated ServiceStartup window, so
// it uses the same 30 s shape as forge's daemon-readiness loop.
const bootstrapProbeDeadline = 30 * time.Second

// bootstrapProbePollInterval is the gap between failed-probe retries.
// 250 ms matches forge/apps/desktop/src/main.ts's connectViaWsl loop
// — fast enough that the WSL2 forwarder install almost never costs us
// more than one extra hop, slow enough that we don't melt the CPU when
// the backend genuinely never comes up.
const bootstrapProbePollInterval = 250 * time.Millisecond

func main() {
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

	app := buildApp(distros, initialURL, transient)
	logBootPhase("launcher.before_run", bootStarted)

	if chosen != "" {
		// We already know which distro; skip the picker and go
		// straight to launching the backend. The launch path takes
		// 5-15 seconds on cold boot, so we run it on a goroutine and
		// the window's /loading state covers the gap.
		//
		// launchAndShow owns the WebView URL on every exit path: on
		// success it points at the WSL backend, on connectivity
		// failure at /connectivity-error (with the actionable
		// .wslconfig fix), on any other failure at /picker. Don't
		// override here — the goroutine just logs.
		go func() {
			if err := app.launchAndShow(chosen, transient); err != nil {
				log.Printf("launch backend: %v", err)
			}
		}()
	}

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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(
		filepath.Join(dir, "launcher.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY,
		0o644,
	)
}

// launcherApp is the Wails service exposing PickDistro to the picker
// HTML. It also owns the Launcher / window references so we can flip
// the window URL once the backend boots.
type launcherApp struct {
	wails   *application.App
	window  *application.WebviewWindow
	distros []wsllauncher.Distro

	mu       sync.Mutex
	launcher *wsllauncher.Launcher

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

	started := time.Now()
	defer logBootPhase("launcher.launch_and_show.total", started)

	phaseStarted := time.Now()
	binPath, err := a.ensurePayloadInstalled(ctx, distro)
	logBootPhase("launcher.ensure_payload", phaseStarted)
	if err != nil {
		a.window.SetURL("/picker")
		return fmt.Errorf("install payload: %w", err)
	}

	phaseStarted = time.Now()
	l, bs, err := wsllauncher.Launch(ctx, wsllauncher.LaunchOptions{
		Distro:     distro,
		BinaryPath: binPath,
	})
	logBootPhase("launcher.wsl_launch", phaseStarted)
	if err != nil {
		a.window.SetURL("/picker")
		return fmt.Errorf("launch backend: %w", err)
	}

	a.mu.Lock()
	a.launcher = l
	a.mu.Unlock()

	// Connectivity probe: confirm the Windows host can reach the WSL
	// backend's listener over localhost before flipping the WebView
	// over. WSL2 forwards 127.0.0.1:<port> from inside the distro to
	// the Windows host's localhost via vEthernet, but only when
	// localhostForwarding=true. A user with localhostForwarding=false
	// in /etc/wsl.conf or %USERPROFILE%/.wslconfig sees the WSL backend
	// boot fine (it's serving inside the distro) but the Windows
	// WebView2 silently fails to connect. Without this probe the
	// WebView would just blank-screen with no actionable feedback.
	phaseStarted = time.Now()
	if err := probeBootstrap(bs.Port, bs.Token); err != nil {
		logBootPhase("launcher.probe_bootstrap", phaseStarted)
		log.Printf("connectivity probe failed: %v", err)
		var httpErr bootstrapHTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusInternalServerError {
			a.window.SetURL("/startup-error")
			return fmt.Errorf("backend failed during startup: %w", err)
		}
		a.window.SetURL("/connectivity-error")
		return fmt.Errorf("backend booted but unreachable from Windows: %w", err)
	}
	logBootPhase("launcher.probe_bootstrap", phaseStarted)

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
	url := fmt.Sprintf("http://127.0.0.1:%d/?t=%s", bs.Port, bs.Token)
	// Same redaction shape probeBootstrap uses (line ~528). The token is
	// the per-launch credential; leaking it through launcher.log (which
	// persists in %APPDATA% across runs and is a likely artifact in user
	// bug reports) would let an attacker with file-system read replay
	// the session for as long as the backend is up.
	log.Printf("backend ready at http://127.0.0.1:%d/ (token=%d bytes)", bs.Port, len(bs.Token))
	// Publish the URL before SetURL so a reload that lands between
	// SetURL and the SPA's first bootstrap fetch still finds the token.
	a.backendURL.Store(&url)
	phaseStarted = time.Now()
	a.window.SetURL(url)
	logBootPhase("launcher.window_set_url", phaseStarted)
	return nil
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
	// the forwarder is about to catch up. forge's connectViaWsl
	// (apps/desktop/src/main.ts) handles this with a 250 ms / 30 s
	// loop; we use the same shape because the backend may now publish
	// its bootstrap port before ServiceStartup has released readiness.
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
	for {
		attempt++
		resp, err := client.Get(url)
		if err == nil {
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
// devMode adds the remote-debugging port (so a developer can attach
// Chrome DevTools / talk CDP from inside WSL) plus the staged
// memory-reduction experiments described in browserArgs.
func buildApp(distros []wsllauncher.Distro, initialURL string, devMode bool) *launcherApp {
	a := &launcherApp{distros: distros}

	app := application.New(application.Options{
		Name:           "Agent Overflow",
		SingleInstance: wslSingleInstanceOptions(func() *application.WebviewWindow { return a.window }),
		Services: []application.Service{
			application.NewService(a),
		},
		Assets: application.AssetOptions{
			Handler: pickerAssetHandler(distros),
		},
		Windows: application.WindowsOptions{
			AdditionalBrowserArgs: browserArgs(devMode),
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
			a.mu.Lock()
			l := a.launcher
			a.mu.Unlock()
			if l != nil {
				_ = l.Stop()
			}
		},
	})
	a.wails = app

	a.window = app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "Agent Overflow",
		Width:            1280,
		Height:           800,
		MinWidth:         800,
		MinHeight:        600,
		BackgroundColour: application.NewRGBA(22, 22, 30, 255),
		// Picker / loading first; once Launch returns we SetURL to
		// the WSL backend's localhost port. We can't use the WSL URL
		// up front because we don't know the port until after Launch.
		URL: initialURL,
		// Without this the WebView2 swallows zoom/reload/fullscreen
		// and there's no menu bar to fall back on. `make dev-wsl`
		// makes this the only window the user touches, so the
		// missing bindings were the most user-visible symptom.
		// BrowserWithReload reads a.backendURL on each reload so
		// Ctrl+R re-navigates with the bootstrap token after the SPA
		// scrubs it from window.location — see uikeys.BrowserWithReload.
		KeyBindings: uikeys.BrowserWithReload(a.currentBackendURL),
	})

	return a
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
// The unconditional set turns off browser-grade subsystems that don't
// apply to a desktop shell pointing at a single trusted localhost
// origin: telemetry, sync, translation, autofill, casting, phishing
// detection, ping beacons, BFCache (we never navigate), prerendering,
// and 3D APIs (no WebGL/WebGPU in the app). Each is pure overhead;
// none affect rendering perf or correctness.
//
// devMode adds the loopback CDP attach point so a developer can talk
// Chrome DevTools / wsjson to the WebView2 from inside WSL. The
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
func browserArgs(devMode bool) []string {
	args := []string{
		"--disable-features=Translate,AutofillServerCommunication,MediaRouter,DialMediaRouteProvider,OptimizationHints,IsolateOrigins,site-per-process,BackForwardCache,Prerender2",
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-default-apps",
		"--disable-sync",
		"--disable-domain-reliability",
		"--disable-client-side-phishing-detection",
		"--disable-3d-apis",
		"--no-pings",
		"--no-experiments",
		"--no-default-browser-check",
	}
	if devMode {
		args = append(args,
			"--remote-debugging-port=9223",
			"--remote-debugging-address=127.0.0.1",
		)
	}
	return args
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
