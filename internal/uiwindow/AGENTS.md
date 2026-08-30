# internal/uiwindow/

Wails glue between a live `WebviewWindow` and the GUI-free placement logic in
`internal/windowgeom`: restore a saved placement into creation options, and
wire window events to a debounced persistence sink.

## Layout

- `uiwindow.go`
  - `RestoreAndTrack(app, baseOpts, saved, sink)` creates the app window with
    `saved` restored, reveals it (already maximized/fullscreen when that's the
    saved mode, on the monitor it was saved on, with no normal-size flash), and
    wires `Track`. Returns the window and the tracker flush func. **Must be
    called from an `ApplicationStarted` handler** (`app.running == true`): only
    then does `NewWithOptions` materialize the window synchronously, so the
    deferred `Maximise`/`Fullscreen`/`Show` act on a live impl instead of
    degrading to the buggy maximize-then-position start state.
  - `prepareOptions(opts, saved, screens)` *(unexported, pure, unit-tested)* is
    the placement decision: clamp `saved` (anchored to the saved `Display` when
    the live screen list is empty), write position/size into the
    `WebviewWindowOptions`, and return the geometry to seed `Track` plus the
    deferred `actions` (maximize/fullscreen). For maximize/fullscreen it sets
    `Hidden` and positions at the *normal* rect rather than using a start state,
    because Wails (alpha) maximizes at creation *before* applying X/Y. There is
    no creation-option ordering that positions first. Centers (leaves opts at
    defaults) when a normal window sits off every known screen.
  - `Track(window, restored, sink)` registers the move/resize/state events
    onto a `windowgeom.Tracker` and returns a flush func (also wired to
    `WindowClosing`; call it again after the app loop as a backstop).

## Responsibility boundary

- What BELONGS here: the Wails-specific reads (`Bounds`, `IsMaximised`,
  `GetScreen`, the event types) and the options mutation. Like `internal/uikeys`
  it imports the Wails `application` package.
- What does NOT belong here: the placement decision logic (that's
  `windowgeom`) or persistence (the sink is supplied by the caller:
  `App.persistWindowGeometry` native, `saveWindowGeometry` launcher).

## Importers (GUI binaries only)

- `main_desktop.go` (native desktop, `!nogui`).
- `cmd/agent-overflow-windows/main.go` (WSL launcher, `windows`).

The nogui WSL backend never imports this, keeping Wails out of that binary.
Same isolation rule as `internal/uikeys`.

## Anti-patterns

- Do NOT put placement math here; delegate to `windowgeom` so it stays
  testable without a window.
- Do NOT read window geometry during teardown; `Flush` persists the tracker's
  in-memory latest, which is safe after the window is destroyed.
