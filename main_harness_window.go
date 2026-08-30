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
	"log"
	"os"
	"os/signal"
	"syscall"

	"agent-overflow/internal/transport"

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
func runWindowedShell(appService *App, srv *transport.Server, title string) error {
	shell := webviewShell{
		title:     title,
		beforeRun: quitOnSignal,
		// Raw URL on purpose: webviewShell.run threads the client id on
		// (its `withClientID`), and it is the one place that rule lives for
		// every windowed boot.
		pageURL: func() string { return appURLWithPageMarker(srv.AppURL(), srv.PageMarker()) },
		// Geometry rides the instance's own settings.json: both sides
		// resolve through the --data-dir override (bootSettingsDir for the
		// read, App.settings for the write), so a windowed harness
		// remembers where IT was, and never moves the user's real window.
		loadGeometry:    loadPersistedWindowGeometry,
		persistGeometry: appService.persistWindowGeometry,
	}
	runErr := shell.run()
	shutdownHeadless(appService, srv)
	return runErr
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
