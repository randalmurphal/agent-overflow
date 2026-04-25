package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"agent-overflow/internal/clientmode"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/transport"

	"github.com/wailsapp/wails/v3/pkg/application"
)

//go:embed all:frontend/dist
var assets embed.FS

// transportShutdownTimeout caps how long the post-Run cleanup waits for
// the embedded HTTP+WS server to drain before returning. The Wails
// process is going away regardless, so connections that haven't closed
// in this window are abandoned.
const transportShutdownTimeout = 5 * time.Second

// headlessShutdownTimeout caps the headless path's graceful shutdown.
// Slightly longer than the transport drain because we also wait for
// App.Shutdown to flush stores and subsystems.
const headlessShutdownTimeout = 10 * time.Second

// bootstrapStdoutPrefix is the line prefix the headless backend writes
// to stdout when it can't reach fd 3 (the common case under wsl.exe,
// where Windows pipe handles don't propagate cleanly through the WSL
// boundary). The Windows-side launcher (cmd/agent-overflow-windows)
// scans stdout for this prefix; the WSL2 localhostForwarding default
// then makes the resulting 127.0.0.1:<port> reachable from the Windows
// WebView2.
const bootstrapStdoutPrefix = "__AO_BOOTSTRAP__:"

func main() {
	flags, err := parseFlags(os.Args[1:])
	if err != nil {
		fatalf("%v", err)
	}

	switch {
	case flags.connect != "":
		runClient(flags.connect)
	case flags.headless:
		runHeadless(flags.listenAddr, flags.printURLFD)
	default:
		runDesktop(flags.listenAddr)
	}
}

// cliFlags carries the parsed command-line state. Three modes are
// mutually exclusive: --connect (Phase F remote-client), --print-url-fd
// (Phase D headless), and the default desktop boot. parseFlags
// enforces "at most one of --connect / --print-url-fd" so mode
// selection is unambiguous.
type cliFlags struct {
	listenAddr string
	printURLFD int
	headless   bool
	connect    string
}

// parseFlags pulls the command-line flags. The flag set is independent
// of the Wails CLI's argument parsing — Wails' alpha builds shell out
// to subprocesses with custom flags and we don't want our flags to
// leak into the wails3 dev/build argv. The defaults preserve the
// desktop launch behaviour.
//
// Returns a typed error rather than calling fatalf so the conflict
// branches are unit-testable. main() converts errors into the
// stderr-and-exit shape callers expect.
func parseFlags(args []string) (cliFlags, error) {
	flagSet := flag.NewFlagSet("agent-overflow", flag.ContinueOnError)
	flagSet.SetOutput(os.Stderr)
	listen := flagSet.String("listen", "", "transport bind address (e.g. 127.0.0.1:0). Empty means use the default loopback + ephemeral port.")
	fdFlag := flagSet.String("print-url-fd", "", "run headless and write {port,token} to this file descriptor as JSON. Falls back to a stdout sentinel when the fd isn't open.")
	connect := flagSet.String("connect", "", "Phase F remote client mode: attach the desktop window to a remote backend at ws://host:port/?token=<value>. Skips local transport boot.")
	if err := flagSet.Parse(args); err != nil {
		return cliFlags{}, fmt.Errorf("parse flags: %w", err)
	}

	out := cliFlags{
		listenAddr: *listen,
		connect:    *connect,
	}

	if out.connect != "" && *fdFlag != "" {
		return cliFlags{}, errors.New("cannot combine --connect with --print-url-fd")
	}
	if out.connect != "" && out.listenAddr != "" {
		// --listen configures the *local* transport bind. In --connect
		// mode there is no local transport (we attach to a remote
		// backend instead), so a --listen value would be silently
		// dropped. Reject explicitly so the operator notices the
		// conflict before the desktop window opens against the wrong
		// origin.
		return cliFlags{}, errors.New("cannot combine --connect with --listen")
	}

	if *fdFlag != "" {
		n, err := strconv.Atoi(*fdFlag)
		if err != nil {
			return cliFlags{}, fmt.Errorf("parse --print-url-fd: %w", err)
		}
		out.printURLFD = n
		out.headless = true
	}
	return out, nil
}

