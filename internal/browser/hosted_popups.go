package browser

import (
	"context"
	"fmt"
	"slices"
	"time"

	"agent-overflow/internal/webview2host"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// Popups on the hosted engine. A window a pane page opens becomes a page of
// the launcher's, which reports it (ReportPopup) under a page id the
// launcher minted, with its opener's page id and its CDP target, after its
// opener and before any other report of the page
// (internal/webview2host/popup.go). The engine hands it to the Manager as
// PopupOpened, which adopts it through AttachPage or closes it through
// DiscardPage, as on every engine.
//
// From its report until the Manager lists it the popup is pending. Its
// address and title reach the Manager by handle, which holds them for the
// page that takes the handle; a target's info that arrives before the
// popup report is held by the connection's record of its targets
// (cdpTargets). Its downloads go to the Manager as they come, naming the
// popup and its openers, and the Manager lists them in the popup's tab or,
// if the popup is never listed, in its nearest listed opener's
// (popup_adoption.go). Its crash and its close are held here, because the
// Manager acts on those only for a listed page, and delivered in that
// order once it is listed (Adopted); a close also reaches the Manager at
// once, so what the popup downloaded is listed while its opener still is.
// A popup reported with no target (one that went before the launcher read
// it) is never attached: the Manager discards it, and its downloads go to
// its opener. A popup's target is on its opener's browser connection, so
// it is attached on that connection or not at all, and a popup whose
// opener has no live connection is refused at its report. Its attach is
// bounded by the engine's attach timeout and ends at its close, its
// discard or Stop; its downloads are settled by the close, not the attach.
//
// The popup navigates as soon as the launcher hands its view over, before
// the engine has heard of it. The engine answers the navigation policy
// browser-wide, as the headless engine does, and only there: a request has the
// authority of the page its frame belongs to, or for a popup not yet
// adopted, of the pages that opened it, nearest first, as the browser
// names them (jarAllows).

// Why the downloads of a popup the Manager never listed end, when the
// launcher cancels them with the popup's page and reports nothing.
const (
	popupClosedReason    = "the window it began in closed before it opened as a tab"
	popupDiscardedReason = "the window it began in was not opened as a tab"
)

// hostedPopup is a popup the launcher reported that the Manager has not
// listed yet.
type hostedPopup struct {
	target  string
	profile string
	// opener is the page id of the page that opened it.
	opener string
	// conn is its opener's browser connection, the one its target is on;
	// nil for a popup with no target.
	conn context.Context
	// crashed is the reason its page crashed, and closed says it closed.
	crashed string
	closed  bool
	// cancelAttach ends its attach while one is under way.
	cancelAttach context.CancelFunc
}

// cancelAttachLocked ends the attach of popup, if one is under way. The
// caller holds e.mu.
func (popup *hostedPopup) cancelAttachLocked() {
	if popup.cancelAttach != nil {
		popup.cancelAttach()
		popup.cancelAttach = nil
	}
}

// popupReported takes the launcher's report of a popup: the engine knows
// its opener, so it hands it to the Manager, and otherwise closes it. A
// report naming a page id the engine already has is ignored before it is
// read, so no report closes a known page; one naming a target a page or a
// pending popup already has is refused, and the target keeps its page.
func (e *hostedEngine) popupReported(pageID, detail string) {
	if err := webview2host.ValidatePageID(pageID); err != nil {
		e.logf("browser: pane host reported a popup with an unusable id: %v", err)
		return
	}
	e.mu.Lock()
	_, taken := e.pageProfile[pageID]
	e.mu.Unlock()
	if taken {
		e.logf("browser: pane host reported popup %s, a page it already has", pageID)
		return
	}
	report, err := webview2host.ParsePopupReport(detail)
	if err != nil {
		e.logf("browser: pane host popup %s: %v", pageID, err)
		e.closePage(pageID)
		return
	}
	e.mu.Lock()
	profile, known := e.pageProfile[report.Opener]
	_, taken = e.pageProfile[pageID]
	_, claimed := e.targetPageLocked(report.Target)
	conn := e.pageConn[report.Opener]
	if pending := e.popups[report.Opener]; pending != nil {
		conn = pending.conn
	}
	switch {
	case taken:
		e.mu.Unlock()
		e.logf("browser: pane host reported popup %s twice", pageID)
		return
	case claimed:
		e.mu.Unlock()
		e.logf("browser: pane host reported popup %s with target %s, which another page has", pageID, report.Target)
		e.closePage(pageID)
		return
	case !known || len(e.popups) >= webview2host.MaxHostPages:
		e.mu.Unlock()
		// No page of the Manager's opened it (it closed meanwhile, or the
		// engine stopped), or the launcher reports more popups than it
		// keeps pages.
		e.closePage(pageID)
		return
	case report.Target != "" && (conn == nil || conn.Err() != nil):
		e.mu.Unlock()
		// Its target is on its opener's connection, which has ended.
		e.logf("browser: pane host reported popup %s of %s, which has no connection", pageID, report.Opener)
		e.closePage(pageID)
		return
	}
	e.popups[pageID] = &hostedPopup{target: report.Target, profile: profile, opener: report.Opener, conn: conn}
	if report.Target != "" {
		e.popupByTarget[report.Target] = pageID
	}
	e.pageProfile[pageID] = profile
	e.mu.Unlock()
	if report.Target != "" {
		e.takeTargetInfo(pageID, report.Target)
	}
	// On its own goroutine, as every engine reports a popup: the Manager
	// adopts it through AttachPage, a CDP round trip, and the launcher's
	// reports must not wait behind it.
	go e.events.PopupOpened(enginePopup{Profile: profile, Opener: report.Opener, Handle: pageID, URL: report.URL})
}

// attachPopup attaches chromedp to a pending popup of profile, on its
// opener's connection; bind refuses it if that connection ended meanwhile.
func (e *hostedEngine) attachPopup(profile *hostedProfile, handle string, hooks pageHooks) (pageDriver, error) {
	e.mu.Lock()
	popup := e.popups[handle]
	var targetID string
	var closed bool
	var browserCtx context.Context
	if popup != nil && popup.profile == profile.id {
		targetID, closed, browserCtx = popup.target, popup.closed, popup.conn
	}
	e.mu.Unlock()
	switch {
	case targetID == "":
		return nil, fmt.Errorf("browser: the pane host reported no popup %s with a target in this profile", handle)
	case closed:
		return nil, fmt.Errorf("browser: popup %s closed before it was adopted", handle)
	case browserCtx.Err() != nil:
		return nil, fmt.Errorf("browser: popup %s: its opener's connection to the pane browser was lost", handle)
	}
	pageCtx, pageCancel := chromedp.NewContext(browserCtx, chromedp.WithTargetID(target.ID(targetID)))
	// The attach is bounded on its own clock, and the popup's close or
	// discard cancels it (cancelAttachLocked); its downloads are settled
	// by that close, not by the attach.
	e.mu.Lock()
	if e.popups[handle] != popup || popup.closed {
		e.mu.Unlock()
		pageCancel()
		return nil, fmt.Errorf("browser: popup %s closed before it was adopted", handle)
	}
	popup.cancelAttach = pageCancel
	e.mu.Unlock()
	expire := time.AfterFunc(e.attachTimeout, pageCancel)
	driver, err := startCDPPage(browserCtx, pageCtx, pageCancel, hooks)
	expire.Stop()
	e.mu.Lock()
	popup.cancelAttach = nil
	canceled := pageCtx.Err()
	e.mu.Unlock()
	if err == nil && canceled != nil {
		driver.Close()
		err = fmt.Errorf("browser: attach popup %s: %w", handle, canceled)
	}
	if err != nil {
		return nil, err
	}
	if err := e.bind(handle, targetID, browserCtx); err != nil {
		driver.Close()
		return nil, err
	}
	e.mu.Lock()
	if cdp, ok := driver.(*cdpPage); ok {
		e.drivers[handle] = cdp
	}
	e.mu.Unlock()
	return &hostedPage{pageDriver: driver, engine: e, id: handle}, nil
}

// popupListed delivers what a popup reported while it was pending, once the
// Manager lists it: its crash, then its close.
func (e *hostedEngine) popupListed(pageID string) {
	e.mu.Lock()
	popup := e.popups[pageID]
	if popup != nil {
		delete(e.popups, pageID)
		delete(e.popupByTarget, popup.target)
	}
	e.mu.Unlock()
	if popup == nil {
		return
	}
	if popup.crashed != "" {
		e.events.PageCrashed(pageID, popup.crashed)
	}
	if popup.closed {
		e.retirePage(pageID)
	}
}

// popupOwnersLocked answers, for a download of pageID, the download's
// start naming the popup it began in, that popup's openers, nearest first,
// and its profile, when pageID is a pending popup; empty otherwise. The
// caller holds e.mu.
func (e *hostedEngine) popupOwnersLocked(pageID string) downloadStart {
	popup := e.popups[pageID]
	if popup == nil {
		return downloadStart{}
	}
	start := downloadStart{Popup: pageID, Profile: popup.profile}
	// Each opener is reported before its popups, so the chain ends; the
	// bound stops a cycle all the same.
	for opener := popup.opener; opener != "" && len(start.Openers) <= len(e.popups); {
		start.Openers = append(start.Openers, opener)
		next := e.popups[opener]
		if next == nil {
			break
		}
		opener = next.opener
	}
	return start
}

// pageCrashed reports a page crashed, or keeps the crash of a pending
// popup for its listing.
func (e *hostedEngine) pageCrashed(pageID, reason string) {
	e.mu.Lock()
	if popup := e.popups[pageID]; popup != nil {
		popup.crashed = reason
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()
	e.events.PageCrashed(pageID, reason)
}

// retirePendingLocked keeps the close of a pending popup for its listing
// and reports whether pageID is one. The caller holds e.mu.
func (e *hostedEngine) retirePendingLocked(pageID string) bool {
	popup := e.popups[pageID]
	if popup == nil {
		return false
	}
	popup.closed = true
	popup.cancelAttachLocked()
	return true
}

// pendingClosed settles a pending popup whose page closed: the launcher
// canceled its running downloads with the page, so they end here, and the
// Manager hears of the close now, which lists what the popup downloaded
// while its opener is still listed. The popup's listing, if its adoption
// is still under way, delivers the close again (popupListed).
func (e *hostedEngine) pendingClosed(pageID string) {
	e.endUnlistedDownloads(pageID, popupClosedReason)
	e.events.PageClosed(pageID)
}

// forgetPopupLocked drops a popup the Manager discarded. The caller holds
// e.mu.
func (e *hostedEngine) forgetPopupLocked(pageID string) {
	if popup := e.popups[pageID]; popup != nil {
		popup.cancelAttachLocked()
		delete(e.popupByTarget, popup.target)
		delete(e.popups, pageID)
	}
}

// allowRequest is the browser-wide navigation policy for a request paused
// in frame: the policy of the profile of the first page the request's
// owners resolve to, asked with those owners, which are the frame, then
// the pages that opened its page, nearest first, as the connection's
// targets name them and, past a popup the launcher reported, as its report
// names its opener (reportedOpenersLocked). It answers unresolved when no
// owner resolves to a page of the engine's.
func (e *hostedEngine) allowRequest(targets *cdpTargets, frame, rawURL string) (allowed, resolved bool) {
	owners := targets.owners(frame)
	e.mu.Lock()
	owners = e.reportedOpenersLocked(owners)
	var allow func([]string, string) (bool, bool)
	for _, owner := range owners {
		if pageID, ok := e.ownerPageLocked(owner); ok {
			allow = e.policies[e.pageProfile[pageID]]
			break
		}
	}
	e.mu.Unlock()
	if allow == nil {
		return false, false
	}
	return allow(owners, rawURL)
}

// reportedOpenersLocked extends owners, a frame and the pages that opened
// it as the connection's targets name them, past each popup the launcher
// reported with the target of the opener its report names: a window a
// click opened in a new tab has a target that names no opener. The caller
// holds e.mu.
func (e *hostedEngine) reportedOpenersLocked(owners []string) []string {
	// Each step adds a page not in owners yet, so the chain ends.
	for {
		popupID, ok := e.popupByTarget[owners[len(owners)-1]]
		if !ok {
			return owners
		}
		opener := e.popups[popupID].opener
		next := e.targetByPage[opener]
		if pending := e.popups[opener]; pending != nil {
			next = pending.target
		}
		if next == "" || slices.Contains(owners, next) {
			return owners
		}
		owners = append(owners, next)
	}
}

// ownerPageLocked answers the page a frame belongs to: the page whose
// target it is, a pending popup's, or the page that reported it as one of
// its frames. The caller holds e.mu.
func (e *hostedEngine) ownerPageLocked(frame string) (string, bool) {
	if pageID, ok := e.targetPageLocked(frame); ok {
		return pageID, true
	}
	for pageID, driver := range e.drivers {
		if driver.OwnsFrame(frame) {
			return pageID, true
		}
	}
	return "", false
}

// enableBrowserPolicy pauses every document request and file read of the
// connection on browserCtx for allowRequest. The handler is subscribed
// first (subscribe), so no request waits for an answer nobody gives.
func enableBrowserPolicy(browserCtx context.Context) error {
	if _, err := chromedp.CallBrowser(browserCtx, fetch.Enable, fetch.EnableParams{Patterns: navigationPolicyPatterns}); err != nil {
		return fmt.Errorf("browser: install the pane's browser-wide navigation policy: %w", err)
	}
	return nil
}
