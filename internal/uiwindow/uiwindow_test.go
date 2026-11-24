package uiwindow

import (
	"testing"

	"github.com/wailsapp/wails/v3/pkg/application"

	"agent-overflow/internal/windowgeom"
)

// screenPrimary is a single-monitor work area used across the placement tests.
var screenPrimary = windowgeom.Rect{X: 0, Y: 0, Width: 1920, Height: 1080}

func rects(rs ...windowgeom.Rect) []windowgeom.Rect { return rs }

// A normal window is created visible with no deferred action: at its saved
// placement, clamped on-screen, or at the centered default when there is no
// placement to restore. The options start at the default 1280x800.
func TestPrepareOptionsRestoresNormalPlacement(t *testing.T) {
	ownDisplay := windowgeom.Rect{X: 0, Y: 0, Width: 2560, Height: 1392}
	for _, tc := range []struct {
		name    string
		saved   windowgeom.Geometry
		screens []windowgeom.Rect
		// valid is the returned geometry's; at is the window's placement.
		valid    bool
		position application.WindowStartPosition
		at       windowgeom.Rect
	}{
		{"never saved", windowgeom.Geometry{X: 50, Y: 50, Width: 900, Height: 700, Valid: false}, rects(screenPrimary),
			false, application.WindowCentered, windowgeom.Rect{Width: 1280, Height: 800}},
		{"on a live screen", windowgeom.Geometry{X: 100, Y: 120, Width: 1000, Height: 740, Valid: true}, rects(screenPrimary),
			true, application.WindowXY, windowgeom.Rect{X: 100, Y: 120, Width: 1000, Height: 740}},
		// Saved hanging off its own monitor's edge, with no live screen list
		// yet: pulled flush to the saved Display's right edge.
		{"off its own edge, screens unknown", windowgeom.Geometry{X: 1541, Y: 296, Width: 1280, Height: 800, Display: ownDisplay, Valid: true}, nil,
			true, application.WindowXY, windowgeom.Rect{X: 1280, Y: 296, Width: 1280, Height: 800}},
		// No live screens AND no saved Display to clamp against: trusted as-is
		// rather than discarded.
		{"no screens or display", windowgeom.Geometry{X: 300, Y: 200, Width: 1000, Height: 740, Valid: true}, nil,
			true, application.WindowXY, windowgeom.Rect{X: 300, Y: 200, Width: 1000, Height: 740}},
		// The monitor it lived on was unplugged: screens are known and the
		// saved rect overlaps none.
		{"off every known screen", windowgeom.Geometry{X: 4000, Y: 3000, Width: 1000, Height: 740, Valid: true}, rects(screenPrimary),
			false, application.WindowCentered, windowgeom.Rect{Width: 1280, Height: 800}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := application.WebviewWindowOptions{Width: 1280, Height: 800}
			got, act := prepareOptions(&opts, tc.saved, tc.screens)

			if got.Valid != tc.valid {
				t.Fatalf("returned geometry = %+v, want valid %v", got, tc.valid)
			}
			if act.reveal() {
				t.Fatalf("actions = %+v, want none for a normal window", act)
			}
			if opts.Hidden {
				t.Fatal("opts.Hidden = true, want false for a normal window")
			}
			if placed := (windowgeom.Rect{X: opts.X, Y: opts.Y, Width: opts.Width, Height: opts.Height}); opts.InitialPosition != tc.position || placed != tc.at {
				t.Fatalf("opts placement = %v %+v, want %v %+v", opts.InitialPosition, placed, tc.position, tc.at)
			}
		})
	}
}

