package main

import (
	"context"
	"embed"
	"encoding/json"
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
	"strings"
	"syscall"
	"time"

	"agent-overflow/internal/aocli"
	"agent-overflow/internal/appdirs"
	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/diagenv"
	"agent-overflow/internal/externalurl"
	"agent-overflow/internal/logging"
	"agent-overflow/internal/observability/goroutinedump"
	"agent-overflow/internal/observability/pprofserve"
	"agent-overflow/internal/orphanreaper"
	"agent-overflow/internal/provider/claudetui"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/shellenv"
	"agent-overflow/internal/transport"

	"github.com/google/uuid"
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

// version is stamped at link time via `-ldflags="-X main.version=..."`.
// Defaults to "dev" for unstamped builds (bare `go build`, IDE runs).
// Single source of truth is build/config.yml#info.version; the
// production Taskfiles read it as {{.VERSION}} and pass it through
// ldflags. App.Version surfaces it on the wire for the Settings footer.
var version = "dev"

// nativeMode is stamped to "dev" by Wails dev builds. Production defaults to
// embedded assets even if FRONTEND_DEVSERVER_URL leaks in from a dirty shell.
var nativeMode = "prod"

// dataDirRoot mirrors the --data-dir flag for the boot-time readers that
// run before App.Start (bootSettingsDir, ensureClientID). Set once in
// main() right after parseFlags, read-only afterwards. Empty means "use
// the OS default config dir" — the pre-flag behaviour.
var dataDirRoot string

// resetTransportPortPin mirrors the --reset-transport-port flag for
// bootTransport, which every boot mode reaches through a different
// wrapper (runDesktop / runHeadless / runHarness). Same set-once-in-main,
// read-only-afterwards contract as dataDirRoot; threading it through
// bootTransportOptions instead would let a mode that forgot the field
// silently ignore an operator's flag. Meaning: discard the persisted
// listen port before binding and adopt whatever we land on
// (main_transport_port.go).
var resetTransportPortPin bool

func main() {
	if os.Getenv(externalurl.BrowserHelperEnvironment) == externalurl.BrowserHelperValue && len(os.Args) == 2 {
		if err := externalurl.Open(context.Background(), os.Args[1]); err != nil {
			fatalf("open URL: %v", err)
		}
		return
	}
	// The orphan-reaper sidecar (macOS) re-execs this binary with the
	// __reap subcommand. Short-circuit before any other startup — flag
	// parsing, shell-env sync, Wails — so the sidecar stays a tiny pipe
	// reader that owns no GUI or transport. See internal/orphanreaper.
	if len(os.Args) > 1 && os.Args[1] == orphanreaper.Subcommand() {
		orphanreaper.RunChild()
		return
	}

	// The interactive Claude TUI provider registers this binary as its hook
	// command; Claude Code re-execs us with the __claude-hook subcommand for
	// every captured hook event. Short-circuit before any other startup — same
	// tiny-sidecar shape as the orphan reaper — so the relay client owns no GUI
	// or transport. See internal/provider/claudetui/hookcmd.go.
	if len(os.Args) > 1 && os.Args[1] == claudetui.HookSubcommand {
		claudetui.RunHookChild()
		return
	}

	// This binary is also the workflow CLI (D30): there is no separate `ao`
	// executable, and a provider session finds this one on its PATH under the
	// canonical name (see ensureCLISymlink). The two sidecars above are internal
	// re-execs of our own making and win the argv outright; everything from here
	// is somebody typing a command.
	switch mode := decideEntry(os.Args[1:], os.LookupEnv); mode {
	case entryCLI:
		os.Exit(aocli.Run(os.Args[1:], os.Stdout, os.Stderr))
	case entryRefuse:
		refuseInSessionBoot(os.Args[1:])
	case entryBoot:
		// Fall through to flag parsing and the mode switch below.
	default:
		// A mode added to entryMode without a branch here would otherwise
		// boot silently, which is the one outcome this dispatch exists to
		// prevent.
		fatalf("entry dispatch: unhandled mode %d", mode)
	}

	flags, err := parseFlags(os.Args[1:])
	if err != nil {
		fatalf("%v", err)
	}
	dataDirRoot = flags.dataDir
	resetTransportPortPin = flags.resetTransportPort

	// Move off any Windows drive mount the launcher left us on (the
	// translated /mnt/c install dir) before any subsystem spawns a child
	// that would inherit the slow 9p cwd — the shell-env probe just below,
	// provider MCP-status probes later. See relocateOffWindowsDriveMount.
	relocateOffWindowsDriveMount()

	if shouldSyncShellEnv(flags) {
		syncShellEnvForBoot()
	}

	// Opt-in loopback pprof listener (AGENT_OVERFLOW_PPROF=1). Started
	// for every backend mode — headless, embedded webview, client —
	// because memory questions don't care which shell the process wears.
	if pprofAddr, _, pprofErr := pprofserve.StartIfEnabled(); pprofErr != nil {
		// The operator explicitly asked for profiling; a silent no-op
		// listener would waste their next hour. Loud, not fatal.
		log.Printf("pprof: %v", pprofErr)
	} else if pprofAddr != "" {
		log.Printf("pprof: serving on http://%s/debug/pprof/", pprofAddr)
	}

	// Always-armed goroutine dump (`kill -USR1 <pid>`), landing next to the
	// engine log. Deliberately NOT behind the pprof gate above: the process
	// you need stacks from is the one already wedged, which is precisely the
	// one nobody thought to enable profiling on. The shipped binary is
	// stripped, so this is the only way in. No-op on Windows.
	goroutinedump.Install(bootLogsDir(), log.Printf)

	switch {
	case flags.connect != "":
		runClient(flags.connect)
	case flags.harness:
		runHarness(flags)
	case flags.soak:
		// Before flags.headless: --soak uses the headless bootstrap
		// channel (the launcher passes --print-url-fd 0 alongside it) but
		// needs its own isolated boot, not the ordinary one.
		runSoak(flags)
	case flags.headless:
		runHeadless(flags.listenAddr, flags.printURLFD)
	default:
		runDesktop(flags.listenAddr)
	}
}

func shouldSyncShellEnv(flags cliFlags) bool {
	// --connect has no local backend, so no subprocess ever resolves a
	// binary. Harness and soak modes spawn only the mock provider (an
	// absolute path) and git (system PATH); skipping the probe keeps boot
	// fast and deterministic — and keeps a login-shell probe out of a
	// mode whose whole promise is that it resolves no real tooling.
	return flags.connect == "" && !flags.harness && !flags.soak
}

func syncShellEnvForBoot() {
	// Sync PATH from the user's login shell before any subsystem
	// resolves a binary. Without this, the WSL backend (spawned by
	// `wsl.exe -d <distro> -- <bin>`, no shell init) and a Finder-
	// launched .app on macOS both see only the OS's default PATH —
	// missing nvm / asdf / ~/.local/bin / ~/.npm-global/bin and
	// everything else the user's rc files put on PATH. The probe is
	// best-effort: a failure here just leaves PATH untouched and the
	// downstream "binary not found" status banner takes over.
	started := time.Now()
	if err := shellenv.Sync(context.Background()); err != nil {
		log.Printf("shellenv: %v", err)
	}
	logBootPhase("shellenv.sync", started)
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
type bootTransportOptions struct {
	LoadPersistedBindAll     bool
	RequireReadyForBootstrap bool
	// HarnessReceiver, when non-nil, is registered on the dispatcher as
	// a second RPC receiver under "main.Harness.<Method>". Only harness
	// mode sets this — in every other boot the harness surface does not
	// exist on the wire at all.
	HarnessReceiver any
	// AllowDevServerAssets honors FRONTEND_DEVSERVER_URL even in a
	// production-stamped binary. Only harness mode sets this: --harness
	// is an explicit operator opt-in, so the "dirty shell must not
	// replace release assets" rule that gates dev builds doesn't apply.
	AllowDevServerAssets bool
}

func bootTransport(appService *App, listenAddr string, opts bootTransportOptions) *transport.Server {
	started := time.Now()
	defer logBootPhase("transport.total", started)

	phaseStarted := time.Now()
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
	if opts.HarnessReceiver != nil {
		harnessMethods, err := dispatcher.Register(opts.HarnessReceiver, transport.RegisterOptions{
			Package:   "main",
			TypeName:  "Harness",
			LocalOnly: true,
		})
		if err != nil {
			fatalf("transport: register Harness methods: %v", err)
		}
		log.Printf("transport: registered %d harness methods", len(harnessMethods))
	}
	logBootPhase("transport.register", phaseStarted)

	bus := transport.NewEventBus(0)
	appService.SetEventBus(bus)

	phaseStarted = time.Now()
	assetHandler, err := buildAssetHandler(assets, isNativeDevMode() || opts.AllowDevServerAssets)
	if err != nil {
		fatalf("transport: build asset handler: %v", err)
	}
	logBootPhase("transport.assets", phaseStarted)

	cfg := transport.Config{
		Dispatcher:               dispatcher,
		EventBus:                 bus,
		AssetHandler:             assetHandler,
		RequireReadyForBootstrap: opts.RequireReadyForBootstrap,
		// Late-bound: appService.DesignServer is a bound method value,
		// not the result of calling it. The transport server consults
		// this getter per-request so the /design/ route registers
		// up-front (at server build) but the underlying handler can be
		// supplied later by App.ServiceStartup → initSubsystems.
		// Snapshotting the result here would always be nil because
		// initSubsystems hasn't run yet, leaving /design/ unregistered
		// and iframe loads falling through to the SPA shell with
		// X-Frame-Options: DENY.
		DesignHandler: appService.DesignServer,
		// Late-bound for the same reason: the store opens during
		// ServiceStartup, after this config is built. The transport only
		// ever sees two strings.
		BackendIdentity: appService.backendIdentity,
		// The `ao` CLI's scoped-token registry. The App owns it because a
		// token's lifetime is a provider session's lifetime; the transport
		// only asks what a presented token is allowed to do.
		ScopedTokens: appService,
		// Diagnostic cross-origin isolation so the renderer exposes
		// measureUserAgentSpecificMemory. Opt-in: COEP breaks remote
		// subresources (chat-markdown images, design-preview assets).
		CrossOriginIsolate: envTruthy(os.Getenv(diagenv.RendererDiag)),
	}
	if cfg.CrossOriginIsolate {
		log.Printf("transport: renderer diag mode — cross-origin isolation headers on (remote subresources will not load)")
	}
	if listenAddr != "" {
		host, port, err := splitListenAddr(listenAddr)
		if err != nil {
			// Never a silent default: a malformed --listen used to
			// collapse to loopback + port 0, which the port pin then
			// resolves to the PINNED port — a bind the operator never
			// asked for and could not explain from the logs.
			fatalf("transport: %v", err)
		}
		cfg.BindAddr = host
		cfg.Port = port
	} else if opts.LoadPersistedBindAll {
		// Honor the persisted Phase E LAN-bind preference at boot so a
		// user who toggled "Allow remote access" in a previous session
		// doesn't see the server snap back to loopback after restart.
		// CLI --listen still wins — operator override beats stored prefs.
		if persisted := loadPersistedNetworkSettings(); persisted.BindAll {
			cfg.BindAddr = "0.0.0.0"
		}
	}

	// Pin the listen port unless the operator named one. Applies to
	// every bind host: a stable port also stabilises the LAN share URL.
	// --reset-transport-port drops the existing pin first, which is how
	// the Windows launcher escapes a pinned port the host cannot reach.
	portPin := pinTransportPort(&cfg, bootSettingsDir(), resetTransportPortPin)

	phaseStarted = time.Now()
	srv, err := transport.New(cfg)
	if err != nil {
		fatalf("transport: construct server: %v", err)
	}
	appService.SetTransportServer(srv)
	logBootPhase("transport.construct", phaseStarted)

	phaseStarted = time.Now()
	if err := srv.Start(); err != nil {
		// The pinned port must never be able to wedge boot twice: the
		// in-server ephemeral fallback should have absorbed a taken
		// port, so reaching here with a pin means an error class the
		// fallback predicate missed. Clear the pin so the next launch
		// binds ephemeral instead of replaying the same failure.
		portPin.clearOnFailedBind(err)
		fatalf("transport: start server: %v", err)
	}
	portPin.adopt(srv.Addr())
	log.Printf("transport: serving on %s", srv.Addr())
	logBootPhase("transport.start", phaseStarted)
	return srv
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
// Bootstrap delivery: a positive --print-url-fd asks for a clean fd
// channel, but propagating fd 3 through `wsl.exe` is unreliable across
// Windows versions. The Windows launcher passes fd=0 to skip that
// doomed probe and use the stdout sentinel directly; Linux-side test
// rigs and direct invocations can still pass fd=3 for the clean channel.
//
// Boot order is writeBootstrap-before-ServiceStartup with a readiness
// gate: the launcher receives the {port,token} as soon as transport is
// bound, but /bootstrap.json returns 503 until ServiceStartup finishes
// and MarkReady releases the WebView navigation. That separates "WSL
// process has published a port" from "backend is ready to render."
func runHeadless(listenAddr string, printURLFD int) {
	appService := newApp()
	// Before the transport server starts, so the updater RPC handlers see a
	// fully wired App.updater / App.wslUpdate without a race. Gated at runtime
	// on the Windows launcher having spawned us; a no-op otherwise.
	initWSLUpdater(appService)
	// Headless mode honors only the explicit --listen flag — the
	// Windows launcher always passes 127.0.0.1:0, so the persisted
	// LAN-bind preference is irrelevant here. The Windows-side
	// WebView2 fetches /bootstrap.json + the SPA over the transport,
	// but the SPA bundle the WebView2 *displays* lives in the
	// Windows binary's embed; the transport just needs an asset
	// handler so non-RPC paths return 404 cleanly.
	srv := bootTransport(appService, listenAddr, bootTransportOptions{RequireReadyForBootstrap: true})
	appService.osNotifications = newTransportNotificationSender(appService)
	// Now that the bus exists, the boot check above can say its piece. The
	// notice itself was recorded before the server started, so a client that
	// checks for updates the instant it connects already sees it.
	appService.notifyPendingUpdateApplyFailure()
	log.Printf("transport: headless mode")

	phaseStarted := time.Now()
	if err := writeBootstrap(printURLFD, srv); err != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), transportShutdownTimeout)
		defer cancel()
		if shutdownErr := srv.Shutdown(shutCtx); shutdownErr != nil {
			log.Printf("transport: shutdown after bootstrap failure: %v", shutdownErr)
		}
		fatalf("write bootstrap: %v", err)
	}
	logBootPhase("headless.write_bootstrap", phaseStarted)

	// After bootstrap, stop writing to stdout so the launcher's
	// readBootstrapLine pipe doesn't accumulate logs (and can't be
	// re-poisoned by an unrelated log line that later starts with the
	// bootstrap prefix). The launcher only parses stdout for the
	// sentinel; once we've handed off the {port,token} the channel is
	// done with us. Routing log.Printf to stderr keeps the diagnostics
	// where the launcher can still surface them via its log mirror.
	os.Stdout = os.Stderr
	log.SetOutput(os.Stderr)

	// Boot the App's subsystems directly. Wails normally calls
	// ServiceStartup with a context that lives until shutdown — we
	// mirror that with a process-scoped context cancelled on signal.
	// We call App.Start, not ServiceStartup: the latter is the Wails-
	// shaped adapter in app_desktop.go which only adds desktop-only
	// wiring (the save-file dialog). The WSL backend has no native
	// window to attach a dialog to, so going straight to Start keeps
	// this binary clear of the Wails import.
	bootCtx, bootCancel := context.WithCancel(context.Background())
	defer bootCancel()
	phaseStarted = time.Now()
	if err := appService.Start(bootCtx); err != nil {
		logBootPhase("headless.service_startup", phaseStarted)
		log.Printf("app: service startup: %v", err)
		srv.MarkStartupFailed()
		log.Printf("headless: startup failed; serving terminal bootstrap failure until shutdown")
		waitForHeadlessShutdown(appService, srv)
		return
	}
	logBootPhase("headless.service_startup", phaseStarted)

	phaseStarted = time.Now()
	srv.MarkReady()
	logBootPhase("headless.mark_ready", phaseStarted)

	waitForHeadlessShutdown(appService, srv)
}

