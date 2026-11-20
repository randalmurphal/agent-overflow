//go:build !nogui

// main_desktop.go owns the desktop-mode entry points (`runDesktop` and
// `runClient`) that construct a Wails v3 application + WebviewWindow;
// runDesktop's window is main_webview_shell.go's webviewShell, which the
// isolated windowed boots share. Package-main code that
// imports the Wails application carries the !nogui constraint so the WSL
// backend payload (built with `-tags nogui` — see
// build/windows/Taskfile.yml) can compile without it and therefore without
// libwebkit2gtk-4.1 / libgtk-3 in its NEEDED entries.
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
	"path/filepath"
	"runtime"
	"sync"

	appservice "agent-overflow/internal/app"
	"agent-overflow/internal/appidentity"
	"agent-overflow/internal/appupdate"
	"agent-overflow/internal/clientmode"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/supervise"
	"agent-overflow/internal/theme"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/uikeys"
	"agent-overflow/internal/uiwindow"
	"agent-overflow/internal/windowgeom"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// desktopUpdateTrial is whether this build applies its in-app updates, and
// migrates its database, through a helper with a snapshot and a trial
// (main_update_apply.go). The preflight reports it to the version that
// downloads this one.
const desktopUpdateTrial = runtime.GOOS == "darwin" || runtime.GOOS == "linux"

