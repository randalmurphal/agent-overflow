//go:build !nogui

// app_updater_desktop.go is the native Wails adapter for internal/appupdate.
// It stays behind !nogui because only the desktop process owns an
// application.App and its framework updater handle.
package app

import (
	"log"
	"net/http"
	"runtime"

	"agent-overflow/internal/appupdate"
	"agent-overflow/internal/selfupdate"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// initUpdater configures app.Updater for in-app self-update and bridges its
// events onto the transport bus. Called once from runDesktop after
// application.New and before the transport server starts, so the configured
// service is visible to RPC handlers without a race.
//
// It leaves the service unconfigured (the RPCs report unsupported) for
// unstamped "dev" builds, for Linux installs the swap could never write to
// (see internal/selfupdate/linuxgate.go), and on any provider/init failure
// (logged): in-app updates simply stay unavailable while the app runs
// normally. Failing to set up the updater must never block startup.
func InitUpdater(appService *App, app *application.App) {
	if appService.version == "dev" {
		log.Printf("updater: disabled for dev build (version=%q)", appService.version)
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
	if err := appService.updater.Configure(app.Updater, appupdate.Config{
		CurrentVersion: appService.version,
		Platform:       runtime.GOOS,
		Arch:           runtime.GOARCH,
		HTTPClient:     &http.Client{},
	}); err != nil {
		log.Printf("updater: init failed: %v — in-app updates disabled", err)
		return
	}

	bridgeUpdaterEvents(appService, app)
	log.Printf("updater: configured (current version %s)", appService.version)
}

// bridgeUpdaterEvents forwards every updater lifecycle event selected by the
// service onto the transport bus. Listener callbacks
// run on a Wails dispatch goroutine; App.emit is goroutine-safe and the
// dispatch is async, so this never blocks the download.
//
// The On() removers are intentionally discarded: this runs exactly once from
// initUpdater and the bridge lives for the whole process, so there is nothing
// to unsubscribe.
func bridgeUpdaterEvents(appService *App, app *application.App) {
	for _, wailsEvent := range appupdate.BridgedEvents() {
		app.Event.On(wailsEvent, func(e *application.CustomEvent) {
			var data any
			if e != nil {
				data = e.Data
			}
			appService.updater.ForwardFrameworkEvent(wailsEvent, data)
		})
	}
}
