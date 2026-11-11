//go:build windows

package webview2host

import "errors"

// Find in a page: the engine's own find bar (ICoreWebView2Find started with
// the default dialog left on), which counts, steps and closes itself. The
// backend opens it (OpFind) and closes it (OpFindStop); everything in
// between is the engine's. ICoreWebView2_28 and ICoreWebView2Environment15
// first shipped in WebView2 SDK 1.0.3405.78; a runtime without them is
// reported by findSupported at create.

var (
	errFindUnsupported   = errors.New("the installed WebView2 runtime has no find API")
	errFindNoEnvironment = errors.New("the pane environment is not available")
)

// findSupported reports whether the page's runtime has the find API. Runs on
// the UI thread.
func (h *Host) findSupported(view *iCoreWebView2) bool {
	view28 := view.queryWebView28()
	if view28 == nil {
		return false
	}
	view28.release()
	h.mu.Lock()
	env10 := h.env10
	h.mu.Unlock()
	if env10 == nil {
		return false
	}
	env15 := env10.queryEnvironment15()
	if env15 == nil {
		return false
	}
	env15.release()
	return true
}

// startFind opens the page's find bar searching for query. Keyboard focus
// moves into the page first, so the bar that opens takes typing whether the
// chord was pressed in the page or in the app. Runs on the UI thread.
func (h *Host) startFind(page *hostPage, query string) {
	if err := h.openFind(page, query); err != nil {
		h.config.Logf("browser host: page %s find: %v", page.id, err)
	}
}

func (h *Host) openFind(page *hostPage, query string) error {
	find, err := page.view.pageFind()
	if err != nil {
		return err
	}
	if find == nil {
		return errFindUnsupported
	}
	defer find.release()
	h.mu.Lock()
	env10 := h.env10
	h.mu.Unlock()
	if env10 == nil {
		return errFindNoEnvironment
	}
	env15 := env10.queryEnvironment15()
	if env15 == nil {
		return errFindUnsupported
	}
	defer env15.release()
	options, err := env15.createFindOptions()
	if err != nil {
		return err
	}
	defer options.release()
	if err := options.putFindTerm(query); err != nil {
		return err
	}
	// Explicit, though both are the documented defaults: the find bar is
	// the engine's, and it highlights every match.
	if err := options.putSuppressDefaultFindDialog(false); err != nil {
		return err
	}
	if err := options.putShouldHighlightAllMatches(true); err != nil {
		return err
	}
	if err := page.controller.moveFocus(moveFocusProgrammatic); err != nil {
		h.config.Logf("browser host: page %s find focus: %v", page.id, err)
	}
	if page.findStarted == nil {
		id := page.id
		page.findStarted = newFindStartCompletedHandler(func(hr uintptr) {
			if err := hresult(hr); err != nil {
				h.config.Logf("browser host: page %s find start: %v", id, err)
			}
		})
		page.pendingHandlers = append(page.pendingHandlers, page.findStarted)
	}
	return find.start(options, page.findStarted)
}

// stopFind ends the page's find and hides its bar. Runs on the UI thread.
func (h *Host) stopFind(page *hostPage) {
	find, err := page.view.pageFind()
	if err == nil && find == nil {
		return
	}
	if err == nil {
		err = find.stop()
		find.release()
	}
	if err != nil {
		h.config.Logf("browser host: page %s stop find: %v", page.id, err)
	}
}