// runClient is the Phase F remote-client entry point. Instead of
// booting the local transport (HTTP+WS server, App service registration,
// SQLite, observability, sessions), the desktop binary points the
// Wails webview at a tiny static-asset HTTP server whose index.html
// has window.__AO_BOOTSTRAP__ pre-injected so the SPA's wsClient
// connects to the operator-supplied remote endpoint instead.
//
// Why we still need a loopback HTTP server: the Wails webview only
// loads `http://`/`https://` URLs (or the embedded asset URL with the
// devserver path). Pointing it directly at `ws://...` won't work, and
// the bootstrap-injection step has to happen somewhere. The stub
// server is single-purpose: serve the SPA shell with the bootstrap
// snippet, plus the static assets the shell loads from /assets/.
func runClient(rawURL string) {
	cfg, err := clientmode.ParseConnectURL(rawURL)
	if err != nil {
		fatalf("--connect: %v", err)
	}

	embeddedSPA, err := fs.Sub(assets, "frontend/dist")
	if err != nil {
		fatalf("--connect: locate embedded frontend/dist: %v", err)
	}
	cfg.Assets = embeddedSPA

	stub, err := clientmode.Serve(cfg)
	if err != nil {
		fatalf("--connect: serve stub: %v", err)
	}
	log.Printf("clientmode: stub serving on %s, attaching to %s", stub.Addr(), cfg.WSURL)

	app := application.New(application.Options{
		Name: "Agent Overflow",
		// No services registered: the App receiver in the local binary
		// would only confuse a webview that's about to RPC against a
		// remote backend instead. Wails expects services to be set
		// before window creation; an empty slice is the documented way
		// to opt out.
	})

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "Agent Overflow",
		Width:            1280,
		Height:           800,
		MinWidth:         800,
		MinHeight:        600,
		BackgroundColour: application.NewRGBA(22, 22, 30, 255),
		URL:              stub.AppURL(),
	})

	runErr := app.Run()

	shutCtx, cancel := context.WithTimeout(context.Background(), transportShutdownTimeout)
	defer cancel()
	if err := stub.Shutdown(shutCtx); err != nil {
		log.Printf("clientmode: shutdown: %v", err)
	}
	if runErr != nil {
		fatalf("wails run (client): %v", runErr)
	}
}

// bootTransport is the shared boot path used by runDesktop and
// runHeadless: build the dispatcher, register App methods, wire the
// event bus, build the asset handler, construct + start the transport
// server, and tell App about both the bus and the server. Returns the
// running server so the caller can read its address / token. On any
// failure the function calls fatalf and does not return.
//
// Why this is shared: desktop and headless differ only in the UI shell
// they wrap around the transport — Wails window vs signal-driven loop.
// The transport boot is identical; duplicating it would mean keeping
// two copies in sync as the registration call evolves (allow lists,
// bus capacity, asset handler choices).
//
// loadPersistedBindAll is the desktop-path-only escape hatch for the
// stored Phase E LAN-bind preference. Headless boots only honor the
// explicit --listen flag (the Windows launcher always passes one), so
// it passes false here. Pulling the load out of the helper keeps the
// boot graph linear: this function makes deterministic decisions from
// its arguments, never reads disk on its own.
func bootTransport(appService *App, listenAddr string, loadPersistedBindAll bool) *transport.Server {
	dispatcher := transport.NewDispatcher()
	methods, err := dispatcher.Register(appService, transport.RegisterOptions{
		Package:   "main",
		TypeName:  "App",
		AllowList: transport.NewMethodAllowList(),
	})
	if err != nil {
		fatalf("transport: register App methods: %v", err)
	}
	log.Printf("transport: registered %d methods", len(methods))

	bus := transport.NewEventBus(0)
	appService.SetEventBus(bus)

	assetHandler, err := buildAssetHandler(assets)
	if err != nil {
		fatalf("transport: build asset handler: %v", err)
	}

	cfg := transport.Config{
		Dispatcher:   dispatcher,
		EventBus:     bus,
		AssetHandler: assetHandler,
	}
	if listenAddr != "" {
		host, port := splitListenAddr(listenAddr)
		cfg.BindAddr = host
		cfg.Port = port
	} else if loadPersistedBindAll {
		// Honor the persisted Phase E LAN-bind preference at boot so a
		// user who toggled "Allow remote access" in a previous session
		// doesn't see the server snap back to loopback after restart.
		// CLI --listen still wins — operator override beats stored prefs.
		if persisted := loadPersistedNetworkSettings(); persisted.BindAll {
			cfg.BindAddr = "0.0.0.0"
		}
	}

	srv, err := transport.New(cfg)
	if err != nil {
		fatalf("transport: construct server: %v", err)
	}
	appService.SetTransportServer(srv)
	if err := srv.Start(); err != nil {
		fatalf("transport: start server: %v", err)
	}
	log.Printf("transport: serving on %s", srv.Addr())
	return srv
}

