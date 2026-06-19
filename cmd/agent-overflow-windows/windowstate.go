//go:build windows

// windowstate.go persists the launcher window's placement (position, size,
// maximized/fullscreen state) so the WSL-backed app reopens where it was last
// closed. It lives beside wsl.json under %APPDATA%\agent-overflow\ but in its
// own file: wsl.json is co-written by the WSL backend (the Settings distro
// switch), whereas window geometry is launcher-only and expressed in Windows
// screen coordinates — keeping them separate stops one writer from clobbering
// the other's fields. The native (macOS/Linux) build persists the equivalent
// state to settings.json instead; the two never share a value because they are
// different windows in different coordinate spaces.
package main

import (
	"log"
	"path/filepath"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/windowgeom"
	"agent-overflow/internal/wsldistro"
)

const windowStateFileName = "window.json"

func windowStatePath() (string, bool) {
	dir, ok := wsldistro.WSLConfigDir()
	if !ok {
		return "", false
	}
	return filepath.Join(dir, windowStateFileName), true
}

// loadWindowGeometry reads the saved launcher window placement. Returns the
// zero (never-saved) Geometry when %APPDATA% is unresolvable or the file is
// absent or corrupt, so the window simply centers at its default size.
func loadWindowGeometry() windowgeom.Geometry {
	path, ok := windowStatePath()
	if !ok {
		return windowgeom.Geometry{}
	}
	var g windowgeom.Geometry
	if _, err := atomicfile.ReadJSON(path, &g); err != nil {
		log.Printf("launcher: read window state: %v", err)
		return windowgeom.Geometry{}
	}
	return g
}

// saveWindowGeometry persists the launcher window placement. Best-effort: a
// failed write only costs the remembered position, so it is logged and
// swallowed rather than surfaced. It is the sink for the debounced geometry
// tracker wired up in buildApp.
func saveWindowGeometry(g windowgeom.Geometry) {
	path, ok := windowStatePath()
	if !ok {
		return
	}
	if err := atomicfile.WriteJSON(path, g); err != nil {
		log.Printf("launcher: persist window state: %v", err)
	}
}
