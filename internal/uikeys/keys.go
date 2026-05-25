// Package uikeys holds WebviewWindow keybindings shared across every
// window Agent Overflow opens.
package uikeys

import "github.com/wailsapp/wails/v3/pkg/application"

// Browser returns the standard browser-style keybindings (reload,
// fullscreen, and zoom-key suppression). The zoom entries are no-ops
// that prevent the webview's native viewport zoom — font scaling is
// handled in the frontend via the fontSize setting. Used by every
// WebviewWindow constructor so a new shortcut lands here instead of
// drifting between call sites.
func Browser() map[string]func(application.Window) {
	return BrowserWithReload(nil)
}

// BrowserWithReload is Browser with an override for the reload paths.
// reloadURL, when non-nil and non-empty, makes Ctrl+R / Ctrl+Shift+R
// re-navigate to the returned URL instead of calling window.Reload().
//
// Why this exists: the SPA scrubs the bootstrap token from
// window.location.search after the first /bootstrap.json fetch (see
// frontend/src/lib/transport/wsClient.ts defaultBootstrap) so the
// token doesn't sit in history / Referer / PerformanceResourceTiming.
// Wails' Reload() reloads the *current* URL, which after the scrub no
// longer carries `?t=`, and the second bootstrap fetch hits an empty
// token and 404s. Re-navigating to the original URL with the token
// still attached gives a clean reload without persisting the token in
// JS-readable storage.
//
// reloadURL is called every reload (not captured at construction) so
// the launcher's mutable URL — set after the WSL backend probes
// successfully — can be picked up.
func BrowserWithReload(reloadURL func() string) map[string]func(application.Window) {
	// Zoom is handled in the frontend via the fontSize setting
	// (Ctrl/Cmd+Plus/Minus adjusts by 1px). The keybindings here
	// are no-ops that suppress the webview's native viewport zoom.
	suppressZoom := func(application.Window) {}
	toggleFullscreen := func(window application.Window) { window.ToggleFullscreen() }

	reload := func(window application.Window) {
		if reloadURL != nil {
			if u := reloadURL(); u != "" {
				window.SetURL(u)
				return
			}
		}
		window.Reload()
	}
	forceReload := func(window application.Window) {
		if reloadURL != nil {
			if u := reloadURL(); u != "" {
				// SetURL re-navigates rather than bypassing cache the
				// way ForceReload does. The win is "reload survives
				// the token scrub"; cache-bypass is the loss. Worth
				// it — the SPA's content-hashed assets already
				// invalidate per-build, so the cache-bust mostly
				// matters for HMR which doesn't apply here anyway.
				window.SetURL(u)
				return
			}
		}
		window.ForceReload()
	}

	return map[string]func(application.Window){
		"CmdOrCtrl+plus":    suppressZoom,
		"CmdOrCtrl+=":       suppressZoom,
		"CmdOrCtrl+-":       suppressZoom,
		"CmdOrCtrl+r":       reload,
		"CmdOrCtrl+Shift+r": forceReload,
		"F11":               toggleFullscreen,
		"Ctrl+Command+F":    toggleFullscreen,
	}
}
