package uiwindow

import (
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"agent-overflow/internal/windowgeom"
)

// saveDebounce is how long the tracker waits after the last window event
// before persisting. Move/resize events fire continuously during a drag; the
// settings/state file is rewritten atomically per save, so a debounce keeps
// that to roughly one write per gesture instead of one per pixel.
const saveDebounce = 500 * time.Millisecond

// geometryEvents are the window events that can change the persisted placement.
// events.Common is initialized at events-package init, before this var, so the
// values are populated by the time it is built.
var geometryEvents = []events.WindowEventType{
	events.Common.WindowDidMove,
	events.Common.WindowDidResize,
	events.Common.WindowMaximise,
	events.Common.WindowUnMaximise,
	events.Common.WindowFullscreen,
	events.Common.WindowUnFullscreen,
}

// actions is what RestoreAndTrack must do to the live window after creating it,
// beyond the position/size already written into the creation options. At most
// one of maximize/fullscreen is set (a window can't be both); fullscreen wins.
type actions struct {
	maximize   bool
	fullscreen bool
}

// reveal reports whether the window was created hidden and therefore needs an
// explicit Show after the deferred maximize/fullscreen.
func (a actions) reveal() bool { return a.maximize || a.fullscreen }

// prepareOptions writes saved into the window-creation options and returns the
// placement to seed Track plus the deferred actions RestoreAndTrack applies once
// the window is live. It is pure (no window, no Wails calls beyond the options
// struct) so the placement decisions stay unit-testable.
//
// The maximize/fullscreen case is the crux. Wails (alpha) maximizes at creation
// *before* it applies X/Y, which drags the monitor-sized window off-screen — and
// there is no creation-option ordering that positions first. So instead of a
// maximized start state we:
//
//   - create the window HIDDEN at the clamped *normal* rect (on the correct
//     monitor), and
//   - report maximize/fullscreen back to the caller, which maximizes the live
//     window (it lands on whatever monitor it currently occupies — the one we
//     positioned it on) and only then reveals it.
//
// The window is never visible at normal size, so there's no expand/flash, and it
// maximizes on the monitor it was saved on rather than always the primary.
//
// screens is the live screen layout (empty when not yet enumerable); the saved
// Display is the fallback reference so an off-its-own-edge rect is still pulled
// on-screen.
func prepareOptions(opts *application.WebviewWindowOptions, saved windowgeom.Geometry, screens []windowgeom.Rect) (windowgeom.Geometry, actions) {
	if !saved.Valid {
		return windowgeom.Geometry{}, actions{}
	}

	// fullscreen wins over maximize; they're never both meaningfully set.
	act := actions{fullscreen: saved.Fullscreen, maximize: saved.Maximized && !saved.Fullscreen}

	if len(screens) == 0 && saved.Display.Width > 0 && saved.Display.Height > 0 {
		screens = []windowgeom.Rect{{
			X: saved.Display.X, Y: saved.Display.Y,
			Width: saved.Display.Width, Height: saved.Display.Height,
		}}
	}

	g, ok := saved.Clamp(screens)
	if !ok {
		switch {
		case len(screens) == 0:
			// No screens and no saved Display to validate against: trust the
			// saved rect as-is. The OS keeps new windows on-screen and tracking
			// re-validates on the first move.
			g = saved
		case act.reveal():
			// Off every known screen, or no normal size recorded yet. Create
			// hidden with no position so it centers on the primary, then the
			// caller maximizes/fullscreens there — the start state survives even
			// without a usable normal rect.
			opts.Hidden = true
			return windowgeom.Geometry{
				Maximized:  saved.Maximized,
				Fullscreen: saved.Fullscreen,
				Display:    saved.Display,
				Valid:      true,
			}, act
		default:
			// Normal window off every known screen → centered default.
			return windowgeom.Geometry{}, actions{}
		}
	}

	// Restore size. After a successful Clamp these are always > 0; the guards
	// only matter on the trust-as-is fall-through above, where g is the
	// unclamped saved rect and may be zero-sized.
	if g.Width > 0 {
		opts.Width = g.Width
	}
	if g.Height > 0 {
		opts.Height = g.Height
	}
	// Position the (normal-rect) window on the correct monitor. For the
	// maximize/fullscreen case we also hide it so the deferred state change is
	// the first thing the user sees.
	if g.Width > 0 && g.Height > 0 {
		opts.InitialPosition = application.WindowXY
		opts.X = g.X
		opts.Y = g.Y
	}
	if act.reveal() {
		opts.Hidden = true
	}
	return g, act
}

