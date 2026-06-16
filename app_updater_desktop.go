//go:build !nogui

// app_updater_desktop.go wires the Wails app.Updater into the native desktop
// path and bridges its lifecycle events onto our transport event bus. It lives
// behind the !nogui tag because it imports the Wails application package (and
// the GitHub provider through it); the headless WSL backend compiles without
// any of this and leaves App.updater nil. The provider-agnostic RPC surface
// and the verifiedProvider wrapper live in app_updater.go (no build tag).
package main

import (
	"log"
	"net/http"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/updater"
	"github.com/wailsapp/wails/v3/pkg/updater/providers/github"
)

// updaterRepository is the GitHub "owner/repo" the updater polls for releases.
// Releases are matched by asset filename (agent-overflow-<goos>-<goarch>[.ext])
// and verified against the SHASUMS256 sidecar.
const updaterRepository = "randalmurphal/agent-overflow"

// updaterChecksumAsset is the exact release-asset name the GitHub provider
// fetches and parses for SHA-256 digests. Must match the file written by
// scripts/package-release-assets.sh; a mismatch makes the provider fall open
// (no verification), which verifiedProvider then rejects.
const updaterChecksumAsset = "SHASUMS256"

// updaterEventBridge maps each Wails updater lifecycle event onto the transport
// channel the Svelte UI subscribes to. The updater emits these on the Wails
// application event bus (pkg/updater/events.go); EventManager.Emit stores a
// single argument as CustomEvent.Data, so e.Data is already the typed payload
// (updater.Progress for progress, *updater.Release for the lifecycle events,
// updater.ErrorInfo for errors) and we forward it verbatim.
//
// Check results (update-available / no-update) are deliberately NOT bridged:
// the frontend drives Check via the CheckForUpdate RPC and uses its return
// value, so re-emitting them as events would be redundant.
var updaterEventBridge = map[string]string{
	updater.EventDownloadStarted:  "updater:download-started",
	updater.EventDownloadProgress: "updater:progress",
	updater.EventVerifying:        "updater:verifying",
	updater.EventInstalling:       "updater:installing",
	updater.EventUpdateReady:      "updater:ready",
	updater.EventError:            "updater:error",
}

// initUpdater configures app.Updater for in-app self-update and bridges its
// events onto the transport bus. Called once from runDesktop after
// application.New and before the transport server starts, so App.updater is
// visible to RPC handlers without a race.
//
// It is a no-op (App.updater stays nil → the RPCs report unsupported) for
// unstamped "dev" builds, and on any provider/init failure (logged): in-app
// updates simply stay unavailable while the app runs normally. Failing to set
// up the updater must never block startup.
func initUpdater(appService *App, app *application.App) {
	if version == "dev" {
		log.Printf("updater: disabled for dev build (version=%q)", version)
		return
	}

	// No global client timeout: the same client streams the (tens-of-MB)
	// release binary, and http.Client.Timeout caps the WHOLE exchange
	// including the body read — a fixed cap would abort downloads on slow
	// links. Per-call deadlines via context (updaterCheckTimeout /
	// updaterDownloadTimeout) bound each operation instead; DefaultTransport
	// still applies sane dial / TLS-handshake timeouts.
	provider, err := github.New(github.Config{
		Repository:    updaterRepository,
		ChecksumAsset: updaterChecksumAsset,
		HTTPClient:    &http.Client{},
	})
	if err != nil {
		log.Printf("updater: github provider init failed: %v — in-app updates disabled", err)
		return
	}

	if err := app.Updater.Init(updater.Config{
		CurrentVersion: version,
		Providers:      []updater.Provider{verifiedProvider{inner: provider}},
		Window:         updater.WindowNone, // we drive our own Svelte UI
	}); err != nil {
		log.Printf("updater: init failed: %v — in-app updates disabled", err)
		return
	}

	appService.updater = app.Updater
	bridgeUpdaterEvents(appService, app)
	log.Printf("updater: configured for %s (current version %s)", updaterRepository, version)
}

// bridgeUpdaterEvents forwards every updater lifecycle event in
// updaterEventBridge onto the transport bus via App.emit. Listener callbacks
// run on a Wails dispatch goroutine; App.emit is goroutine-safe and the
// dispatch is async, so this never blocks the download.
//
// The On() removers are intentionally discarded: this runs exactly once from
// initUpdater and the bridge lives for the whole process, so there is nothing
// to unsubscribe.
func bridgeUpdaterEvents(appService *App, app *application.App) {
	for wailsEvent, channel := range updaterEventBridge {
		app.Event.On(wailsEvent, func(e *application.CustomEvent) {
			var data any
			if e != nil {
				data = e.Data
			}
			appService.emit(channel, data)
		})
	}
}
