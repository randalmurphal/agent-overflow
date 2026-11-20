//go:build windows

package webview2host

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The launcher's half of a page's popups (popup.go). Every function here
// runs on the UI thread: WebView2 raises NewWindowRequested and
// WindowCloseRequested there, and controller completions arrive there.
//
// What the real runtime does, measured by popup_smoke_windows_test.go:
// every way a page opens a window (window.open with or without a user
// gesture, a target=_blank link or form, a middle, Ctrl or Shift click on
// a link) raises NewWindowRequested. While the deferral is open the
// opener's script waits in window.open. A view handed over with
// put_NewWindow gets a new CDP target when the deferral completes, which
// names the opener as its opener and starts the window's navigation at
// once; the target of a middle, Ctrl or Shift click's window names no
// opener, and the opener stays where it is. Handled with no
// new window opens nothing and window.open answers null. A download
// started in the view reaches its DownloadStarting, and window.close()
// raises WindowCloseRequested on the view, where the page may close itself
// (a popup, or a page with one history entry).
//
// A popup's life from its request to its announcement is one state
// (popupState.stage), changed only here:
//
//   - requested: its controller is being created and its window request
//     is open (newWindowRequested);
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
// announced yet with it (take). A departing popup whose opener still
// awaits announcement leaves its downloads with that opener (departTaken).

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
// until the backend hears of it, its address, bounded as it is taken, and
// the reports it made. The opener is kept by id, so a popup keeps no page
// that went alive.
type popupState struct {
	openerID string
	stage    popupStage
	request  popupRequest
	uri      string
	target   string
	held     heldReports
}

// heldReports are a popup's reports before its announcement: whether its
// runtime has no find API, each kind of process failure it had, in the
// order each first came, and the newest snapshot of each download, in the
// order the downloads started. A download snapshot is whole in its newest;
// a process failure is an event, and the backend treats its kinds apart (a
// crash ends the tab, an unresponsive renderer does not), so no kind
// replaces another.
type heldReports struct {
	findUnavailable bool
	processFailed   []string
	downloads       []DownloadReport
}

// maxHeldProcessFailures bounds the kinds of process failure a popup holds.
// WebView2 defines seven (COREWEBVIEW2_PROCESS_FAILED_KIND), so every kind
// a runtime reports is held.
const maxHeldProcessFailures = 16

// maxHeldPopupDownloads bounds the snapshots a popup keeps before its
// announcement. The host cancels new downloads past the bound and logs
// snapshots it cannot keep from departing descendants.
const maxHeldPopupDownloads = 16

// popupCreateBound bounds controller creation and target discovery together.
const popupCreateBound = 10 * time.Second

// unannounced reports whether page is a popup the backend has not heard of.
func (p *hostPage) unannounced() bool {
	return p.popup != nil && p.popup.stage != popupAnnounced
}

// watchWindows takes every window the page opens and its own close. A page
// it fails for is not created (controllerCompleted): a window it opened
// would be WebView2's own.
func (h *Host) watchWindows(page *hostPage, view *iCoreWebView2) error {
	// The refusal outlives the page's detach, so it keeps the page's id,
	// never the page.
	id := page.id
	opened := newNewWindowRequestedHandler(
		func(args *iNewWindowRequestedEventArgs) { h.newWindowRequested(page, args) },
		func(err error) {
			h.config.Logf("browser host: page %s: refuse a new window after the page went: %v", id, err)
		},
	)
	page.newWindow = opened
	err := view.addNewWindowRequested(opened)
	opened.ref.release()
	if err != nil {
		return fmt.Errorf("new-window subscription: %w", err)
	}
	closing := newViewEventHandler(func() { h.closeAsked(page) })
	page.closeRequested = closing
	err = view.addWindowCloseRequested(closing)
	closing.ref.release()
	if err != nil {
		return fmt.Errorf("close-request subscription: %w", err)
	}
	return nil
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
	rawURI, err := args.uri()
	if err != nil {
		// The popup still opens; it is reported with no address.
		h.config.Logf("browser host: page %s: %v", opener.id, err)
	}
	uri := strings.ToValidUTF8(rawURI, "\uFFFD")
	prefix := truncateUTF8(uri, MaxPopupURLBytes)
	if len(prefix) < len(uri) {
		prefix = strings.Clone(prefix)
	}
	page := &hostPage{
		id: popupPageID(h.downloadToken, h.popupSeq), profileID: opener.profileID, ephemeral: opener.ephemeral,
		popup: &popupState{
			openerID: opener.id, request: popupRequest{args: args, deferral: deferral},
			uri: prefix,
		},
	}
	why := "its page is closing"
	if h.pages[opener.id] == opener {
		why = h.admit(page.id)
	}
	if why == "" {
		h.pages[page.id] = page
	} else {
		h.config.Logf("browser host: page %s: refuse a new window: %s", opener.id, why)
		h.answerNoWindow(page)
		return
	}
	h.startController(page, env.env10, h.config.HostWindow())
	id := page.id
	h.after(popupCreateBound, func() { h.config.Post(func() { h.popupExpired(id) }) })
}

// popupExpired discards a popup that still has no target, answering any
// open window request with no window. The timer retains only the popup id.
func (h *Host) popupExpired(id string) {
	page := h.pages[id]
	if page == nil || page.popup == nil || page.popup.stage >= popupTargeted {
		return
	}
	h.discardCreated(page, fmt.Sprintf("its target was not available within %v", popupCreateBound))
}

