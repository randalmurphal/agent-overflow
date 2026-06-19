# internal/windowgeom/

GUI-free desktop-window placement: the persisted `Geometry` shape, the
validate/clamp-against-screens logic that decides where a saved window
reopens, and the debounced `Tracker` that coalesces a storm of
move/resize/state events into one write.

## Layout

- `geometry.go` — `Geometry` (persisted shape; X/Y/W/H always describe the
  *normal* rect) and `Rect`, plus `Clamp` (anchor to the saved display if it
  still exists, else the most-overlapped screen; shift fully on-screen; reject
  when off every screen → caller centers).
- `tracker.go` — `Tracker` + `Sample`. Owns the policy: drop minimized
  samples, keep the normal rect while maximized/fullscreen, debounce writes,
  and `Flush` synchronously on close.

## Responsibility boundary

- What BELONGS here: the placement data shape and all pure decision logic
  around it. No Wails, no filesystem — so `internal/settings` can embed
  `Geometry` without pulling in the GUI, and the nogui WSL backend stays clean.
- What does NOT belong here: reading a live window, registering window
  events, or persisting — that's `internal/uiwindow` (Wails glue) and the
  per-build sinks (`settings.json` native, `window.json` launcher).

## Persistence sites

`Geometry` is stored in two places that never share a value because they are
different windows in different coordinate spaces:

- Native (macOS/Linux/Windows): `settings.json` `window` field, written via
  `App.persistWindowGeometry`.
- WSL/Windows launcher: `%APPDATA%\agent-overflow\window.json`, written via
  `cmd/agent-overflow-windows/windowstate.go`.

## Anti-patterns

- Do NOT import Wails or `os` here — it would defeat the GUI-free contract and
  risk an import cycle through `internal/settings`.
- Do NOT persist the maximized/fullscreen bounds as the normal rect; the
  Tracker keeps them separate so un-maximize lands somewhere usable.
