//go:build windows

package webview2host

import (
	"errors"
	"fmt"
)

// The launcher's half of a page's popups (popup.go). Every function here
// runs on the UI thread: WebView2 raises NewWindowRequested and
// WindowCloseRequested there, and controller completions arrive there.
//
// What the real runtime does, measured by popup_smoke_windows_test.go:
// every way a page opens a window (window.open with or without a user
// gesture, a target=_blank link or form, a middle, Ctrl or Shift click)
// raises NewWindowRequested, IsUserInitiated true for each but a script's
// call with no gesture. While the deferral is open the opener's script
// waits in window.open. A view handed over with put_NewWindow gets a new
// CDP target when the deferral completes, which names the opener as its
// opener and starts the window's navigation at once; a click that opens a
// background tab keeps the view's target and has no opener. Handled with no
// new window opens nothing and window.open answers null. A download
// started in the view reaches its DownloadStarting, and window.close()
// raises WindowCloseRequested on the view, where the page may close itself
// (a popup, or a page with one history entry).

// popupRequest is a NewWindowRequested a popup answers: its args, held
// with a reference, and the deferral that keeps the opener's script
// waiting.
type popupRequest struct {
	args     *iNewWindowRequestedEventArgs
	deferral *iCoreWebView2Deferral
}

// popupState is a popup page's link to the window request it answers and,
// until the backend hears of it, the reports it made.
type popupState struct {
	opener  *hostPage
	request popupRequest
	uri     string
	// announced is set once the popup report is out. Until then the page's
	// reports wait in held, in order (pageReport).
	announced bool
	held      []heldReport
}

type heldReport struct {
	kind   ReportKind
	detail string
}

// maxHeldPopupReports bounds the reports a popup makes between its
// navigation's start and its popup report, one target-info round trip. A
// page cannot report more than a handful in that time; past the bound the
// newest are dropped, and a download's next snapshot heals its row.
const maxHeldPopupReports = 64

// watchWindows takes every window the page opens and its own close.
func (h *Host) watchWindows(page *hostPage, view *iCoreWebView2) {
	opened := newNewWindowRequestedHandler(func(args *iNewWindowRequestedEventArgs) { h.newWindowRequested(page, args) })
	page.newWindow = opened
	if _, err := view.addNewWindowRequested(opened); err != nil {
		h.config.Logf("browser host: page %s new-window subscription: %v; a window it opens is WebView2's own", page.id, err)
	}
	opened.ref.release()
	closing := newViewEventHandler(func() { h.windowCloseRequested(page) })
	page.closeRequested = closing
	if _, err := view.addWindowCloseRequested(closing); err != nil {
		h.config.Logf("browser host: page %s close-request subscription: %v", page.id, err)
	}
	closing.ref.release()
}

// newWindowRequested makes the window a page asks for a popup page of the
// launcher's, or refuses it: the opener is closing, the pane environment is
// gone, or a bound is reached. A refused window opens nothing.
func (h *Host) newWindowRequested(opener *hostPage, args *iNewWindowRequestedEventArgs) {
	refuse := func(why string) {
		h.config.Logf("browser host: page %s: refuse a new window: %s", opener.id, why)
		if err := args.putHandled(true); err != nil {
			h.config.Logf("browser host: page %s: refuse a new window: %v", opener.id, err)
		}
	}
	env, ok := h.env.current()
	if !ok {
		refuse("the pane environment is gone")
		return
	}
	if h.pendingPopups >= MaxPendingPopups {
		refuse(fmt.Sprintf("%d new windows are already being opened", h.pendingPopups))
		return
	}
	deferral, err := args.deferral()
	if err != nil {
		refuse(err.Error())
		return
	}
	args.addRef()
	h.popupSeq++
	page := &hostPage{
		id: popupPageID(h.downloadToken, h.popupSeq), profileID: opener.profileID, ephemeral: opener.ephemeral,
		popup: &popupState{opener: opener, request: popupRequest{args: args, deferral: deferral}, uri: args.uri()},
	}
	h.pendingPopups++
	h.mu.Lock()
	why := ""
	switch {
	case h.pages[opener.id] != opener:
		why = "its page is closing"
	case len(h.pages) >= MaxHostPages:
		why = fmt.Sprintf("the pane has %d pages", len(h.pages))
	default:
		h.pages[page.id] = page
	}
	h.mu.Unlock()
	if why != "" {
		h.config.Logf("browser host: page %s: refuse a new window: %s", opener.id, why)
		h.answerNoWindow(page)
		return
	}
	h.startController(page, env.env10, h.config.HostWindow())
}