// runClient opens a desktop frontend. A paired target uses a local connection
// manager with one independent carrier per computer and no execution backend.
// A launch-token URL retains the single-upstream relay for same-host tooling.
// New pairing still confirms in the terminal before the window opens; existing
// pairings never require an upstream probe to open the local frontend.
func runClient(rawURL string) {
	// Interrupt reaches the ceremony's waits: the confirmation poll runs
	// for up to ten minutes, and Ctrl-C during it must end the process
	// rather than be swallowed by a client that is mid-request.
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt)
	var cfg clientmode.Config
	var err error
	if rawURL != "" {
		cfg, err = prepareConnection(ctx, rawURL)
	}
	stopSignals()
	if err != nil {
		fatalConnect(err)
	}

	embeddedSPA, err := fs.Sub(assets, "frontend/dist")
	if err != nil {
		fatalf("frontend: locate embedded frontend/dist: %v", err)
	}
	cfg.Assets = embeddedSPA
	// The stable screen identity scopes device preferences on execution hosts.
	// The independent frontend keeps its own browser storage and presentation files.
	appearanceDir := bootSettingsDir()
	if cfg.Paired != nil || cfg.WSURL == "" {
		appearanceDir = filepath.Join(appearanceDir, "frontend")
	} else {
		cfg.ClientID = ensureClientID()
	}

	title := appidentity.AppTitle(nativeSingleInstanceMode())
	// Native services are supplied through narrow hooks. Execution remains on
	// the independently connected computers, so no App receiver is registered.
	app := application.New(desktopApplicationOptions(title))
	var window *application.WebviewWindow
	stub, err := serveClientWindow(cfg, clientWindowHooks{
		setBackground: func(r, g, b uint8) {
			if window != nil {
				window.SetBackgroundColour(application.NewRGBA(r, g, b, 255))
			}
		},
		configureUpdater: func(service *appupdate.Service) {
			appservice.InitWindowUpdater(service, app, version, frontendRelaunchArgs(dataDirRoot))
		},
	})
	if err != nil {
		fatalf("frontend: serve window: %v", err)
	}
	log.Printf("clientmode: frontend serving on %s", stub.Addr())

	window = uiwindow.New(app, application.WebviewWindowOptions{
		Title:            title,
		Width:            1280,
		Height:           800,
		MinWidth:         800,
		MinHeight:        600,
		BackgroundColour: windowBackgroundColour(appearanceDir),
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

// runDesktop is the original Wails-window entry point used on
// macOS/Linux/Windows native builds. The Windows binary that proxies
// into WSL is a separate cmd/ — see cmd/agent-overflow-windows.
//
// helper is the update helper that started this launch
// (main_update_apply.go), zero when none did: it holds the single-instance
// identity and the backend lock until it exits.
func runDesktop(listenAddr string, helper supervise.ProcessRef) {
	if helper.PID != 0 {
		if err := supervise.WaitForExit(context.Background(), helper, desktopWaitTimeout); err != nil {
			log.Printf("updater: the update helper (pid %d) has not exited: %v", helper.PID, err)
		}
	}
	if runExistingDesktop() {
		return
	}
	appService := newApp()
	// Assigned inside beforeRun, on this goroutine, before the app loop
	// starts — so every later read (the reload keybinding on the UI
	// thread, the shutdown below) sees the started server.
	var srv *transport.Server
	// launch is beforeRun's decision to start the App, which registers it.
	var launch bool
	// startErr is App.Start's failure, which ends the process once the
	// app loop and the transport have shut down.
	var (
		startErrMu sync.Mutex
		startErr   error
	)
	setStartErr := func(err error) {
		startErrMu.Lock()
		startErr = err
		startErrMu.Unlock()
	}
	pages := newDesktopPages("")

	shell := webviewShell{
		title:          appidentity.AppTitle(nativeSingleInstanceMode()),
		singleInstance: true,
		pages:          pages,
		// The embedded browser's in-process engine hosts its views inside
		// this window. Handed over before Start, because the manager picks
		// its engine while starting; answering nil (no window yet, or a
		// platform with no in-process engine) leaves the process with NO
		// browser engine and no browser tools. The isolated windowed boots
		// reach the same call through the same hook (main_harness_window.go).
		withWindow: func(getWindow func() *application.WebviewWindow) {
			appservice.SetBrowserNativeWindow(appService.App, nativeWindowPointer(getWindow))
		},
		services: func(getWindow func() *application.WebviewWindow) []application.Service {
			if !launch {
				return nil
			}
			return []application.Service{
				application.NewService(appservice.NewDesktopNotificationService(appService.App, getWindow)),
				application.NewService(appService),
			}
		},
		beforeRun: func(app *application.App) bool {
			plan := desktopBootPlan{launch: true}
			var gate desktopGate
			if desktopUpdateTrial {
				gate, plan = reconcileDesktopUpdate(appService)
			}
			if plan.page != nil {
				pages.showFailure(*plan.page)
				return true
			}
			if !plan.launch {
				return false
			}
			launch = true
			// Configure in-app self-update before the transport serves, so the updater
			// RPC handlers observe appService.updater.handle without a race. No-op for dev
			// builds and on provider/init failure (logged) — updates stay unavailable
			// and the app runs normally.
			var trial appupdate.DesktopTrial
			if gate.boot != nil {
				trial = gate.boot.handoff()
			}
			appservice.InitUpdater(appService.App, app, trial)
			if plan.failedTo != "" {
				appservice.ReportUnsuccessfulUpdate(appService.App, plan.failedTo, plan.failedReason)
			}
			// An empty AppURL is refused by the shell
			// (webviewShell.windowOptions), immediately after this returns
			// and before the window options are built, on the path that
			// would hand Wails the empty URL, and the transport is shut
			// down before that error is reported.
			srv = bootTransport(appService, listenAddr, bootTransportOptions{UpdatingTo: plan.updatingTo})
			// ServiceStartup runs App.Start on its own goroutine so the
			// window opens and shows the boot's progress. Success releases
			// the readiness gate. A database the store refused to migrate
			// live shows why in the window (desktopGate.startFailed), and
			// the process exits with the error once it closes. Any other
			// failure serves the terminal bootstrap answer and quits, and
			// the process exits with the error. Quit on its own goroutine:
			// it waits on
			// ServiceShutdown, which waits for this Start.
			appservice.SetStartDone(appService.App, func(err error) {
				if err == nil {
					srv.MarkReady()
					appservice.NotifyPendingUpdateApplyFailure(appService.App)
					return
				}
				log.Printf("app: service startup: %v", err)
				srv.MarkStartupFailed()
				setStartErr(err)
				if page := gate.startFailed(err); page != nil {
					pages.showFailure(*page)
					return
				}
				go app.Quit()
			})
			return true
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
			// The ticket of the backend's own window (transport
			// MintWindowTicket).
			return srv.MintWindowTicket()
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
	startErrMu.Lock()
	err := startErr
	startErrMu.Unlock()
	if err != nil {
		fatalf("app: service startup: %v", err)
	}
}

// reconcileDesktopUpdate takes the backend lock, which bootTransport then
// keeps, and applies the update record's recovery and the migration gate
// before anything opens the database. The App refuses pending migrations
// whatever the gate can do (rule 7): a boot whose update half is
// unavailable launches, and a database it would migrate shows why instead.
func reconcileDesktopUpdate(appService *App) (desktopGate, desktopBootPlan) {
	if err := holdBackendLock(bootSettingsDir()); err != nil {
		fatalf("backend: %v", err)
	}
	appservice.RefusePendingMigrations(appService.App)
	gate := newDesktopGate(heldBackendLock.file)
	return gate, gate.reconcile(context.Background())
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
	return windowBackgroundColour(bootSettingsDir())
}

func windowBackgroundColour(dir string) application.RGBA {
	hex := theme.WindowBackground(dir)
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
			uiwindow.Reveal(window())
		},
	}
}
