package browser

import (
	"context"
	"fmt"

	"agent-overflow/internal/webview2host"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// Popups on the hosted engine. A window a pane page opens becomes a page of
// the launcher's, which reports it (ReportPopup) under a page id the
// launcher minted, with its opener's page id and its CDP target, before any
// other report of the page (internal/webview2host/popup.go). The engine
// hands it to the Manager as PopupOpened, which adopts it through
// AttachPage or closes it through DiscardPage, as on every engine.
//
// From its report until the Manager lists it the popup is pending, and the
// engine keeps what the page reports meanwhile: its address and title
// reach the Manager by handle, which holds them for the page that takes
// the handle; its downloads, a crash and its close are held here, because
// the Manager acts on those only for a listed page, and delivered in that
// order once it is listed (Adopted). A target's info that arrives before
// its popup report is held by target, the newest per target, bounded.
//
// The popup navigates as soon as the launcher hands its view over, before
// the engine has heard of it, so the page-level navigation policy
// (cdp_page.go) cannot see its first requests. The engine also answers the
// policy browser-wide, as the headless engine does: a request has the
// authority of the page its frame belongs to, or for a popup not yet
// adopted, of the pages that opened it, nearest first, as the browser
// names them (jarAllows).

// maxHeldPopupDownloads bounds the download reports a pending popup keeps.
// The launcher reports progress at most every DownloadProgressInterval, so
// a popup adopted in time keeps a handful; past the bound the newest are
// dropped, and the download's next snapshot starts its row.
const maxHeldPopupDownloads = 64

// hostedPopup is a popup the launcher reported that the Manager has not
// listed yet.
type hostedPopup struct {
	target  string
	profile string
	// downloads are the page's download reports, in arrival order; crashed
	// is the reason its page crashed, and closed says it closed.
	downloads []string
	crashed   string
	closed    bool
}

// heldTargetInfo is a target's newest info change before any page answers
// to it, numbered as it arrived.
type heldTargetInfo struct {
	url, title string
	order      uint64
}

// popupReported takes the launcher's report of a popup: the engine knows
// its opener, so it hands it to the Manager, and otherwise closes it.
func (e *hostedEngine) popupReported(pageID, detail string) {
	if err := webview2host.ValidatePageID(pageID); err != nil {
		e.logf("browser: pane host reported a popup with an unusable id: %v", err)
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
	_, taken := e.pageProfile[pageID]
	switch {
	case taken:
		e.mu.Unlock()
		e.logf("browser: pane host reported popup %s twice", pageID)
		return
	case !known || len(e.popups) >= webview2host.MaxHostPages:
		e.mu.Unlock()
		// No page of the Manager's opened it (it closed meanwhile, or the
		// engine stopped), or the launcher reports more popups than it
		// keeps pages.
		e.closePage(pageID)
		return
	}
	e.popups[pageID] = &hostedPopup{target: report.Target, profile: profile}
	e.popupByTarget[report.Target] = pageID
	e.pageProfile[pageID] = profile
	held, hasHeld := e.heldInfo.take(report.Target)
	e.mu.Unlock()
	if hasHeld {
		e.events.PageInfoChanged(pageID, held.url, held.title, held.order)
	}
	// On its own goroutine, as every engine reports a popup: the Manager
	// adopts it through AttachPage, a CDP round trip, and the launcher's
	// reports must not wait behind it.
	go e.events.PopupOpened(enginePopup{Profile: profile, Opener: report.Opener, Handle: pageID, URL: report.URL})
}

// attachPopup attaches chromedp to a pending popup of profile.
func (e *hostedEngine) attachPopup(profile *hostedProfile, handle string, hooks pageHooks) (pageDriver, error) {
	e.mu.Lock()
	popup := e.popups[handle]
	var targetID string
	var closed bool
	if popup != nil && popup.profile == profile.id {
		targetID, closed = popup.target, popup.closed
	}
	e.mu.Unlock()
	switch {
	case targetID == "":
		return nil, fmt.Errorf("browser: the pane host reported no popup %s in this profile", handle)
	case closed:
		return nil, fmt.Errorf("browser: popup %s closed before it was adopted", handle)
	}
	browserCtx, err := e.ensureBrowser()
	if err != nil {
		return nil, err
	}
	pageCtx, pageCancel := chromedp.NewContext(browserCtx, chromedp.WithTargetID(target.ID(targetID)))
	driver, err := startCDPPage(browserCtx, pageCtx, pageCancel, hooks)
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
// Manager lists it: its downloads in order, then its crash, then its close.
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
	for _, detail := range popup.downloads {
		e.downloadReport(pageID, detail)
	}
	if popup.crashed != "" {
		e.events.PageCrashed(pageID, popup.crashed)
	}
	if popup.closed {
		e.retirePage(pageID)
	}
}

// holdPopupDownloadLocked keeps a download report of a pending popup and
// reports whether pageID is one. The caller holds e.mu.
func (e *hostedEngine) holdPopupDownloadLocked(pageID, detail string) bool {
	popup := e.popups[pageID]
	if popup == nil {
		return false
	}
	if len(popup.downloads) < maxHeldPopupDownloads {
		popup.downloads = append(popup.downloads, detail)
	} else {
		e.logf("browser: popup %s: drop a download report made before it was adopted", pageID)
	}
	return true
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
	return true
}

// forgetPopupLocked drops a popup the Manager discarded. The caller holds
// e.mu.
func (e *hostedEngine) forgetPopupLocked(pageID string) {
	if popup := e.popups[pageID]; popup != nil {
		delete(e.popupByTarget, popup.target)
		delete(e.popups, pageID)
	}
}

// targetCreated records the page that opened a target, for the
// browser-wide navigation policy.
func (e *hostedEngine) targetCreated(event target.EventTargetCreated) {
	info := event.TargetInfo
	if info == nil || info.OpenerID == "" {
		return
	}
	e.mu.Lock()
	e.targetOpeners[string(info.TargetID)] = string(info.OpenerID)
	e.mu.Unlock()
}

// allowRequest is the browser-wide navigation policy: the policy of the
// profile of the first page the request's owners resolve to, asked with
// those owners, or with no page's authority when none resolves.
func (e *hostedEngine) allowRequest(frame, rawURL string) bool {
	e.mu.Lock()
	owners := []string{frame}
	// A target has one opener, so the chain is at most every target the
	// engine knows one for.
	for id := frame; len(owners) <= len(e.targetOpeners); {
		opener := e.targetOpeners[id]
		if opener == "" {
			break
		}
		owners = append(owners, opener)
		id = opener
	}
	var allow func([]string, string) bool
	for _, owner := range owners {
		if pageID, ok := e.ownerPageLocked(owner); ok {
			allow = e.policies[e.pageProfile[pageID]]
			break
		}
	}
	if allow == nil {
		// No page resolves: any profile's policy answers with no
		// workspace's authority, since none of the owners is its page.
		for _, policy := range e.policies {
			allow = policy
			break
		}
	}
	e.mu.Unlock()
	return allow != nil && allow(owners, rawURL)
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
