//go:build linux && cgo && !gtk3 && !android && !server && !nogui

package browser

import (
	"errors"
	"log"
	"strconv"
	"strings"
)

// The engine half of the Manager's pane presentation (`paneHost`). Where the
// hosted engine answers these calls with directives to the launcher, this
// engine moves its own GTK widgets: present puts the page's host over the
// pane's clip rect and draws the page scaled to the placement, hide is a
// re-park into an empty box, and devtools is the WebKit inspector
// (docked in-app; developer extras are enabled at view creation). The page's
// viewport is never touched here; SetViewport owns it, parked or presented.
// The page's base (SetPageBase) is the view's own background, parked or
// presented.
//
// The Manager always sends bounds before a show, so ShowPage presents at the
// recorded placement and SetPageBounds on an already-presented page
// repositions it.
// Both are deduped under e.mu — bookkeeping only, the GTK call happens after
// the lock is released (the one locking rule this engine has).

// webkitPaneState is the desired presentation of one page, plus what was last
// pushed to GTK so an unchanged sync costs no main-thread dispatch.
type webkitPaneState struct {
	placement    PanePlacement
	hasRect      bool
	shown        bool
	applied      PanePlacement
	appliedShown bool
}

// webkitPaneApplied compares the WHOLE placement, so the clip and the scale
// ride along: any change to either re-presents, which is exactly what a
// moved clip needs.
func webkitPaneApplied(st webkitPaneState) bool {
	return st.appliedShown && st.applied == st.placement
}

// webkitRGB is one parsed base color in GdkRGBA's 0..1 space.
type webkitRGB struct{ R, G, B float64 }

// webkitParseBackground reads a "#rrggbb" base. Anything else (empty, a named
// color, a CSS function, a short hex) is "no answer", and the caller leaves
// the view's background alone rather than guessing a color.
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

// present shows the page's view at placement, or hides it when no overlay
// pixel of it is left to show (webkitPresentView).
func (p *webkitPage) present(placement PanePlacement) {
	if err := webkitPresentView(p.liveView(), placement); err != nil && !errors.Is(err, errViewClosed) {
		log.Printf("browser: present page %d: %v", p.id, err)
	}
}

func (e *webkitEngine) SetPageBounds(handle string, placement PanePlacement) {
	p := webkitPaneTarget(handle)
	if p == nil {
		return
	}
	e.mu.Lock()
	st := e.pane[p.id]
	st.placement = placement
	st.hasRect = true
	present := st.shown && !webkitPaneApplied(st)
	if present {
		st.appliedShown = true
		st.applied = st.placement
	}
	e.pane[p.id] = st
	e.mu.Unlock()
	if present {
		p.present(placement)
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
		st.applied = st.placement
	}
	placement := st.placement
	e.pane[p.id] = st
	e.mu.Unlock()
	if present {
		p.present(placement)
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
		if err := webkitHideView(p.liveView()); err != nil && !errors.Is(err, errViewClosed) {
			log.Printf("browser: hide page %d: %v", p.id, err)
		}
	}
}

// SetPageBase sets the view's background color, which WebKit paints under
// the document whether the view is parked or presented.
func (e *webkitEngine) SetPageBase(handle, color string) {
	p := webkitPaneTarget(handle)
	if p == nil {
		return
	}
	rgb, ok := webkitParseBackground(color)
	if !ok {
		log.Printf("browser: page %d base %q is not #rrggbb", p.id, color)
		return
	}
	if err := webkitSetViewBackground(p.liveView(), rgb); err != nil && !errors.Is(err, errViewClosed) {
		log.Printf("browser: page %d base: %v", p.id, err)
	}
}

// FocusPage gives the presented page's view keyboard focus within the
// window while the request is live. Both are read on the GTK thread, after
// any hide queued before it. A page that closed is not presented.
func (e *webkitEngine) FocusPage(handle string, req focusRequest) (focusResult, error) {
	p := webkitPaneTarget(handle)
	if p == nil {
		return focusNotPresented, nil
	}
	result, err := webkitFocusPresentedView(p.liveView(), req.Live)
	if errors.Is(err, errViewClosed) {
		return focusNotPresented, nil
	}
	return result, err
}

var _ paneHost = (*webkitEngine)(nil)

func (e *webkitEngine) OpenPageDevTools(handle string) {
	if p := webkitPaneTarget(handle); p != nil {
		_ = webkitOpenInspector(p.liveView())
	}
}

// forgetPane drops a closed page's presentation bookkeeping. The view itself
// is gone with the page; only the map entry would otherwise linger.
func (e *webkitEngine) forgetPane(id uint64) {
	e.mu.Lock()
	delete(e.pane, id)
	e.mu.Unlock()
}
