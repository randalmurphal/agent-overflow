package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
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
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"agent-overflow/internal/aocli"
	appservice "agent-overflow/internal/app"
	"agent-overflow/internal/appdirs"
	"agent-overflow/internal/appidentity"
	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/bundle"
	"agent-overflow/internal/cdprelay"
	"agent-overflow/internal/diagenv"
	"agent-overflow/internal/harness/darwinbundle"
	"agent-overflow/internal/logging"
	"agent-overflow/internal/network"
	"agent-overflow/internal/observability/goroutinedump"
	"agent-overflow/internal/observability/pprofserve"
	"agent-overflow/internal/orphanreaper"
	"agent-overflow/internal/platform"
	"agent-overflow/internal/provider/claudetui"
	"agent-overflow/internal/servercert"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/shellenv"
	"agent-overflow/internal/supervise"
	"agent-overflow/internal/transport"
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
	if err := disclaimHarnessResponsibility(); err != nil {
		fatalf("isolate macOS harness responsibility: %v", err)
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

	// A supervisor asks a staged binary what it is before it writes anything
	// down. Short-circuit with the other internal re-execs: the answer is one
	// JSON line and an exit, and a version being asked whether it can be
	// talked to must not boot a transport to say so. See internal/supervise.
	if len(os.Args) > 1 && os.Args[1] == supervise.PreflightSubcommand {
		if err := supervise.WritePreflight(os.Stdout, version); err != nil {
			fatalf("service preflight: %v", err)
		}
		return
	}

	// This binary is also the workflow CLI (D30): there is no separate `ao`
	// executable, and a provider session finds this one on its PATH under the
	// canonical name (see ensureCLISymlink). The two sidecars above are internal
	// re-execs of our own making and win the argv outright; everything from here
	// is somebody typing a command.
	bootArgs := os.Args[1:]
	serveMode := false
	superviseMode := false
	switch mode := decideEntry(os.Args[1:], os.LookupEnv); mode {
	case entryCLI:
		os.Exit(aocli.Run(os.Args[1:], os.Stdout, os.Stderr))
	case entryRefuse:
		refuseInSessionBoot(os.Args[1:])
	case entryServe:
		// The verb names the mode; everything after it is an ordinary boot
		// flag, so the flag set below is the same one every other boot
		// parses (main_serve.go argues why serve is not an aocli row).
		serveMode = true
		bootArgs = bootArgsAfterVerb(os.Args[1:], serveVerb)
	case entrySupervise:
		// Same shape one layer up: the flags after the verb are the ones the
		// supervisor will hand to the `serve` child it starts.
		superviseMode = true
		bootArgs = bootArgsAfterVerb(os.Args[1:], superviseVerb)
	case entryBoot:
		// Fall through to flag parsing and the mode switch below.
	default:
		// A mode added to entryMode without a branch here would otherwise
		// boot silently, which is the one outcome this dispatch exists to
		// prevent.
		fatalf("entry dispatch: unhandled mode %d", mode)
	}

	flags, err := parseFlags(bootArgs)
	if err != nil {
		fatalf("%v", err)
	}
	if serveMode {
		if err := checkBackendVerbFlags(serveVerb, flags); err != nil {
			fatalf("%v", err)
		}
	}
	if superviseMode {
		// The same refusals, for the same reason one layer up: every flag
		// checkBackendVerbFlags rejects names a different mode, and the
		// supervisor passes its flags straight through to a `serve` child
		// that would reject them anyway — after the unit had already started.
		if err := checkBackendVerbFlags(superviseVerb, flags); err != nil {
			fatalf("%v", err)
		}
	}
	if runtime.GOOS == "darwin" && flags.window {
		// A windowed isolated macOS boot must come from the per-run bundle
		// wrapper. Verify before any Wails/GTK initialization can cache the
		// production bundle's WebKit data store.
		expected := os.Getenv("AO_EXPECTED_BUNDLE_ID")
		nonce := os.Getenv("AO_HARNESS_BUNDLE_NONCE")
		if err := darwinbundle.Verify(os.Args[0], flags.dataDir, expected, nonce); err != nil {
			fatalf("harness: %v", err)
		}
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
	// Isolated boots first give up the fixed default port; see
	// unpinIsolatedPprofPort.
	if flags.harness || flags.soak {
		unpinIsolatedPprofPort()
	}
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
	case superviseMode:
		// Before serve: `supervise` starts one, and a supervisor that fell
		// through into being its own child would be an install with no
		// launch state and no way to update.
		runSupervise(bootArgs)
	case serveMode:
		// Before every other arm: `serve` is the mode the operator NAMED,
		// and checkBackendVerbFlags already refused every flag that would
		// have selected a different one.
		runServe(flags)
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
// LoadPersistedNetwork is the desktop-path-only escape hatch for the
// stored network preferences: the Phase E LAN-bind toggle and the
// canonical domain. Headless boots only honor the explicit --listen flag
// (the Windows launcher always passes one), so it passes false here.
// Pulling the load out of the helper keeps the boot graph linear: this
// function makes deterministic decisions from its arguments, never reads
// disk on its own.
type bootTransportOptions struct {
	LoadPersistedNetwork     bool
	RequireReadyForBootstrap bool
	// HarnessReceiver, when non-nil, is registered on the dispatcher as
	// a second RPC receiver under "main.Harness.<Method>". Only harness
	// mode sets this — in every other boot the harness surface does not
	// exist on the wire at all.
	HarnessReceiver    any
	HarnessPageMarker  string
	HarnessMethodsSink func([]string)
	// AllowDevServerAssets honors FRONTEND_DEVSERVER_URL even in a
	// production-stamped binary. Only harness mode sets this: --harness
	// is an explicit operator opt-in, so the "dirty shell must not
	// replace release assets" rule that gates dev builds doesn't apply.
	AllowDevServerAssets bool
}

// bootBrowserCDPRelay binds the loopback endpoint the Windows launcher's
// CDP tunnel terminates on, or returns nil where no launcher can exist.
//
// A failure here is not fatal: the app is fully usable without a browser
// pane, and a backend that refuses to boot because one loopback port was
// unavailable would be a far worse trade. The Manager then keeps its
// managed-Chrome engine, which still works — it is the pane that is lost,
// and the log line is what says so.
func bootBrowserCDPRelay() *cdprelay.Endpoint {
	if !platform.IsWSL() {
		return nil
	}
	relay, err := cdprelay.New(cdprelay.Config{})
	if err != nil {
		log.Printf("browser pane: cdp relay unavailable, keeping the managed browser engine: %v", err)
		return nil
	}
	log.Printf("browser pane: cdp relay listening on %s", relay.Addr())
	return relay
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
		// Hand the receiver the full wire surface (App methods included) so
		// HarnessListMethods can answer without reaching into the
		// dispatcher. Set before the listener serves, so no RPC can observe
		// a half-filled list.
		if opts.HarnessMethodsSink != nil {
			names := make([]string, 0, len(methods)+len(harnessMethods))
			for _, m := range append(append([]*transport.Method{}, methods...), harnessMethods...) {
				names = append(names, m.Name)
			}
			opts.HarnessMethodsSink(names)
		}
	}
	logBootPhase("transport.register", phaseStarted)

	bus := transport.NewEventBus(0)
	appService.SetEventBus(bus)

	phaseStarted = time.Now()
	assetHandler, devAssetProxy, spaBundle, err := buildAssetHandler(assets, isNativeDevMode() || opts.AllowDevServerAssets)
	if err != nil {
		fatalf("transport: build asset handler: %v", err)
	}
	if opts.AllowDevServerAssets {
		if warning := isolatedDevAssetWarning(os.Getenv("FRONTEND_DEVSERVER_URL")); warning != "" {
			log.Print(warning)
		}
	}
	logBootPhase("transport.assets", phaseStarted)

	// The other machines this installation drives. Built here rather than
	// during startup because the transport's routes for them are decided
	// at construction, and the profile directory is known before anything
	// opens: it is this installation's identity, not this launch's state.
	//
	// A boot with no resolvable config root attaches to nothing. That is
	// a real state (a relocation mid-flight, a locked-down profile) and
	// not a failure to abort on — the four admin methods answer it, the
	// routes are absent, and the local backend still works.
	attached := bootAttachedBackends()
	if attached != nil {
		appservice.SetAttachedBackends(appService.App, attached)
	}

	cfg := transport.Config{
		Dispatcher:               dispatcher,
		EventBus:                 bus,
		AssetHandler:             assetHandler,
		DevAssetProxy:            devAssetProxy,
		RequireReadyForBootstrap: opts.RequireReadyForBootstrap,
		// The link-time stamp, injected rather than read by the transport:
		// /healthz reports it, and the update watchdog compares it across
		// a restart to tell a new build from a bounce.
		Version: version,
		// One condition, two surfaces: the boots that register the Harness
		// receiver are exactly the boots whose /bootstrap.json says
		// harness, so the SPA's bridge can never load against a wire that
		// has no harness methods on it.
		Harness:    opts.HarnessReceiver != nil,
		PageMarker: opts.HarnessPageMarker,
		// Late-bound for the same reason: the store opens during
		// ServiceStartup, after this config is built. The transport only
		// ever sees two strings.
		BackendIdentity: func() (string, string) {
			return appservice.BackendIdentity(appService.App)
		},
		// Not late-bound, unlike the identity above: a hostname is
		// knowable before the store opens, and it is the same string the
		// pairing payload shows a device deciding whether to trust this
		// offer (internal/app backendDisplayName).
		BackendName: appidentity.HostDisplayName(),
		// The `ao` CLI's scoped-token registry. The App owns it because a
		// token's lifetime is a provider session's lifetime; the transport
		// only asks what a presented token is allowed to do.
		ScopedTokens: appService,
		// The session core's five seams, all late-bound for the reason
		// BackendIdentity is: identity boots during ServiceStartup, after
		// this config is built. The transport never learns what a session
		// row is — it asks these five questions and nothing else.
		SessionForRequest: func(r *http.Request) (string, bool) {
			return appservice.SessionForRequest(appService.App, r)
		},
		SessionLive: func(sessionID string) bool {
			return appservice.SessionLive(appService.App, sessionID)
		},
		SessionScopes: func(sessionID string) ([]string, string) {
			return appservice.SessionScopes(appService.App, sessionID)
		},
		StepUpProof: func(sessionID, token string) bool {
			return appservice.StepUpProof(appService.App, sessionID, token)
		},
		// Whether this backend can drive a browser for an agent at all.
		// Late-bound like the seams above: the browser Manager picks its
		// engine during ServiceStartup, and on a serve host that choice
		// depends on finding a Chromium installed on the machine
		// (docs/specs/remote-access.md §7).
		BrowserAvailable: func() bool {
			return appservice.BrowserToolsAvailable(appService.App)
		},
		PageSessionCredential: func() string {
			return appservice.PageSessionCredential(appService.App)
		},
		// Pairing redemption and credential rotation. The App adapts the
		// session core onto the transport's dumb DTOs.
		AuthEndpoints: appservice.AuthEndpoints(appService.App),
		// Attachment bytes, which cross on HTTP rather than inside a WS
		// frame. Late-bound the same way: the adapter holds the App and
		// reads its attachment store per call, so the store opening during
		// ServiceStartup is not a problem this config has to sequence.
		AttachmentTransfer: appservice.AttachmentTransfer(appService.App),
		// The SPA a paired phone shell downloads from this backend
		// (internal/bundle, docs/specs/remote-access.md §9). Nil on a
		// dev-server boot, which leaves the two routes answering 404 and
		// the hello frame silent about bundles — the answer that keeps a
		// shell running what it has.
		Bundle: spaBundle,
		// Diagnostic cross-origin isolation so the renderer exposes
		// measureUserAgentSpecificMemory. Opt-in: COEP breaks remote
		// subresources such as chat-markdown images.
		CrossOriginIsolate: envTruthy(os.Getenv(diagenv.RendererDiag)),
		// Nil when this boot keeps no pairings, which is what leaves the
		// three carried route families unregistered rather than serving
		// 404s from an empty set.
		AttachedBackends: attachedBackendsSeam(attached),
	}
	// The embedded browser pane's Windows leg. Inside WSL the browser
	// engine lives in the launcher process, reached over a tunnel the
	// launcher DIALS on this route; everywhere else there is no launcher,
	// so neither the route nor the relay exists and the browser Manager
	// keeps its managed-Chrome engine. The relay is built here rather than
	// during startup because the transport route needs it at construction.
	if relay := bootBrowserCDPRelay(); relay != nil {
		cfg.CDPTunnel = relay
		appservice.SetBrowserCDPRelay(appService.App, relay)
	}
	if cfg.CrossOriginIsolate {
		log.Printf("transport: renderer diag mode — cross-origin isolation headers on (remote subresources will not load)")
	}
	applyServerCertificate(&cfg, appService)
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
	}
	settingsPort := 0
	canonicalDomain := ""
	if opts.LoadPersistedNetwork {
		// Honor the persisted network preferences at boot so a user who
		// turned these on in a previous session doesn't see the server
		// snap back after a restart. CLI --listen still wins for the BIND
		// — operator override beats stored prefs — but the canonical
		// domain is not an address and applies either way: it decides
		// which Host header this listener answers to, whatever it bound.
		persisted := loadPersistedNetworkSettings()
		if persisted.BindAll && listenAddr == "" {
			cfg.BindAddr = "0.0.0.0"
		}
		// The saved port is applied by pinTransportPort, which owns the
		// whole three-way precedence and the cache interaction.
		settingsPort = persisted.ListenPort
		cfg.CanonicalHost = persisted.CanonicalDomain
		// The origin allow-list is NOT computed here. Every pattern it
		// emits names the bound port, and this boot has not resolved one
		// yet — the pin below and then the bind itself decide it. It is
		// installed after Start, where the number is a fact.
		canonicalDomain = persisted.CanonicalDomain
	}

	// Resolve the listen port: --listen, else the saved network.listenPort,
	// else the pin. Applies to every bind host — a stable port also
	// stabilises the LAN share URL. --reset-transport-port drops the
	// existing pin first, which is how the Windows launcher escapes a
	// pinned port the host cannot reach.
	portPin := pinTransportPort(&cfg, bootSettingsDir(), settingsPort, resetTransportPortPin)

	// One decoration rule for every page URL this backend hands out, and
	// the transport serves it (PageURLPath) to the local tooling that
	// navigates more than once over a backend's life: the Windows
	// launcher, `ao-harness open` / `attach`, the e2e rig. The transport
	// owns the credential half (a `?t=` ticket for a browser, injection
	// for a window host); this owns the two parameters only the boot
	// knows.
	// Closing over srv is safe because nothing calls this before New
	// returns.
	var srv *transport.Server
	cfg.DecoratePageURL = func(base string) string { return decoratePageURL(base, srv) }

	phaseStarted = time.Now()
	srv, err = transport.New(cfg)
	if err != nil {
		fatalf("transport: construct server: %v", err)
	}
	appService.SetTransportServer(srv)
	// A settings-driven rebind moves the listener without going through
	// the boot path, so the port cache would otherwise keep naming an
	// address nothing is on. Installed only when there is a directory to
	// write to; a nil recorder is a no-op inside the App.
	if dir := bootSettingsDir(); dir != "" {
		appservice.SetBoundPortRecorder(appService.App, func(port int) { storeTransportPort(dir, port) })
	}
	// Revocation is only real if it reaches live connections: hand the
	// session core the registry of open sockets, so revoking a session
	// force-closes the ones carrying it. Before Start, so no connection
	// can be accepted into a registry the core cannot reach.
	appservice.AttachSessionConns(appService.App, srv.SessionConns())
	logBootPhase("transport.construct", phaseStarted)

	phaseStarted = time.Now()
	if err := srv.Start(); err != nil {
		// The pinned port must never be able to wedge boot twice: the
		// in-server ephemeral fallback should have absorbed a taken
		// port, so reaching here with a pin means an error class the
		// fallback predicate missed. Clear the pin so the next launch
		// binds ephemeral instead of replaying the same failure.
		portPin.clearOnFailedBind(err)
		if settingsPort != 0 && listenAddr == "" {
			// A settings-chosen port deliberately has no ephemeral fallback,
			// so this is the one bind failure the operator can fix from the
			// UI. Name the setting and both ways out, because the raw
			// "address already in use" says nothing about where the number
			// came from.
			fatalf("transport: start server: %v (network.listenPort is %d in Settings > Network; change or clear it there, or pass --listen for one launch)",
				err, settingsPort)
		}
		fatalf("transport: start server: %v", err)
	}
	portPin.adopt(srv.Addr())
	// The WS origin allow-list, installed now that the listener has a
	// port. Every pattern names it exactly (internal/network.OriginPatterns
	// argues why), so it can only be built once the bind has happened —
	// --listen, the saved port and the pin all feed the same answer, and
	// an ephemeral bind has no answer at all until Start returns. Nothing
	// has been accepted yet, so there is no window where a connection is
	// judged against an empty list.
	bindAll := cfg.BindAddr == "0.0.0.0"
	srv.SetOriginPatterns(network.OriginPatterns(
		bindAll, bootLANIP(bindAll), canonicalDomain, portFromAddr(srv.Addr()),
	))
	log.Printf("transport: serving on %s", srv.Addr())
	logBootPhase("transport.start", phaseStarted)
	return srv
}

