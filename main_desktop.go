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
	"io/fs"
	"log"
	"net/url"
	"sync"

	"agent-overflow/internal/appidentity"
	"agent-overflow/internal/clientmode"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/uikeys"
	"agent-overflow/internal/uiwindow"
	"agent-overflow/internal/windowgeom"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

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

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            title,
		Width:            1280,
		Height:           800,
		MinWidth:         800,
		MinHeight:        600,
		BackgroundColour: application.NewRGBA(22, 22, 30, 255),
		URL:              stub.AppURL(),
		KeyBindings:      uikeys.WithDevTools(uikeys.BrowserWithReload(stub.AppURL)),
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

// runDesktop is the original Wails-window entry point used on
// macOS/Linux/Windows native builds. The Windows binary that proxies
// into WSL is a separate cmd/ — see cmd/agent-overflow-windows.
func runDesktop(listenAddr string) {
	appService := newApp()
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
	notificationService := newDesktopNotificationService(appService, getWindow)
	title := appidentity.AppTitle(nativeSingleInstanceMode())
	appOpts := desktopApplicationOptions(title)
	appOpts.SingleInstance = desktopSingleInstanceOptions(getWindow)
	appOpts.Services = []application.Service{
		application.NewService(notificationService),
		application.NewService(appService),
	}
	app := application.New(appOpts)

	// Configure in-app self-update before the transport serves, so the updater
	// RPC handlers observe appService.updater without a race. No-op for dev
	// builds and on provider/init failure (logged) — updates stay unavailable
	// and the app runs normally.
	initUpdater(appService, app)

	srv := bootTransport(appService, listenAddr, bootTransportOptions{LoadPersistedBindAll: true})

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

	// Thread the durable UI-state client ID onto the page URL (and the
	// Ctrl+R reload URL) so the frontend's per-client ui_state bucket
	// survives the per-launch origin change. Empty cid degrades to the
	// frontend's browser-cached fallback identity.
	clientID := ensureClientID()
	withClientID := func(pageURL string) string {
		if clientID == "" || pageURL == "" {
			return pageURL
		}
		return pageURL + "&cid=" + url.QueryEscape(clientID)
	}
	reloadURL := func() string { return withClientID(srv.AppURL()) }

	// Context-menu policy lives in the frontend guard
	// (browserHistoryGuard.ts): native menu allowed in editable fields
	// and on selected text, suppressed elsewhere. Don't set
	// DefaultContextMenuDisabled — it would hard-disable the allowed
	// menus below the JS layer (on the platforms where it does
	// anything at all). F12 devtools is a compiled no-op in production
	// builds, so WithDevTools is safe unconditionally here.
	opts := application.WebviewWindowOptions{
		Title:            title,
		Width:            1280,
		Height:           800,
		MinWidth:         800,
		MinHeight:        600,
		BackgroundColour: application.NewRGBA(22, 22, 30, 255),
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
		w, flush := uiwindow.RestoreAndTrack(app, opts, loadPersistedWindowGeometry(), appService.persistWindowGeometry)
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

	shutCtx, cancel := context.WithTimeout(context.Background(), transportShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Printf("transport: shutdown: %v", err)
	}
	if runErr != nil {
		fatalf("wails run: %v", runErr)
	}
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
