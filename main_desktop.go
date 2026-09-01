//go:build !nogui

// main_desktop.go owns the desktop-mode entry points (`runDesktop` and
// `runClient`) that construct a Wails v3 application + WebviewWindow.
// Everything Wails-related at the package-main level lives here so the
// WSL backend payload (built with `-tags nogui` — see
// build/windows/Taskfile.yml) can compile without the Wails import and
// therefore without libwebkit2gtk-4.1 / libgtk-3 in its NEEDED entries.
//
// main_nogui.go provides matching stubs for the build with the tag set.
package main

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"sync"

	appservice "agent-overflow/internal/app"
	"agent-overflow/internal/appidentity"
	"agent-overflow/internal/clientmode"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/theme"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/uikeys"
	"agent-overflow/internal/uiwindow"
	"agent-overflow/internal/windowgeom"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// runClient is the remote-client entry point. Instead of
// booting the local transport (HTTP+WS server, App service registration,
// SQLite, observability, sessions), the desktop binary points the
// Wails webview at a tiny loopback stub that serves the SPA shell
// verbatim, answers /bootstrap.json on its own origin, and carries the
// SPA's WebSocket to the backend named on the command line with the
// upstream credential attached in Go (internal/clientmode).
//
// What that credential is, and whether reaching it needs a pairing
// ceremony first, is prepareConnection's answer (main_connect.go). The
// ceremony runs BEFORE the window exists, on purpose: it is a
// conversation between two people at two screens, and a window that
// opened first would be showing an outage for the length of it.
//
// Why we still need a loopback HTTP server: the Wails webview only
// loads `http://`/`https://` URLs (or the embedded asset URL with the
// devserver path). Pointing it directly at `ws://...` won't work, and
// the page needs an origin of its own for its cookie and its
// same-origin manifest. The stub server is single-purpose: the shell,
// the assets under /assets/, the manifest, and the /ws carry.
func runClient(rawURL string) {
	// Interrupt reaches the ceremony's waits: the confirmation poll runs
	// for up to ten minutes, and Ctrl-C during it must end the process
	// rather than be swallowed by a client that is mid-request.
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt)
	cfg, err := prepareConnection(ctx, rawURL)
	stopSignals()
	if err != nil {
		fatalConnect(err)
	}

	embeddedSPA, err := fs.Sub(assets, "frontend/dist")
	if err != nil {
		fatalf("--connect: locate embedded frontend/dist: %v", err)
	}
	cfg.Assets = embeddedSPA
	// Same durable identity the local-backend path uses: one client ID
	// per installation, so this machine keeps one ui_state bucket on
	// every backend it talks to, local or remote.
	cfg.ClientID = ensureClientID()

	stub, err := clientmode.Serve(cfg)
	if err != nil {
		fatalf("--connect: serve stub: %v", err)
	}
	log.Printf("clientmode: stub serving on %s, attaching to %s", stub.Addr(), cfg.WSURL)

	title := appidentity.AppTitle(nativeSingleInstanceMode())
	// No services registered (Services left nil): the App receiver in
	// the local binary would only confuse a webview that's about to
	// RPC against a remote backend instead.
	app := application.New(desktopApplicationOptions(title))

	window := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            title,
		Width:            1280,
		Height:           800,
		MinWidth:         800,
		MinHeight:        600,
		BackgroundColour: bootWindowBackgroundColour(),
		URL:              stub.AppURL(),
		KeyBindings:      uikeys.WithDevTools(uikeys.BrowserWithReload(stub.AppURL)),
	})
	// The stub's page URL carries no credential — this process owns the
	// window, so each document it loads is handed its one-time ticket
	// directly (internal/uiwindow, internal/pagehost).
	uiwindow.DeliverPageTicket(window, stub.MintPageTicket)

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

// webviewShell is the window half of a GUI boot: it builds the Wails
// application, opens one WebviewWindow on a live transport URL, restores
// and tracks the window's geometry, and runs the app loop until the
// window closes.
//
// Two callers with different backends share it. runDesktop registers the
// App as a Wails service and lets Wails drive its lifecycle; the
// isolated windowed boots (--harness/--soak --window, see
// main_harness_window.go) have already started and MarkReady'd their
// backend by the time they get here, so they register no services, no
// single instance, and no updater. Everything that does NOT differ —
// window size, background colour, keybindings, cid threading, the
// ApplicationStarted creation order and the post-Run geometry flush —
// lives here once, so a windowed harness is the same shell the user
// runs rather than a lookalike of it.
type webviewShell struct {
	// title is both the application name and the window title.
	title string
	// singleInstance registers the desktop single-instance id. Isolated
	// instances leave it off: they are collision-free by construction and
	// N of them (one per checkout) may be open at once, so bouncing a
	// second launch into the first window would be wrong.
	singleInstance bool
	// services builds the Wails service list, given a getter for the
	// window (which does not exist until ApplicationStarted). Nil
	// registers none.
	services func(getWindow func() *application.WebviewWindow) []application.Service
	// beforeRun runs after application.New and before the app loop
	// starts. runDesktop boots its transport here, because the updater
	// must observe the application first.
	beforeRun func(app *application.App)
	// pageURL returns the CURRENT page URL, bare and marked
	// webview-hosted (the transport's WebviewPageURL). A getter, not a
	// value: Ctrl+R re-reads it so a rebind (the LAN toggle) reloads
	// onto the new origin.
	pageURL func() string
	// mintTicket hands out one one-time page ticket. Separate from
	// pageURL because this shell owns the window: the credential is
	// injected into each document it loads rather than written into the
	// URL that loaded it (internal/uiwindow.DeliverPageTicket).
	mintTicket func() (string, error)
	// loadGeometry / persistGeometry are the saved window placement's
	// reader and writer. Both resolve through the boot data dir, so an
	// isolated instance remembers its own window, not the user's.
	loadGeometry    func() windowgeom.Geometry
	persistGeometry func(windowgeom.Geometry)
}

