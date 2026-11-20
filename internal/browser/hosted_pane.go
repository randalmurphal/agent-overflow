package browser

import (
	"context"
	"fmt"
	"time"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/webview2host"
)

type pageRatio struct{ pane, page float64 }

// viewScale is what a page's device-metrics override is asked to draw at: the
// placement's scale, and the pane's device pixel ratio the webview zoom is
// recovered from (paneZoom). A hidden page asks for {1, 0}: no zoom.
type viewScale struct{ placement, paneRatio float64 }

type viewScaleState struct {
	want  viewScale
	asked bool
	busy  bool
}

func (e *hostedEngine) ShowPage(handle ids.PageHandle) {
	if e.setShown(handle, true) {
		e.dispatch(webview2host.Directive{Op: webview2host.OpShow, PageID: string(handle)})
		// A hide put the override back at 1; a re-show at an unchanged
		// placement sends no bounds, so the placement's scale is restored
		// here or the page would draw at 1:1 and be cropped by the pane.
		e.syncViewScale(handle)
	}
}

// FocusPage asks the launcher to move keyboard focus into the page's
// controller for the request's intent. The directive rides the page's lane
// after its show, and the launcher decides against what it shows, whether
// its window is in the foreground, and whether the SPA has withdrawn the
// intent (webview2host.FocusIntents); it answers ReportFocused or
// ReportFocusFailed for a move it made or that was refused.
func (e *hostedEngine) FocusPage(handle ids.PageHandle, req focusRequest) (focusResult, error) {
	e.mu.Lock()
	shown := e.shown[handle]
	e.mu.Unlock()
	if !shown {
		return focusNotPresented, nil
	}
	if !req.Live() {
		return focusWithdrawn, nil
	}
	if !e.dispatch(webview2host.Directive{Op: webview2host.OpFocus, PageID: string(handle), Mount: string(req.Mount), Intent: req.Intent}) {
		return focusNotPresented, fmt.Errorf("browser: the pane host could not be asked to focus page %s", handle)
	}
	return focusDispatched, nil
}

// HidePage hides the page's clip container. The controller itself stays
// visible to Chromium, so a hidden page keeps compositing: screenshots and
// scroll state stay live exactly as on the WebKit engines' 1x1 park.
func (e *hostedEngine) HidePage(handle ids.PageHandle) {
	if e.setShown(handle, false) {
		e.dispatch(webview2host.Directive{Op: webview2host.OpHide, PageID: string(handle)})
		e.syncViewScale(handle)
	}
}

// SetPageBounds positions the controller at the FITTED rect (the page scaled
// to fit the pane) and tells the page's device-metrics override the same
// scale, so the emulated viewport draws exactly into that rect. The override
// draws at the OS scale alone, so the webview zoom is recovered from the two
// device pixel ratios.
//
// The directive also names the pane mount, the page's viewport and the
// report's sequence: a launcher that measures the mount itself places the
// page from its own rect with the same rule (paneplace.Fit) unless this
// directive's report is newer than its own.
func (e *hostedEngine) SetPageBounds(handle ids.PageHandle, placement PanePlacement) {
	if !e.setBounds(handle, placement) {
		return
	}
	rect := placement.Rect
	e.dispatch(webview2host.Directive{
		Op: webview2host.OpBounds, PageID: string(handle),
		X: rect.X, Y: rect.Y, W: rect.Width, H: rect.Height,
		CX: rect.ClipX, CY: rect.ClipY, CW: rect.ClipWidth, CH: rect.ClipHeight,
		VW: rect.ViewportWidth, VH: rect.ViewportHeight,
		DPR:   rect.DevicePixelRatio,
		Mount: string(placement.Mount), PageW: placement.PageWidth, PageH: placement.PageHeight,
		Seq: rect.Seq,
	})
	e.syncViewScale(handle)
}

// SetPageBase sends the page's base to the launcher, which paints it as the
// controller's default background whether or not the page has bounds or is
// shown. It rides the page's lane, so it lands after the page's create.
func (e *hostedEngine) SetPageBase(handle ids.PageHandle, color string) {
	e.mu.Lock()
	_, known := e.targetByPage[handle]
	e.mu.Unlock()
	if known {
		e.dispatch(webview2host.Directive{Op: webview2host.OpBackground, PageID: string(handle), Bg: color})
	}
}

// syncViewScale asks the page's override for the scale its presentation
// state calls for: the placement's scale times the pane zoom while shown,
// 1 while hidden. A placement that only moved asks for what was already
// asked, which costs nothing.
func (e *hostedEngine) syncViewScale(handle ids.PageHandle) {
	e.mu.Lock()
	shown := e.shown[handle]
	placement, hasBounds := e.bounds[handle]
	e.mu.Unlock()
	if !shown || !hasBounds {
		e.applyViewScale(handle, viewScale{placement: 1})
		return
	}
	e.applyViewScale(handle, viewScale{placement: placement.Scale, paneRatio: max(placement.Rect.DevicePixelRatio, 0)})
}