// runDesktop is the original Wails-window entry point used on
// macOS/Linux/Windows native builds. The Windows binary that proxies
// into WSL is a separate cmd/ — see cmd/agent-overflow-windows.
func runDesktop(listenAddr string) {
	appService := NewApp()
	srv := bootTransport(appService, listenAddr, true)

	// Assert non-empty before constructing the WebviewWindowOptions.
	// Wails maps URL "" back to its built-in scheme, which would expose
	// the in-process IPC surface we deliberately replaced with HTTP+WS.
	// AppURL() returns "" pre-Start or when both the cached and live
	// listener addresses fail to parse — both are pathological boot
	// states that warrant an obvious failure rather than a silent IPC
	// fallthrough or a port-less URL hitting port 80.
	appURL := srv.AppURL()
	if appURL == "" {
		fatalf("transport: AppURL is empty after Start (server addr = %q); refusing to fall through to Wails IPC scheme", srv.Addr())
	}

	app := application.New(application.Options{
		Name: "Agent Overflow",
		Services: []application.Service{
			application.NewService(appService),
		},
	})

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "Agent Overflow",
		Width:            1280,
		Height:           800,
		MinWidth:         800,
		MinHeight:        600,
		BackgroundColour: application.NewRGBA(22, 22, 30, 255),
		URL:              appURL,
	})

	runErr := app.Run()

	shutCtx, cancel := context.WithTimeout(context.Background(), transportShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Printf("transport: shutdown: %v", err)
	}
	if runErr != nil {
		fatalf("wails run: %v", runErr)
	}
}

