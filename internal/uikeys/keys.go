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
// The Windows launcher returns the backend's current page URL, which a
// rebind moves to another origin, so a reload reaches the backend where
// it serves now rather than the address the window first loaded.
//
// reloadURL is called on every reload, not captured at construction,
// because the launcher has no URL until the WSL backend has booted.
func BrowserWithReload(reloadURL func() string) map[string]func(application.Window) {
	// Zoom is handled in the frontend via the fontSize setting
	// (Ctrl/Cmd+Plus/Minus adjusts by 1px). The keybindings here
	// are no-ops that suppress the webview's native viewport zoom.
	suppressZoom := func(application.Window) {}
	suppressHistoryNavigation := func(application.Window) {}
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
				// SetURL re-navigates without the cache bypass
				// ForceReload gives. The SPA's content-hashed assets
				// already change per build, so the bypass matters only
				// for HMR, which this window never serves.
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
		"Alt+Left":          suppressHistoryNavigation,
		"Alt+Right":         suppressHistoryNavigation,
		"F11":               toggleFullscreen,
		"Ctrl+Command+F":    toggleFullscreen,
	}
}

// WithDevTools adds the F12 → OpenDevTools binding to bindings and
// returns it. Opt-in rather than part of Browser because the caller
// decides whether devtools should exist at runtime: the native desktop
// and --connect windows add it unconditionally (production builds
// compile OpenDevTools to a no-op via Wails build tags), while the WSL
// launcher ships one .exe for dev and prod with devtools always
// compiled in, so it adds the binding only in dev launcher mode.
func WithDevTools(bindings map[string]func(application.Window)) map[string]func(application.Window) {
	bindings["F12"] = func(window application.Window) { window.OpenDevTools() }
	return bindings
}