// A maximized or fullscreen window is created HIDDEN at its clamped NORMAL rect
// so the caller can apply the state on the right monitor and reveal it already
// maximized: no normal-size flash. The maximize-then-position start state must
// NEVER be used; it is the bug. The returned normal rect seeds the tracker for
// un-maximize.
func TestPrepareOptionsMaximizedHidesAndDefersWithoutStartState(t *testing.T) {
	ownDisplay := windowgeom.Rect{X: 0, Y: 0, Width: 2560, Height: 1392}
	maximize, fullscreen := actions{maximize: true}, actions{fullscreen: true}
	for _, tc := range []struct {
		name    string
		saved   windowgeom.Geometry
		screens []windowgeom.Rect
		act     actions
		// position and at are the window's placement; normal is the returned rect.
		position application.WindowStartPosition
		at       windowgeom.Rect
		normal   windowgeom.Rect
	}{
		{"maximized on a live screen", windowgeom.Geometry{X: 100, Y: 120, Width: 1000, Height: 740, Maximized: true, Valid: true}, rects(screenPrimary),
			maximize, application.WindowXY, windowgeom.Rect{X: 100, Y: 120, Width: 1000, Height: 740}, windowgeom.Rect{X: 100, Y: 120, Width: 1000, Height: 740}},
		// Regression: a window maximized on a monitor whose saved normal rect
		// hangs off its right edge reopened maximized fully off-screen. With no
		// live screen list (creation time), the saved Display is the clamp
		// reference.
		{"maximized off its own edge, screens unknown", windowgeom.Geometry{X: 1541, Y: 296, Width: 1280, Height: 800, Maximized: true, Display: ownDisplay, Valid: true}, nil,
			maximize, application.WindowXY, windowgeom.Rect{X: 1280, Y: 296, Width: 1280, Height: 800}, windowgeom.Rect{X: 1280, Y: 296, Width: 1280, Height: 800}},
		// No live screens AND no saved Display: the normal rect is trusted
		// as-is, and Hidden comes from the trailing reveal() check rather than
		// the off-screen branch.
		{"maximized, no screens or display", windowgeom.Geometry{X: 300, Y: 200, Width: 1000, Height: 740, Maximized: true, Valid: true}, nil,
			maximize, application.WindowXY, windowgeom.Rect{X: 300, Y: 200, Width: 1000, Height: 740}, windowgeom.Rect{X: 300, Y: 200, Width: 1000, Height: 740}},
		// The Tracker persists a zero normal rect when the first observed
		// event is a maximize. It still opens maximized, centered at the
		// caller's default size; the start state fills a screen on its own.
		{"maximized without a normal rect, display fallback", windowgeom.Geometry{Maximized: true, Display: ownDisplay, Valid: true}, nil,
			maximize, application.WindowCentered, windowgeom.Rect{Width: 1280, Height: 800}, windowgeom.Rect{}},
		{"maximized without a normal rect, live screens", windowgeom.Geometry{Maximized: true, Display: ownDisplay, Valid: true}, rects(ownDisplay),
			maximize, application.WindowCentered, windowgeom.Rect{Width: 1280, Height: 800}, windowgeom.Rect{}},
		{"fullscreen", windowgeom.Geometry{X: 100, Y: 120, Width: 1000, Height: 740, Fullscreen: true, Valid: true}, rects(screenPrimary),
			fullscreen, application.WindowXY, windowgeom.Rect{X: 100, Y: 120, Width: 1000, Height: 740}, windowgeom.Rect{X: 100, Y: 120, Width: 1000, Height: 740}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := application.WebviewWindowOptions{Width: 1280, Height: 800}
			got, act := prepareOptions(&opts, tc.saved, tc.screens)

			if act != tc.act {
				t.Fatalf("actions = %+v, want %+v", act, tc.act)
			}
			if !opts.Hidden {
				t.Fatal("opts.Hidden = false, want true for a deferred state")
			}
			if opts.StartState != application.WindowStateNormal {
				t.Fatalf("opts.StartState = %v, want WindowStateNormal (deferred, not a start state)", opts.StartState)
			}
			if placed := (windowgeom.Rect{X: opts.X, Y: opts.Y, Width: opts.Width, Height: opts.Height}); opts.InitialPosition != tc.position || placed != tc.at {
				t.Fatalf("opts placement = %v %+v, want %v %+v", opts.InitialPosition, placed, tc.position, tc.at)
			}
			normal := windowgeom.Rect{X: got.X, Y: got.Y, Width: got.Width, Height: got.Height}
			if normal != tc.normal || got.Maximized != tc.saved.Maximized || got.Fullscreen != tc.saved.Fullscreen || !got.Valid {
				t.Fatalf("returned geometry = %+v, want normal rect %+v with the saved state, valid", got, tc.normal)
			}
		})
	}
}

// TestDeliverPageTicketToleratesAnAbsentWindow: the ticket delivery is
// wired from an ApplicationStarted handler, which is exactly the boot
// path where a window or a backend may not exist yet. Neither is worth a
// panic on a path whose failure mode is already "the page waits and
// retries".
func TestDeliverPageTicketToleratesAnAbsentWindow(t *testing.T) {
	stop := DeliverPageTicket(nil, func() (string, error) { return "ticket", nil })
	if stop == nil {
		t.Fatal("DeliverPageTicket returned no unsubscribe func")
	}
	stop()

	stop = DeliverPageTicket(&application.WebviewWindow{}, nil)
	if stop == nil {
		t.Fatal("DeliverPageTicket returned no unsubscribe func for a nil mint")
	}
	stop()
}

// A window without a sink shows the saved placement without owning it: it
// subscribes to none of the window's events and its flush writes nothing.
func TestTrackWithoutASinkOwnsNothing(t *testing.T) {
	flush := Track(nil, windowgeom.Geometry{X: 10, Y: 10, Width: 900, Height: 700, Valid: true}, nil)
	flush()
	flush()
}
