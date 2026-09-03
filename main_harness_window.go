//go:build !nogui

// main_harness_window.go is the window half of `--harness --window` and
// `--soak --window`: the real Wails webview shell, opened over a backend
// that runHarness / runSoak have already booted, started, and marked
// ready. Nothing about the backend changes — same prepareHarness, same
// mock providers, same Harness receiver, same bootstrap line — which is
// the whole point: an agent validating a change in this window is
// looking at the app, not at a test double of it.
//
// main_harness_window_nogui.go carries the fatal stub for the WSL
// backend payload, which is linked without Wails.
package main

import (
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"

	appservice "agent-overflow/internal/app"
	"agent-overflow/internal/harnessrpc"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/windowgeom"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// requireWindowedBuild is a no-op here: this build has a window.
func requireWindowedBuild() {}

// runWindowedShell opens the isolated instance's window and blocks
// until it closes, then tears the backend down in the same order the
// headless path uses (transport drain inside App.Shutdown). Returns the
// Wails run error, if any; the caller owns its discovery files and
// decides how loudly to fail.
//
// Differences from runDesktop, all deliberate:
//
//   - No single-instance registration. Isolated instances are
//     collision-free by construction (own data root, own port, own
//     webview storage) and one per checkout may be open at once, so a
//     second launch must open its own window rather than raise someone
//     else's.
//   - No Wails services. The backend's lifecycle already ran directly
//     (App.Start + MarkReady), exactly as in the headless harness;
//     registering App as a service here would start it a second time.
//   - No updater. An isolated instance must never swap the binary it is
//     testing.
func runWindowedShell(appService *App, srv *transport.Server, title string, nativeWindow *isolatedNativeWindow) error {
	shell := webviewShell{
		title:     title,
		beforeRun: quitOnSignal,
		// The browser engine's window getter was installed empty before
		// App.Start (newIsolatedProviderApp) and is filled here, which is the
		// first moment a window can exist at all on this path. Selection only
		// asked whether a getter EXISTS; the pointer behind it is resolved
		// lazily, when the first browser tool call starts the engine — long
		// after this. Unconditional and inert when the fake-engine pin is on
		// (it wins selection first) and on WSL (the launcher-hosted engine
		// wins before the native one).
		withWindow: func(getWindow func() *application.WebviewWindow) {
			nativeWindow.publish(
				nativeWindowPointer(getWindow),
				func() (harnessrpc.WindowState, error) { return harnessWindowState(getWindow()) },
				func(command harnessrpc.WindowCommand) error { return driveHarnessWindow(getWindow(), command) },
			)
		},
		// Raw URL on purpose: webviewShell.run threads the client id on
		// (its `withClientID`), and it is the one place that rule lives for
		// every windowed boot. Bare, like every window this binary owns —
		// the ticket goes in by injection (mintTicket below).
		pageURL:    func() string { return appURLWithPageMarker(srv.WebviewPageURL(), srv.PageMarker()) },
		mintTicket: srv.MintPageTicket,
		// Geometry rides the instance's own settings.json: both sides
		// resolve through the --data-dir override (bootSettingsDir for the
		// read, App.settings for the write), so a windowed harness
		// remembers where IT was, and never moves the user's real window.
		loadGeometry: loadPersistedWindowGeometry,
		persistGeometry: func(geometry windowgeom.Geometry) {
			appservice.PersistWindowGeometry(appService.App, geometry)
		},
	}
	runErr := shell.run()
	shutdownHeadless(appService, srv)
	return runErr
}

func harnessWindowState(window *application.WebviewWindow) (harnessrpc.WindowState, error) {
	if window == nil {
		return harnessrpc.WindowState{}, errors.New("native window has not been created")
	}
	bounds := window.Bounds()
	screen, err := window.GetScreen()
	if err != nil {
		return harnessrpc.WindowState{}, err
	}
	if screen == nil {
		return harnessrpc.WindowState{}, errors.New("native window has no screen")
	}
	return harnessrpc.WindowState{
		Bounds:     harnessWindowRect(bounds),
		Maximized:  window.IsMaximised(),
		Fullscreen: window.IsFullscreen(),
		Minimized:  window.IsMinimised(),
		Screen: harnessrpc.WindowScreen{
			ID:               screen.ID,
			Bounds:           harnessWindowRect(screen.Bounds),
			WorkArea:         harnessWindowRect(screen.WorkArea),
			PhysicalWorkArea: harnessWindowRect(screen.PhysicalWorkArea),
			ScaleFactor:      screen.ScaleFactor,
		},
	}, nil
}

func driveHarnessWindow(window *application.WebviewWindow, command harnessrpc.WindowCommand) error {
	if window == nil {
		return errors.New("native window has not been created")
	}
	if command.Bounds != nil {
		bounds := *command.Bounds
		window.SetBounds(application.Rect{X: bounds.X, Y: bounds.Y, Width: bounds.Width, Height: bounds.Height})
		return nil
	}

	switch command.Action {
	case "maximize":
		window.Maximise()
	case "unmaximize":
		window.UnMaximise()
	case "fullscreen":
		window.Fullscreen()
	case "unfullscreen":
		window.UnFullscreen()
	case "minimize":
		window.Minimise()
	case "unminimize":
		window.UnMinimise()
	default:
		return errors.New("unknown window action: use maximize, unmaximize, fullscreen, unfullscreen, minimize, or unminimize")
	}
	return nil
}

func harnessWindowRect(rect application.Rect) harnessrpc.WindowRect {
	return harnessrpc.WindowRect{X: rect.X, Y: rect.Y, Width: rect.Width, Height: rect.Height}
}

// quitOnSignal turns SIGINT/SIGTERM into an ordinary window close, so
// the caller's teardown — App.Shutdown, and the withdrawal of the
// instance's discovery files — actually runs.
//
// The ordinary desktop app has no such handler because a user quits it
// by closing the window. An isolated instance is a foreground process a
// human Ctrl-Cs and a script SIGTERMs, which without this kills it where
// it stands: transport undrained, and a registry row left claiming a pid
// that no longer exists (observed 2026-08-26 — readers survive it by
// treating the pid as dead, but the tidy path should not depend on that).
func quitOnSignal(app *application.App) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		defer signal.Stop(sigCh)
		sig := <-sigCh
		log.Printf("harness: received %s, closing window", sig)
		app.Quit()
	}()
}
