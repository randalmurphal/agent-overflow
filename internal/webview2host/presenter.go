package webview2host

import (
	"fmt"

	"agent-overflow/internal/paneplace"
)

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

// paneView is where and whether a page's presentation wants it shown.
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
	// focusNoIntent leaves focus alone: no intent is pending for the page,
	// or the one pending was withdrawn, replaced or outlived its age
	// (FocusIntents.Take).
	focusNoIntent
)

// appliedPlacement is the place both of a page's native objects, its clip
// container and its controller, were last moved to. Only placePage makes a
// known one, after every write it made to b succeeded; the zero value is
// no known place, which a move takes from its first write until it
// succeeds.
type appliedPlacement struct {
	bounds paneBounds
	known  bool
}

// at reports whether the page is known to stand at b.
func (p appliedPlacement) at(b paneBounds) bool { return p.known && p.bounds == b }

// pageWrites are the native writes that move one page: its clip
// container's position, nil for a controller parented directly to the host
// window, and its controller's bounds.
type pageWrites struct {
	clip       func(rect) error
	controller func(rect) error
}

// placePage moves a page to b, scaled by sx and sy (deviceScale), with its
// writes, and answers its placement at b once every write succeeded. With a
// clip container the container moves first, then the controller inside it:
// moving the crop before its content means a growing pane never shows a
// frame of content outside the new clip, and a clip that did not move
// leaves the controller unwritten. With none the controller, a direct
// child of the host window, takes the full rect host-relative and
// overhangs. A refused write answers its error and no known place.
func placePage(b paneBounds, sx, sy float64, writes pageWrites) (appliedPlacement, error) {
	layout := resolvePaneLayout(b, sx, sy)
	if writes.clip == nil {
		if err := writes.controller(rect{
			Left:   layout.Container.Left + layout.Controller.Left,
			Top:    layout.Container.Top + layout.Controller.Top,
			Right:  layout.Container.Left + layout.Controller.Right,
			Bottom: layout.Container.Top + layout.Controller.Bottom,
		}); err != nil {
			return appliedPlacement{}, fmt.Errorf("controller bounds: %w", err)
		}
		return appliedPlacement{bounds: b, known: true}, nil
	}
	if err := writes.clip(layout.Container); err != nil {
		return appliedPlacement{}, fmt.Errorf("clip bounds: %w", err)
	}
	if err := writes.controller(layout.Controller); err != nil {
		return appliedPlacement{}, fmt.Errorf("controller bounds: %w", err)
	}
	return appliedPlacement{bounds: b, known: true}, nil
}

// shownView is what the host has applied to a page: where both its native
// objects stand, whether its clip is shown, and whether the raise of the
// show that made it so failed, which leaves it shown but perhaps under the
// SPA. Each field changes only by the native call it records: visible as
// the show or hide succeeded, whatever the raise did.
type shownView struct {
	placement   appliedPlacement
	visible     bool
	raiseFailed bool
}

// focusDecision answers what the host does with a page's pending focus
// intent, given the view it shows (the view it last applied). Only a page
// the host presents and shows takes keyboard focus, and only while the
// launcher window is the foreground window: moving focus into a window that
// is not would activate it, and AO never takes the OS focus from another
// application (the rule the accelerator hand-back keeps, focusWindow). An
// ineligible page keeps its intent pending. Eligibility is not permission:
// take, asked last and only of an eligible page, consumes the pending intent
// and answers whether the user's request still stands.
func focusDecision(shown shownView, launcherForeground bool, take func() bool) focusOutcome {
	switch {
	case !shown.visible || shown.raiseFailed || !shown.placement.known:
		return focusNotShown
	case !launcherForeground:
		return focusNotForeground
	case !take():
		return focusNoIntent
	default:
		return focusMove
	}
}

// presentStep is what the host must do to go from the view it shows to the
// one it wants: move first (so a show never paints at the old place), then
// show or hide, then, with focus, ask again whether the page's pending
// focus intent applies (focusDecision). A hidden page is not moved; it
// moves when it is next shown.
type presentStep struct {
	move   bool
	bounds paneBounds
	show   bool
	hide   bool
	focus  bool
}

// nextPresentStep is what takes a page from the view the host applied to
// the one it wants. A page at no known place is moved whatever the bounds.
// raise asks for a show even when the page is already shown: a backend show
// re-raises the page above the SPA, which Wails' renderer recovery can
// recreate on top of it; so does a shown page whose last raise failed. A
// step that can make a page eligible for its
// pending focus intent asks for it: a show, and a move that gives a page
// already shown a known place again. The host records each part only as
// its native call succeeds (Host.present).
func nextPresentStep(shown shownView, want paneView, raise bool) presentStep {
	var step presentStep
	if want.visible && want.hasBounds && !shown.placement.at(want.bounds) {
		step.move, step.bounds = true, want.bounds
	}
	switch {
	case want.visible && (!shown.visible || shown.raiseFailed || raise):
		step.show = true
	case !want.visible && shown.visible:
		step.hide = true
	}
	step.focus = step.show || step.move && shown.visible && !shown.placement.known
	return step
}
