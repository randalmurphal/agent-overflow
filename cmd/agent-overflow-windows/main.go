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
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

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

// shutdownTimeout caps how long the launcher waits for the WSL-side
// backend to drain before tearing the Job Object down.
const shutdownTimeout = 5 * time.Second

// bootstrapProbeTimeout caps the connectivity probe that validates the
// WSL-side bootstrap is actually reachable from the Windows host.
// Generous enough that a normal cold boot doesn't false-positive, short
// enough that a localhostForwarding-disabled distro surfaces an error
// quickly. The Launch step has already waited for the bootstrap line,
// so by the time we probe the backend has bound its listener — failure
// here points at the Windows ⇄ WSL2 networking path, not the backend.
const bootstrapProbeTimeout = 5 * time.Second

func main() {
	logFile, err := openLog()
	if err == nil {
		log.SetOutput(io.MultiWriter(os.Stderr, logFile))
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

	distros, err := wsllauncher.ListDistros(context.Background())
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

	app := buildApp(distros, initialURL)

	if chosen != "" {
		// We already know which distro; skip the picker and go
		// straight to launching the backend. The launch path takes
		// 5-15 seconds on cold boot, so we run it on a goroutine and
		// the window's /loading state covers the gap.
		//
		// On failure we route the WebView back to /picker so the user
		// can choose a different distro; pairs with the
		// non-persist-on-failure rule in launchAndShow.
		go func() {
			if err := app.launchAndShow(chosen, transient); err != nil {
				log.Printf("launch backend: %v", err)
				app.window.SetURL("/picker")
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
	rule := wsldistro.AppDataEnv + "/p"
	existing := os.Getenv("WSLENV")
	merged := rule
	if existing != "" {
		merged = rule + ":" + existing
	}
	if err := os.Setenv("WSLENV", merged); err != nil {
		log.Printf("set WSLENV: %v", err)
	}
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
	if err := a.launchAndShow(name, false); err != nil {
		// Pre-launch failure: route the WebView back to the picker so
		// the user can pick a different distro or surface the error.
		// We intentionally do not persist cfg.Distro here.
		a.window.SetURL("/picker")
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

	if err := a.ensurePayloadInstalled(ctx, distro); err != nil {
		return fmt.Errorf("install payload: %w", err)
	}

	binPath, err := wslHomePath(ctx, distro)
	if err != nil {
		return fmt.Errorf("resolve WSL home: %w", err)
	}

	l, bs, err := wsllauncher.Launch(ctx, wsllauncher.LaunchOptions{
		Distro:     distro,
		BinaryPath: binPath,
	})
	if err != nil {
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
	if err := probeBootstrap(bs.Port, bs.Token); err != nil {
		log.Printf("connectivity probe failed: %v", err)
		a.window.SetURL("/connectivity-error")
		return fmt.Errorf("backend booted but unreachable from Windows: %w", err)
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

	url := fmt.Sprintf("http://localhost:%d/?t=%s", bs.Port, bs.Token)
	log.Printf("backend ready at %s", url)
	a.window.SetURL(url)
	return nil
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
	url := fmt.Sprintf("http://localhost:%d/bootstrap.json?t=%s", port, token)
	// Errors include the host:path but redact the token. The token is
	// the launch credential — leaking it through a log line that gets
	// surfaced anywhere (launcher.log, bug-report scrape, screenshot)
	// is a credential leak. The host:port is what an operator needs to
	// debug "is the WSL backend actually listening?".
	redacted := fmt.Sprintf("http://localhost:%d/bootstrap.json", port)
	client := &http.Client{Timeout: bootstrapProbeTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("GET %s: %w", redacted, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("GET %s: status %d", redacted, resp.StatusCode)
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
func buildApp(distros []wsllauncher.Distro, initialURL string) *launcherApp {
	a := &launcherApp{distros: distros}

	app := application.New(application.Options{
		Name: "Agent Overflow",
		Services: []application.Service{
			application.NewService(a),
		},
		Assets: application.AssetOptions{
			Handler: pickerAssetHandler(distros),
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
	})

	return a
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
