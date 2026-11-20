//go:build linux || darwin

package browser

import "testing"

// A page base is exactly #rrggbb, as a pane report's color is (paneColor):
// anything else, surrounding space included, is no color rather than a
// partly parsed one, because it reaches a native color.
func TestNativePaneColorParsesOnlyRRGGBB(t *testing.T) {
	for value, want := range map[string]uint32{
		"#000000": 0x000000,
		"#ffffff": 0xffffff,
		"#FFFFFF": 0xffffff,
		"#1a1B26": 0x1a1b26,
		"#0a0b0c": 0x0a0b0c,
		"#ff0000": 0xff0000,
	} {
		if got, ok := nativePaneColor(value); !ok || got != want {
			t.Fatalf("nativePaneColor(%q) = %#x, %v; want %#x", value, got, ok, want)
		}
	}
	for _, value := range []string{
		"", "   ", "#", "#fff", "1a1b26", "#1a1b2", "#1a1b26 ", " #1a1b26",
		"  #ff0000  ", "#1a1b2g", "#12 456", "#12345678", "#+00000", "#0x0000",
		"rgb(0,0,0)", "transparent",
	} {
		if got, ok := nativePaneColor(value); ok {
			t.Fatalf("nativePaneColor(%q) = %#x, true; want no answer", value, got)
		}
	}
}

func TestNativePanesPresentOnlyWhatChanged(t *testing.T) {
	rect := PaneRect{X: 10, Y: 20, Width: 300, Height: 400, ClipX: 10, ClipY: 20, ClipWidth: 300, ClipHeight: 400}
	placement := PanePlacement{Rect: rect, PageWidth: 1280, PageHeight: 720, Scale: 0.5}
	var panes nativePanes

	// A show before any rect stays parked, and bounds for a hidden page
	// only record them.
	if _, present := panes.show(1); present {
		t.Fatal("a show with no rect presented")
	}
	if panes.hide(1) {
		t.Fatal("a page never presented was parked")
	}
	if panes.bounds(1, placement) {
		t.Fatal("bounds for a hidden page presented it")
	}
	if got, present := panes.show(1); !present || got != placement {
		t.Fatalf("a show after bounds = %+v, %v", got, present)
	}
	if _, present := panes.show(1); present {
		t.Fatal("a second show at the same placement presented again")
	}
	if panes.bounds(1, placement) {
		t.Fatal("unchanged bounds presented again")
	}

	// The whole placement is compared: a clip that moved under a still rect,
	// or a changed scale, is presented again.
	moved := placement
	moved.Rect.ClipY, moved.Rect.ClipHeight = 60, 360
	if !panes.bounds(1, moved) {
		t.Fatal("a moved clip was treated as applied")
	}
	scaled := moved
	scaled.Scale = 0.25
	if !panes.bounds(1, scaled) {
		t.Fatal("a changed scale was treated as applied")
	}

	// A hide parks a presented page once; a show after it presents again.
	if !panes.hide(1) {
		t.Fatal("a presented page was not parked")
	}
	if panes.hide(1) {
		t.Fatal("a parked page was parked again")
	}
	if got, present := panes.show(1); !present || got != scaled {
		t.Fatalf("a show after a hide = %+v, %v", got, present)
	}

	// Pages are independent, and a forgotten page starts over.
	if _, present := panes.show(2); present {
		t.Fatal("another page took this page's rect")
	}
	panes.forget(1)
	if _, present := panes.show(1); present {
		t.Fatal("a forgotten page kept its rect")
	}
}
