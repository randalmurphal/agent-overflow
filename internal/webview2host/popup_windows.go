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
//
// A popup's life from its request to its announcement is one state
// (popupState.stage), changed only here:
//
//   - requested: its controller is being created, its window request open
//     and counted in pendingPopups (newWindowRequested);
//   - created: its view answered the request, its target is being read
//     (popupReady);
//   - targeted: its target is known, and it waits for its opener to be
//     announced (popupTargeted);
//   - announced: the backend has its report and every report since
//     (announcePopup).
//
// The backend knows a popup's opener only once the opener is announced, so
// a popup is announced after its opener, by its opener's announcement if
// its own target came first. A page that goes takes its popups that are not
// announced yet with it (takeLocked), and every popup that goes leaves the
// same way (departPopup).

// popupStage is where a popup is between its request and its announcement.
type popupStage int

const (
	popupRequested popupStage = iota
	popupCreated
	popupTargeted
	popupAnnounced
)

// popupRequest is a NewWindowRequested a popup answers: its args, held
// with a reference, and the deferral that keeps the opener's script
// waiting.
type popupRequest struct {
	args     *iNewWindowRequestedEventArgs
	deferral *iCoreWebView2Deferral
}

// popupState is a popup page's link to the window request it answers and,
// until the backend hears of it, the reports it made. The opener is kept
// by id, so a popup keeps no page that went alive.
type popupState struct {
	openerID string
	stage    popupStage
	request  popupRequest
	uri      string
	target   string
	held     heldReports
}

// heldReports are a popup's reports before its announcement: whether its
// runtime has no find API, its newest process failure, and the newest
// snapshot of each download, in the order the downloads started. Each kind
// is whole in its newest report, so nothing older is kept.
type heldReports struct {
	findUnavailable bool
	processFailed   string
	downloads       []DownloadReport
}

// maxHeldPopupDownloads bounds the downloads a popup starts before its
// announcement, one target-info round trip after its view is up. A download
// past it is canceled as it starts, so nothing is saved that the backend
// would not hear of.
const maxHeldPopupDownloads = 16

// unannounced reports whether page is a popup the backend has not heard of.
func (p *hostPage) unannounced() bool {
	return p.popup != nil && p.popup.stage != popupAnnounced
}

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
		popup: &popupState{openerID: opener.id, request: popupRequest{args: args, deferral: deferral}, uri: args.uri()},
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
// It completes the deferral, lets the request go and frees its place under
// MaxPendingPopups, and is a no-op for a page with no request open.
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
// is up and every handler is attached, and reads the CDP target the view
// has then. The view starts at its opener's bounds, hidden, so the window
// lays out at its opener's size until the backend sets its viewport.
func (h *Host) popupReady(page *hostPage, view *iCoreWebView2) {
	h.mu.Lock()
	opener := h.pages[page.popup.openerID]
	h.mu.Unlock()
	if opener == nil || opener.controller == nil {
		// A page that goes takes its popups (takeLocked), so this one is
		// already going; its teardown answers the request.
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
	page.popup.stage = popupCreated
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
		h.popupTargeted(page, targetID)
	})
	page.targetInfo = done
	err := view.callDevToolsProtocolMethod("Target.getTargetInfo", "{}", done)
	done.ref.release()
	if err != nil {
		h.discardCreated(page, "Target.getTargetInfo dispatch: "+err.Error())
	}
}

// popupTargeted records the popup's target and announces it, unless its
// opener is a popup not announced yet, whose announcement then announces
// this one.
func (h *Host) popupTargeted(page *hostPage, targetID string) {
	page.popup.target, page.popup.stage = targetID, popupTargeted
	h.mu.Lock()
	opener := h.pages[page.popup.openerID]
	h.mu.Unlock()
	if opener != nil && opener.unannounced() {
		return
	}
	h.announcePopup(page)
}