// applyServerCertificate resolves this install's self-signed TLS
// certificate and hands it to the two halves that need it: the listener
// terminates TLS with it on the port it already binds, and every pairing
// link this backend mints carries its fingerprint so a client that owns
// its own TLS configuration pins it (docs/specs/remote-access.md §7).
//
// One resolution feeding both, deliberately: the string a device is told
// to pin and the certificate that listener presents can then never be two
// different things.
//
// The certificate SOURCE is installed either way, even when this
// resolution fails. It is the slot the canonical domain's certificate
// lands in later (internal/app reconciles it from settings, and
// internal/acmecert renews it), and a listener bound without one could
// not serve that certificate without a restart.
//
// Best-effort, also deliberately. With no resolvable config directory
// there is nowhere to keep a certificate that survives a restart, and one
// re-minted every boot would un-pin every paired device each time — worse
// than not offering TLS at all. Boot continues on the cleartext half,
// which is what every browser uses regardless.
func applyServerCertificate(cfg *transport.Config, appService *App) {
	source := transport.NewCertificateSource()
	cfg.Certificates = source
	dir := bootSettingsDir()
	if dir == "" {
		log.Print("transport: no config directory resolved — serving cleartext only, so no client can pin this backend")
		return
	}
	material, err := servercert.Load(dir)
	if err != nil {
		log.Printf("transport: %v — serving cleartext only, so no client can pin this backend", err)
		return
	}
	source.SetSelfSigned(&material.Certificate)
	appservice.SetCertFingerprint(appService.App, material.Fingerprint)
	if material.Minted {
		// The replacement case logs its own reason inside servercert,
		// because a fingerprint change un-pins paired devices and must be
		// loud wherever Load is called from.
		log.Printf("transport: minted this install's TLS certificate; fingerprint %s", material.Fingerprint)
		return
	}
	log.Printf("transport: TLS certificate fingerprint %s", material.Fingerprint)
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
	// fully wired App.updater.handle / App.updater.wsl without a race. Gated at runtime
	// on the Windows launcher having spawned us; a no-op otherwise.
	appservice.InitWSLUpdater(appService.App, bootSettingsDir())
	// Headless mode honors only the explicit --listen flag — the
	// Windows launcher always passes 127.0.0.1:0, so the persisted
	// LAN-bind preference is irrelevant here. The Windows-side
	// WebView2 fetches /bootstrap.json + the SPA over the transport,
	// but the SPA bundle the WebView2 *displays* lives in the
	// Windows binary's embed; the transport just needs an asset
	// handler so non-RPC paths return 404 cleanly.
	srv := bootTransport(appService, listenAddr, bootTransportOptions{RequireReadyForBootstrap: true})
	appservice.ConfigureTransportNotifications(appService.App)
	// Now that the bus exists, the boot check above can say its piece. The
	// notice itself was recorded before the server started, so a client that
	// checks for updates the instant it connects already sees it.
	appservice.NotifyPendingUpdateApplyFailure(appService.App)
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

// writeBootstrap publishes the listener address, the session credential
// and a ready-to-navigate page URL to the launcher.
//
// The page URL is assembled here, not on the Windows side, because the
// client id and page marker it carries are only this process's to know.
// It carries NO credential: the launcher owns the window it navigates,
// so it asks the transport's PageURLPath route (`?host=webview`) for a
// ticket and injects it into the document that navigation produces
// (internal/pagehost). The same route answers a fresh bare URL whenever
// the launcher needs to navigate again.
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
		PID   int    `json:"pid"`
		// PageURL is the fully assembled URL the launcher's WebView2
		// navigates to. Bare — the launcher's ticket arrives by
		// injection (webviewPageURL).
		PageURL string `json:"pageUrl,omitempty"`
		// ClientID is this installation's durable UI-state identity
		// (see ensureClientID). It rides PageURL as ?cid= already, and
		// is reported separately for the launcher's own diagnostics.
		// Empty when the backend couldn't persist one; the frontend then
		// falls back to a best-effort browser-cached ID.
		ClientID   string `json:"clientId,omitempty"`
		PageMarker string `json:"pageMarker,omitempty"`
	}{
		Port:       portFromAddr(srv.Addr()),
		Token:      srv.Token(),
		PID:        os.Getpid(),
		PageURL:    webviewPageURL(srv),
		ClientID:   ensureClientID(),
		PageMarker: srv.PageMarker(),
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
	return appservice.PortFromAddr(addr)
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
	appservice.LogBootPhase(phase, started)
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
	appservice.SetDataDirOverride(appService.App, dataDirRoot)
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
	return appservice.EnsureClientIDIn(bootSettingsDir())
}

