package windowgeom

import (
	"sync"
	"time"
)

// Sample is one raw observation of window state taken from a move/resize/state
// window event, before the keep-last-normal-rect and skip-minimized policy is
// applied. Callers fill it from the live window at the GUI boundary.
type Sample struct {
	Bounds     Rect
	Maximized  bool
	Fullscreen bool
	Minimized  bool
	// Display is the work area of the screen the window is currently on, or
	// the zero Rect when it could not be determined.
	Display Rect
}

// Tracker coalesces a burst of window events into at most one persisted write
// per quiet period, and owns the policy that keeps the stored placement
// sensible:
//
//   - Minimized samples are dropped entirely. Some platforms report bogus
//     off-screen coordinates (e.g. Windows' (-32000,-32000)) while minimized;
//     persisting those would strand the window on next launch.
//   - While maximized or fullscreen, the flags and current Display are
//     updated but the normal rect is preserved, so un-maximizing restores to
//     a real window (not the maximized bounds) while restore still lands on
//     the monitor the window actually occupied.
//
// Construct with NewTracker. All methods are safe for concurrent use; the sink
// is always invoked off the caller's goroutine without the lock held.
type Tracker struct {
	debounce time.Duration
	sink     func(Geometry)

	mu      sync.Mutex
	latest  Geometry
	hasData bool
	timer   *time.Timer
	stopped bool
}

// NewTracker returns a Tracker that writes through sink no more than once per
// debounce interval of quiet. initial seeds the accumulated geometry — pass the
// placement the window was just restored to so the very first Maximise event
// preserves a real normal rect rather than seeding from the maximized bounds.
// Pass the zero Geometry when there was nothing to restore (first launch).
func NewTracker(debounce time.Duration, sink func(Geometry), initial Geometry) *Tracker {
	t := &Tracker{debounce: debounce, sink: sink}
	if initial.Valid {
		t.latest = initial
		t.hasData = true
	}
	return t
}

// Record folds a sample into the pending geometry and (re)schedules the
// debounced write. It returns immediately; the write happens later on the
// timer's goroutine.
func (t *Tracker) Record(s Sample) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped || s.Minimized {
		return
	}

	if !t.hasData {
		// First observation with nothing restored: mark it valid, but leave the
		// normal rect to the non-maximized branch below. Seeding the rect
		// unconditionally here would capture the maximized bounds when the
		// first sample is itself maximized, which is exactly what we must never
		// persist as the restore rect.
		t.hasData = true
		t.latest.Valid = true
	}

	t.latest.Maximized = s.Maximized
	t.latest.Fullscreen = s.Fullscreen
	// The current screen is trustworthy in every non-minimized state — a
	// maximized/fullscreen window unambiguously occupies its screen — so track
	// it on every sample. This is what keeps restore on the right monitor when
	// the window changes screens while (or racing into) maximized/fullscreen:
	// samples are read from the live window at handler-run time, so a fast
	// move-then-maximize gesture can deliver every sample already maximized,
	// and the flag-only branch below would otherwise freeze the old Display.
	if !s.Display.empty() {
		t.latest.Display = s.Display
	}
	// Only a normal (non-maximized, non-fullscreen) sample updates the stored
	// rect: while maximized/fullscreen the event reports the maximized bounds,
	// which must NOT overwrite the rect we restore to on un-maximize. Restore
	// re-anchors this rect to the tracked Display via Clamp, so a stale rect
	// from another screen still reopens on the correct monitor.
	if !s.Maximized && !s.Fullscreen && !s.Bounds.empty() {
		t.latest.X = s.Bounds.X
		t.latest.Y = s.Bounds.Y
		t.latest.Width = s.Bounds.Width
		t.latest.Height = s.Bounds.Height
	}

	if t.timer != nil {
		t.timer.Stop()
	}
	t.timer = time.AfterFunc(t.debounce, t.fire)
}

func (t *Tracker) fire() {
	t.mu.Lock()
	if t.stopped || !t.hasData {
		t.mu.Unlock()
		return
	}
	g := t.latest
	t.mu.Unlock()
	t.sink(g)
}

// Flush persists the pending geometry immediately, cancels any scheduled write,
// and stops the tracker so later events are ignored. Call it once when the
// window closes (and again as a backstop after the app loop returns — it uses
// the in-memory latest, so it is safe even after the window is destroyed).
// No-ops when nothing has been recorded.
func (t *Tracker) Flush() {
	t.mu.Lock()
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	already := t.stopped
	t.stopped = true
	if already || !t.hasData {
		t.mu.Unlock()
		return
	}
	g := t.latest
	t.mu.Unlock()
	t.sink(g)
}
