package paneplace

import (
	"math"
	"testing"
)

func full(x, y, w, h float64) Rect {
	return Rect{X: x, Y: y, W: w, H: h, ClipX: x, ClipY: y, ClipW: w, ClipH: h}
}

// A page that follows the pane is the rect's whole pixels, so its margin is
// under a pixel. It must start at the rect's origin: a half-pixel centering
// offset put the view between device pixels and blurred it.
func TestFitPutsAFollowingPageAtTheRectOrigin(t *testing.T) {
	rect := full(100.4, 50.75, 800.6, 600.9)
	fitted, scale, ok := Fit(rect, 800, 600)
	if !ok || scale != 1 {
		t.Fatalf("fit = %+v scale=%v ok=%v", fitted, scale, ok)
	}
	if fitted.X != rect.X || fitted.Y != rect.Y || fitted.W != 800 || fitted.H != 600 {
		t.Fatalf("a following page must start at the rect's origin, got %+v", fitted)
	}
}

func TestFitScalesDownCentersByWholePixelsAndClips(t *testing.T) {
	// Width binds; the 150px vertical margin centers by 75.
	fitted, scale, ok := Fit(full(10, 20, 800, 600), 1280, 720)
	if !ok || scale != 0.625 {
		t.Fatalf("scale = %v ok=%v", scale, ok)
	}
	if fitted.X != 10 || fitted.Y != 95 || fitted.W != 800 || fitted.H != 450 {
		t.Fatalf("fitted = %+v", fitted)
	}
	// An odd margin centers by whole pixels, rounding down.
	fitted, _, _ = Fit(full(0, 0, 1001, 721), 1000, 720)
	if fitted.X != 0 || fitted.Y != 0 {
		t.Fatalf("a one-pixel margin must not offset the page: %+v", fitted)
	}
	fitted, _, _ = Fit(full(0, 0, 1003, 720), 1000, 720)
	if fitted.X != 1 {
		t.Fatalf("a three-pixel margin centers by one whole pixel: %+v", fitted)
	}
	// Height binds at one third: the binding axis's float remainder never
	// becomes a negative offset.
	fitted, scale, ok = Fit(full(0, 0, 1000, 300), 600, 900)
	if !ok || scale != 300.0/900.0 || fitted.X != 400 || fitted.Y != 0 || math.Abs(fitted.H-300) > 1e-9 {
		t.Fatalf("tall page = %+v scale=%v", fitted, scale)
	}
	// The clip is intersected with the fitted rect.
	rect := full(0, 0, 800, 600)
	rect.ClipY, rect.ClipH = 300, 300
	fitted, _, ok = Fit(rect, 1280, 720)
	if !ok || fitted.ClipY != 300 || fitted.ClipH != 225 {
		t.Fatalf("clip = %+v ok=%v", fitted, ok)
	}
	rect.ClipY, rect.ClipH = 0, 50
	if _, _, ok := Fit(rect, 1280, 720); ok {
		t.Fatal("a clip above the fitted rect leaves nothing to show")
	}
	if _, _, ok := Fit(Rect{}, 1280, 720); ok {
		t.Fatal("an empty rect places nothing")
	}
	if _, _, ok := Fit(full(0, 0, 800, 600), 0, 600); ok {
		t.Fatal("a page with no size places nothing")
	}
}

