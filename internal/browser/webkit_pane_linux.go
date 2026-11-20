//go:build linux && cgo && !gtk3 && !android && !server && !nogui

package browser

import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	"agent-overflow/internal/browser/ids"
)

// The engine half of the Manager's pane presentation (`paneHost`). Where the
// hosted engine answers these calls with directives to the launcher, this
// engine moves its own GTK widgets: present puts the page's host over the
// pane's clip rect and draws the page scaled to the placement, hide is a
// re-park into an empty box, and devtools is the WebKit inspector
// (docked in-app; developer extras are enabled at view creation). The page's
// viewport is never touched here; SetViewport owns it, parked or presented.
// The page's base (SetPageBase) is the view's own background, parked or
// presented. What to present is decided in nativePanes (native_pane.go);
// the GTK call follows, outside its lock.

// webkitPaneTarget answers the page a pane call addresses, or nil for a
// handle this engine never issued (a page that already closed is a lookup
// miss, not an error).
func webkitPaneTarget(handle ids.PageHandle) *webkitPage {
	id, err := strconv.ParseUint(strings.TrimPrefix(string(handle), "page-"), 10, 64)
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

func (e *webkitEngine) SetPageBounds(handle ids.PageHandle, placement PanePlacement) {
	p := webkitPaneTarget(handle)
	if p == nil {
		return
	}
	if e.pane.bounds(p.id, placement) {
		p.present(placement)
	}
}

func (e *webkitEngine) ShowPage(handle ids.PageHandle) {
	p := webkitPaneTarget(handle)
	if p == nil {
		return
	}
	if placement, present := e.pane.show(p.id); present {
		p.present(placement)
	}
}

func (e *webkitEngine) HidePage(handle ids.PageHandle) {
	p := webkitPaneTarget(handle)
	if p == nil {
		return
	}
	if !e.pane.hide(p.id) {
		return
	}
	if err := webkitHideView(p.liveView()); err != nil && !errors.Is(err, errViewClosed) {
		log.Printf("browser: hide page %d: %v", p.id, err)
	}
}

// SetPageBase sets the view's background color, which WebKit paints under
// the document whether the view is parked or presented.
func (e *webkitEngine) SetPageBase(handle ids.PageHandle, color string) {
	p := webkitPaneTarget(handle)
	if p == nil {
		return
	}
	packed, ok := nativePaneColor(color)
	if !ok {
		log.Printf("browser: page %d base %q is not #rrggbb", p.id, color)
		return
	}
	if err := webkitSetViewBackground(p.liveView(), packed); err != nil && !errors.Is(err, errViewClosed) {
		log.Printf("browser: page %d base: %v", p.id, err)
	}
}

// FocusPage gives the presented page's view keyboard focus within the
// window while the request is live. Both are read on the GTK thread, after
// any hide queued before it. A page that closed is not presented.
func (e *webkitEngine) FocusPage(handle ids.PageHandle, req focusRequest) (focusResult, error) {
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

func (e *webkitEngine) OpenPageDevTools(handle ids.PageHandle) error {
	p := webkitPaneTarget(handle)
	if p == nil {
		return fmt.Errorf("browser: open devtools: %w", errViewClosed)
	}
	if err := webkitOpenInspector(p.liveView()); err != nil {
		return fmt.Errorf("browser: open devtools: %w", err)
	}
	return nil
}

// forgetPane drops a closed page's presentation bookkeeping. The view itself
// is gone with the page; only the entry would otherwise linger.
func (e *webkitEngine) forgetPane(id uint64) { e.pane.forget(id) }
