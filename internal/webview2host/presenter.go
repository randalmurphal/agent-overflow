package webview2host

import "agent-overflow/internal/paneplace"

// pagePresentation is what the launcher knows about one page's presentation
// from the backend: its last bounds directive (the placement, the mount it
// was placed in, the page's viewport, the sequence of the report it was
// placed from) and whether the backend shows the page. The page-to-mount
// binding comes only from here; local geometry never names a page.
type pagePresentation struct {
	directive    paneBounds
	hasBounds    bool
	mount        string
	pageW, pageH int
	seq          uint64
	backendShown bool
}

// noteBounds records a validated bounds directive.
func (p *pagePresentation) noteBounds(d Directive) {
	p.directive, p.hasBounds = d.paneBounds(), true
	p.mount, p.pageW, p.pageH, p.seq = d.Mount, d.PageW, d.PageH, d.Seq
}

// paneView is where and whether the host shows a page.
type paneView struct {
	bounds    paneBounds
	hasBounds bool
	visible   bool
}

// view decides where and whether a page shows. The SPA numbers each report
// once and sends that number on both paths, so the newer of the local report
// and the directive's report places the page. With local geometry for the
// page's mount at the directive's sequence or later, the page is placed from
// it with the backend's own rule (paneplace.Fit at the directive's page
// size) and shows only while the backend shows it AND the local report says
// the pane is visible, so a directive that lags the pane cannot move the
// view back. A directive newer than the local report (its post failed, or
// has not arrived) stands, as it does without local geometry (an older SPA,
// or a mount the mailbox evicted). A directive with no sequence (an older
// backend) yields to any local report.
func (p pagePresentation) view(local PaneGeometry, hasLocal bool) paneView {
	if p.mount == "" || !hasLocal || p.pageW <= 0 || p.pageH <= 0 || local.Mount != p.mount || local.Seq < p.seq {
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

// focusOutcome is what an OpFocus does.
type focusOutcome int

const (
	// focusMove moves keyboard focus into the page's controller.
	focusMove focusOutcome = iota
	// focusNotShown leaves focus alone: the host does not show the page.
	focusNotShown
	// focusNotForeground leaves focus alone: the launcher window is not the
	// foreground window.
	focusNotForeground
)

// focusDecision answers what an OpFocus does for a page the host shows as
// shown (the view it last applied). Only a page the host presents and shows
// takes keyboard focus, and only while the launcher window is the foreground
// window: moving focus into a window that is not would activate it, and AO
// never takes the OS focus from another application (the rule the
// accelerator hand-back keeps, focusWindow).
func focusDecision(shown paneView, launcherForeground bool) focusOutcome {
	switch {
	case !shown.visible || !shown.hasBounds:
		return focusNotShown
	case !launcherForeground:
		return focusNotForeground
	default:
		return focusMove
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