// run opens the window and blocks until the app loop exits, returning
// the Wails run error. It does not tear down the backend: what
// shutdown means differs per caller, and the shell knows nothing about
// transports or App lifecycles.
func (s webviewShell) run() error {
	// window + its tracker flush are created on the ApplicationStarted handler
	// goroutine (see below) and read by the single-instance callback and the
	// post-Run backstop, so guard both.
	var (
		winMu         sync.Mutex
		window        *application.WebviewWindow
		flushGeometry func()
	)
	getWindow := func() *application.WebviewWindow {
		winMu.Lock()
		defer winMu.Unlock()
		return window
	}
	appOpts := desktopApplicationOptions(s.title)
	if s.singleInstance {
		appOpts.SingleInstance = desktopSingleInstanceOptions(getWindow)
	}
	if s.services != nil {
		appOpts.Services = s.services(getWindow)
	}
	app := application.New(appOpts)
	if s.beforeRun != nil {
		s.beforeRun(app)
	}

	// Assert non-empty before constructing the WebviewWindowOptions.
	// Wails maps URL "" back to its built-in scheme, which would expose
	// the in-process IPC surface we deliberately replaced with HTTP+WS.
	// AppURL() returns "" pre-Start or when both the cached and live
	// listener addresses fail to parse — both are pathological boot
	// states that warrant an obvious failure rather than a silent IPC
	// fallthrough or a port-less URL hitting port 80.
	appURL := s.pageURL()
	if appURL == "" {
		return errors.New("transport: page URL is empty after Start; refusing to fall through to the Wails IPC scheme")
	}

	// Thread the durable UI-state client ID onto the page URL (and the
	// Ctrl+R reload URL) so the frontend's per-client ui_state bucket
	// survives the per-launch origin change. Empty cid degrades to the
	// frontend's browser-cached fallback identity.
	clientID := ensureClientID()
	withClientID := func(pageURL string) string { return appURLWithClientID(pageURL, clientID) }
	reloadURL := func() string { return withClientID(s.pageURL()) }

	// Context-menu policy lives in the frontend guard
	// (browserHistoryGuard.ts): native menu allowed in editable fields
	// and on selected text, suppressed elsewhere. Don't set
	// DefaultContextMenuDisabled — it would hard-disable the allowed
	// menus below the JS layer (on the platforms where it does
	// anything at all). F12 devtools is a compiled no-op in production
	// builds, so WithDevTools is safe unconditionally here.
	opts := application.WebviewWindowOptions{
		Title:            s.title,
		Width:            1280,
		Height:           800,
		MinWidth:         800,
		MinHeight:        600,
		BackgroundColour: bootWindowBackgroundColour(),
		URL:              withClientID(appURL),
		KeyBindings:      uikeys.WithDevTools(uikeys.BrowserWithReload(reloadURL)),
	}
	// Reopen where we left off last. The window is created on ApplicationStarted
	// (not here) so it materializes synchronously against a live app loop — that
	// lets uiwindow.RestoreAndTrack maximize/fullscreen a restored window on the
	// monitor it was saved on, and reveal it already in that state, instead of
	// flashing at normal size or always landing on the primary. See
	// uiwindow.RestoreAndTrack for why creation must happen here.
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		w, flush := uiwindow.RestoreAndTrack(app, opts, s.loadGeometry(), s.persistGeometry)
		uiwindow.DeliverPageTicket(w, s.mintTicket)
		winMu.Lock()
		window = w
		flushGeometry = flush
		winMu.Unlock()
	})

	runErr := app.Run()
	// Backstop the WindowClosing flush: persist the final placement from the
	// tracker's in-memory latest (safe even though the window is now gone).
	winMu.Lock()
	flush := flushGeometry
	winMu.Unlock()
	if flush != nil {
		flush()
	}
	return runErr
}

