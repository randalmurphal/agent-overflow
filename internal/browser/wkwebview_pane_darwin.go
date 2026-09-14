//go:build darwin && cgo && !ios && !server && !nogui

package browser

import (
	"strconv"
	"strings"
)

// The engine half of the Manager's pane presentation (`paneHost`), the
// direct analogue of webkit_pane_linux.go: present moves the view over the
// placement's fitted rect as a subview of Wails' content view, drawn scaled
// over its own viewport, and hide re-parks it at its slot in the 1x1 clipping
// park view. The page's viewport is never touched here; SetViewport owns it. There is deliberately no
// OpenPageDevTools — WKWebView has no public call that opens its inspector,
// so this engine does not implement `paneDevTools` and the Manager's refusal
// stands as the true answer (devtools here are Safari's Develop menu against
// the inspectable view).
//
// The Manager always sends bounds before a show, so ShowPage presents at the
// recorded placement and SetPageBounds on an already-presented page
// repositions it. Both are deduped under e.mu — bookkeeping only, the AppKit
// call happens after the lock is released (the same locking rule the Linux
// engine states). The dedupe compares the WHOLE PanePlacement, so one that
// moved only its clip, its scale or its background colour still reaches
// AppKit, and one that changed nothing still costs no main-thread dispatch.

// wkNoBackground is what a pane with no resolved colour reports to the
// Objective-C half, which then leaves the engine default in place.
const wkNoBackground = -1

// wkBackgroundCode turns PaneRect.Background into the packed 0xRRGGBB the host
// takes. Only "#rrggbb" is a colour: the value reaches an NSColor and a layer,
// so anything else (empty, rgb(), a name, a short hex) is "no colour" rather
// than a half-parsed one. Byte-wise and allocation-free — this runs on every
// changed pane frame while a pane is dragged.
func wkBackgroundCode(value string) int {
	if len(value) != 7 || value[0] != '#' {
		return wkNoBackground
	}
	code := 0
	for i := 1; i < 7; i++ {
		var digit int
		switch c := value[i]; {
		case c >= '0' && c <= '9':
			digit = int(c - '0')
		case c >= 'a' && c <= 'f':
			digit = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			digit = int(c-'A') + 10
		default:
			return wkNoBackground
		}
		code = code<<4 | digit
	}
	return code
}

// wkPaneState is the desired presentation of one page, plus what was last
// pushed to AppKit so an unchanged sync costs no main-thread dispatch.
type wkPaneState struct {
	placement    PanePlacement
	hasRect      bool
	shown        bool
	applied      PanePlacement
	appliedShown bool
}

func wkPaneApplied(st wkPaneState) bool {
	return st.appliedShown && st.applied == st.placement
}

// wkPaneTarget answers the page a pane call addresses, or nil for a handle
// this engine never issued (a page that already closed is a lookup miss,
// not an error).
func wkPaneTarget(handle string) *wkPage {
	id, err := strconv.ParseUint(strings.TrimPrefix(handle, "page-"), 10, 64)
	if err != nil {
		return nil
	}
	return wkLookupPage(id)
}

func (e *wkEngine) SetPageBounds(handle string, placement PanePlacement) {
	p := wkPaneTarget(handle)
	if p == nil {
		return
	}
	e.mu.Lock()
	st := e.pane[p.id]
	st.placement = placement
	st.hasRect = true
	present := st.shown && !wkPaneApplied(st)
	if present {
		st.appliedShown = true
		st.applied = st.placement
	}
	e.pane[p.id] = st
	e.mu.Unlock()
	if present {
		_ = wkPresentView(p.view, placement)
	}
}

func (e *wkEngine) ShowPage(handle string) {
	p := wkPaneTarget(handle)
	if p == nil {
		return
	}
	e.mu.Lock()
	st := e.pane[p.id]
	st.shown = true
	// A show before any rect arrives stays parked: presenting at a made-up
	// place would paint the view over whatever happens to be there.
	present := st.hasRect && !wkPaneApplied(st)
	if present {
		st.appliedShown = true
		st.applied = st.placement
	}
	placement := st.placement
	e.pane[p.id] = st
	e.mu.Unlock()
	if present {
		_ = wkPresentView(p.view, placement)
	}
}

func (e *wkEngine) HidePage(handle string) {
	p := wkPaneTarget(handle)
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
		p.mu.Lock()
		slot := p.slot
		p.mu.Unlock()
		_ = wkHideView(p.view, slot)
	}
}

// forgetPane drops a closed page's presentation bookkeeping. The view itself
// is gone with the page; only the map entry would otherwise linger.
func (e *wkEngine) forgetPane(id uint64) {
	e.mu.Lock()
	delete(e.pane, id)
	e.mu.Unlock()
}
