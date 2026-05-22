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

	"agent-overflow/internal/appidentity"
	"agent-overflow/internal/clientmode"
	"agent-overflow/internal/uikeys"

	"github.com/wailsapp/wails/v3/pkg/application"
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
		KeyBindings:      uikeys.BrowserWithReload(stub.AppURL),
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
	appService := NewApp()
	var window *application.WebviewWindow
	app := application.New(application.Options{
		Name:           "Agent Overflow",
		SingleInstance: desktopSingleInstanceOptions(func() *application.WebviewWindow { return window }),
		Services: []application.Service{
			application.NewService(appService),
		},
	})

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

	window = app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "Agent Overflow",
		Width:            1280,
		Height:           800,
		MinWidth:         800,
		MinHeight:        600,
		BackgroundColour: application.NewRGBA(22, 22, 30, 255),
		URL:              appURL,
		KeyBindings:      uikeys.BrowserWithReload(srv.AppURL),
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