// settlePopup answers an open window request exactly once and releases it.
func (h *Host) settlePopup(page *hostPage, view *iCoreWebView2) error {
	if page.popup == nil || page.popup.request.args == nil {
		return nil
	}
	request := page.popup.request
	page.popup.request = popupRequest{}
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
	opener := h.pages[page.popup.openerID]
	if opener == nil || opener.controller == nil {
		// A page that goes takes its popups (take), so this one is
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
	opener := h.pages[page.popup.openerID]
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
	var waiting []*hostPage
	for _, child := range h.pages {
		if child.popup != nil && child.popup.openerID == page.id && child.popup.stage == popupTargeted {
			waiting = append(waiting, child)
		}
	}
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
	page.popup.held, page.popup.uri = heldReports{}, ""
	h.config.Report(page.id, ReportPopup, detail)
	if held.findUnavailable {
		h.config.Report(page.id, ReportFindUnavailable, "")
	}
	for _, download := range held.downloads {
		h.report(page, download)
	}
	for _, kind := range held.processFailed {
		h.config.Report(page.id, ReportProcessFailed, kind)
	}
	return nil
}

// ReportUndelivered tells the host that a report it made did not reach the
// backend: the bridge was down, or the call failed, timed out or was
// refused. A popup whose popup report went so is closed with its popups
// not announced yet, since the backend may never adopt it and it would
// stay a hidden controller; a call that timed out may still have landed,
// and the closed report that follows settles the backend's adoption, which
// ignores closed for a page it never heard of. Any other report is the
// backend's to re-derive. Safe from any goroutine, Config.Report included.
func (h *Host) ReportUndelivered(pageID string, kind ReportKind) {
	if kind != ReportPopup {
		return
	}
	h.config.Post(func() {
		page := h.pages[pageID]
		if page == nil || page.popup == nil {
			return
		}
		h.config.Logf("browser host: popup %s: its report did not reach the backend; it closes", page.id)
		h.closeTaken(h.take(func(p *hostPage) bool { return p == page }), nil)
	})
}

// departPopup answers any open request. An announced popup needs a closed
// report. An unannounced popup with downloads, or needed to announce a
// descendant (departTaken), first reports its popup and held snapshots,
// under the target it has or none, which the backend never attaches; any
// other goes unheard. Runs on the UI thread, once the page is out of the
// registry and before its handlers retire, and answers whether to report
// the page closed.
func (h *Host) departPopup(page *hostPage, announce bool) bool {
	h.answerNoWindow(page)
	if !page.unannounced() {
		return true
	}
	if !announce && len(page.popup.held.downloads) == 0 {
		return false
	}
	h.finishHeldDownloads(page)
	if err := h.reportPopup(page); err != nil {
		h.config.Logf("browser host: popup %s: announce it as it goes: %v; its downloads are not listed", page.id, err)
		return false
	}
	return true
}

// finishHeldDownloads ends the downloads of a popup that goes before its
// announcement, each read first (reconcileDownload): one WebView2 ended,
// with or without its state change, is held as it ended, and one still
// running, or unread, is canceled and held with PopupGoneReason.
func (h *Host) finishHeldDownloads(page *hostPage) {
	for _, d := range page.downloads {
		h.reconcileDownload(d, sampleRetire)
	}
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
		held := &page.popup.held
		switch {
		case slices.Contains(held.processFailed, detail):
		case len(held.processFailed) < maxHeldProcessFailures:
			held.processFailed = append(held.processFailed, detail)
		default:
			h.config.Logf("browser host: page %s: drop a %s failure past %d kinds held", page.id, detail, maxHeldProcessFailures)
		}
	case ReportAccelerator:
		// A popup is hidden until the backend shows it, and a chord is not
		// worth keeping.
		h.config.Logf("browser host: page %s: drop a %s report made before the page was reported", page.id, kind)
	case ReportCreated, ReportCreateFailed, ReportClosed, ReportCleared, ReportClearFailed,
		ReportFocused, ReportFocusFailed, ReportDownload, ReportPopup:
		// Reported by their own paths, never through pageReport.
		h.config.Logf("browser host: page %s: a %s report reached the popup hold, which keeps none; dropped", page.id, kind)
	}
	return true
}

// holdDownload keeps a download snapshot of a popup the backend has not
// heard of, replacing its older snapshot. True means the report must
// stay local, even if the held set is full.
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
	if page.popupDownloadRefused() {
		h.config.Logf("browser host: page %s: drop download %s past %d held snapshots", page.id, r.ID, maxHeldPopupDownloads)
		return true
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

// closeAsked closes a page whose document asked to close (window.close()),
// as every engine closes a view that asks, in a UI-thread turn posted
// after the caller's event, since a controller must not be closed inside
// its own event. The turn decides on the page as it is then: one already
// gone, its downloads retired, is not in the registry to take, and one
// with a download running, its state read again, stays open until its
// last download ends (endDownload), as WebView2 also asks to close a popup
// whose navigation
// became a download and closing the page would cancel it.
func (h *Host) closeAsked(page *hostPage) {
	h.config.Post(func() {
		for _, d := range page.downloads {
			h.reconcileDownload(d, sampleStatus)
		}
		if len(page.downloads) > 0 {
			h.config.Logf("browser host: page %s asked to close while it downloads; it closes when its downloads end", page.id)
			page.closeAfterDownloads = true
			return
		}
		h.closeTaken(h.take(func(p *hostPage) bool { return p == page }), nil)
	})
}
