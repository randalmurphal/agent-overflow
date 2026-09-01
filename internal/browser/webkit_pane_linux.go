//go:build linux && cgo && !gtk3 && !android && !server && !nogui

package browser

import (
	"strconv"
	"strings"
)

// The engine half of the Manager's pane presentation (`paneHost`). Where the
// hosted engine answers these calls with directives to the launcher, this
// engine moves its own GTK widgets: present is the GtkOverlay margin surgery
// (plus a clipping box when the rect is only partly visible), hide is a re-park
// into the 1x1 clipping slot, and devtools is the WebKit inspector (docked
// in-app; developer extras are enabled at view creation). The pane's own
// background color rides the same rect and is pushed to the view, not the host.
//
// The Manager always sends bounds before a show, so ShowPage presents at the
// recorded rect and SetPageBounds on an already-presented page repositions it.
// Both are deduped under e.mu — bookkeeping only, the GTK call happens after
// the lock is released (the one locking rule this engine has).

// webkitPaneState is the desired presentation of one page, plus what was last
// pushed to GTK so an unchanged sync costs no main-thread dispatch.
type webkitPaneState struct {
	rect         PaneRect
	hasRect      bool
	shown        bool
	applied      PaneRect
	appliedShown bool
	// The background is its own dedupe: it is pushed to the view rather than to
	// the host, it survives hide/park, and it must be right BEFORE the first
	// present rather than repeated with every rect.
	background    webkitRGB
	hasBackground bool
}

// webkitPaneApplied compares the WHOLE rect, so the clip and the background
// ride along: any change to either re-presents, which is exactly what a moved
// clip needs, and a background-only change costs one extra margin write.
func webkitPaneApplied(st webkitPaneState) bool {
	return st.appliedShown && st.applied == st.rect
}

// webkitRGB is one parsed pane background in GdkRGBA's 0..1 space.
type webkitRGB struct{ R, G, B float64 }

// webkitParseBackground reads the "#rrggbb" a PaneRect carries. Anything else —
// empty, a named color, a CSS function, a short hex — is "no answer", and the
// caller leaves the view's background alone rather than guessing a color that
// would be wrong in the other theme.
func webkitParseBackground(value string) (webkitRGB, bool) {
	value = strings.TrimSpace(value)
	if len(value) != 7 || value[0] != '#' {
		return webkitRGB{}, false
	}
	packed, err := strconv.ParseUint(value[1:], 16, 32)
	if err != nil {
		return webkitRGB{}, false
	}
	return webkitRGB{
		R: float64((packed>>16)&0xff) / 255,
		G: float64((packed>>8)&0xff) / 255,
		B: float64(packed&0xff) / 255,
	}, true
}

// takeBackground records a changed background and reports whether the caller
// must push it. Called under e.mu; the GTK call happens after the unlock.
func (st *webkitPaneState) takeBackground(rect PaneRect) (webkitRGB, bool) {
	color, ok := webkitParseBackground(rect.Background)
	if !ok || (st.hasBackground && st.background == color) {
		return webkitRGB{}, false
	}
	st.background, st.hasBackground = color, true
	return color, true
}

// webkitPaneTarget answers the page a pane call addresses, or nil for a
// handle this engine never issued (a page that already closed is a lookup
// miss, not an error).
func webkitPaneTarget(handle string) *webkitPage {
	id, err := strconv.ParseUint(strings.TrimPrefix(handle, "page-"), 10, 64)
	if err != nil {
		return nil
	}
	return webkitLookupPage(id)
}

func (e *webkitEngine) SetPageBounds(handle string, rect PaneRect) {
	p := webkitPaneTarget(handle)
	if p == nil {
		return
	}
	e.mu.Lock()
	st := e.pane[p.id]
	st.rect = rect
	st.hasRect = true
	present := st.shown && !webkitPaneApplied(st)
	if present {
		st.appliedShown = true
		st.applied = st.rect
	}
	color, paint := st.takeBackground(rect)
	e.pane[p.id] = st
	e.mu.Unlock()
	// Background first: a present that exposes new pixels must find the pane's
	// own color already under the page.
	if paint {
		webkitSetViewBackground(p.view, color)
	}
	if present {
		_ = webkitPresentView(p.view, rect)
	}
}

func (e *webkitEngine) ShowPage(handle string) {
	p := webkitPaneTarget(handle)
	if p == nil {
		return
	}
	e.mu.Lock()
	st := e.pane[p.id]
	st.shown = true
	// A show before any rect arrives stays parked: presenting at a made-up
	// place would paint the view over whatever happens to be there.
	present := st.hasRect && !webkitPaneApplied(st)
	if present {
		st.appliedShown = true
		st.applied = st.rect
	}
	rect := st.rect
	color, paint := st.takeBackground(rect)
	e.pane[p.id] = st
	e.mu.Unlock()
	if paint {
		webkitSetViewBackground(p.view, color)
	}
	if present {
		_ = webkitPresentView(p.view, rect)
	}
}

func (e *webkitEngine) HidePage(handle string) {
	p := webkitPaneTarget(handle)
	if p == nil {
		return
	}
	e.mu.Lock()
	st := e.pane[p.id]
	st.shown = false
	unpresent := st.appliedShown
	st.appliedShown = false
	e.pane[p.id] = st
	e.mu.Unlock()
	if unpresent {
		_ = webkitHideView(p.view, p.slot, webkitHiddenWidth, webkitHiddenHeight)
	}
}

func (e *webkitEngine) OpenPageDevTools(handle string) {
	if p := webkitPaneTarget(handle); p != nil {
		webkitOpenInspector(p.view)
	}
}

// forgetPane drops a closed page's presentation bookkeeping. The view itself
// is gone with the page; only the map entry would otherwise linger.
func (e *webkitEngine) forgetPane(id uint64) {
	e.mu.Lock()
	delete(e.pane, id)
	e.mu.Unlock()
}
