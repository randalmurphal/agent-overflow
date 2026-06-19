package windowgeom

// Rect is a screen-space rectangle in device-independent pixels (DIP). It
// mirrors the shape of Wails' application.Rect but stays GUI-free so this
// package never imports Wails; callers convert at the boundary.
type Rect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

func (r Rect) empty() bool { return r.Width <= 0 || r.Height <= 0 }

// overlapArea returns the area of the intersection of r and other, or 0 when
// they are disjoint.
func (r Rect) overlapArea(other Rect) int {
	w := min(r.X+r.Width, other.X+other.Width) - max(r.X, other.X)
	h := min(r.Y+r.Height, other.Y+other.Height) - max(r.Y, other.Y)
	if w <= 0 || h <= 0 {
		return 0
	}
	return w * h
}

// Geometry is a persisted window placement. The X/Y/Width/Height fields always
// describe the *normal* (non-maximized, non-fullscreen) window so that
// restoring after an un-maximize lands somewhere usable; Maximized/Fullscreen
// are restored as start states on top of that normal rect.
//
// Stored in DIP. Persisted as a nested object in settings.json (native build)
// and in %APPDATA%\agent-overflow\window.json (WSL/Windows launcher build).
type Geometry struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`

	Maximized  bool `json:"maximized"`
	Fullscreen bool `json:"fullscreen"`

	// Display is the work area of the screen the window occupied when the
	// geometry was saved. It lets Clamp detect that the monitor layout has
	// changed (display unplugged / resolution changed) precisely, matching
	// what VSCode-style window-state restore does, rather than relying on
	// overlap heuristics alone.
	Display Rect `json:"display"`

	// Valid is false until the first real save. A zero-valued Geometry
	// therefore means "never saved" and callers center the window instead.
	Valid bool `json:"valid"`
}

// Clamp validates g against the current screen work areas and returns a
// placement guaranteed to sit fully within one of them. ok is false when g is
// unrestorable — never saved, zero-sized, or no screens were supplied — and the
// caller should fall back to its centered default.
//
// The window is anchored to the screen it was saved on when that screen still
// exists (exact work-area match); otherwise to the screen it most overlaps. If
// it overlaps none (e.g. the monitor it lived on was unplugged) ok is false.
func (g Geometry) Clamp(screens []Rect) (Geometry, bool) {
	if !g.Valid || g.Width <= 0 || g.Height <= 0 || len(screens) == 0 {
		return Geometry{}, false
	}

	target, ok := g.targetScreen(screens)
	if !ok {
		return Geometry{}, false
	}

	out := g
	// Never restore larger than the screen it lands on.
	out.Width = min(out.Width, target.Width)
	out.Height = min(out.Height, target.Height)
	// Shift fully inside the work area so no part opens off-screen.
	out.X = clamp(out.X, target.X, target.X+target.Width-out.Width)
	out.Y = clamp(out.Y, target.Y, target.Y+target.Height-out.Height)
	out.Display = target
	return out, true
}

// targetScreen picks the screen to anchor to: the exact saved display if it
// still exists, else the screen with the largest overlap. ok is false when the
// window overlaps no screen at all.
func (g Geometry) targetScreen(screens []Rect) (Rect, bool) {
	for _, s := range screens {
		if !s.empty() && s == g.Display {
			return s, true
		}
	}
	win := Rect{X: g.X, Y: g.Y, Width: g.Width, Height: g.Height}
	var best Rect
	bestArea := 0
	for _, s := range screens {
		if a := win.overlapArea(s); a > bestArea {
			bestArea = a
			best = s
		}
	}
	return best, bestArea > 0
}

// clamp bounds v to [lo, hi]. When the range is inverted (the window is wider
// than the work area after capping, which cap prevents, but guard anyway) it
// pins to lo so the top-left stays on-screen.
func clamp(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	return min(max(v, lo), hi)
}
