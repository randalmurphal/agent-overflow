// Package windowgeom holds the persisted desktop-window placement shape and
// the pure logic that surrounds it: validating a saved placement against the
// current screen layout (Clamp) and coalescing a storm of move/resize events
// into a single debounced write (Tracker).
//
// It is deliberately free of any GUI dependency so the settings package (and
// the nogui WSL backend that imports it transitively) can carry the Geometry
// type without pulling in Wails. The Wails-aware glue that reads a live window
// and wires the event handlers lives in internal/uiwindow.
package windowgeom
