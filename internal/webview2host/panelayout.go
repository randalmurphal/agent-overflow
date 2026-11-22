package webview2host

import (
	"math"

	"agent-overflow/internal/paneplace"
)

// rect is the Win32 RECT shape: pixel edges in some window's client
// coordinates, right/bottom exclusive. It lives in a cross-platform file
// because the pane's layout arithmetic below is pure and unit-tested off
// Windows; the COM and Win32 calls that consume it are `_windows`-tagged.
type rect struct{ Left, Top, Right, Bottom int32 }

func (r rect) width() int32  { return r.Right - r.Left }
func (r rect) height() int32 { return r.Bottom - r.Top }

// deviceScale is the factor pair from the SPA's CSS pixels to the host
// window's client pixels. The SPA's devicePixelRatio is exactly that factor
// (OS scale times webview zoom), so it wins when the rect carries one. Without
// it the proportion of the client size to the SPA viewport stands in, which
// is off by the fraction innerWidth truncates away. No viewport means the
// rect is already in client pixels.
func deviceScale(b paneBounds, clientW, clientH int32, hasClient bool) (sx, sy float64) {
	if b.DPR > 0 {
		return b.DPR, b.DPR
	}
	if b.VW > 0 && b.VH > 0 && hasClient && clientW > 0 && clientH > 0 {
		return float64(clientW) / b.VW, float64(clientH) / b.VH
	}
	return 1, 1
}

// paneLayout is one bounds rect resolved into the two rectangles the
// host actually sets: where the clip container goes inside the launcher
// window, and where the controller goes inside that container.
type paneLayout struct {
	// Container is the visible clip rect in the host window's client
	// pixels. The container window is positioned here and clips its child,
	// which is how a pane half behind the sidebar shows its visible half.
	Container rect
	// Controller is the pane's full rect expressed RELATIVE to Container.
	// Its edges go negative (or past the container's size) exactly where
	// the pane is cropped, so the page keeps its full layout size and only
	// its presentation is cropped.
	Controller rect
}

// resolvePaneLayout scales a CSS-pixel bounds rect by sx/sy and splits it
// into the container and controller rectangles.
//
// Every edge snaps to the nearest pixel by the shared rule
// (paneplace.SnapEdge), so two rects that share an edge share a device
// pixel and neither overlaps the other's hit area, and the controller's
// offset is derived from the SAME snapped numbers as the container's
// origin: an edge the clip shares with the full rect snaps to one pixel
// for both, so no half-pixel scale factor can shave a 1px sliver of
// launcher background across the page.
//
// A zero clip pair means unclipped: the clip IS the full rect, and the
// controller then sits at the container's origin with the container's
// exact size.
func resolvePaneLayout(d paneBounds, sx, sy float64) paneLayout {
	clipX, clipY, clipW, clipH := d.CX, d.CY, d.CW, d.CH
	if clipW <= 0 || clipH <= 0 {
		clipX, clipY, clipW, clipH = d.X, d.Y, d.W, d.H
	}
	container := rect{
		Left:   snapInt32(clipX * sx),
		Top:    snapInt32(clipY * sy),
		Right:  snapInt32((clipX + clipW) * sx),
		Bottom: snapInt32((clipY + clipH) * sy),
	}
	full := rect{
		Left:   snapInt32(d.X * sx),
		Top:    snapInt32(d.Y * sy),
		Right:  snapInt32((d.X + d.W) * sx),
		Bottom: snapInt32((d.Y + d.H) * sy),
	}
	return paneLayout{
		Container: container,
		Controller: rect{
			Left:   full.Left - container.Left,
			Top:    full.Top - container.Top,
			Right:  full.Right - container.Left,
			Bottom: full.Bottom - container.Top,
		},
	}
}

// maxPaneDevicePX bounds the pixel results. Validate already bounds every
// DIP at maxPaneDIP, but the scale factor is measured from a live client
// area and is not part of that contract, so the conversion to int32 is
// clamped here rather than left to wrap.
const maxPaneDevicePX = 1 << 24

// snapInt32 snaps one scaled edge (paneplace.SnapEdge) within the Win32
// range maxPaneDevicePX bounds.
func snapInt32(v float64) int32 {
	if math.IsNaN(v) {
		return 0
	}
	return int32(paneplace.SnapEdge(math.Max(-maxPaneDevicePX, math.Min(v, maxPaneDevicePX))))
}

// paneColor is COREWEBVIEW2_COLOR's field order — A, R, G, B, one byte
// each. The order matters twice: it is the struct's memory layout, and
// that layout is what the by-value put marshals into a register word.
type paneColor struct{ A, R, G, B uint8 }

// parsePaneColor turns a validated "#rrggbb" into an opaque pane color.
//
// Alpha is always 255. WebView2 rejects every alpha but 0 and 255 with
// E_INVALIDARG, and a transparent default background would show the
// launcher window through the pane rather than the pane's own surface —
// which is the opposite of what this color exists for.
//
// It reports ok=false for anything else, including "", so a caller can
// tell "leave the engine default" from a color. Directive.Validate has
// already refused every non-"#rrggbb" spelling by the time the host calls
// this; the check is repeated because the value reaches a COM call.
func parsePaneColor(bg string) (paneColor, bool) {
	if len(bg) != 7 || bg[0] != '#' {
		return paneColor{}, false
	}
	var component [3]uint8
	for i := range component {
		hi, hiOK := hexNibble(bg[1+2*i])
		lo, loOK := hexNibble(bg[2+2*i])
		if !hiOK || !loOK {
			return paneColor{}, false
		}
		component[i] = hi<<4 | lo
	}
	return paneColor{A: 255, R: component[0], G: component[1], B: component[2]}, true
}

// word packs the color the way the by-value put_DefaultBackgroundColor
// receives it.
//
// COREWEBVIEW2_COLOR is a 4-byte struct passed BY VALUE. The Windows x64
// (and arm64) convention passes any integer struct of 1/2/4/8 bytes as if
// it were an integer of that width, so the callee reads the struct's bytes
// out of the low half of one register word — little-endian, which puts
// field A (offset 0) in the least significant byte. Packing explicitly
// rather than reinterpreting the struct's memory keeps that reasoning
// visible at the one place it matters.
func (c paneColor) word() uint32 {
	return uint32(c.A) | uint32(c.R)<<8 | uint32(c.G)<<16 | uint32(c.B)<<24
}

func hexNibble(c byte) (uint8, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}