// announcePopup reports the popup, then what it held, then announces each
// of its own popups that waited for it.
func (h *Host) announcePopup(page *hostPage) {
	if err := h.reportPopup(page); err != nil {
		h.discardCreated(page, err.Error())
		return
	}
	h.mu.Lock()
	var waiting []*hostPage
	for _, child := range h.pages {
		if child.popup != nil && child.popup.openerID == page.id && child.popup.stage == popupTargeted {
			waiting = append(waiting, child)
		}
	}
	h.mu.Unlock()
	for _, child := range waiting {
		h.announcePopup(child)
	}
}

// reportPopup sends the popup report and the reports it held, in that
// order, and marks the popup announced.
func (h *Host) reportPopup(page *hostPage) error {
	detail, err := PopupReport{Opener: page.popup.openerID, Target: page.popup.target, URL: page.popup.uri}.Encode()
	if err != nil {
		return err
	}
	page.popup.stage = popupAnnounced
	held := page.popup.held
	page.popup.held = heldReports{}
	h.config.Report(page.id, ReportPopup, detail)
	if held.findUnavailable {
		h.config.Report(page.id, ReportFindUnavailable, "")
	}
	for _, download := range held.downloads {
		h.report(page, download)
	}
	if held.processFailed != "" {
		h.config.Report(page.id, ReportProcessFailed, held.processFailed)
	}
	return nil
}

// departPopup is how every popup goes, by its page's close, its opener's,
// the host's, a failed create or the backend's close, once it is out of the
// registry. Its window request is answered if still open, which frees its
// place at once (a late controller completion then only closes what it
// created). The backend hears closed of an announced popup. One it has not
// heard of that made a download is announced, with its held reports, then
// closed, so a download is never saved unlisted, under the target it has,
// or none when it went before its target was read, which the backend never
// attaches; one that made none goes unheard. Runs on the UI thread before
// the page's handlers retire, and answers whether to report it closed.
func (h *Host) departPopup(page *hostPage) bool {
	h.answerNoWindow(page)
	if !page.unannounced() {
		return true
	}
	if len(page.popup.held.downloads) == 0 {
		return false
	}
	// The page's teardown cancels what still runs and reports nothing; the
	// backend, which never listed the page, would settle none of it.
	for _, d := range page.downloads {
		h.holdDownload(page, d.snapshot(DownloadInterrupted, PopupGoneReason))
	}
	if err := h.reportPopup(page); err != nil {
		h.config.Logf("browser host: popup %s: announce it as it goes: %v; its downloads are not listed", page.id, err)
		return false
	}
	return true
}

// holdReport keeps one report of a popup the backend has not heard of,
// newest of each kind, and reports whether it held it.
func (h *Host) holdReport(page *hostPage, kind ReportKind, detail string) bool {
	if !page.unannounced() {
		return false
	}
	switch kind {
	case ReportFindUnavailable:
		page.popup.held.findUnavailable = true
	case ReportProcessFailed:
		page.popup.held.processFailed = detail
	default:
		// A key chord is the only other: a popup is hidden until the
		// backend shows it, and a chord is not worth keeping.
		h.config.Logf("browser host: page %s: drop a %s report made before the page was reported", page.id, kind)
	}
	return true
}

// holdDownload keeps a download snapshot of a popup the backend has not
// heard of, in place of the download's older one, and reports whether it
// held it.
func (h *Host) holdDownload(page *hostPage, r DownloadReport) bool {
	if !page.unannounced() {
		return false
	}
	for i := range page.popup.held.downloads {
		if page.popup.held.downloads[i].ID == r.ID {
			page.popup.held.downloads[i] = r
			return true
		}
	}
	page.popup.held.downloads = append(page.popup.held.downloads, r)
	return true
}

// popupDownloadRefused reports whether a download page starts now would be
// past maxHeldPopupDownloads.
func (p *hostPage) popupDownloadRefused() bool {
	return p.unannounced() && len(p.popup.held.downloads) >= maxHeldPopupDownloads
}

// pageReport reports one of a page's own events, holding it while the page
// is a popup the backend has not heard of.
func (h *Host) pageReport(page *hostPage, kind ReportKind, detail string) {
	if !h.holdReport(page, kind, detail) {
		h.config.Report(page.id, kind, detail)
	}
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
