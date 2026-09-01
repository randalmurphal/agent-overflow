package webview2host

import "testing"

func TestResolvePaneLayoutUnclippedPutsTheControllerAtTheContainerOrigin(t *testing.T) {
	got := resolvePaneLayout(Directive{X: 10, Y: 20, W: 300, H: 400}, 1, 1)
	want := paneLayout{
		Container:  rect{Left: 10, Top: 20, Right: 310, Bottom: 420},
		Controller: rect{Left: 0, Top: 0, Right: 300, Bottom: 400},
	}
	if got != want {
		t.Fatalf("layout = %+v, want %+v", got, want)
	}
}

func TestResolvePaneLayoutClipCropsWithoutResizingTheController(t *testing.T) {
	// A pane whose left 100px sit behind the sidebar: the container starts
	// where the visible part does, and the controller keeps its full 300px
	// width by starting 100px to the LEFT of the container.
	got := resolvePaneLayout(Directive{
		X: 10, Y: 20, W: 300, H: 400,
		CX: 110, CY: 20, CW: 200, CH: 400,
	}, 1, 1)
	want := paneLayout{
		Container:  rect{Left: 110, Top: 20, Right: 310, Bottom: 420},
		Controller: rect{Left: -100, Top: 0, Right: 200, Bottom: 400},
	}
	if got != want {
		t.Fatalf("layout = %+v, want %+v", got, want)
	}
	if w := got.Controller.width(); w != 300 {
		t.Fatalf("controller width = %d, want the full rect's 300", w)
	}
	if h := got.Controller.height(); h != 400 {
		t.Fatalf("controller height = %d, want the full rect's 400", h)
	}
}

func TestResolvePaneLayoutScalesBothRectsByTheSameFactor(t *testing.T) {
	got := resolvePaneLayout(Directive{
		X: 100, Y: 50, W: 400, H: 200,
		CX: 200, CY: 50, CW: 300, CH: 200,
	}, 2, 2)
	want := paneLayout{
		Container:  rect{Left: 400, Top: 100, Right: 1000, Bottom: 500},
		Controller: rect{Left: -200, Top: 0, Right: 600, Bottom: 400},
	}
	if got != want {
		t.Fatalf("layout = %+v, want %+v", got, want)
	}
}

func TestResolvePaneLayoutNeverClipsASharedEdgeByAFractionalPixel(t *testing.T) {
	// A fractional scale factor is the common case (any non-integer DPI or
	// webview zoom). On every edge the clip SHARES with the full rect the
	// controller must land exactly on the container edge, not a pixel
	// inside it: a 1px inset there is a hairline of launcher background
	// cutting across the page.
	for _, sx := range []float64{1.25, 1.5, 1.75, 2.25, 0.9} {
		layout := resolvePaneLayout(Directive{
			X: 37, Y: 11, W: 613, H: 409,
			// Clipped on the RIGHT only, so left and top are shared.
			CX: 37, CY: 11, CW: 300, CH: 409,
		}, sx, sx)
		if layout.Controller.Left != 0 {
			t.Fatalf("sx=%v: shared left edge inset by %d px", sx, layout.Controller.Left)
		}
		if layout.Controller.Top != 0 {
			t.Fatalf("sx=%v: shared top edge inset by %d px", sx, layout.Controller.Top)
		}
		if layout.Controller.Right < layout.Container.width() {
			t.Fatalf("sx=%v: controller right %d falls short of container width %d",
				sx, layout.Controller.Right, layout.Container.width())
		}
		if layout.Controller.Bottom < layout.Container.height() {
			t.Fatalf("sx=%v: controller bottom %d falls short of container height %d",
				sx, layout.Controller.Bottom, layout.Container.height())
		}
	}
}

func TestResolvePaneLayoutRoundsOutward(t *testing.T) {
	got := resolvePaneLayout(Directive{X: 10, Y: 10, W: 100, H: 100}, 1.5, 1.5)
	// 10*1.5 = 15 exactly; 110*1.5 = 165 exactly. Shift by a third of a
	// pixel and both edges must move OUT, never in.
	if got.Container != (rect{Left: 15, Top: 15, Right: 165, Bottom: 165}) {
		t.Fatalf("container = %+v", got.Container)
	}
	got = resolvePaneLayout(Directive{X: 10.4, Y: 10.4, W: 100.2, H: 100.2}, 1, 1)
	if got.Container != (rect{Left: 10, Top: 10, Right: 111, Bottom: 111}) {
		t.Fatalf("container = %+v, want outward rounding on every edge", got.Container)
	}
}

func TestResolvePaneLayoutClampsAbsurdDevicePixels(t *testing.T) {
	// Validate bounds the DIPs, but the scale factor comes from a live
	// client area and is not part of that contract. int32 wrap here would
	// place a window at a random coordinate.
	got := resolvePaneLayout(Directive{X: maxPaneDIP, Y: maxPaneDIP, W: maxPaneDIP, H: maxPaneDIP}, 1e6, 1e6)
	if got.Container.Right != maxPaneDevicePX || got.Container.Bottom != maxPaneDevicePX {
		t.Fatalf("container = %+v, want clamped edges", got.Container)
	}
}

func TestParsePaneColor(t *testing.T) {
	for _, tc := range []struct {
		bg   string
		want paneColor
		ok   bool
	}{
		{bg: "#000000", want: paneColor{A: 255}, ok: true},
		{bg: "#ffffff", want: paneColor{A: 255, R: 255, G: 255, B: 255}, ok: true},
		{bg: "#FFFFFF", want: paneColor{A: 255, R: 255, G: 255, B: 255}, ok: true},
		{bg: "#1a2b3c", want: paneColor{A: 255, R: 0x1a, G: 0x2b, B: 0x3c}, ok: true},
		{bg: ""},
		{bg: "#fff"},
		{bg: "1a2b3c7"},
		{bg: "#1a2b3g"},
		{bg: "#1a2b3c "},
		{bg: "rgb(1,2,3)"},
	} {
		got, ok := parsePaneColor(tc.bg)
		if ok != tc.ok {
			t.Fatalf("parsePaneColor(%q) ok = %v, want %v", tc.bg, ok, tc.ok)
		}
		if ok && got != tc.want {
			t.Fatalf("parsePaneColor(%q) = %+v, want %+v", tc.bg, got, tc.want)
		}
	}
}

// TestParsePaneColorAcceptsEverythingValidateDoes keeps the two halves of
// the same rule together: a bg the wire contract admits must never reach
// the COM call as "not a color".
func TestParsePaneColorAcceptsEverythingValidateDoes(t *testing.T) {
	for _, bg := range []string{"#000000", "#abcdef", "#ABCDEF", "#012345", "#987654"} {
		if err := validateBg(bg); err != nil {
			t.Fatalf("validateBg(%q) = %v", bg, err)
		}
		if _, ok := parsePaneColor(bg); !ok {
			t.Fatalf("parsePaneColor(%q) refused a value Validate accepts", bg)
		}
	}
}

func TestPaneColorMarshalsAsCOREWEBVIEW2COLOR(t *testing.T) {
	// COREWEBVIEW2_COLOR is four bytes {A,R,G,B} and is passed BY VALUE.
	// On win64 a 4-byte struct travels in the low half of one register
	// word, little-endian, so A is the least significant byte. Getting
	// this backwards paints the pane in the wrong colour with no error.
	got := paneColor{A: 255, R: 0x1a, G: 0x2b, B: 0x3c}.word()
	if want := uint32(0x3c2b1aff); got != want {
		t.Fatalf("word = %#08x, want %#08x", got, want)
	}
}