// settlePopup answers page's window request once: with view as the new
// window, or, with view nil or one put_NewWindow refuses, with no window.
// It completes the deferral and lets the request go, and is a no-op for a
// page with no request open.
func (h *Host) settlePopup(page *hostPage, view *iCoreWebView2) error {
	if page.popup == nil || page.popup.request.args == nil {
		return nil
	}
	request := page.popup.request
	page.popup.request = popupRequest{}
	h.pendingPopups--
	var errs []error
	if view != nil {
		if err := request.args.putNewWindow(view); err != nil {
			errs = append(errs, fmt.Errorf("put_NewWindow: %w", err))
		}
	}
	if err := request.args.putHandled(true); err != nil {
		errs = append(errs, fmt.Errorf("put_Handled: %w", err))
	}
	if err := request.deferral.complete(); err != nil {
		errs = append(errs, fmt.Errorf("complete the deferral: %w", err))
	}
	request.deferral.release()
	request.args.release()
	return errors.Join(errs...)
}

// answerNoWindow settles page's window request with no window. The opener's
// window.open answers null whether or not WebView2 took each call.
func (h *Host) answerNoWindow(page *hostPage) {
	if err := h.settlePopup(page, nil); err != nil {
		h.config.Logf("browser host: page %s: answer its window request: %v", page.id, err)
	}
}

// popupReady hands the popup's new view to its request, once its controller
// is up and every handler is attached, and reports the page under the CDP
// target the view has then. The view starts at its opener's bounds, hidden,
// so the window lays out at its opener's size until the backend sets its
// viewport.
func (h *Host) popupReady(page *hostPage, view *iCoreWebView2) {
	opener := page.popup.opener
	if !h.pageRegistered(opener) {
		h.discardCreated(page, "its opener closed")
		return
	}
	if bounds, err := opener.controller.bounds(); err != nil {
		h.config.Logf("browser host: page %s: read its opener's bounds: %v", page.id, err)
	} else if err := page.controller.putBounds(bounds); err != nil {
		h.config.Logf("browser host: page %s: initial bounds: %v", page.id, err)
	}
	if err := h.settlePopup(page, view); err != nil {
		h.discardCreated(page, err.Error())
		return
	}
	// The window's own target exists once the deferral completed.
	done := newDevToolsCompletedHandler(func(hr uintptr, resultJSON string) {
		if !h.pageRegistered(page) {
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
		h.announcePopup(page, targetID)
	})
	page.targetInfo = done
	err := view.callDevToolsProtocolMethod("Target.getTargetInfo", "{}", done)
	done.ref.release()
	if err != nil {
		h.discardCreated(page, "Target.getTargetInfo dispatch: "+err.Error())
	}
}

// announcePopup reports the popup, then every report it made meanwhile.
func (h *Host) announcePopup(page *hostPage, targetID string) {
	detail, err := PopupReport{Opener: page.popup.opener.id, Target: targetID, URL: page.popup.uri}.Encode()
	if err != nil {
		h.discardCreated(page, err.Error())
		return
	}
	page.popup.announced = true
	h.config.Report(page.id, ReportPopup, detail)
	for _, held := range page.popup.held {
		h.config.Report(page.id, held.kind, held.detail)
	}
	page.popup.held = nil
}

// pageReport reports one of a page's own events, holding it while the page
// is a popup the backend has not heard of.
func (h *Host) pageReport(page *hostPage, kind ReportKind, detail string) {
	if page.popup == nil || page.popup.announced {
		h.config.Report(page.id, kind, detail)
		return
	}
	if len(page.popup.held) >= maxHeldPopupReports {
		h.config.Logf("browser host: page %s: drop a %s report made before the page was reported", page.id, kind)
		return
	}
	page.popup.held = append(page.popup.held, heldReport{kind: kind, detail: detail})
}

// windowCloseRequested closes a page whose document asked to close
// (window.close()), as every engine closes a view that asks. WebView2 also
// asks to close a popup whose navigation became a download; closing the
// page then would cancel the download, so a page with a download running
// closes once its last download ends (endDownload).
func (h *Host) windowCloseRequested(page *hostPage) {
	if len(page.downloads) > 0 {
		h.config.Logf("browser host: page %s asked to close while it downloads; it closes when its downloads end", page.id)
		page.closeAfterDownloads = true
		return
	}
	h.closeAsked(page)
}

// closeAsked closes a page that asked to close. Off the caller's callback:
// closePages runs on the UI thread through OnMain, and a controller must
// not be closed inside its own event.
func (h *Host) closeAsked(page *hostPage) {
	go h.closePages(func(p *hostPage) bool { return p == page })
}
