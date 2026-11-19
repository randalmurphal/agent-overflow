//go:build windows

package webview2host

// Directive application: geometry, presentation and focus.

// Apply validates and executes one directive. It is safe to call from any
// goroutine and blocks until the work has run on the UI thread.
//
// An invalid directive is logged and dropped. That is deliberate at both
// ends: the launcher never guesses at a command that creates OS windows,
// and dropping one costs a retry rather than tearing down the bridge the
// retry would arrive on.
func (h *Host) Apply(directive Directive) {
	if err := directive.Validate(); err != nil {
		h.config.Logf("browser host: ignore invalid directive: %v", err)
		return
	}
	switch directive.Op {
	case OpCreate:
		h.create(directive)
	case OpBounds:
		h.withPage(directive.PageID, func(page *hostPage) {
			page.pres.noteBounds(directive)
			h.present(page, false)
		})
	case OpBackground:
		h.withPage(directive.PageID, func(page *hostPage) { h.applyBackground(page, directive.Bg) })
	case OpShow:
		h.withPage(directive.PageID, func(page *hostPage) {
			page.pres.backendShown = true
			h.present(page, true)
		})
	case OpHide:
		h.withPage(directive.PageID, func(page *hostPage) {
			page.pres.backendShown = false
			h.present(page, false)
		})
	case OpDevTools:
		h.withPage(directive.PageID, func(page *hostPage) {
			if err := page.view.openDevToolsWindow(); err != nil {
				h.config.Logf("browser host: page %s devtools: %v", page.id, err)
			}
		})
	case OpClose:
		h.closePages(func(page *hostPage) bool { return page.id == directive.PageID })
	case OpCloseProfile:
		h.closePages(func(page *hostPage) bool { return page.profileID == directive.ProfileID })
	case OpClearData:
		h.clearData(directive.PageID)
	case OpAccelerators:
		h.setAccelerators(directive.Accelerators)
	case OpFind:
		h.withPage(directive.PageID, func(page *hostPage) { h.startFind(page, directive.Query) })
	case OpFindStop:
		h.withPage(directive.PageID, h.stopFind)
	case OpFocus:
		h.config.Focus.Deliver(directive.Mount, directive.Intent, directive.PageID)
		h.withPage(directive.PageID, h.applyPendingFocus)
	case OpCancelDownload:
		h.withPage(directive.PageID, func(page *hostPage) { h.answerDownload(page, directive.Download, true) })
	case OpDownloadStatus:
		h.withPage(directive.PageID, func(page *hostPage) { h.answerDownload(page, directive.Download, false) })
	}
}

// withPage runs fn on the UI thread with the named page, found in the same
// turn, or logs that it is gone. A directive for a page that already died
// is normal (the backend's view lags a process failure), not an error.
func (h *Host) withPage(pageID string, fn func(*hostPage)) {
	h.config.OnMain(func() {
		page := h.pages[pageID]
		if page == nil || page.controller == nil {
			h.config.Logf("browser host: no live page %s", pageID)
			return
		}
		fn(page)
	})
}

// applyPendingFocus moves keyboard focus into the page for the focus intent
// delivered for it in the mount it is presented in, while the host shows
// the page, the launcher window is the foreground window and the intent
// still stands (focusDecision). A page not shown or a window in the
// background leaves the intent pending, and the page's next show applies it
// (present). Runs on the UI thread, so the view and mount it checks are the
// ones on screen.
func (h *Host) applyPendingFocus(page *hostPage) {
	mount := page.pres.mount
	var intent uint64
	take := func() bool {
		var ok bool
		intent, ok = h.config.Focus.Take(mount, page.id)
		return ok
	}
	if focusDecision(page.shown, h.foreground(h.config.HostWindow()), take) != focusMove {
		return
	}
	report := FocusReport{Mount: mount, Intent: intent}
	if err := page.controller.moveFocus(moveFocusProgrammatic); err != nil {
		h.config.Logf("browser host: page %s focus: %v", page.id, err)
		report.Error = err.Error()
		h.config.Report(page.id, ReportFocusFailed, report.Encode())
		return
	}
	h.config.Report(page.id, ReportFocused, report.Encode())
}

// ApplyGeometry re-presents every page bound to a mount the reports name.
// It runs on the UI thread, as the launcher's single pending drain of the
// geometry mailbox, and must not be called from anywhere else.
func (h *Host) ApplyGeometry(reports []PaneGeometry) {
	if len(reports) == 0 {
		return
	}
	mounts := make(map[string]bool, len(reports))
	for _, report := range reports {
		mounts[report.Mount] = true
	}
	for _, page := range h.pages {
		if page.controller != nil && page.pres.mount != "" && mounts[page.pres.mount] {
			h.present(page, false)
		}
	}
}

