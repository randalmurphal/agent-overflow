package browser

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"agent-overflow/internal/browser/ids"
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
// The engine keeps a record of each popup (hostedPopup) from its report
// until its page goes. Its address and title reach the Manager by handle,
// which holds them for the page that takes the handle; a target's info
// that arrives before the popup report is held by the connection's record
// of its targets (cdpTargets). Its downloads go to the Manager as they
// come, naming the popup and its openers, and the Manager lists them in
// the popup's tab or, if the popup is never listed, in its nearest listed
// opener's (popup_adoption.go). Its crash and its close go to the Manager
// as they come too, whose admission of the popup decides whether it
// becomes a tab (engineEvents). A popup reported with no target (one that
// went before the launcher read it) is never offered for adoption: the
// engine keeps it, as the owner of the downloads the launcher reports for
// it, until the launcher's close, which lists them in its nearest listed
// opener's tab. A popup's target is on its opener's browser connection, so
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

// Why a page's downloads still running end when the page goes, which the
// launcher cancels with the page and reports nothing of (retirePage):
// it closed by itself, or the Manager closed it. A listed page's downloads
// the Manager has ended by then, so these name a popup's it never listed.
const (
	pageClosedReason     = "the window it began in closed"
	popupDiscardedReason = "the window it began in was not opened as a tab"
)

// hostedPopup is a popup the launcher reported, from its report until its
// page goes: its lineage and its attach.
type hostedPopup struct {
	target  target.ID
	profile profileHandle
	// opener is the page id of the page that opened it.
	opener ids.PageHandle
	// conn is its opener's browser connection, the one its target is on;
	// nil for a popup with no target.
	conn context.Context
	// cancelAttach ends its attach while one is under way.
	cancelAttach context.CancelFunc
}

// takeAttachCancelLocked takes the cancel of popup's attach, if one is under
// way, for its caller to call once it releases e.mu: chromedp's cancel waits
// for the target's cleanup. The caller holds e.mu.
func (popup *hostedPopup) takeAttachCancelLocked() func() {
	cancel := popup.cancelAttach
	popup.cancelAttach = nil
	if cancel == nil {
		return func() {}
	}
	return cancel
}

