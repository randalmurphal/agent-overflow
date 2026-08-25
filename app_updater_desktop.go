//go:build !nogui

// app_updater_desktop.go wires the Wails app.Updater into the native desktop
// path and bridges its lifecycle events onto our transport event bus. It lives
// behind the !nogui tag because it imports the Wails application package (and
// the GitHub provider through it); the headless WSL backend has no Wails
// application to hang an Updater off, so it builds its own — see
// app_updater_wsl.go. The provider-agnostic RPC surface, the shared provider
// constants, the wails-event → transport-channel bridge table, and the
// verifiedProvider wrapper all live in app_updater.go (no build tag).
package main

import (
	"log"
	"net/http"
	"runtime"

	"agent-overflow/internal/selfupdate"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/updater"
	"github.com/wailsapp/wails/v3/pkg/updater/providers/github"
)

// initUpdater configures app.Updater for in-app self-update and bridges its
// events onto the transport bus. Called once from runDesktop after
// application.New and before the transport server starts, so App.updater.handle is
// visible to RPC handlers without a race.
//
// It is a no-op (App.updater.handle stays nil → the RPCs report unsupported) for
// unstamped "dev" builds, for Linux installs the swap could never write to
// (see internal/selfupdate/linuxgate.go), and on any provider/init failure
// (logged): in-app updates simply stay unavailable while the app runs
// normally. Failing to set up the updater must never block startup.
func initUpdater(appService *App, app *application.App) {
	if version == "dev" {
		log.Printf("updater: disabled for dev build (version=%q)", version)
		return
	}

	// Native-Linux preflight: an AppImage's squashfs mount is read-only, and
	// a system-wide install (/usr/bin, /opt) is not writable by the user
	// running us. Neither can host the in-place binary swap, so report the
	// feature unsupported now rather than after a tens-of-MB download. See
	// internal/selfupdate/linuxgate.go; macOS is deliberately unaffected.
	if runtime.GOOS == "linux" {
		if reason := selfupdate.LinuxUpdaterBlocked(); reason != "" {
			log.Printf("updater: %s — in-app updates disabled", reason)
			return
		}
	}

	// No global client timeout: the same client streams the (tens-of-MB)
	// release binary, and http.Client.Timeout caps the WHOLE exchange
	// including the body read — a fixed cap would abort downloads on slow
	// links. Per-call deadlines via context (updaterCheckTimeout /
	// updaterDownloadTimeout) bound each operation instead; DefaultTransport
	// still applies sane dial / TLS-handshake timeouts. Shared with the
	// targetable wrapper so list/by-tag API calls use the same client.
	httpClient := &http.Client{}
	provider, err := github.New(github.Config{
		Repository:    updaterRepository,
		ChecksumAsset: updaterChecksumAsset,
		HTTPClient:    httpClient,
	})
	if err != nil {
		log.Printf("updater: github provider init failed: %v — in-app updates disabled", err)
		return
	}

	// One CheckRequest describes what this build asks the release feed for, and
	// it feeds both the Updater (which passes it to every Provider.Check) and
	// the targetable wrapper (whose ListReleases has no Updater to ask), so the
	// two can never disagree about which assets are installable here. The
	// native desktop path targets the running platform verbatim.
	req := updater.CheckRequest{
		CurrentVersion: version,
		Platform:       runtime.GOOS,
		Arch:           runtime.GOARCH,
	}

	// targetable adds version selection (ListReleases + by-tag download) on top
	// of the stock latest-only provider; verifiedProvider still wraps it, so
	// every resolved release — latest or a specific tag — is checksum-verified
	// or rejected fail-closed.
	targetable := newTargetableProvider(provider, updaterRepository, updaterChecksumAsset, req, httpClient)

	if err := app.Updater.Init(updater.Config{
		CurrentVersion: req.CurrentVersion,
		Platform:       req.Platform,
		Arch:           req.Arch,
		Providers:      []updater.Provider{verifiedProvider{inner: targetable}},
		Window:         updater.WindowNone, // we drive our own Svelte UI
	}); err != nil {
		log.Printf("updater: init failed: %v — in-app updates disabled", err)
		return
	}

	appService.updater.handle = app.Updater
	appService.updater.provider = targetable
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
