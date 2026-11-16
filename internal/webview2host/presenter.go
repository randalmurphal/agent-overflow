package webview2host

import "agent-overflow/internal/paneplace"

// pagePresentation is what the launcher knows about one page's presentation
// from the backend: its last bounds directive (the placement, the mount it
// was placed in, the page's viewport) and whether the backend shows the page.
// The page-to-mount binding comes only from here; local geometry never names
// a page.
type pagePresentation struct {
	directive    paneBounds
	hasBounds    bool
	mount        string
	pageW, pageH int
	backendShown bool
}

// noteBounds records a validated bounds directive.
func (p *pagePresentation) noteBounds(d Directive) {
	p.directive, p.hasBounds = d.paneBounds(), true
	p.mount, p.pageW, p.pageH = d.Mount, d.PageW, d.PageH
}

// paneView is where and whether the host shows a page.
type paneView struct {
	bounds    paneBounds
	hasBounds bool
	visible   bool
}

// view decides where and whether a page shows. With local geometry for the
// page's mount, the page is placed from it with the backend's own rule
// (paneplace.Fit at the directive's page size) and shows only while the
// backend shows it AND the local report says the pane is visible: the local
// report is never older than the one the backend placed from, so the
// directive's position is ignored and a stale directive cannot move the view
// back. Without local geometry (an older SPA or backend, or a mount the
// mailbox evicted) the directive stands as it always did.
func (p pagePresentation) view(local PaneGeometry, hasLocal bool) paneView {
	if p.mount == "" || !hasLocal || p.pageW <= 0 || p.pageH <= 0 || local.Mount != p.mount {
		return paneView{bounds: p.directive, hasBounds: p.hasBounds, visible: p.backendShown}
	}
	fit, _, ok := paneplace.Fit(paneplace.Rect{
		X: local.X, Y: local.Y, W: local.Width, H: local.Height,
		ClipX: local.ClipX, ClipY: local.ClipY, ClipW: local.ClipWidth, ClipH: local.ClipHeight,
	}, p.pageW, p.pageH)
	if !ok || !local.Visible {
		return paneView{}
	}
	return paneView{
		bounds: paneBounds{
			X: fit.X, Y: fit.Y, W: fit.W, H: fit.H,
			CX: fit.ClipX, CY: fit.ClipY, CW: fit.ClipW, CH: fit.ClipH,
			VW: local.ViewportWidth, VH: local.ViewportHeight, DPR: local.DevicePixelRatio,
			Bg: local.Background,
		},
		hasBounds: true,
		visible:   p.backendShown,
	}
}

// presentStep is what the host must do to go from the view it shows to the
// one it wants: move first (so a show never paints at the old place), then
// show or hide. A hidden page is not moved; it moves when it is next shown.
type presentStep struct {
	move   bool
	bounds paneBounds
	show   bool
	hide   bool
}

// nextPresentStep diffs two views. raise asks for a show even when the page
// is already shown: a backend show re-raises the page above the SPA, which
// Wails' renderer recovery can recreate on top of it.
func nextPresentStep(shown, want paneView, raise bool) (presentStep, paneView) {
	var step presentStep
	next := shown
	if want.visible && want.hasBounds && (!shown.hasBounds || shown.bounds != want.bounds) {
		step.move, step.bounds = true, want.bounds
		next.bounds, next.hasBounds = want.bounds, true
	}
	switch {
	case want.visible && (!shown.visible || raise):
		step.show = true
	case !want.visible && shown.visible:
		step.hide = true
	}
	next.visible = want.visible
	return step, next
}