// runHeadless is the Phase D entry point used by the Windows-side
// launcher. It boots the transport server inside the WSL distro,
// writes a bootstrap line back to the launcher, and runs until SIGTERM.
//
// The headless path deliberately does NOT construct a Wails
// application: there's no display server inside WSL, and even if we
// hid the window the GTK/CGO link would refuse to start under WSLg
// when the Windows-side WebView2 is the actual UI surface. Instead
// we drive the App's lifecycle hooks directly:
//
//   - App.ServiceStartup boots stores, observability, subsystems —
//     identical to the Wails-managed path.
//   - App.Shutdown drains the transport server + flushes subsystems.
//
// Bootstrap delivery: the spec asks for fd 3, but propagating a real
// fd 3 through `wsl.exe` is unreliable across Windows versions. We
// try fd 3 first (so a unit test or Linux-side launcher can read the
// "clean" channel) and fall back to a stdout sentinel when fd 3 isn't
// open. The Windows-side launcher only knows how to scan stdout, so
// in practice that fallback path is the daily-driver.
func runHeadless(listenAddr string, printURLFD int) {
	appService := NewApp()
	// Headless mode honors only the explicit --listen flag — the
	// Windows launcher always passes 127.0.0.1:0, so the persisted
	// LAN-bind preference is irrelevant here. The Windows-side
	// WebView2 fetches /bootstrap.json + the SPA over the transport,
	// but the SPA bundle the WebView2 *displays* lives in the
	// Windows binary's embed; the transport just needs an asset
	// handler so non-RPC paths return 404 cleanly.
	srv := bootTransport(appService, listenAddr, false)
	log.Printf("transport: headless mode")

	// Boot the App's subsystems directly. Wails normally calls
	// ServiceStartup with a context that lives until shutdown — we
	// mirror that with a process-scoped context cancelled on signal.
	bootCtx, bootCancel := context.WithCancel(context.Background())
	defer bootCancel()
	if err := appService.ServiceStartup(bootCtx, application.ServiceOptions{Name: "App"}); err != nil {
		fatalf("app: service startup: %v", err)
	}

	if err := writeBootstrap(printURLFD, srv); err != nil {
		// We've already started subsystems — shut them down before
		// exiting so SQLite isn't left in a half-flushed state.
		shutdownHeadless(appService, srv)
		fatalf("write bootstrap: %v", err)
	}

	// After bootstrap, stop writing to stdout so the launcher's
	// readBootstrapLine pipe doesn't accumulate logs (and can't be
	// re-poisoned by an unrelated log line that later starts with the
	// bootstrap prefix). The launcher only parses stdout for the
	// sentinel; once we've handed off the {port,token} the channel is
	// done with us. Routing log.Printf to stderr keeps the diagnostics
	// where the launcher can still surface them via its log mirror.
	os.Stdout = os.Stderr
	log.SetOutput(os.Stderr)

	// Wait for SIGINT / SIGTERM. Wails' Run() handles this for us in
	// the desktop path; here we own the loop directly.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("headless: received %s, shutting down", sig)

	shutdownHeadless(appService, srv)
}

// shutdownHeadless tears down the headless backend in the same order
// the Wails-managed path uses: drain transport (so in-flight RPCs
// finish), then call App.Shutdown (which flushes stores, terminals,
// telemetry, etc).
func shutdownHeadless(appService *App, srv *transport.Server) {
	shutCtx, cancel := context.WithTimeout(context.Background(), headlessShutdownTimeout)
	defer cancel()

	// App.Shutdown internally drains the transport server first
	// (it has the same wiring as the Wails-managed path), so we
	// don't need to call srv.Shutdown ahead of it. The defensive
	// post-Shutdown call below is idempotent.
	if err := appService.Shutdown(shutCtx); err != nil {
		log.Printf("app: shutdown: %v", err)
	}
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Printf("transport: post-shutdown: %v", err)
	}
}

// writeBootstrap publishes the listener address + token to the launcher.
//
// Why two channels: the spec asks for fd 3 because it gives the
// launcher a clean separation from any startup chatter the backend
// prints to stdout/stderr. In practice, propagating a real fd 3
// through `wsl.exe` is unreliable on Windows — the wsl.exe process
// only forwards stdout (1) and stderr (2). So we try fd 3 first
// (Linux-side test rigs and direct invocations get the clean channel)
// and fall back to a sentinel-prefixed stdout line that the Windows
// launcher knows how to recognise.
func writeBootstrap(fd int, srv *transport.Server) error {
	bs := struct {
		Port  int    `json:"port"`
		Token string `json:"token"`
	}{
		Port:  portFromAddr(srv.Addr()),
		Token: srv.Token(),
	}
	payload, err := json.Marshal(bs)
	if err != nil {
		return fmt.Errorf("marshal bootstrap: %w", err)
	}

	if fd > 0 {
		// Try the requested fd first. On Linux a launcher script
		// passes a real fd 3; under wsl.exe this fd is closed and
		// os.NewFile returns nil-on-write so we fall back.
		f := os.NewFile(uintptr(fd), fmt.Sprintf("bootstrap-fd-%d", fd))
		if f != nil {
			_, writeErr := f.Write(append(payload, '\n'))
			closeErr := f.Close()
			if writeErr == nil && closeErr == nil {
				return nil
			}
			log.Printf("headless: fd %d write failed (%v, close=%v), falling back to stdout sentinel", fd, writeErr, closeErr)
		}
	}

	// Stdout sentinel fallback: the Windows-side launcher matches on
	// `__AO_BOOTSTRAP__: ` prefix. We add a leading newline so the
	// match is robust to any preceding partial line from log output.
	if _, err := fmt.Fprintf(os.Stdout, "\n%s %s\n", bootstrapStdoutPrefix, string(payload)); err != nil {
		return fmt.Errorf("write bootstrap to stdout: %w", err)
	}
	return nil
}