// RestoreAndTrack creates the application window with the saved placement
// restored, reveals it (already maximized/fullscreen when that was the saved
// mode, on the monitor it was saved on — no normal-size flash), and wires
// geometry tracking. It returns the window and the tracker's flush func.
//
// It MUST be called from an ApplicationStarted handler (app.running == true):
// only then does NewWithOptions materialize the window synchronously, so the
// subsequent Maximise/Fullscreen/Show act on a live impl instead of silently
// degrading to the buggy maximize-then-position start state. The deferred window
// calls self-dispatch to the main thread, so calling them from the event
// goroutine is safe.
func RestoreAndTrack(app *application.App, base application.WebviewWindowOptions, saved windowgeom.Geometry, sink func(windowgeom.Geometry)) (*application.WebviewWindow, func()) {
	restored, act := prepareOptions(&base, saved, screenRects(app.Screen.GetAll()))

	window := app.Window.NewWithOptions(base)

	// Maximize/fullscreen the now-live window. It currently sits at the normal
	// rect we positioned it on, so it lands on the correct monitor; then reveal
	// it so the maximized/fullscreen state is the first thing painted.
	switch {
	case act.fullscreen:
		window.Fullscreen()
	case act.maximize:
		window.Maximise()
	}
	if act.reveal() {
		window.Show()
	}

	return window, Track(window, restored, sink)
}

// Track wires w's geometry-affecting events to a debounced persistence sink and
// returns a flush function. restored is the placement the window opened with so
// the first maximize/fullscreen event preserves a real normal rect.
//
// The returned flush persists the latest geometry immediately and stops
// tracking; it is also wired to WindowClosing. Callers should additionally call
// it once after the app loop returns as a backstop — it uses the in-memory
// latest, so it is safe even after the window is destroyed.
func Track(w *application.WebviewWindow, restored windowgeom.Geometry, sink func(windowgeom.Geometry)) func() {
	tr := windowgeom.NewTracker(saveDebounce, sink, restored)
	record := func(*application.WindowEvent) { tr.Record(sampleWindow(w)) }
	for _, et := range geometryEvents {
		w.OnWindowEvent(et, record)
	}
	// Flush WITHOUT sampling: by WindowClosing the window can already be
	// decomposing its state (Windows reports a closing fullscreen window as
	// un-fullscreened + maximized), so a sample here corrupts the persisted
	// flags. Flush writes the tracker's in-memory latest — built only from
	// trustworthy pre-close events — and stops the tracker so teardown
	// transition events are ignored.
	w.OnWindowEvent(events.Common.WindowClosing, func(*application.WindowEvent) { tr.Flush() })
	return tr.Flush
}

func sampleWindow(w *application.WebviewWindow) windowgeom.Sample {
	b := w.Bounds()
	return windowgeom.Sample{
		Bounds:     windowgeom.Rect{X: b.X, Y: b.Y, Width: b.Width, Height: b.Height},
		Maximized:  w.IsMaximised(),
		Fullscreen: w.IsFullscreen(),
		Minimized:  w.IsMinimised(),
		Display:    workArea(w),
	}
}

func workArea(w *application.WebviewWindow) windowgeom.Rect {
	s, err := w.GetScreen()
	if err != nil || s == nil {
		return windowgeom.Rect{}
	}
	return rect(s.WorkArea)
}

func screenRects(screens []*application.Screen) []windowgeom.Rect {
	out := make([]windowgeom.Rect, 0, len(screens))
	for _, s := range screens {
		if s == nil || s.WorkArea.Width <= 0 || s.WorkArea.Height <= 0 {
			continue
		}
		out = append(out, rect(s.WorkArea))
	}
	return out
}

func rect(r application.Rect) windowgeom.Rect {
	return windowgeom.Rect{X: r.X, Y: r.Y, Width: r.Width, Height: r.Height}
}
