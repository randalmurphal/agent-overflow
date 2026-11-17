//go:build windows

package webview2host

import (
	"fmt"
)

// Page creation: admission, the controller's start and its completion.

func (h *Host) create(directive Directive) {
	page := &hostPage{id: directive.PageID, profileID: directive.ProfileID, ephemeral: directive.Ephemeral}
	var request environmentRequest
	var refused string
	h.config.OnMain(func() {
		refused = h.admit(page.id)
		if refused != "" {
			return
		}
		h.pages[page.id] = page
		var start bool
		request, start = h.env.request()
		if start {
			h.env.begin(request.gen)
		}
	})
	if refused != "" {
		h.config.Report(page.id, ReportCreateFailed, refused)
		return
	}
	env, err := h.env.await(request)
	h.config.OnMain(func() {
		if !h.pageRegistered(page) {
			return
		}
		if err != nil {
			h.failUncreated(page, err.Error())
			return
		}
		h.startController(page, env.env10, h.config.HostWindow())
	})
}

// admit answers why a new page cannot be registered under id, or "" when
// it can. Every page, a backend create's and a popup's, is admitted here
// alone. Runs on the UI thread.
func (h *Host) admit(id string) string {
	switch {
	case h.closed:
		return "the browser host is closed"
	case h.clearing:
		return "the browser host is clearing site data"
	case h.pages[id] != nil:
		return "page id already exists"
	case len(h.pages) >= MaxHostPages:
		return fmt.Sprintf("the pane has %d pages", len(h.pages))
	}
	return ""
}

// startController starts the asynchronous create of page's controller in
// its profile, under hwnd. Runs on the UI thread.
func (h *Host) startController(page *hostPage, env10 *iCoreWebView2Environment10, hwnd uintptr) {
	if hwnd == 0 {
		h.failUncreated(page, "launcher window is not available")
		return
	}
	options, err := env10.createControllerOptions()
	if err != nil {
		h.failUncreated(page, "controller options: "+err.Error())
		return
	}
	defer options.release()
	if err := options.putProfileName(page.profileID); err != nil {
		h.failUncreated(page, "profile name: "+err.Error())
		return
	}
	if err := options.putInPrivate(page.ephemeral); err != nil {
		h.failUncreated(page, "inprivate: "+err.Error())
		return
	}

	// Create the final parent BEFORE the controller. Reparenting a live
	// windowed WebView2 controller can leave its compositor presenting
	// stale/blank strips while the document scrolls. Passing the clip
	// container to controller creation gives Chromium one stable parent
	// for the controller's whole lifetime.
	parent := hwnd
	container, err := createClipContainer(hwnd)
	if err != nil {
		// Clipping is a presentation refinement, not a precondition. Keep
		// the older direct-parent path and identify its child so it can be
		// raised over the SPA controller.
		h.config.Logf("browser host: page %s clip container: %v; pane will not clip", page.id, err)
		page.beforeChildren = childWindows(hwnd)
	} else {
		page.container = container
		parent = container
	}

	handler := newControllerCompletedHandler(func(hr uintptr, controller *iCoreWebView2Controller) {
		if page.popup != nil {
			h.pendingPopups--
		}
		h.controllerCompleted(page, hwnd, hr, controller)
	})
	// WebView2 holds its own reference until it calls back; this is the
	// constructor's. The handler is not detached when the page closes:
	// its completion closes a controller that outlived the page.
	defer handler.ref.release()
	if page.popup != nil {
		h.pendingPopups++
	}
	if err := env10.createControllerWithOptions(parent, options, handler); err != nil {
		if page.popup != nil {
			h.pendingPopups--
		}
		h.failUncreated(page, "create controller: "+err.Error())
	}
}

