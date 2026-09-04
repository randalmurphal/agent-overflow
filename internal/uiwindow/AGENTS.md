# internal/uiwindow/

Wails glue for a live `WebviewWindow`, in three unrelated jobs. Placement: the
GUI-free logic in `internal/windowgeom`, restored into creation options and
wired to a debounced persistence sink. Credential delivery: handing each
document the window loads its one-time page ticket, so the URL it navigates to
carries none (`internal/pagehost`). Reveal: bringing the window forward
without changing its size.

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
- `reveal.go`
  - `Reveal(window)` brings the window forward without changing its size:
    `Show`, `UnMinimise` only if minimised, `Focus`. Every "bring the app
    forward" path (OS notification click, second launch of the binary)
    goes through it. NEVER `Window.Restore()` for this: Wails defines
    Restore as "undo minimised / fullscreen / maximised", so revealing a
    maximized window through it drops the window to its normal size (that
    was the 2026-09-03 notification-click shrink). `reveal_test.go` fails
    the build on any zero-arg `.Restore()` call in a file importing the
    Wails application package.
- `pageticket.go`
  - `DeliverPageTicket(window, mint)` subscribes to `WindowRuntimeReady` and
    answers each one by minting a ticket and `ExecJS`-ing
    `pagehost.DeliveryScript`. Returns the unsubscribe. **The trigger is not a
    free choice**: `ExecJS` QUEUES until a document announces itself to its
    host, and this app's SPA replaces `@wailsio/runtime` with its own transport
    shim, so nothing announces unless the page does — which it does, from
    `frontend/src/lib/transport/pageHost.ts`, and re-announces on a bounded
    cadence until its ticket lands. That re-announcement is what covers the one
    race a subscription cannot: a document that finished loading before the
    caller subscribed. Minting per ANNOUNCEMENT (not per navigation) is what
    gives a page that reloaded itself a live ticket; re-delivering a spent one
    is harmless, since such a page already holds the cookie and
    `Credential.Exchange` authenticates before it looks at a ticket.

## Responsibility boundary

- What BELONGS here: the Wails-specific reads (`Bounds`, `IsMaximised`,
  `GetScreen`, the event types), the options mutation, and the `ExecJS` call.
  Like `internal/uikeys` it imports the Wails `application` package.
- What does NOT belong here: the placement decision logic (that's
  `windowgeom`), persistence (the sink is supplied by the caller:
  `App.persistWindowGeometry` native, `saveWindowGeometry` launcher), or the
  page-ticket vocabulary and the script text (that's `pagehost`, which stays
  stdlib-only so the page contract has one definition and no Wails dependency).
  Minting is the caller's too — each host passes the `mint` its own backend
  reaches.
- Every `Bounds`, creation-option size, and `Screen.WorkArea` crossing this
  boundary is DIP. On macOS that depends on the pinned Wails fork converting
  requested outer frames to AppKit content rects at construction and resolving
  `GetScreen` through the laid-out screen cache; returning raw `NSScreen`
  values recreates progressive title-bar growth and 2x Retina display records.

## Importers (GUI binaries only)

- `main_desktop.go` (native desktop and the `--connect` stub's window,
  `!nogui`).
- `internal/app/app_notifications_desktop.go` (`!nogui`): `Reveal` on a
  notification click.
- `cmd/agent-overflow-windows/main.go` (WSL launcher, `windows`). It gates
  `DeliverPageTicket`'s `mint` on having a backend bootstrap, so the picker,
  loading and error pages — which are not the SPA and never announce — cannot
  be injected into.

The nogui WSL backend never imports this, keeping Wails out of that binary.
Same isolation rule as `internal/uikeys`.

## Anti-patterns

- Do NOT put placement math here; delegate to `windowgeom` so it stays
  testable without a window.
- Do NOT read window geometry during teardown; `Flush` persists the tracker's
  in-memory latest, which is safe after the window is destroyed.