// decoratePageURL threads the two parameters a shell has to add onto a
// page URL the transport built: the harness page marker and the durable
// client id. One expression, because the stdout bootstrap line, the
// Windows launcher and `ao-harness` must all open the SAME page — a URL
// missing the client id silently changes which ui_state bucket the
// frontend reads.
//
// It is the transport's Config.DecoratePageURL hook, so the same rule
// applies to both credential shapes: the ticketed URL a browser opens
// and the bare one a window host loads.
func decoratePageURL(base string, srv *transport.Server) string {
	if base == "" || srv == nil {
		return base
	}
	return appURLWithClientID(appURLWithPageMarker(base, srv.PageMarker()), ensureClientID())
}

// fullPageURL assembles the page URL this backend's own tooling should
// navigate to with a BROWSER: a freshly minted one-time ticket on the
// URL, plus the boot's own parameters.
//
// A nil or pre-Start server yields "", which every caller already treats
// as "no page to open yet".
func fullPageURL(srv *transport.Server) string {
	if srv == nil {
		return ""
	}
	return decoratePageURL(srv.AppURL(), srv)
}

// webviewPageURL assembles the page URL a host that owns its WINDOW
// should load: the same page with no credential on it at all, because
// that host delivers the ticket by injection instead
// (internal/pagehost, internal/uiwindow.DeliverPageTicket). The ticket
// minted alongside is deliberately discarded here — the caller of this
// function is publishing a URL for another process to navigate, and that
// process asks PageURLPath for its own ticket once its document is live.
func webviewPageURL(srv *transport.Server) string {
	if srv == nil {
		return ""
	}
	return decoratePageURL(srv.WebviewPageURL(), srv)
}