// runDesktop is the original Wails-window entry point used on
// macOS/Linux/Windows native builds. The Windows binary that proxies
// into WSL is a separate cmd/ — see cmd/agent-overflow-windows.
func runDesktop(listenAddr string) {
	appService := newApp()
	// Assigned inside beforeRun, on this goroutine, before the app loop
	// starts — so every later read (the reload keybinding on the UI
	// thread, the shutdown below) sees the started server.
	var srv *transport.Server

	shell := webviewShell{
		title:          appidentity.AppTitle(nativeSingleInstanceMode()),
		singleInstance: true,
		services: func(getWindow func() *application.WebviewWindow) []application.Service {
			return []application.Service{
				application.NewService(appservice.NewDesktopNotificationService(appService.App, getWindow)),
				application.NewService(appService),
			}
		},
		beforeRun: func(app *application.App) {
			// Configure in-app self-update before the transport serves, so the updater
			// RPC handlers observe appService.updater.handle without a race. No-op for dev
			// builds and on provider/init failure (logged) — updates stay unavailable
			// and the app runs normally.
			appservice.InitUpdater(appService.App, app)
			// An empty AppURL is refused by the shell, immediately after
			// this returns and before the window options are built — one
			// check, on the path that would actually hand Wails the empty
			// URL. Repeating it here only added a second wording for one
			// failure, and a fatalf that skipped the transport shutdown
			// the shell's error return runs.
			srv = bootTransport(appService, listenAddr, bootTransportOptions{LoadPersistedNetwork: true})
		},
		pageURL: func() string {
			if srv == nil {
				return ""
			}
			return srv.WebviewPageURL()
		},
		mintTicket: func() (string, error) {
			if srv == nil {
				return "", errors.New("transport: not started")
			}
			return srv.MintPageTicket()
		},
		loadGeometry: loadPersistedWindowGeometry,
		persistGeometry: func(geometry windowgeom.Geometry) {
			appservice.PersistWindowGeometry(appService.App, geometry)
		},
	}

	runErr := shell.run()

	shutCtx, cancel := context.WithTimeout(context.Background(), transportShutdownTimeout)
	defer cancel()
	if srv != nil {
		if err := srv.Shutdown(shutCtx); err != nil {
			log.Printf("transport: shutdown: %v", err)
		}
	}
	if runErr != nil {
		fatalf("wails run: %v", runErr)
	}
}

// defaultWindowBackgroundColour is the compiled-in fallback ground: the
// dark palette's surface-0. It is what a fresh install, a light-theme
// user who has not repainted yet, and every failure to read the
// appearance cache all get.
var defaultWindowBackgroundColour = application.NewRGBA(22, 22, 30, 255)

// bootWindowBackgroundColour resolves the native window's construction
// color — the ground visible while a resize outruns the webview's paint
// — from the frontend's cached `themes/appearance.json#windowBackground`.
//
// This runs before the App service exists, so it goes through the
// package-level reader rather than the theme Service: one small file
// read on the boot path, and any failure (no config dir, absent file,
// malformed value) keeps the compiled-in dark default. Getting it wrong
// costs a wrong-colored resize flash, never a failed boot — which is why
// nothing here is fatal and nothing here creates a directory.
func bootWindowBackgroundColour() application.RGBA {
	hex := theme.WindowBackground(bootSettingsDir())
	if hex == "" {
		return defaultWindowBackgroundColour
	}
	red, green, blue, err := theme.ParseHexColor(hex)
	if err != nil {
		return defaultWindowBackgroundColour
	}
	return application.NewRGBA(red, green, blue, 255)
}

// loadPersistedWindowGeometry reads the saved desktop window placement from
// settings.json before ServiceStartup constructs the settings service,
// mirroring loadPersistedNetworkSettings. Returns the zero (never-saved)
// Geometry on any failure so a missing or corrupt file simply centers the
// window at the default size.
func loadPersistedWindowGeometry() windowgeom.Geometry {
	dir := bootSettingsDir()
	if dir == "" {
		return windowgeom.Geometry{}
	}
	return settings.NewService(dir).Get().Window
}

// desktopApplicationOptions is the shared application.Options base for
// both desktop entry points (runDesktop, runClient).
//
// Mac.ApplicationShouldTerminateAfterLastWindowClosed aligns macOS with
// the Linux/Windows default (quit when the last window closes). Wails
// defaults it to false on macOS, and with no tray, app menu, or
// dock-reopen window to restore, closing the window would otherwise
// leave a headless zombie backend — transport server, SQLite handle,
// provider subprocesses — running until Force Quit, and ServiceShutdown
// would never fire.
func desktopApplicationOptions(title string) application.Options {
	return application.Options{
		Name: title,
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	}
}

func desktopSingleInstanceOptions(window func() *application.WebviewWindow) *application.SingleInstanceOptions {
	return &application.SingleInstanceOptions{
		UniqueID: appidentity.SingleInstanceID("desktop", nativeSingleInstanceMode()),
		OnSecondInstanceLaunch: func(_ application.SecondInstanceData) {
			if w := window(); w != nil {
				w.Show()
				w.Restore()
				w.Focus()
			}
		},
	}
}
