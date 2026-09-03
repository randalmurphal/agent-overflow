package uiwindow

import (
	"testing"

	"github.com/wailsapp/wails/v3/pkg/application"

	"agent-overflow/internal/windowgeom"
)

// screenPrimary is a single-monitor work area used across the placement tests.
var screenPrimary = windowgeom.Rect{X: 0, Y: 0, Width: 1920, Height: 1080}

func rects(rs ...windowgeom.Rect) []windowgeom.Rect { return rs }

func TestPrepareOptionsIgnoresNeverSavedPlacement(t *testing.T) {
	opts := application.WebviewWindowOptions{Width: 1280, Height: 800}
	got, act := prepareOptions(&opts, windowgeom.Geometry{X: 50, Y: 50, Width: 900, Height: 700, Valid: false}, rects(screenPrimary))

	if got.Valid {
		t.Fatalf("returned geometry = %+v, want zero (never saved)", got)
	}
	if act.reveal() {
		t.Fatalf("actions = %+v, want none", act)
	}
	if opts.Hidden || opts.InitialPosition != application.WindowCentered || opts.X != 0 || opts.Y != 0 {
		t.Fatalf("opts mutated = %+v, want defaults (visible, centered, no X/Y)", opts)
	}
	if opts.Width != 1280 || opts.Height != 800 {
		t.Fatalf("opts size = %dx%d, want default 1280x800 untouched", opts.Width, opts.Height)
	}
}

func TestPrepareOptionsRestoresNormalPlacement(t *testing.T) {
	opts := application.WebviewWindowOptions{Width: 1280, Height: 800}
	saved := windowgeom.Geometry{X: 100, Y: 120, Width: 1000, Height: 740, Valid: true}
	got, act := prepareOptions(&opts, saved, rects(screenPrimary))

	if !got.Valid {
		t.Fatal("returned geometry not valid")
	}
	if act.reveal() {
		t.Fatalf("actions = %+v, want none for a normal window", act)
	}
	// A normal window is created visible at its restored position.
	if opts.Hidden {
		t.Fatal("opts.Hidden = true, want false for a normal window")
	}
	if opts.InitialPosition != application.WindowXY {
		t.Fatalf("opts.InitialPosition = %v, want WindowXY", opts.InitialPosition)
	}
	if opts.X != 100 || opts.Y != 120 || opts.Width != 1000 || opts.Height != 740 {
		t.Fatalf("opts placement = %d,%d %dx%d, want 100,120 1000x740", opts.X, opts.Y, opts.Width, opts.Height)
	}
}

func TestPrepareOptionsMaximizedHidesAndDefersWithoutStartState(t *testing.T) {
	opts := application.WebviewWindowOptions{Width: 1280, Height: 800}
	saved := windowgeom.Geometry{X: 100, Y: 120, Width: 1000, Height: 740, Maximized: true, Valid: true}
	got, act := prepareOptions(&opts, saved, rects(screenPrimary))

	if !act.maximize || act.fullscreen {
		t.Fatalf("actions = %+v, want maximize only", act)
	}
	// Created hidden at the NORMAL rect so the caller can maximize it on the
	// right monitor and reveal it already maximized — no normal-size flash.
	if !opts.Hidden {
		t.Fatal("opts.Hidden = false, want true for a deferred maximize")
	}
	if opts.InitialPosition != application.WindowXY || opts.X != 100 || opts.Y != 120 {
		t.Fatalf("opts position = %v %d,%d, want normal rect 100,120", opts.InitialPosition, opts.X, opts.Y)
	}
	if opts.Width != 1000 || opts.Height != 740 {
		t.Fatalf("opts size = %dx%d, want normal 1000x740", opts.Width, opts.Height)
	}
	// The maximize-then-position start state must NEVER be used — it's the bug.
	if opts.StartState != application.WindowStateNormal {
		t.Fatalf("opts.StartState = %v, want WindowStateNormal (deferred, not a start state)", opts.StartState)
	}
	// The returned normal rect seeds the tracker for un-maximize.
	if got.X != 100 || got.Y != 120 || got.Width != 1000 || got.Height != 740 || !got.Maximized {
		t.Fatalf("returned geometry = %+v, want maximized normal rect 100,120 1000x740", got)
	}
}

