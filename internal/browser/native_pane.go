//go:build linux || darwin

package browser

import (
	"strconv"
	"sync"
)

// The presentation bookkeeping the WebKitGTK and WKWebView engines keep
// alike for the Manager's pane calls (paneHost). Each engine decides here,
// under the lock, and makes its UI-thread call after the lock is released.

// nativePaneColor packs a page's base (pageBase) as 0xRRGGBB. The spelling
// it accepts is paneColor's, "#rrggbb" with no surrounding space, because
// the value reaches a native color; anything else is no answer.
func nativePaneColor(value string) (uint32, bool) {
	color, ok := paneColor(value)
	if !ok {
		return 0, false
	}
	packed, err := strconv.ParseUint(color[1:], 16, 32)
	if err != nil {
		return 0, false
	}
	return uint32(packed), true
}

// nativePanes is each page's presentation: what the Manager asked for and
// what was last pushed to the view, so an unchanged sync costs no UI-thread
// dispatch. The Manager always sends bounds before a show, so a show
// presents at the recorded placement and bounds for a presented page
// reposition it. The zero value is ready to use.
type nativePanes struct {
	mu    sync.Mutex
	pages map[uint64]nativePaneState
}

type nativePaneState struct {
	placement    PanePlacement
	hasRect      bool
	shown        bool
	applied      PanePlacement
	appliedShown bool
}

// current compares the whole placement, so a placement that moved only its
// clip or changed only its scale is presented again.
func (st nativePaneState) current() bool {
	return st.appliedShown && st.applied == st.placement
}

// present answers whether the page is to be presented at its placement now
// and, when it is, records that placement as applied. A shown page with no
// rect yet stays parked: presenting at a made-up place would paint the view
// over whatever is there.
func (st *nativePaneState) present() bool {
	if !st.shown || !st.hasRect || st.current() {
		return false
	}
	st.applied, st.appliedShown = st.placement, true
	return true
}

// change runs edit on the page's state under the lock and stores it.
func (n *nativePanes) change(id uint64, edit func(*nativePaneState)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.pages == nil {
		n.pages = make(map[uint64]nativePaneState)
	}
	st := n.pages[id]
	edit(&st)
	n.pages[id] = st
}

// bounds records the page's placement and answers whether to present it
// there.
func (n *nativePanes) bounds(id uint64, placement PanePlacement) (present bool) {
	n.change(id, func(st *nativePaneState) {
		st.placement, st.hasRect = placement, true
		present = st.present()
	})
	return present
}

// show marks the page shown and answers the placement to present it at,
// and false when it is not to be presented now.
func (n *nativePanes) show(id uint64) (placement PanePlacement, present bool) {
	n.change(id, func(st *nativePaneState) {
		st.shown = true
		present = st.present()
		placement = st.placement
	})
	return placement, present
}

// hide marks the page hidden and answers whether it was presented, which is
// when its view is to be parked.
func (n *nativePanes) hide(id uint64) (park bool) {
	n.change(id, func(st *nativePaneState) {
		st.shown = false
		park = st.appliedShown
		st.appliedShown = false
	})
	return park
}

// forget drops a closed page's state.
func (n *nativePanes) forget(id uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.pages, id)
}