// portFromAddr extracts the numeric port from a "host:port" address.
// Returns 0 if the addr can't be split — the launcher detects that
// and surfaces an error.
func portFromAddr(addr string) int {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(portStr)
	if err != nil {
		return 0
	}
	return n
}

// splitListenAddr parses "host:port" or ":port" into a (host, port)
// pair. Invalid input returns ("127.0.0.1", 0) so the transport's
// own defaults kick in.
func splitListenAddr(addr string) (string, int) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "127.0.0.1", 0
	}
	if host == "" {
		host = "127.0.0.1"
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return host, 0
	}
	return host, port
}

// fatalf prints the formatted message to stderr and exits 1. Used at
// startup before logging is fully wired so the developer sees the
// reason for boot failure on the console.
func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// loadPersistedNetworkSettings reads the user's settings.json without
// involving App.ServiceStartup so the boot-time transport bind can
// honor the persisted Phase E LAN-bind preference. Returns the zero
// value (BindAll=false) on any failure — a corrupt or missing file
// must not block startup.
//
// Why duplicate the configDir math from app.go: the App service hasn't
// initialised yet when main.go constructs the transport, so we can't
// reach through it for the settings path. The lookup mirrors
// initStores' fallback chain (UserConfigDir → UserHomeDir).
func loadPersistedNetworkSettings() settings.NetworkSettings {
	configDir, err := os.UserConfigDir()
	if err != nil {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return settings.NetworkSettings{}
		}
		configDir = home
	}
	dir := filepath.Join(configDir, "agent-overflow")
	return settings.NewService(dir).Get().Network
}

// buildAssetHandler returns the http.Handler that the transport mounts
// at "/" for non-RPC requests. Two cases:
//
//   - FRONTEND_DEVSERVER_URL set: `wails3 dev` is running the Vite dev
//     server. We proxy every request through so HMR's WebSocket and
//     module fetches reach the live bundler. Without this, the embedded
//     dist would shadow the dev server and HMR breaks silently.
//   - FRONTEND_DEVSERVER_URL empty: production / `wails3 build` path.
//     Serve the embedded frontend/dist bundle. http.FS over fs.Sub is
//     the safe pairing — http.Dir would expose path traversal of the
//     developer's local filesystem.
func buildAssetHandler(embeddedAssets embed.FS) (http.Handler, error) {
	if devURL := os.Getenv("FRONTEND_DEVSERVER_URL"); devURL != "" {
		parsed, err := url.Parse(devURL)
		if err != nil {
			return nil, fmt.Errorf("parse FRONTEND_DEVSERVER_URL %q: %w", devURL, err)
		}
		log.Printf("transport: dev mode — proxying assets to %s", devURL)
		return httputil.NewSingleHostReverseProxy(parsed), nil
	}
	embeddedSPA, err := fs.Sub(embeddedAssets, "frontend/dist")
	if err != nil {
		return nil, fmt.Errorf("locate embedded frontend/dist: %w", err)
	}
	return http.FileServer(http.FS(embeddedSPA)), nil
}
