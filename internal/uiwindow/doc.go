// Package uiwindow is the Wails-aware glue that connects a live WebviewWindow
// to the GUI-free placement logic in internal/windowgeom: creating the app
// window with a saved placement restored (RestoreAndTrack — called from an
// ApplicationStarted handler so the window materializes synchronously and a
// maximized/fullscreen placement can be restored on the correct monitor without
// a flash) and wiring the move/resize/state events to a debounced persistence
// sink (Track).
//
// Like internal/uikeys it imports the Wails application package and is used
// only by the GUI binaries (the native desktop entry point and the Windows
// WSL launcher). The nogui WSL backend never imports it.
package uiwindow
