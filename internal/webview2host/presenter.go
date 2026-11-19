package webview2host

import (
	"errors"
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
// container and its controller, were last moved to. Only commitPlacement
// makes a known one, after every write succeeded; the zero value is no
// known place, which a move takes from its first write until it commits.
type appliedPlacement struct {
	bounds paneBounds
	known  bool
}

// at reports whether the page is known to stand at b.
func (p appliedPlacement) at(b paneBounds) bool { return p.known && p.bounds == b }

// nativeWrite is the result of one native write that moves a page: its
// controller's bounds, or its clip container's position. Only writeNative
// makes one, by running the write; the zero value is no write.
type nativeWrite struct {
	written bool
	err     error
}

// writeNative runs write and answers its result.
func writeNative(write func() error) nativeWrite {
	return nativeWrite{written: true, err: write()}
}

// done answers nil for a write that ran and succeeded, else why not.
func (w nativeWrite) done(what string) error {
	switch {
	case !w.written:
		return fmt.Errorf("%s was not written", what)
	case w.err != nil:
		return fmt.Errorf("%s: %w", what, w.err)
	}
	return nil
}

// clipWrite is what a placement needs of the page's clip container: no
// write for a controller parented directly to the host window
// (directlyParented), the container's own write otherwise (clippedBy). The
// zero value is neither, and commits nothing.
type clipWrite struct {
	kind  clipKind
	write nativeWrite
}

// clipKind is how a page's controller is clipped.
type clipKind int

const (
	// clipParent: the controller is a direct child of the host window.
	clipParent clipKind = iota + 1
	// clipContainer: the controller is inside the page's clip container.
	clipContainer
)

// directlyParented is the clip of a controller with no clip container.
func directlyParented() clipWrite { return clipWrite{kind: clipParent} }

// clippedBy is the clip of a controller inside a clip container, which
// write moved.
func clippedBy(write nativeWrite) clipWrite { return clipWrite{kind: clipContainer, write: write} }

// commitPlacement answers the placement at b that the page's writes made:
// its controller's (controller), and before it its clip container's where
// it has one (clip). It is known only when every write the page needs ran
// and succeeded; otherwise it is no known place, with why.
func commitPlacement(b paneBounds, clip clipWrite, controller nativeWrite) (appliedPlacement, error) {
	switch clip.kind {
	case clipContainer:
		if err := clip.write.done("clip bounds"); err != nil {
			return appliedPlacement{}, err
		}
	case clipParent:
	default:
		return appliedPlacement{}, errors.New("the page's clip is unknown")
	}
	if err := controller.done("controller bounds"); err != nil {
		return appliedPlacement{}, err
	}
	return appliedPlacement{bounds: b, known: true}, nil
}

// shownView is what the host has applied to a page: where both its native
// objects stand, and whether its clip is shown. Each field changes only
// after the native call that makes it true succeeded.
type shownView struct {
	placement appliedPlacement
	visible   bool
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
	case !shown.visible || !shown.placement.known:
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
// show or hide. A hidden page is not moved; it moves when it is next shown.
type presentStep struct {
	move   bool
	bounds paneBounds
	show   bool
	hide   bool
}

// nextPresentStep is what takes a page from the view the host applied to
// the one it wants. A page at no known place is moved whatever the bounds.
// raise asks for a show even when the page is already shown: a backend show
// re-raises the page above the SPA, which Wails' renderer recovery can
// recreate on top of it. The host records each part only as its native
// call succeeds (Host.present).
func nextPresentStep(shown shownView, want paneView, raise bool) presentStep {
	var step presentStep
	if want.visible && want.hasBounds && !shown.placement.at(want.bounds) {
		step.move, step.bounds = true, want.bounds
	}
	switch {
	case want.visible && (!shown.visible || raise):
		step.show = true
	case !want.visible && shown.visible:
		step.hide = true
	}
	return step
}