// present brings one page to the view its presentation calls for
// (presenter.go): moved before it is shown, never moved while hidden. raise
// re-raises a page already shown. Runs on the UI thread.
func (h *Host) present(page *hostPage, raise bool) {
	local, hasLocal := h.config.Geometry.Latest(page.pres.mount)
	step, next := nextPresentStep(page.shown, page.pres.view(local, hasLocal), raise)
	if step.move {
		h.applyBounds(page, step.bounds)
	}
	if step.show && !h.showPage(page) {
		next.visible = page.shown.visible
	}
	if step.hide {
		h.hidePage(page)
	}
	page.shown = next
	if step.show {
		h.applyPendingFocus(page)
	}
}

// showPage shows the page's clip container and raises it.
func (h *Host) showPage(page *hostPage) bool {
	if err := page.controller.putIsVisible(true); err != nil {
		h.config.Logf("browser host: page %s show: %v", page.id, err)
		return false
	}
	// The container tracks visibility too. It is not decoration: a shown
	// container with nothing in it is an invisible window swallowing every
	// click over its rectangle, which is exactly what a hidden pane must not
	// do.
	showWindow(page.container, true)
	// Every backend show re-raises: Wails' own recovery path can recreate
	// the SPA controller, which would land it above the pane again, and a
	// raise is cheap.
	raiseChild(page.raiseTarget())
	return true
}

// hidePage hides the page's clip container. Hiding is the CONTAINER's: the
// controller stays IsVisible so Chromium keeps compositing the page (rAF,
// scroll state, and the frame a screenshot copies). IsVisible=false stops
// all of that and evicts the surface, so a hidden page's first capture would
// wait for a frame that never comes (spike 2026-09-14). Only the unclipped
// fallback, with no container to hide, has to hide the controller itself.
func (h *Host) hidePage(page *hostPage) {
	held := focusWithin(page.raiseTarget())
	if page.container != 0 {
		showWindow(page.container, false)
	} else if err := page.controller.putIsVisible(false); err != nil {
		h.config.Logf("browser host: page %s hide: %v", page.id, err)
	}
	h.releaseFocus(page, held)
}

// applyBounds positions one page. Runs on the UI thread.
//
// The rect arrives in the SPA's CSS pixels with the pixel ratio and viewport
// it was measured under (deviceScale). BOTH rects are scaled by that one
// factor pair: a clip scaled differently from the rect it crops would slide
// across the page as the window resized.
func (h *Host) applyBounds(page *hostPage, b paneBounds) {
	cw, ch, hasClient := clientSize(h.config.HostWindow())
	sx, sy := deviceScale(b, cw, ch, hasClient)
	layout := resolvePaneLayout(b, sx, sy)

	bounds := layout.Controller
	if page.container == 0 {
		// No container: the controller is still a direct child of the host,
		// so its bounds are host-relative and there is nothing to clip
		// against. Position the full rect and let it overhang, which is the
		// behaviour that shipped before clipping existed.
		bounds = rect{
			Left:   layout.Container.Left + layout.Controller.Left,
			Top:    layout.Container.Top + layout.Controller.Top,
			Right:  layout.Container.Left + layout.Controller.Right,
			Bottom: layout.Container.Top + layout.Controller.Bottom,
		}
	} else {
		// Container first, then the controller inside it: moving the crop
		// before its content means a growing pane never shows a frame of
		// content outside the new clip.
		moveWindow(page.container, layout.Container)
	}
	if err := page.controller.putBounds(bounds); err != nil {
		h.config.Logf("browser host: page %s bounds: %v", page.id, err)
	}
}

// applyBackground sets the controller's default background colour (an
// OpBackground's) when it changed. Runs on the UI thread, shown or hidden.
func (h *Host) applyBackground(page *hostPage, bg string) {
	if bg == page.bg {
		return
	}
	if page.controller2 == nil {
		if !page.bgUnsupported {
			page.bgUnsupported = true
			h.config.Logf("browser host: page %s: installed WebView2 runtime has no "+
				"ICoreWebView2Controller2; pane background colour is unavailable", page.id)
		}
		return
	}
	color, ok := parsePaneColor(bg)
	if !ok {
		// Validate already refused every other spelling; reaching here
		// would mean the two halves of that rule drifted apart.
		h.config.Logf("browser host: page %s: ignoring unparseable background %q", page.id, bg)
		return
	}
	if err := page.controller2.putDefaultBackgroundColor(color); err != nil {
		h.config.Logf("browser host: page %s background %s: %v", page.id, bg, err)
		return
	}
	page.bg = bg
}