func TestSnapEdgeRoundsHalvesUpOnBothSidesOfZero(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want int
	}{
		{0, 0}, {0.49, 0}, {0.5, 1}, {1.5, 2}, {2.4999, 2},
		// Truncation toward zero turned -0.6 into 0 and -1.4 into -1 and
		// shifted a page scrolled off the left edge by a pixel.
		{-0.4, 0}, {-0.5, 0}, {-0.6, -1}, {-1.4, -1}, {-1.5, -1}, {-1.6, -2},
	} {
		if got := SnapEdge(tc.in); got != tc.want {
			t.Errorf("SnapEdge(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// Sizes come from snapped edges, never from a separately rounded size, so the
// page and its clip share edges exactly and a sliding pane never changes its
// pixel width by rounding.
func TestLayoutSnapsEdgesNotSizes(t *testing.T) {
	// x 10.4 + w 20.4 = 30.8: edges 10 and 31 (width 21). Rounding the size on
	// its own gives 20 and drops the right column.
	fitted := Rect{X: 10.4, Y: 0, W: 20.4, H: 10, ClipX: 10.4, ClipY: 0, ClipW: 20.4, ClipH: 10}
	got := mustLayout(t, fitted, 1, 1)
	if got.Clip != (Edges{Left: 10, Top: 0, Right: 31, Bottom: 10}) || got.OffsetX != 0 || got.OffsetY != 0 {
		t.Fatalf("layout = %+v", got)
	}

	// A pane scrolled partly off the left of the window: the page starts at
	// -5.6 CSS px and the clip at 0. Scaled by 1.5: page left -8.4 -> -8.
	fitted = Rect{X: -5.6, Y: 3.3, W: 400, H: 300, ClipX: 0, ClipY: 3.3, ClipW: 394.4, ClipH: 300}
	got = mustLayout(t, fitted, 1.5, 1.5)
	wantClip := Edges{Left: 0, Top: 5, Right: SnapEdge(394.4 * 1.5), Bottom: SnapEdge(303.3 * 1.5)}
	if got.Clip != wantClip {
		t.Fatalf("clip = %+v, want %+v", got.Clip, wantClip)
	}
	if got.OffsetX != -8 || got.OffsetY != 0 {
		t.Fatalf("offset = %d,%d, want -8,0", got.OffsetX, got.OffsetY)
	}

	// A clip that rounds past the page is cropped to the page.
	fitted = Rect{X: 10, Y: 10, W: 100, H: 100, ClipX: 9.6, ClipY: 9.6, ClipW: 101, ClipH: 101}
	if got := mustLayout(t, fitted, 1, 1); got.Clip != (Edges{Left: 10, Top: 10, Right: 110, Bottom: 110}) {
		t.Fatalf("clip must never overhang the page: %+v", got.Clip)
	}

	// An absent clip means unclipped.
	fitted = Rect{X: 10, Y: 10, W: 100, H: 100}
	if got := mustLayout(t, fitted, 2, 2); got.Clip != (Edges{Left: 20, Top: 20, Right: 220, Bottom: 220}) {
		t.Fatalf("an absent clip must mean the whole page: %+v", got.Clip)
	}
}

// mustLayout is Layout for a placement with host pixels to show.
func mustLayout(t *testing.T, fitted Rect, sx, sy float64) HostLayout {
	t.Helper()
	layout, ok := Layout(fitted, sx, sy)
	if !ok {
		t.Fatalf("Layout(%+v, %v, %v) shows nothing", fitted, sx, sy)
	}
	return layout
}

// A supplied clip that keeps a whole CSS pixel (Fit's minimum) can round
// away at the host's scale: at 50%, the CSS interval 1..2 is host edges 1
// and 1. That placement shows nothing, never the whole page.
func TestLayoutHidesAClipThatSnapsEmpty(t *testing.T) {
	fitted := Rect{X: 0, Y: 0, W: 400, H: 300, ClipX: 1, ClipY: 0, ClipW: 1, ClipH: 300}
	if layout, ok := Layout(fitted, 0.5, 0.5); ok {
		t.Fatalf("a clip with no host pixel lays out as %+v", layout)
	}
	// The neighboring interval 2..3 is host edges 1 and 2: one pixel shows.
	fitted.ClipX = 2
	if got := mustLayout(t, fitted, 0.5, 0.5); got.Clip != (Edges{Left: 1, Top: 0, Right: 2, Bottom: 150}) || got.OffsetX != -1 {
		t.Fatalf("a one-pixel clip lays out as %+v", got)
	}
}
