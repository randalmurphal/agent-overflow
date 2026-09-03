package app

import (
	"errors"

	appbrowser "agent-overflow/internal/browser"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/webview2host"
)

// The backend end of the embedded browser pane's Windows leg.
//
// Two directions, both riding wires that already exist. Backend → launcher
// is a directive on eventchan.BrowserHost, emitted exactly the way
// updater:install and webview:trim are. Launcher → backend is the
// BrowserHostReport RPC below, posted over the same notification-bridge
// connection the directive arrived on.
//
// Neither direction knows anything about browser policy: the Manager owns
// that, and this file only carries frames between it and the launcher.

// paneHostOptions builds the hosted engine's wiring, or nil when this
// deployment has no launcher to host a pane. The CDP relay is created at
// boot (main.go) only inside WSL, so its presence IS the deployment test —
// no second runtime guess that could disagree with it.
func (a *App) paneHostOptions() *appbrowser.PaneHostOptions {
	relay := a.browser.cdpRelay
	if relay == nil {
		return nil
	}
	return &appbrowser.PaneHostOptions{
		Relay: relay,
		Directive: func(directive webview2host.Directive) {
			a.emit(eventchan.BrowserHost, directive)
		},
	}
}

// BrowserHostReport is how the Windows launcher answers a browser:host
// directive. Its name is pinned by webview2host.RPCReport, which both
// sides of the wire import.
//
// kind is one of created (detail is the page's CDP target id, which is
// what lets this backend attach chromedp to a controller it did not
// create), create-failed, closed, or process-failed. An unrecognised kind
// or a malformed page id is refused rather than guessed at: both ends
// validate, because a near-miss would settle the wrong page's create.
//
// The report is best-effort in the launcher — a lost one costs a page
// handle the backend re-derives — so this returns quickly and never
// blocks on browser work.
//
// Scoped host: it settles pane-host state for real browser windows on
// this machine, and its only legitimate caller is the launcher process
// beside this backend. No remote session can host a native view, so no
// grant opens it.
//
//ao:scope host
//ao:route home
func (a *App) BrowserHostReport(pageID, kind, detail string) error {
	manager := a.browser.manager
	if manager == nil {
		return errors.New("browser: manager is not running")
	}
	return manager.ReportPaneHost(pageID, webview2host.ReportKind(kind), detail)
}