// paneZoom is the SPA's webview zoom: its device pixel ratio over the page's
// own (the OS scale). Unknown ratios mean no zoom.
func paneZoom(paneRatio, pageRatio float64) float64 {
	if paneRatio <= 0 || pageRatio <= 0 {
		return 1
	}
	return paneRatio / pageRatio
}

func (e *hostedEngine) driverFor(handle ids.PageHandle) *cdpPage {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.drivers[handle]
}

// pageDevicePixelRatio answers the page's own devicePixelRatio, read once per
// pane ratio it is asked under (see ratios), so only a monitor or zoom change
// costs a CDP round trip. 0 means unknown, which paneZoom treats as no zoom.
// It runs in the scale drain, never in a presentation pass.
func (e *hostedEngine) pageDevicePixelRatio(handle ids.PageHandle, paneRatio float64) float64 {
	e.mu.Lock()
	cached, ok := e.ratios[handle]
	p := e.drivers[handle]
	e.mu.Unlock()
	if ok && cached.pane == paneRatio && cached.page > 0 {
		return cached.page
	}
	if p == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()
	ratio, err := e.readPageRatio(ctx, p)
	if err != nil {
		if p.ctx.Err() == nil {
			e.logf("browser: pane page %s device pixel ratio: %v", handle, err)
		}
		return 0
	}
	e.mu.Lock()
	if _, known := e.drivers[handle]; known {
		e.ratios[handle] = pageRatio{pane: paneRatio, page: ratio}
	}
	e.mu.Unlock()
	return ratio
}

// applyViewScale asks the page's override for a scale and returns at once.
// A pane drag reports a rect per frame, and each override is a CDP round
// trip; applied inline and in order they queued behind one another for
// seconds after the drag ended. So the call is latest-wins: one override in
// flight per page, and the newest scale asked for while it runs is the only
// one applied after it. Asking for the scale already asked for does nothing,
// which is what keeps a position-only change free of metrics work. Failures
// are logged rather than returned: the caller (a pane rect report) has
// nothing to do with one, and a page that refuses is one that is closing.
func (e *hostedEngine) applyViewScale(handle ids.PageHandle, want viewScale) {
	e.mu.Lock()
	if _, known := e.drivers[handle]; !known {
		e.mu.Unlock()
		return
	}
	st := e.scales[handle]
	if st == nil {
		st = &viewScaleState{}
		e.scales[handle] = st
	}
	if st.asked && st.want == want {
		e.mu.Unlock()
		return
	}
	st.want, st.asked = want, true
	asked := e.viewScaleAsked
	start := !st.busy
	st.busy = true
	e.mu.Unlock()
	if asked != nil {
		asked(handle, want)
	}
	if start {
		go e.drainViewScale(handle, st)
	}
}

func (e *hostedEngine) drainViewScale(handle ids.PageHandle, st *viewScaleState) {
	for {
		e.mu.Lock()
		want := st.want
		p := e.drivers[handle]
		e.mu.Unlock()
		if p == nil {
			e.mu.Lock()
			st.busy = false
			e.mu.Unlock()
			return
		}
		scale := want.placement
		applied := true
		if want.paneRatio > 0 {
			pageRatio := e.pageDevicePixelRatio(handle, want.paneRatio)
			applied = pageRatio > 0
			scale *= paneZoom(want.paneRatio, pageRatio)
		}
		ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
		err := p.SetViewScale(ctx, scale)
		cancel()
		if err != nil && p.ctx.Err() == nil {
			e.logf("browser: pane page %s view scale: %v", handle, err)
		}
		applied = applied && err == nil
		e.mu.Lock()
		if st.want == want || p.ctx.Err() != nil {
			// A scale that did not fully apply is not remembered as asked,
			// so the next presentation asks again.
			st.asked = st.asked && applied
			st.busy = false
			e.mu.Unlock()
			return
		}
		e.mu.Unlock()
	}
}

// setBounds is the bounds half of the setShown dedupe: the presentation sync
// re-sends the active page's placement on every selection, focus and
// page-list change, and only an actually-changed one should cost a directive.
func (e *hostedEngine) setBounds(handle ids.PageHandle, placement PanePlacement) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, known := e.targetByPage[handle]; !known {
		return false
	}
	if e.bounds[handle] == placement {
		return false
	}
	e.bounds[handle] = placement
	return true
}

// OpenPageDevTools sends the devtools directive; a directive the pane host
// cannot take fails the request.
func (e *hostedEngine) OpenPageDevTools(handle ids.PageHandle) error {
	if !e.dispatch(webview2host.Directive{Op: webview2host.OpDevTools, PageID: string(handle)}) {
		return fmt.Errorf("browser: open devtools: the pane host refused the request")
	}
	return nil
}

// setShown reports whether the visibility actually changed for a page this
// engine still owns. Visibility is recomputed on every selection, focus
// and page-list change, so re-emitting an unchanged state would put a
// directive on the wire for every one of them.
func (e *hostedEngine) setShown(handle ids.PageHandle, visible bool) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, known := e.targetByPage[handle]; !known {
		return false
	}
	if e.shown[handle] == visible {
		return false
	}
	e.shown[handle] = visible
	return true
}