// appURLWithClientID threads the durable UI-state client ID onto a page
// URL as `&cid=`. The `&` (not `?`) is correct: every page URL the
// transport hands out already carries its one-time page ticket.
//
// One helper because every window-opening boot needs the same rule and
// they are spread across three files (runDesktop, runWindowedShell, the
// harness bootstrap the Windows launcher reads). An empty id or URL
// passes through untouched — the frontend then falls back to its
// browser-cached identity, which is a degraded bucket rather than a
// broken page.
func appURLWithClientID(pageURL, clientID string) string {
	if pageURL == "" || clientID == "" {
		return pageURL
	}
	return pageURL + "&cid=" + url.QueryEscape(clientID)
}

// appURLWithPageMarker adds the per-harness page marker without changing an
// ordinary boot URL. The marker remains in browser history after the ticket is
// scrubbed, which lets CDP match the exact page rather than a same-origin tab.
func appURLWithPageMarker(pageURL, marker string) string {
	if pageURL == "" || marker == "" {
		return pageURL
	}
	separator := "&"
	if !strings.Contains(pageURL, "?") {
		separator = "?"
	}
	return pageURL + separator + "page=" + url.QueryEscape(marker)
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

// bootLANIP answers the LAN address the boot-time origin allow-list
// names, and only walks the interfaces when a LAN bind is what was
// asked for. A loopback boot pays nothing.
func bootLANIP(bindAll bool) string {
	if !bindAll {
		return ""
	}
	return network.DiscoverLocalLANIP()
}

// isolatedPprofEphemeralAddr answers what an isolated boot should bind
// pprof to, given the raw AGENT_OVERFLOW_PPROF value. Empty means "leave
// it exactly as the operator wrote it".
//
// The rule: a BARE enable ("1"/"true") becomes an ephemeral loopback port
// on an isolated boot, because a bare enable expresses "profile this
// process", not "own port 6363". pprofserve's DefaultAddr is a fixed
// singleton, and isolated boots are the one shape that is deliberately
// run N-at-a-time — a soak beside the developer's own app, a harness per
// checkout, an e2e run beside both — so the second instance's listener
// fails to bind and logs an error for a resource nobody asked it to
// claim. An EXPLICIT host:port is honored verbatim: the operator naming
// a port is choosing one, and silently moving it would break the
// bookmark they are about to open.
func isolatedPprofEphemeralAddr(raw string) string {
	switch strings.TrimSpace(raw) {
	case "1", "true", "TRUE", "True":
		// Port 0: the kernel picks. The bound address is logged, and an
		// isolated instance's discovery files already tell a tool where
		// that instance lives.
		return "127.0.0.1:0"
	}
	return ""
}

// unpinIsolatedPprofPort applies isolatedPprofEphemeralAddr to the
// environment, in place, before pprofserve reads it. Rewriting the
// variable rather than passing an address keeps pprofserve's single
// entry point (and its loopback refusal) as the only binder.
func unpinIsolatedPprofPort() {
	addr := isolatedPprofEphemeralAddr(os.Getenv(pprofserve.EnvVar))
	if addr == "" {
		return
	}
	if err := os.Setenv(pprofserve.EnvVar, addr); err != nil {
		log.Printf("pprof: keep the default port (rewrite %s: %v)", pprofserve.EnvVar, err)
	}
}

// isolatedDevAssetWarning is the boot-time notice an isolated
// (--harness / --soak) instance prints when it is actually serving a Vite
// dev server's assets instead of the embedded bundle. Empty when
// FRONTEND_DEVSERVER_URL is unset, i.e. the ordinary case.
//
// It exists because the opt-in is INHERITED, not chosen: the variable is
// exported by `make dev` and by the wails3 dev shell, so a harness or
// soak launched from that terminal silently measures an unminified,
// HMR-instrumented bundle. Every number a perf run or a renderer-hang
// soak produces then describes a build nobody ships, and nothing on
// screen says so. Loud at boot is the only place it can be said before
// the measurements start.
func isolatedDevAssetWarning(devURL string) string {
	if devURL == "" {
		return ""
	}
	return "WARNING: isolated boot is serving DEV-SERVER assets from " + devURL +
		" (FRONTEND_DEVSERVER_URL is set and --harness/--soak honors it). " +
		"Every measurement from this instance — perf runs, memory samples, soak observations — " +
		"is of the DEV bundle, not the shipped one. Unset FRONTEND_DEVSERVER_URL to measure the embedded build."
}

// deviceProfileDirName holds this installation's device identity: the one
// key every backend knows it by, and one session file per backend it has
// paired with.
//
// Under the app config root and NOT under --data-dir, which `--connect`
// refuses to be combined with anyway (main_entry.go): a data dir is one
// backend's database, while the device key is this installation's name on
// every backend it has ever met.
const deviceProfileDirName = "device"

func deviceProfileDir() (string, error) {
	root := bootSettingsDir()
	if root == "" {
		return "", errors.New("no config directory is resolvable, so this device has nowhere to keep its pairing")
	}
	return filepath.Join(root, deviceProfileDirName), nil
}

// deviceLabel is what this installation asks to be called in the owner's
// device list on every backend it pairs with. The hostname is what the
// person confirming the pairing recognises — the same string this backend
// publishes as its OWN name — and a machine that will not tell us its name
// gets a generic label rather than an empty row.
func deviceLabel() string {
	if host := appidentity.HostDisplayName(); host != "" {
		return host
	}
	return "Agent Overflow desktop"
}

// bootAttachedBackends builds the set of other machines this installation
// drives, or nil when there is nowhere to keep pairings.
func bootAttachedBackends() *attachedbackends.Manager {
	dir, err := deviceProfileDir()
	if err != nil {
		log.Printf("attached backends: %v", err)
		return nil
	}
	manager, err := attachedbackends.New(dir, deviceLabel(), runtime.GOOS)
	if err != nil {
		log.Printf("attached backends: %v", err)
		return nil
	}
	return manager
}

// attachedBackendsSeam hands the manager to the transport as an
// interface. A typed nil in an interface is not nil, and the transport
// registers its carried routes on exactly that test — so the conversion
// has to happen where the nil is still visible as one.
func attachedBackendsSeam(manager *attachedbackends.Manager) transport.AttachedBackends {
	if manager == nil {
		return nil
	}
	return manager
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
//
// The devProxy return says which case was taken. It is the transport's
// Config.DevAssetProxy — the one input that picks the relaxed CSP — so
// the policy is decided by the same condition that decided the handler,
// rather than by a second reading of the environment that could drift
// from it.
//
// The `spa` return is the same tree seen as the BUNDLE a phone shell may
// download (internal/bundle, transport's Config.Bundle). It comes back
// from here for the same reason devProxy does: whether this boot has a
// bundle to publish is exactly the question "are the assets embedded",
// and answering it anywhere else would be a second reading of the
// environment. A dev-server boot answers nil — a bundle that changes on
// every save is not something a phone should stage, and there is no
// file tree to hash.
func buildAssetHandler(embeddedAssets embed.FS, allowDevAssets bool) (handler http.Handler, devProxy bool, spa *bundle.Bundle, err error) {
	if devURL := os.Getenv("FRONTEND_DEVSERVER_URL"); devURL != "" && allowDevAssets {
		parsed, err := url.Parse(devURL)
		if err != nil {
			return nil, false, nil, fmt.Errorf("parse FRONTEND_DEVSERVER_URL %q: %w", devURL, err)
		}
		log.Printf("transport: dev mode — proxying assets to %s", devURL)
		return httputil.NewSingleHostReverseProxy(parsed), true, nil, nil
	}
	embeddedSPA, err := fs.Sub(embeddedAssets, "frontend/dist")
	if err != nil {
		return nil, false, nil, fmt.Errorf("locate embedded frontend/dist: %w", err)
	}
	// Nothing is read here: the walk is lazy, so a backend no shell ever
	// pairs with never hashes the tree at all.
	return http.FileServer(http.FS(embeddedSPA)), false, bundle.New(embeddedSPA, version), nil
}