func waitForHeadlessShutdown(appService *App, srv *transport.Server) {
	// Wait for SIGINT / SIGTERM. Wails' Run() handles this for us in
	// the desktop path; here we own the loop directly.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
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
// only forwards stdout (1) and stderr (2). So positive fd values try
// the clean channel first (Linux-side test rigs and direct invocations
// can pass fd=3), while fd=0 goes directly to the sentinel-prefixed
// stdout line that the Windows launcher knows how to recognise.
func writeBootstrap(fd int, srv *transport.Server) error {
	bs := struct {
		Port  int    `json:"port"`
		Token string `json:"token"`
		// ClientID is this installation's durable UI-state identity
		// (see ensureClientID). The launcher threads it onto the
		// webview URL as ?cid= so the frontend's per-client ui_state
		// bucket survives the per-launch origin change. Empty when the
		// backend couldn't persist one; the frontend then falls back
		// to a best-effort browser-cached ID.
		ClientID string `json:"clientId,omitempty"`
	}{
		Port:     portFromAddr(srv.Addr()),
		Token:    srv.Token(),
		ClientID: ensureClientID(),
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

// defaultListenHost is what an omitted host in --listen means: loopback,
// the same default a bare desktop boot takes.
const defaultListenHost = "127.0.0.1"

// splitListenAddr parses "host:port" or ":port" into a (host, port)
// pair. Port 0 keeps its meaning — "let the transport choose", which is
// what `--listen 127.0.0.1:0` (the Windows WSL launcher) and `:0` ask
// for, and what pinTransportPort then resolves against the pinned port.
//
// Malformed input is an error, never a silent default. A value the
// operator typed wrong ("8080", "0.0.0.0", "host:nan") used to collapse
// to ("127.0.0.1", 0) — and since the port pin landed, that resolves to
// the PINNED port: a bind neither the operator nor the log would
// explain. The caller turns this into a fatal boot error naming the
// expected form.
func splitListenAddr(addr string) (string, int, error) {
	const form = `expected host:port (e.g. "127.0.0.1:8080", "0.0.0.0:0", ":0")`
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, fmt.Errorf("--listen %q: %v; %s", addr, err, form)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("--listen %q: port %q is not a number; %s", addr, portStr, form)
	}
	if port < 0 || port > 65535 {
		return "", 0, fmt.Errorf("--listen %q: port %d is outside 0-65535; %s", addr, port, form)
	}
	if host == "" {
		host = defaultListenHost
	}
	return host, port, nil
}

func nativeSingleInstanceMode() string {
	if isNativeDevMode() {
		return "dev"
	}
	return "prod"
}

func isNativeDevMode() bool {
	return nativeMode == "dev"
}

func logBootPhase(phase string, started time.Time) {
	log.Printf("boot: phase=%s duration=%s", phase, time.Since(started).Round(time.Millisecond))
}

// envTruthy treats "1"/"true" (any case) as enabled; everything else,
// including empty, is off. Deliberately narrower than strconv.ParseBool
// so a typo'd value reads as off rather than a surprise opt-in.
func envTruthy(v string) bool {
	v = strings.TrimSpace(v)
	return v == "1" || strings.EqualFold(v, "true")
}

// fatalf prints the formatted message to stderr and exits 1. Used at
// startup before logging is fully wired so the developer sees the
// reason for boot failure on the console.
func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// bootSettingsDir resolves the directory holding settings.json for the
// boot-time reads that run before App.ServiceStartup constructs the settings
// service (the App service hasn't initialised yet when main.go builds the
// transport / window, so we can't reach through it for the path). It shares
// initStores' resolution — --data-dir override, else the appdirs chain — so
// every pre-Start reader resolves the same file the App later writes.
// Returns "" when no base dir is resolvable; callers treat that as "no
// persisted preference".
func bootSettingsDir() string {
	if dataDirRoot != "" {
		return filepath.Join(dataDirRoot, appdirs.DirName)
	}
	root, err := appdirs.Root()
	if err != nil {
		return ""
	}
	return root
}

// bootLogsDir resolves the directory the App's loggers write into — the
// one holding engine-YYYY-MM-DD.ndjson — for the boot-time diagnostics
// that arm before App.ServiceStartup runs. It composes bootSettingsDir
// (which resolves the same root initStores does) with logging.Dir, so a
// dump always lands beside the logs rather than beside a copy of the
// path rule. Returns "" when no base dir is resolvable, which the
// goroutine dumper reports as an error the first time it is signalled.
func bootLogsDir() string {
	dir := bootSettingsDir()
	if dir == "" {
		return ""
	}
	return logging.Dir(dir)
}

// newApp constructs the App for a local-backend boot, threading the
// --data-dir override through so initStores resolves the same root the
// boot-time readers (bootSettingsDir) already use.
func newApp() *App {
	appService := NewApp()
	appService.dataDirOverride = dataDirRoot
	return appService
}

// ensureClientID loads (or mints and persists) this installation's
// opaque UI-state client ID — the durable identity behind the
// per-client ui_state buckets. It lives in a small JSON file next to
// settings.json because the webview's own storage cannot be trusted to
// hold it: the origin is host+port, and while the port is now pinned
// per install (see pinTransportPort) it still churns whenever the
// pinned one is taken or reset, which would silently mint a new
// identity and orphan that client's ui_state. Returns "" when no config dir is
// resolvable or persistence fails; callers omit the cid URL param and
// the frontend degrades to a best-effort browser-cached identity.
func ensureClientID() string {
	return ensureClientIDIn(bootSettingsDir())
}

// ensureClientIDIn is the dir-parameterized core of ensureClientID,
// split out so initStores' settings→ui_state migration can resolve the
// same identity under a test-overridden config dir.
func ensureClientIDIn(dir string) string {
	if dir == "" {
		return ""
	}
	path := filepath.Join(dir, "client-id.json")
	var state struct {
		ClientID string `json:"clientId"`
	}
	found, err := atomicfile.ReadJSON(path, &state)
	if err != nil {
		log.Printf("client-id: read %s: %v", path, err)
	}
	if found && validClientID(state.ClientID) {
		return state.ClientID
	}
	state.ClientID = uuid.NewString()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("client-id: mkdir %s: %v", dir, err)
		return ""
	}
	if err := atomicfile.WriteJSON(path, state); err != nil {
		log.Printf("client-id: write %s: %v", path, err)
		return ""
	}
	return state.ClientID
}

// loadPersistedNetworkSettings reads the user's settings.json without
// involving App.ServiceStartup so the boot-time transport bind can
// honor the persisted Phase E LAN-bind preference. Returns the zero
// value (BindAll=false) on any failure — a corrupt or missing file
// must not block startup.
func loadPersistedNetworkSettings() settings.NetworkSettings {
	dir := bootSettingsDir()
	if dir == "" {
		return settings.NetworkSettings{}
	}
	return settings.NewService(dir).Get().Network
}

// buildAssetHandler returns the http.Handler that the transport mounts
// at "/" for non-RPC requests. Two cases:
//
//   - allowDevAssets + FRONTEND_DEVSERVER_URL set: a Vite dev server is
//     running (either `wails3 dev`, which stamps nativeMode=dev, or
//     harness mode's explicit opt-in). We proxy every request through so
//     HMR's WebSocket and module fetches reach the live bundler.
//     Production binaries outside harness mode deliberately ignore the
//     env var so a dirty shell cannot replace the embedded release
//     assets.
//   - Otherwise: production / `wails3 build` path.
//     Serve the embedded frontend/dist bundle. http.FS over fs.Sub is
//     the safe pairing — http.Dir would expose path traversal of the
//     developer's local filesystem.
func buildAssetHandler(embeddedAssets embed.FS, allowDevAssets bool) (http.Handler, error) {
	if devURL := os.Getenv("FRONTEND_DEVSERVER_URL"); devURL != "" && allowDevAssets {
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
