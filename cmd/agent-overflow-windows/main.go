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
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

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

	cfg, err := loadConfig()
	if err != nil {
		log.Printf("warning: load config: %v", err)
	}

	distros, err := wsllauncher.ListDistros(context.Background())
	if err != nil {
		log.Printf("list distros: %v", err)
	}

	// If we have a known-good distro from a previous launch, verify
	// it still exists. If it doesn't, fall through to the picker.
	chosen := ""
	if cfg != nil && cfg.Distro != "" {
		for _, d := range distros {
			if d.Name == cfg.Distro {
				chosen = d.Name
				break
			}
		}
	}

	// initialURL controls the page the WebView2 shows during the
	// brief gap between window creation and the WSL backend coming
	// online. With a saved distro we land on /loading (no actionable
	// picker rows) and SetURL flips us over once Launch returns;
	// without one, we land on /picker so the user can choose.
	initialURL := "/picker"
	if chosen != "" {
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
			if err := app.launchAndShow(chosen); err != nil {
				log.Printf("launch backend: %v", err)
				app.window.SetURL("/picker")
			}
		}()
	}

	app.run()
}

// openLog opens %APPDATA%\agent-overflow\launcher.log for append. The
// file is best-effort; if AppData isn't writable we fall back to
// stderr only. Logging is essential here because the Windows binary
// has no console UI for surfacing errors before the WebView opens.
func openLog() (*os.File, error) {
	dir, err := configDir()
	if err != nil {
		return nil, err
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

	if err := a.launchAndShow(name); err != nil {
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
// Outer timeout removed — cold WSL2 boot can exceed any single budget
// (the WSL VM itself can take 20+ seconds, then 9P startup, then SQLite
// migrations). Inner phase timeouts (install, bootstrap) bound the
// user-visible wait per step. If the user wants to abort they close
// the window; the Wails OnShutdown hook tears the WSL child down.
func (a *launcherApp) launchAndShow(distro string) error {
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
	if err := a.persistSuccessfulLaunch(distro); err != nil {
		log.Printf("save config after launch: %v", err)
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
func (a *launcherApp) persistSuccessfulLaunch(distro string) error {
	cfg, _ := loadConfig()
	if cfg == nil {
		cfg = &config{}
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