// popupReported takes the launcher's report of a popup: the engine knows
// its opener, so it hands it to the Manager, and otherwise closes it. A
// report naming a page id the engine already has is ignored before it is
// read, so no report closes a known page; one naming a target a page or a
// pending popup already has is refused, and the target keeps its page.
func (e *hostedEngine) popupReported(pageID ids.PageHandle, detail string) {
	if err := webview2host.ValidatePageID(string(pageID)); err != nil {
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
	opener, targetID := ids.PageHandle(report.Opener), target.ID(report.Target)
	e.mu.Lock()
	profile, known := e.pageProfile[opener]
	// An opener whose close or discard was received opens nothing more.
	known = known && e.pages.admits(opener)
	_, taken = e.pageProfile[pageID]
	_, claimed := e.targetPageLocked(targetID)
	conn := e.pageConn[opener]
	if pending := e.popups[opener]; pending != nil {
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
	case targetID != "" && (conn == nil || conn.Err() != nil):
		e.mu.Unlock()
		// Its target is on its opener's connection, which has ended.
		e.logf("browser: pane host reported popup %s of %s, which has no connection", pageID, report.Opener)
		e.closePage(pageID)
		return
	}
	if !e.pages.addPopup(pageID, opener, profile) {
		e.mu.Unlock()
		e.logf("browser: pane host reported popup %s, a page it already has", pageID)
		return
	}
	e.popups[pageID] = &hostedPopup{target: targetID, profile: profile, opener: opener, conn: conn}
	if targetID != "" {
		e.popupByTarget[targetID] = pageID
	}
	e.pageProfile[pageID] = profile
	e.mu.Unlock()
	if targetID == "" {
		// Nothing to attach: it stays the owner of its downloads until its
		// close (retirePage).
		return
	}
	e.takeTargetInfo(pageID, targetID)
	// Before any later report of the page, its close among them: the
	// Manager records it at once and adopts it on a goroutine of its own.
	e.events.PopupOpened(enginePopup{Profile: profile, Opener: opener, Handle: pageID, URL: report.URL})
}

// attachPopup attaches chromedp to a pending popup of profile, on its
// opener's connection; bind refuses it if that connection ended meanwhile.
func (e *hostedEngine) attachPopup(profile *hostedProfile, handle ids.PageHandle, hooks pageHooks) (pageDriver, error) {
	e.mu.Lock()
	popup := e.popups[handle]
	var targetID target.ID
	var browserCtx context.Context
	if popup != nil && popup.profile == profile.id {
		targetID, browserCtx = popup.target, popup.conn
	}
	e.mu.Unlock()
	switch {
	case targetID == "":
		return nil, fmt.Errorf("browser: the pane host reported no popup %s with a target in this profile", handle)
	case browserCtx.Err() != nil:
		return nil, fmt.Errorf("browser: popup %s: its opener's connection to the pane browser was lost", handle)
	}
	pageCtx, pageCancel := chromedp.NewContext(browserCtx, chromedp.WithTargetID(targetID))
	// The attach is bounded on its own clock, and the popup's close or
	// discard cancels it (takeAttachCancelLocked); its downloads are settled
	// by that close, not by the attach.
	e.mu.Lock()
	if e.popups[handle] != popup {
		e.mu.Unlock()
		pageCancel()
		return nil, fmt.Errorf("browser: popup %s closed before it was adopted", handle)
	}
	popup.cancelAttach = pageCancel
	e.mu.Unlock()
	setup, release := e.setups.begin(context.Background(), e.attachTimeout)
	driver, err := startCDPPage(setup, browserCtx, pageCtx, pageCancel, hooks)
	release()
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
	// Bound only while its record stands and its close was not received: a
	// popup that went meanwhile left no page to bind.
	e.mu.Lock()
	switch {
	case e.popups[handle] != popup || !e.pages.admits(handle):
		err = fmt.Errorf("browser: popup %s closed before it was adopted", handle)
	case browserCtx.Err() != nil:
		err = errors.New("browser: the connection to the pane browser was lost")
	default:
		e.bindLocked(handle, targetID, browserCtx)
		if cdp, ok := driver.(*cdpPage); ok {
			e.drivers[handle] = cdp
		}
	}
	e.mu.Unlock()
	if err != nil {
		driver.Close()
		return nil, err
	}
	return &hostedPage{pageDriver: driver, engine: e, id: handle}, nil
}

// forgetPopupLocked drops the record of popup pageID, whose page goes, and
// answers the cancel of its attach for the caller to call once it releases
// e.mu. The caller holds e.mu.
func (e *hostedEngine) forgetPopupLocked(pageID ids.PageHandle) func() {
	popup := e.popups[pageID]
	if popup == nil {
		return func() {}
	}
	delete(e.popupByTarget, popup.target)
	delete(e.popups, pageID)
	return popup.takeAttachCancelLocked()
}

// allowRequest is the browser-wide navigation policy for a request paused
// in frame: the policy of the profile of the first page the request's
// owners resolve to, asked with those owners, which are the frame, then
// the pages that opened its page, nearest first, as the connection's
// targets name them and, past a popup the launcher reported, as its report
// names its opener (reportedOpenersLocked). When no owner resolves to a
// page of the engine's, every profile's policy is asked in turn, in case a
// page the engine has not bound yet is one of its jar's, and the last
// answers unresolved with what a page with no workspace's authority may
// open (jarAllows); with no profile, it is refused unresolved.
func (e *hostedEngine) allowRequest(targets *cdpTargets, frame frameID, rawURL string) (allowed, resolved bool) {
	owners := targets.owners(frame)
	e.mu.Lock()
	owners = e.reportedOpenersLocked(owners)
	var policies []func([]frameID, string) (bool, bool)
	for _, owner := range owners {
		if pageID, ok := e.ownerPageLocked(owner); ok {
			policies = append(policies, e.policies[e.pageProfile[pageID]])
			break
		}
	}
	if len(policies) == 0 {
		for _, policy := range e.policies {
			policies = append(policies, policy)
		}
	}
	e.mu.Unlock()
	for _, allow := range policies {
		if allow == nil {
			continue
		}
		if allowed, resolved = allow(owners, rawURL); resolved {
			return allowed, true
		}
	}
	return allowed, false
}

// reportedOpenersLocked extends owners, a frame and the pages that opened
// it as the connection's targets name them, past each popup the launcher
// reported with the target of the opener its report names: a window a
// click opened in a new tab has a target that names no opener. The caller
// holds e.mu.
func (e *hostedEngine) reportedOpenersLocked(owners []frameID) []frameID {
	// Each step adds a page not in owners yet, so the chain ends.
	for {
		popupID, ok := e.popupByTarget[target.ID(owners[len(owners)-1])]
		if !ok {
			return owners
		}
		opener := e.popups[popupID].opener
		next := e.targetByPage[opener]
		if pending := e.popups[opener]; pending != nil {
			next = pending.target
		}
		if next == "" || slices.Contains(owners, frameID(next)) {
			return owners
		}
		owners = append(owners, frameID(next))
	}
}

// ownerPageLocked answers the page a frame belongs to: the page whose
// target it is, a pending popup's, or the page that reported it as one of
// its frames. The caller holds e.mu.
func (e *hostedEngine) ownerPageLocked(frame frameID) (ids.PageHandle, bool) {
	if pageID, ok := e.targetPageLocked(target.ID(frame)); ok {
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