func TestPrepareOptionsMaximizedOffOwnEdgeStaysOnScreen(t *testing.T) {
	// Regression: a window maximized on a monitor whose saved normal rect hangs
	// off the monitor's right edge (x+width > display width) reopened maximized
	// fully off-screen. With no live screen list (creation time), the saved
	// Display must be the clamp reference and the deferred-maximize must be
	// positioned on-screen.
	opts := application.WebviewWindowOptions{Width: 1280, Height: 800}
	saved := windowgeom.Geometry{
		X: 1541, Y: 296, Width: 1280, Height: 800, // 1541+1280 = 2821 > 2560
		Maximized: true,
		Display:   windowgeom.Rect{X: 0, Y: 0, Width: 2560, Height: 1392},
		Valid:     true,
	}
	got, act := prepareOptions(&opts, saved, nil) // no live screens

	if !act.maximize {
		t.Fatalf("actions = %+v, want maximize", act)
	}
	if !opts.Hidden {
		t.Fatal("opts.Hidden = false, want true")
	}
	if opts.X+opts.Width > saved.Display.Width {
		t.Fatalf("opts X+W = %d, want <= %d (positioned on-screen)", opts.X+opts.Width, saved.Display.Width)
	}
	if got.X+got.Width > saved.Display.Width {
		t.Fatalf("returned normal rect X+W = %d, want <= %d (on-screen)", got.X+got.Width, saved.Display.Width)
	}
}

func TestPrepareOptionsMaximizedWithoutNormalRectStillMaximizes(t *testing.T) {
	// The Tracker persists a maximized geometry with a zero normal rect when the
	// first observed event is a maximize. It must still open maximized; the
	// start state fills a screen on its own.
	cases := map[string][]windowgeom.Rect{
		"no screens, display fallback": nil,
		"live screens present":         rects(windowgeom.Rect{X: 0, Y: 0, Width: 2560, Height: 1392}),
	}
	for name, screens := range cases {
		t.Run(name, func(t *testing.T) {
			opts := application.WebviewWindowOptions{Width: 1280, Height: 800}
			saved := windowgeom.Geometry{
				Maximized: true, // no normal rect: Width/Height zero
				Display:   windowgeom.Rect{X: 0, Y: 0, Width: 2560, Height: 1392},
				Valid:     true,
			}
			got, act := prepareOptions(&opts, saved, screens)

			if !act.maximize {
				t.Fatalf("actions = %+v, want maximize", act)
			}
			if !opts.Hidden {
				t.Fatal("opts.Hidden = false, want true")
			}
			if !got.Maximized || !got.Valid {
				t.Fatalf("returned geometry = %+v, want maximized+valid to seed the tracker", got)
			}
			// No normal rect → leave size at the caller's defaults, no position.
			if opts.Width != 1280 || opts.Height != 800 {
				t.Fatalf("opts size = %dx%d, want default 1280x800 (no normal rect)", opts.Width, opts.Height)
			}
			if opts.InitialPosition != application.WindowCentered || opts.X != 0 || opts.Y != 0 {
				t.Fatalf("opts position = %v %d,%d, want unset (centers on primary)", opts.InitialPosition, opts.X, opts.Y)
			}
		})
	}
}

func TestPrepareOptionsFullscreenHidesAndDefers(t *testing.T) {
	opts := application.WebviewWindowOptions{Width: 1280, Height: 800}
	saved := windowgeom.Geometry{X: 100, Y: 120, Width: 1000, Height: 740, Fullscreen: true, Valid: true}
	_, act := prepareOptions(&opts, saved, rects(screenPrimary))

	if !act.fullscreen || act.maximize {
		t.Fatalf("actions = %+v, want fullscreen only", act)
	}
	if !opts.Hidden {
		t.Fatal("opts.Hidden = false, want true for a deferred fullscreen")
	}
	if opts.StartState != application.WindowStateNormal {
		t.Fatalf("opts.StartState = %v, want WindowStateNormal", opts.StartState)
	}
	if opts.Width != 1000 || opts.Height != 740 {
		t.Fatalf("opts size = %dx%d, want normal 1000x740", opts.Width, opts.Height)
	}
}