// controllerCompleted runs on the UI thread from the message pump.
func (h *Host) controllerCompleted(page *hostPage, hwnd uintptr, hr uintptr, controller *iCoreWebView2Controller) {
	if err := hresult(hr); err != nil {
		// ERROR_INVALID_STATE (0x8007139F) here means "same user data
		// folder, different browser arguments", not a process-wide
		// conflict: the error surfaces at CONTROLLER creation even
		// though the mismatch is the ENVIRONMENT's. If it ever appears,
		// suspect a collapsed user data folder (see EnvOverrideNames)
		// before suspecting the argument rule.
		h.failUncreated(page, err.Error())
		return
	}
	if controller == nil {
		h.failUncreated(page, "controller completed with no controller")
		return
	}
	controller.addRef()
	if !h.pageRegistered(page) {
		// A close or close-profile settled this page while the create was
		// in flight; the backend has already heard closed. Adopting the
		// controller now would leak a window no directive can address, and
		// reporting created would arrive after closed.
		if err := controller.close(); err != nil {
			h.config.Logf("browser host: page %s close raced controller: %v", page.id, err)
		}
		controller.release()
		destroyWindow(page.container)
		page.container = 0
		return
	}
	page.controller = controller
	// ICoreWebView2Controller2 is the whole background-colour surface.
	// Querying once here keeps every background directive off
	// QueryInterface; a nil result is reported the first time a colour is
	// actually asked for, not now.
	page.controller2 = controller.queryController2()

	// Pages start hidden: the clip container is created unshown, and
	// browser_visibility is what shows it. The controller itself is visible
	// from the start, for the reason OpHide gives.
	if err := controller.putIsVisible(page.container != 0); err != nil {
		h.config.Logf("browser host: page %s initial visibility: %v", page.id, err)
	}

	if page.container != 0 {
		// Normal path: the container was the controller's parent from its
		// creation call, so no live WebView2 window is ever reparented.
		raiseChild(page.container)
	} else {
		// Container creation failed before the controller started. The
		// controller is a direct child of the host, so identify and raise it
		// over the SPA's own WebView2.
		page.child = newChildWindow(page.beforeChildren, childWindows(hwnd))
		page.beforeChildren = nil
		if page.child == 0 {
			h.config.Logf("browser host: page %s child window not identified; "+
				"the pane may render behind the app and cannot be clipped", page.id)
		} else {
			raiseChild(page.child)
		}
	}

	view, err := controller.coreWebView2()
	if err != nil || view == nil {
		h.discardCreated(page, "core webview: "+errString(err))
		return
	}
	page.view = view

	failHandler := newProcessFailedHandler(func(kind uint32) {
		h.pageReport(page, ReportProcessFailed, processFailedKindName(kind))
	})
	page.failHandler = failHandler
	if _, err := view.addProcessFailed(failHandler); err != nil {
		h.config.Logf("browser host: page %s process-failed subscription: %v", page.id, err)
	}
	failHandler.ref.release()
	keyHandler := newAcceleratorKeyPressedHandler(func(args *iAcceleratorKeyPressedEventArgs) {
		h.acceleratorPressed(page, args)
	})
	page.keyHandler = keyHandler
	if _, err := controller.addAcceleratorKeyPressed(keyHandler); err != nil {
		h.config.Logf("browser host: page %s accelerator subscription: %v", page.id, err)
	}
	keyHandler.ref.release()
	for _, watch := range []func(*hostPage, *iCoreWebView2) error{h.watchDownloads, h.watchDownloadDialog, h.watchWindows} {
		if err := watch(page, view); err != nil {
			h.discardCreated(page, err.Error())
			return
		}
	}

	// A runtime without the find API has to be known before the page is
	// used, so its report precedes created; a popup's follows its popup
	// report at once.
	if !h.findSupported(view) {
		h.pageReport(page, ReportFindUnavailable, "")
	}
	if page.popup != nil {
		h.popupReady(page, view)
		return
	}

	// Target.getTargetInfo through the controller's own DevTools session
	// answers with THIS page's targetInfo, which is the handle the
	// backend attaches chromedp to. Reporting created only after it
	// lands means the backend never sees a page it cannot drive.
	done := newDevToolsCompletedHandler(func(hr uintptr, resultJSON string) {
		if !h.pageRegistered(page) {
			// Closed while the target-info round trip was in flight; the
			// closed report has already settled this page.
			return
		}
		if err := hresult(hr); err != nil {
			h.discardCreated(page, "Target.getTargetInfo: "+err.Error())
			return
		}
		targetID, err := ParseTargetID(resultJSON)
		if err != nil {
			h.discardCreated(page, err.Error())
			return
		}
		h.config.Report(page.id, ReportCreated, targetID)
	})
	page.targetInfo = done
	err = view.callDevToolsProtocolMethod("Target.getTargetInfo", "{}", done)
	done.ref.release()
	if err != nil {
		h.discardCreated(page, "Target.getTargetInfo dispatch: "+err.Error())
	}
}

// pageRegistered reports whether page is still the registry's entry for
// its id. A page that lost a race with close or close-profile is already
// settled: its report went out and its teardown belongs to whoever
// removed it. Runs on the UI thread.
func (h *Host) pageRegistered(page *hostPage) bool {
	return h.pages[page.id] == page
}

// failUncreated settles a create that failed on the UI thread and tears down
// the clip container that may already have become its final parent. The exact
// pointer comparison keeps a late callback from deleting or reporting failure
// for a newer page that reused the same id after this one was closed.
func (h *Host) failUncreated(page *hostPage, detail string) {
	mine := h.pages[page.id] == page
	if mine {
		delete(h.pages, page.id)
	}
	destroyWindow(page.container)
	page.container = 0
	if page.popup != nil {
		// Never created, so it made no report: it goes unheard, its window
		// request answered with no window.
		h.departPopup(page, false)
		h.config.Logf("browser host: popup %s of page %s: %s", page.id, page.popup.openerID, detail)
		return
	}
	if mine {
		h.config.Report(page.id, ReportCreateFailed, TruncateDetail(detail))
	}
}

// discardCreated unwinds a create that failed AFTER its controller came
// up: a create-failed report with a live controller left behind would be
// a window no directive can ever address. Runs on the UI thread, like
// every caller.
func (h *Host) discardCreated(page *hostPage, detail string) {
	// Whichever of this and a close takes the page owns its teardown, so
	// the controller can never be closed twice. A popup takes its own
	// popups with it.
	taken := h.take(func(p *hostPage) bool { return p == page })
	if len(taken) == 0 {
		// Already settled by close or close-profile; their teardown owns
		// the controller and their closed report is the final word.
		return
	}
	for _, p := range taken {
		if p.popup != nil {
			h.config.Logf("browser host: popup %s of page %s: %s", p.id, p.popup.openerID, detail)
		}
	}
	if page.popup != nil {
		h.closeTaken(taken, nil)
		return
	}
	h.closeTaken(taken, page)
	h.config.Report(page.id, ReportCreateFailed, TruncateDetail(detail))
}
