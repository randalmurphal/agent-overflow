//go:build darwin && cgo && !ios && !server && !nogui

package browser

import (
	"errors"
	"log"
	"strconv"
	"strings"

	"agent-overflow/internal/browser/ids"
)

// The engine half of the Manager's pane presentation (`paneHost`): present
// moves the view over the placement's fitted rect as a subview of Wails'
// content view, drawn scaled over its own viewport, and hide re-parks it at
// its slot in the 1x1 clipping park view. The page's viewport is never
// touched here; SetViewport owns it. The page's base (SetPageBase) is the
// view's own, parked or presented. What to present is decided in
// nativePanes (native_pane.go); the AppKit call follows, outside its lock.
//
// There is no OpenPageDevTools: WKWebView has no public call that opens its
// inspector, so this engine does not implement `paneDevTools` and the
// Manager's refusal stands (devtools here are Safari's Develop menu against
// the inspectable view).

// wkPaneTarget answers the page a pane call addresses, or nil for a handle
// this engine never issued (a page that already closed is a lookup miss,
// not an error).
func wkPaneTarget(handle ids.PageHandle) *wkPage {
	id, err := strconv.ParseUint(strings.TrimPrefix(string(handle), "page-"), 10, 64)
	if err != nil {
		return nil
	}
	return wkLookupPage(id)
}

// parkSlot is the page's row in the park view.
func (p *wkPage) parkSlot() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.slot
}

// present shows the page's view at placement, or parks it in its slot when
// no backing pixel of it is left to show (wkPresentView).
func (p *wkPage) present(placement PanePlacement) {
	if err := wkPresentView(p.liveView(), p.parkSlot(), placement); err != nil && !errors.Is(err, errViewClosed) {
		log.Printf("browser: present page %d: %v", p.id, err)
	}
}

func (e *wkEngine) SetPageBounds(handle ids.PageHandle, placement PanePlacement) {
	p := wkPaneTarget(handle)
	if p == nil {
		return
	}
	if e.pane.bounds(p.id, placement) {
		p.present(placement)
	}
}

func (e *wkEngine) ShowPage(handle ids.PageHandle) {
	p := wkPaneTarget(handle)
	if p == nil {
		return
	}
	if placement, present := e.pane.show(p.id); present {
		p.present(placement)
	}
}

func (e *wkEngine) HidePage(handle ids.PageHandle) {
	p := wkPaneTarget(handle)
	if p == nil {
		return
	}
	if !e.pane.hide(p.id) {
		return
	}
	if err := wkHideView(p.liveView(), p.parkSlot()); err != nil && !errors.Is(err, errViewClosed) {
		log.Printf("browser: hide page %d: %v", p.id, err)
	}
}

// SetPageBase sets the view's base color, which it keeps parked or
// presented and its clip container takes when present creates it.
func (e *wkEngine) SetPageBase(handle ids.PageHandle, color string) {
	p := wkPaneTarget(handle)
	if p == nil {
		return
	}
	packed, ok := nativePaneColor(color)
	if !ok {
		log.Printf("browser: page %d base %q is not #rrggbb", p.id, color)
		return
	}
	if err := wkSetViewBackground(p.liveView(), int(packed)); err != nil && !errors.Is(err, errViewClosed) {
		log.Printf("browser: page %d base: %v", p.id, err)
	}
}

// FocusPage makes the presented page's view its window's first responder,
// as a click into it does. Whether it is presented is read on the main
// thread, after any hide queued before it. A window that is not key keeps
// the responder for when it becomes key; AppKit never activates the app for
// it. The request's Live is read there too. A page that closed is not
// presented.
func (e *wkEngine) FocusPage(handle ids.PageHandle, req focusRequest) (focusResult, error) {
	p := wkPaneTarget(handle)
	if p == nil {
		return focusNotPresented, nil
	}
	result, err := wkFocusPresentedView(p.liveView(), req.Live)
	if errors.Is(err, errViewClosed) {
		return focusNotPresented, nil
	}
	return result, err
}

var _ paneHost = (*wkEngine)(nil)

// forgetPane drops a closed page's presentation bookkeeping. The view itself
// is gone with the page; only the entry would otherwise linger.
func (e *wkEngine) forgetPane(id uint64) { e.pane.forget(id) }
