package app

import (
	"log"

	"agent-overflow/internal/windowgeom"
)

// persistWindowGeometry writes the desktop window placement through the
// settings service so the app reopens where it was last closed. It is the sink
// for the debounced window-geometry tracker (internal/uiwindow) wired up in
// runDesktop, and is invoked off the UI thread by that tracker.
//
// Best-effort by design: a failed write only costs the remembered position,
// which is cosmetic, not a user-facing error state — so the error is logged and
// swallowed rather than surfaced. Restore reads the file directly at boot (see
// loadPersistedWindowGeometry); it can't go through here because the settings
// service isn't constructed until ServiceStartup, after the window is created.
func (a *App) persistWindowGeometry(g windowgeom.Geometry) {
	if a.settings == nil {
		return
	}
	if _, err := a.settings.Update(map[string]any{"window": g}); err != nil {
		log.Printf("settings: persist window geometry: %v", err)
	}
}