func TestPrepareOptionsClampsAgainstSavedDisplayWhenScreensUnknown(t *testing.T) {
	// A normal window saved hanging off its own monitor's edge, with no live
	// screen list yet, is pulled fully on-screen using the saved Display.
	opts := application.WebviewWindowOptions{Width: 1280, Height: 800}
	saved := windowgeom.Geometry{
		X: 1541, Y: 296, Width: 1280, Height: 800,
		Display: windowgeom.Rect{X: 0, Y: 0, Width: 2560, Height: 1392},
		Valid:   true,
	}
	prepareOptions(&opts, saved, nil)

	if opts.Hidden {
		t.Fatal("opts.Hidden = true, want false for a normal window")
	}
	if opts.InitialPosition != application.WindowXY {
		t.Fatalf("opts.InitialPosition = %v, want WindowXY", opts.InitialPosition)
	}
	if opts.X+opts.Width > saved.Display.Width {
		t.Fatalf("opts X+W = %d, want <= %d (shifted on-screen)", opts.X+opts.Width, saved.Display.Width)
	}
	if opts.X != saved.Display.Width-opts.Width {
		t.Fatalf("opts.X = %d, want %d (flush to right edge)", opts.X, saved.Display.Width-opts.Width)
	}
}

func TestPrepareOptionsTrustsSavedWhenNoScreensOrDisplay(t *testing.T) {
	// No live screens yet AND no saved Display to clamp against: trust the saved
	// placement as-is rather than discard it.
	opts := application.WebviewWindowOptions{Width: 1280, Height: 800}
	saved := windowgeom.Geometry{X: 300, Y: 200, Width: 1000, Height: 740, Valid: true}
	got, _ := prepareOptions(&opts, saved, nil)

	if !got.Valid || opts.Hidden || opts.InitialPosition != application.WindowXY || opts.X != 300 || opts.Y != 200 {
		t.Fatalf("opts = %+v, returned = %+v, want saved placement trusted (visible)", opts, got)
	}
}

func TestPrepareOptionsMaximizedTrustedWhenNoScreensOrDisplay(t *testing.T) {
	// Maximized, no live screens AND no saved Display: the normal rect is
	// trusted as-is, but the deferred-maximize must still be set up. This path
	// is subtle — `act` is computed before the clamp and Hidden is set by the
	// trailing reveal() check, not the off-screen branch — so guard it directly.
	opts := application.WebviewWindowOptions{Width: 1280, Height: 800}
	saved := windowgeom.Geometry{X: 300, Y: 200, Width: 1000, Height: 740, Maximized: true, Valid: true}
	got, act := prepareOptions(&opts, saved, nil)

	if !act.maximize {
		t.Fatalf("actions = %+v, want maximize", act)
	}
	if !opts.Hidden {
		t.Fatal("opts.Hidden = false, want true (deferred maximize on the trust-as-is path)")
	}
	if opts.StartState != application.WindowStateNormal {
		t.Fatalf("opts.StartState = %v, want WindowStateNormal", opts.StartState)
	}
	// Trusted as-is: position + size are the saved normal rect.
	if opts.InitialPosition != application.WindowXY || opts.X != 300 || opts.Y != 200 {
		t.Fatalf("opts position = %v %d,%d, want trusted 300,200", opts.InitialPosition, opts.X, opts.Y)
	}
	if opts.Width != 1000 || opts.Height != 740 {
		t.Fatalf("opts size = %dx%d, want trusted 1000x740", opts.Width, opts.Height)
	}
	if !got.Maximized || !got.Valid {
		t.Fatalf("returned geometry = %+v, want maximized+valid to seed the tracker", got)
	}
}

func TestPrepareOptionsCentersWhenNormalWindowOffEveryKnownScreen(t *testing.T) {
	// Monitor the window lived on was unplugged; screens are known and the saved
	// rect overlaps none → centered default.
	opts := application.WebviewWindowOptions{Width: 1280, Height: 800}
	saved := windowgeom.Geometry{X: 4000, Y: 3000, Width: 1000, Height: 740, Valid: true}
	got, act := prepareOptions(&opts, saved, rects(screenPrimary))

	if got.Valid {
		t.Fatalf("returned geometry = %+v, want zero (off-screen → center)", got)
	}
	if act.reveal() {
		t.Fatalf("actions = %+v, want none", act)
	}
	if opts.Hidden || opts.InitialPosition != application.WindowCentered || opts.X != 0 || opts.Y != 0 {
		t.Fatalf("opts mutated = %+v, want centered default", opts)
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
