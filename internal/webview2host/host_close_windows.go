//go:build windows

package webview2host

import (
	"slices"
)

// Page close and teardown, every page's on the UI thread.

// Close tears every controller down, a page's whose own close request is
// still queued among them, and admits no page after. The environment
// itself is released with the process. A controller whose create is still
// in flight is closed by its completion.
func (h *Host) Close() {
	h.config.OnMain(func() {
		h.closed = true
		h.closeTaken(h.take(func(*hostPage) bool { return true }), nil)
	})
}

// closePages closes the pages match selects, with every popup of theirs
// not announced yet, in one UI-thread turn.
func (h *Host) closePages(match func(*hostPage) bool) {
	h.config.OnMain(func() { h.closeTaken(h.take(match), nil) })
}

// closeTaken settles the pages taken from the registry together, tears
// them down and reports closed for each the backend knows but uncreated,
// a backend page whose create failed: a popup before its opener, so the
// backend still knows the opener when it hears of the popup. Runs on the
// UI thread, in the turn that took them.
func (h *Host) closeTaken(taken []*hostPage, uncreated *hostPage) {
	reported := h.departTaken(taken)
	for _, page := range taken {
		h.teardown(page)
	}
	for i := len(reported) - 1; i >= 0; i-- {
		if reported[i] != uncreated {
			h.config.Report(reported[i].id, ReportClosed, "")
		}
	}
}

// take removes from the registry the pages match selects and, with each,
// every popup of its not announced yet, transitively: the backend cannot
// hear of a popup whose opener it has stopped knowing. It answers them
// each after the openers taken with it. Runs on the UI thread.
func (h *Host) take(match func(*hostPage) bool) []*hostPage {
	var taken []*hostPage
	for id, page := range h.pages {
		if match(page) {
			taken = append(taken, page)
			delete(h.pages, id)
		}
	}
	for i := 0; i < len(taken); i++ {
		for id, page := range h.pages {
			if page.unannounced() && page.popup.openerID == taken[i].id {
				taken = append(taken, page)
				delete(h.pages, id)
			}
		}
	}
	byID := make(map[string]*hostPage, len(taken))
	for _, page := range taken {
		byID[page.id] = page
	}
	depth := make(map[*hostPage]int, len(taken))
	for _, page := range taken {
		// A popup has one opener, so the chain is at most every page taken.
		for p := page; p.popup != nil && depth[page] < len(taken); depth[page]++ {
			if p = byID[p.popup.openerID]; p == nil {
				break
			}
		}
	}
	slices.SortStableFunc(taken, func(a, b *hostPage) int { return depth[a] - depth[b] })
	return taken
}

// departTaken settles pages taken from the registry together, openers
// first, with what the backend hears of them, before their teardown, and
// answers those to report closed: a page the backend created, and a popup
// as departPopup says. A popup not announced is announced as it goes when
// it or a popup of it going with it has a download to report, so the
// backend hears of its opener first. If that opener still lives and is
// unannounced, its departing subtree leaves its download snapshots with it.
// Runs on the UI thread.
func (h *Host) departTaken(taken []*hostPage) []*hostPage {
	byID := make(map[string]*hostPage, len(taken))
	for _, page := range taken {
		byID[page.id] = page
	}
	announce := make(map[*hostPage]bool)
	for i := len(taken) - 1; i >= 0; i-- {
		page := taken[i]
		if page.popup == nil || !announce[page] && len(page.popup.held.downloads) == 0 {
			continue
		}
		announce[page] = true
		if opener := byID[page.popup.openerID]; opener != nil {
			announce[opener] = true
		}
	}
	recipients := make(map[string]*hostPage)
	var reported []*hostPage
	for _, page := range taken {
		if page.unannounced() {
			openerID := page.popup.openerID
			recipient := recipients[openerID]
			if opener := h.pages[openerID]; opener != nil && opener.unannounced() {
				recipient = opener
			}
			if recipient != nil {
				recipients[page.id] = recipient
				h.answerNoWindow(page)
				h.finishHeldDownloads(page)
				for _, download := range page.popup.held.downloads {
					h.holdDownload(recipient, download)
				}
				continue
			}
		}
		if page.popup == nil || h.departPopup(page, announce[page]) {
			reported = append(reported, page)
		}
	}
	return reported
}

// teardown lets go of a page taken from the registry: its handlers, view
// and controller, and the clip container once the controller is closed,
// each cleared as it goes, so a late use is a nil check. Runs on the UI
// thread, in the turn that took the page.
func (h *Host) teardown(page *hostPage) {
	h.retireHandlers(page)
	if page.view != nil {
		page.view.release()
		page.view = nil
	}
	if page.controller2 != nil {
		page.controller2.release()
		page.controller2 = nil
	}
	if page.controller != nil {
		held := focusWithin(page.raiseTarget())
		if err := page.controller.close(); err != nil {
			h.config.Logf("browser host: close page %s: %v", page.id, err)
		}
		page.controller.release()
		page.controller = nil
		// After the controller, always: closing it destroys the window
		// inside the container, and a container that outlives its page
		// is an invisible window eating every click over its rectangle.
		destroyWindow(page.container)
		page.container = 0
		h.releaseFocus(page, held)
	}
	// A container with no controller is the parent of an asynchronous
	// create still in flight. Its completion owns both the controller and
	// container after observing that this page was unregistered;
	// destroying the parent now destabilizes the WebView2 create itself.
}

// retireHandlers detaches the callbacks of a page that is going, so a late
// call from WebView2 does nothing and a reference it never releases keeps
// only the small handler, not the page. The controller-completed handler is
// not among them: its completion must still close a controller that
// outlived the page. Runs on the UI thread.
func (h *Host) retireHandlers(page *hostPage) {
	if page.failHandler != nil {
		page.failHandler.done.detach()
	}
	if page.keyHandler != nil {
		page.keyHandler.pressed.detach()
	}
	if page.targetInfo != nil {
		page.targetInfo.done.detach()
	}
	if page.newWindow != nil {
		page.newWindow.requested.detach()
	}
	if page.closeRequested != nil {
		page.closeRequested.event.detach()
	}
	if page.dialogOpened != nil {
		page.dialogOpened.event.detach()
	}
	if page.findStarted != nil {
		page.findStarted.done.detach()
		page.findStarted.ref.release()
		page.findStarted = nil
	}
	h.retireDownloads(page)
}
