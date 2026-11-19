// Package paneplace is the one placement rule for a browser page shown in a
// pane: where the page's view goes inside the pane's host rect, and how a host
// that positions in whole pixels snaps it. The backend (internal/browser) and
// the Windows launcher (internal/webview2host) both place pages with Fit, so a
// page lands in the same place whichever side placed it.
//
// Stdlib-only: the launcher links it.
package paneplace

import "math"

// Rect is a pane's host rect and its visible clip, in the SPA's CSS pixels.
// The clip is the part of the rect no ancestor clips away; an unclipped rect
// has clip == rect.
type Rect struct {
	X, Y, W, H   float64
	ClipX, ClipY float64
	ClipW, ClipH float64
}

// Fit places a page of pageW x pageH CSS pixels inside rect. The page is
// scaled down to fit (never up, so text stays sharp) and centered by whole
// CSS pixels, so a page that follows the pane (its size is the rect's whole
// pixels) starts exactly at the rect's origin with no sub-pixel offset. The
// fitted rect's clip is the rect's clip intersected with it. ok is false when
// nothing of the page would be visible.
func Fit(rect Rect, pageW, pageH int) (fitted Rect, scale float64, ok bool) {
	if pageW <= 0 || pageH <= 0 || rect.W < 1 || rect.H < 1 {
		return Rect{}, 0, false
	}
	scale = math.Min(rect.W/float64(pageW), rect.H/float64(pageH))
	if scale > 1 {
		scale = 1
	}
	fitted.W = float64(pageW) * scale
	fitted.H = float64(pageH) * scale
	fitted.X = rect.X + centerOffset(rect.W, fitted.W)
	fitted.Y = rect.Y + centerOffset(rect.H, fitted.H)
	left := math.Max(fitted.X, rect.ClipX)
	top := math.Max(fitted.Y, rect.ClipY)
	right := math.Min(fitted.X+fitted.W, rect.ClipX+rect.ClipW)
	bottom := math.Min(fitted.Y+fitted.H, rect.ClipY+rect.ClipH)
	if right-left < 1 || bottom-top < 1 {
		return Rect{}, 0, false
	}
	fitted.ClipX, fitted.ClipY = left, top
	fitted.ClipW, fitted.ClipH = right-left, bottom-top
	return fitted, scale, true
}

// centerOffset is half the margin, in whole CSS pixels and never negative
// (the binding axis's float remainder can be a hair below zero).
func centerOffset(outer, inner float64) float64 {
	return math.Max(0, math.Floor((outer-inner)/2))
}

// SnapEdge rounds one edge coordinate, already scaled to the host's pixels,
// to the nearest pixel with halves going up: floor(v + 0.5). A host snaps a
// rect's EDGES and derives its size as right minus left, so two rects that
// share an edge share a pixel, and a negative coordinate (a pane scrolled
// partly off the window) rounds the way a positive one does. The macOS glue
// mirrors it (wkwebviewglue_darwin.m ao_snap_edge).
func SnapEdge(v float64) int {
	return int(math.Floor(v + 0.5))
}

// Edges is a rect's snapped edges, right and bottom exclusive.
type Edges struct{ Left, Top, Right, Bottom int }

// Empty reports whether the edges enclose no pixel.
func (e Edges) Empty() bool { return e.Right <= e.Left || e.Bottom <= e.Top }

// HostLayout is a fitted rect resolved for a host that positions in whole
// pixels: the clip box the host crops to, and the fitted rect's top-left
// relative to that box (negative where the clip crops the page).
type HostLayout struct {
	Clip             Edges
	OffsetX, OffsetY int
}

// Layout scales a fitted rect (Fit's answer) by sx/sy into host pixels and
// snaps it. Both rects snap by edge from the same scaled coordinates, and the
// clip is cropped to the snapped fitted rect, so rounding can never let the
// clip box overhang the page. A clip the caller left empty (no width or no
// height) means unclipped. ok is false when no host pixel is left to show:
// a clip that rounds away to nothing at this scale, which Fit's whole CSS
// pixel can be, is to be hidden, never shown unclipped.
func Layout(fitted Rect, sx, sy float64) (layout HostLayout, ok bool) {
	page := snapRect(fitted.X, fitted.Y, fitted.W, fitted.H, sx, sy)
	clip := page
	if fitted.ClipW > 0 && fitted.ClipH > 0 {
		clip = snapRect(fitted.ClipX, fitted.ClipY, fitted.ClipW, fitted.ClipH, sx, sy)
		clip.Left = max(clip.Left, page.Left)
		clip.Top = max(clip.Top, page.Top)
		clip.Right = min(clip.Right, page.Right)
		clip.Bottom = min(clip.Bottom, page.Bottom)
	}
	if clip.Empty() {
		return HostLayout{}, false
	}
	return HostLayout{Clip: clip, OffsetX: page.Left - clip.Left, OffsetY: page.Top - clip.Top}, true
}

func snapRect(x, y, w, h, sx, sy float64) Edges {
	return Edges{
		Left:   SnapEdge(x * sx),
		Top:    SnapEdge(y * sy),
		Right:  SnapEdge((x + w) * sx),
		Bottom: SnapEdge((y + h) * sy),
	}
}
