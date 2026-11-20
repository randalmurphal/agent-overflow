package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/keybindings"
	"agent-overflow/internal/webview2host"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/google/uuid"
)

// createPage runs the full round trip: directive out, report back, chromedp
// attached to the target the report named.
func (e *hostedEngine) createPage(ctx context.Context, profile *hostedProfile, hooks pageHooks) (pageDriver, error) {
	pageID := newHostedPageID()
	waiter, err := e.waiters.watch(pageID)
	if err != nil {
		return nil, err
	}
	defer e.waiters.unwatch(pageID)
	// Before the directive: a close the launcher reports once the page is
	// created ends this record, and with it the bind.
	if !e.pages.addCreating(pageID) {
		return nil, fmt.Errorf("browser: pane page %s already has a record", pageID)
	}

	if !e.dispatch(webview2host.Directive{Op: webview2host.OpCreate, PageID: string(pageID), ProfileID: string(profile.id), Ephemeral: profile.ephemeral}) {
		// Nothing was sent, so the launcher has no page to report.
		e.pages.abandon(pageID)
		return nil, errors.New("browser: pane host refused the create directive")
	}

	report, err := awaitHostReport(ctx, waiter, e.createTimeout)
	if err != nil {
		// The launcher may still be mid-create; a close for a page it has
		// not finished is harmless, and skipping it would leak a controller.
		e.endCreate(ctx, pageID)
		return nil, fmt.Errorf("browser: create pane page: %w", err)
	}
	if report.kind != webview2host.ReportCreated {
		// No controller is left to close, but a find-unavailable report may
		// have preceded the failure.
		e.pages.abandon(pageID)
		e.mu.Lock()
		delete(e.noFind, pageID)
		e.mu.Unlock()
		return nil, fmt.Errorf("browser: pane host reported %s: %s", report.kind, report.detail)
	}
	targetID := target.ID(strings.TrimSpace(report.detail))
	if targetID == "" {
		e.endCreate(ctx, pageID)
		return nil, errors.New("browser: pane host reported a page with no CDP target")
	}
	// Claimed before the attach, whose end would close the target.
	if err := e.claimTarget(pageID, targetID); err != nil {
		e.endCreate(ctx, pageID)
		return nil, err
	}

	browserCtx, err := e.ensureBrowser()
	if err != nil {
		e.endCreate(ctx, pageID)
		return nil, err
	}
	pageCtx, pageCancel := chromedp.NewContext(browserCtx, chromedp.WithTargetID(targetID))
	setup, release := e.setups.begin(ctx, e.attachTimeout)
	driver, err := startCDPPage(setup, browserCtx, pageCtx, pageCancel, hooks)
	release()
	if err != nil {
		e.endCreate(ctx, pageID)
		return nil, err
	}
	if err := e.bind(pageID, targetID, browserCtx, driver, profile.id); err != nil {
		driver.Close()
		e.endCreate(ctx, pageID)
		return nil, err
	}
	// An info change that reached the engine before the page was bound.
	e.takeTargetInfo(pageID, targetID)
	return &hostedPage{pageDriver: driver, engine: e, id: pageID}, nil
}

// endCreate closes a page whose create failed, the launcher asked to
// destroy whatever it made (closePage), and waits within ctx for the
// page's retirement, which nothing is ahead of on a page being created. A
// wait that ends first is logged: the lane still retires the page, or Stop
// forgot it.
func (e *hostedEngine) endCreate(ctx context.Context, pageID ids.PageHandle) {
	if err := e.closePage(pageID).wait(ctx); err != nil {
		e.logf("browser: close pane page %s after its create failed: %v", pageID, err)
	}
}

// DownloadSaving: WebView2 saves to the Windows user's Downloads folder
// (hosted_downloads.go).
func (e *hostedEngine) DownloadSaving() downloadSaving { return downloadsSavedByEngine }

// DiscardPage closes a controller the Manager declined to adopt. The handle
// is this engine's page id, because that is what pageDriver.Handle answers.
// The discard is received at once (closePage) and retires the page on its
// lane, after the reports received before it; DiscardPage waits for that
// within ctx, and past it answers errDiscardPending while the lane still
// finishes the discard. Stop ends the wait with errDiscardInterrupted. A
// second discard of the page joins the first.
func (e *hostedEngine) DiscardPage(ctx context.Context, handle ids.PageHandle) error {
	if err := e.closePage(handle).wait(ctx); err != nil {
		return fmt.Errorf("browser: discard pane page %s: %w", handle, err)
	}
	return nil
}

// Report routes one launcher answer. Called from the App's
// BrowserHostReport binding, which is the backend end of the RPC the
// launcher posts over the notification bridge, so its return acknowledges
// the report: a download snapshot, a close or a crash is received here and
// processed on the page's lane (hosted_pages.go, hosted_report_lanes.go).
func (e *hostedEngine) Report(pageID ids.PageHandle, kind webview2host.ReportKind, detail string) {
	switch kind {
	case webview2host.ReportCreated:
		if !e.waiters.deliver(pageID, hostReport{kind: kind, detail: detail}) {
			// Nobody is waiting: the create already timed out or was
			// abandoned. The controller is real, so close it rather than
			// leaving an orphan window in the launcher.
			e.closePage(pageID)
		}
	case webview2host.ReportCreateFailed:
		e.waiters.deliver(pageID, hostReport{kind: kind, detail: detail})
	case webview2host.ReportProcessFailed:
		// A process death can land on a page still being created, which
		// fails its creation, OR on a live one, whose tab turns crashed
		// (crash.go) when the page's own process ended.
		if e.waiters.deliver(pageID, hostReport{kind: kind, detail: detail}) {
			return
		}
		if !processFailureCrashes(detail) {
			e.logf("browser: pane host page %s reported %s; the page keeps running", pageID, detail)
			return
		}
		e.acceptCrash(pageID, detail)
	case webview2host.ReportClosed:
		if !e.waiters.deliver(pageID, hostReport{kind: kind, detail: detail}) {
			e.acceptClose(pageID)
		}
	case webview2host.ReportAccelerator:
		// The launcher already matched and swallowed the chord; the Manager
		// re-matches (same set) and routes it to the owning thread.
		var pressed keybindings.Accelerator
		if err := json.Unmarshal([]byte(detail), &pressed); err != nil {
			e.logf("browser: pane host accelerator for page %s is unreadable: %v", pageID, err)
			return
		}
		e.events.KeyChord(pageID, pressed, keyPress{})
	case webview2host.ReportFindUnavailable:
		e.noteFindUnavailable(pageID)
	case webview2host.ReportFocused, webview2host.ReportFocusFailed:
		report, err := webview2host.ParseFocusReport(detail)
		if err != nil {
			e.logf("browser: pane host focus report for page %s is unreadable: %v", pageID, err)
			return
		}
		var failure error
		if kind == webview2host.ReportFocusFailed {
			failure = fmt.Errorf("browser: the page did not take keyboard focus: %s", report.Error)
		}
		e.events.PageFocusReported(pageID, MountID(report.Mount), report.Intent, failure)
	case webview2host.ReportDownload:
		e.acceptDownload(pageID, detail)
	case webview2host.ReportPopup:
		e.popupReported(pageID, detail)
	case webview2host.ReportCleared, webview2host.ReportClearFailed:
		// A clear names a correlation id, not a page: there is no controller
		// to close and no page to retire. Nobody waiting means ClearSiteData
		// already gave up on its own bound and has reported that.
		if !e.waiters.deliver(pageID, hostReport{kind: kind, detail: detail}) {
			e.logf("browser: pane host reported %s for an abandoned clear %s", kind, pageID)
		}
	default:
		e.logf("browser: ignoring unknown pane host report %q for page %s", kind, pageID)
	}
}

// processFailureCrashes reports whether a WebView2 ProcessFailed kind, as
// processFailedKindName names it, ended the page: its renderer or the whole
// browser process exited. The other kinds (an unresponsive renderer, a
// frame's or a utility, GPU or plug-in process) leave the page running,
// and WebView2 restarts what it needs.
func processFailureCrashes(detail string) bool {
	return detail == "render-process-exited" || detail == "browser-process-exited"
}

// forgetPageLocked drops pageID from the engine's page bookkeeping, its
// record and downloads aside (retirePage). The caller holds e.mu.
func (e *hostedEngine) forgetPageLocked(pageID ids.PageHandle) {
	targetID := e.targetByPage[pageID]
	delete(e.targetByPage, pageID)
	delete(e.pageByTarget, targetID)
	delete(e.pageConn, pageID)
	delete(e.shown, pageID)
	delete(e.bounds, pageID)
	delete(e.drivers, pageID)
	delete(e.ratios, pageID)
	delete(e.scales, pageID)
	delete(e.noFind, pageID)
	delete(e.pageProfile, pageID)
}

// hostWaiters are the creates and clears waiting for the launcher's answer,
// by page or correlation id. Its lock is its own and nothing is called
// under it, so a report's receipt (deliver) waits for no engine lock.
type hostWaiters struct {
	mu     sync.Mutex
	byPage map[ids.PageHandle]chan hostReport
}

func (w *hostWaiters) watch(pageID ids.PageHandle) (chan hostReport, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, exists := w.byPage[pageID]; exists {
		return nil, fmt.Errorf("browser: pane page %s is already being created", pageID)
	}
	if w.byPage == nil {
		w.byPage = make(map[ids.PageHandle]chan hostReport)
	}
	waiter := make(chan hostReport, 1)
	w.byPage[pageID] = waiter
	return waiter, nil
}

func (w *hostWaiters) unwatch(pageID ids.PageHandle) {
	w.mu.Lock()
	delete(w.byPage, pageID)
	w.mu.Unlock()
}

// has reports whether pageID waits for an answer.
func (w *hostWaiters) has(pageID ids.PageHandle) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.byPage[pageID]
	return ok
}

// deliver hands one report to its waiter. Reports whether a waiter took
// it, which is what tells the caller a late or unmatched report needs its
// own handling.
func (w *hostWaiters) deliver(pageID ids.PageHandle, report hostReport) bool {
	w.mu.Lock()
	waiter, ok := w.byPage[pageID]
	if ok {
		delete(w.byPage, pageID)
	}
	w.mu.Unlock()
	if ok {
		waiter <- report
	}
	return ok
}

// interrupt closes every waiter, which its caller reads as the launcher
// interrupted (awaitHostReport).
func (w *hostWaiters) interrupt() {
	w.mu.Lock()
	waiters := w.byPage
	w.byPage = nil
	w.mu.Unlock()
	for _, waiter := range waiters {
		close(waiter)
	}
}

// claimTarget pairs a page the engine is creating with the CDP target the
// launcher reported for it, unless its close was received or another page,
// bound or a popup, has that target. Its close forgets the pair
// (forgetPageLocked), in an e.mu hold after it was received, so no pair
// outlives a page closed meanwhile.
func (e *hostedEngine) claimTarget(pageID ids.PageHandle, targetID target.ID) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.pages.creating(pageID); err != nil {
		return err
	}
	if owner, claimed := e.targetPageLocked(targetID); claimed {
		return fmt.Errorf("browser: pane host reported page %s with target %s, which page %s has", pageID, targetID, owner)
	}
	e.targetByPage[pageID] = targetID
	e.pageByTarget[targetID] = pageID
	return nil
}

// bind pairs a page the engine created with its CDP target, its driver,
// its profile and conn, the browser connection its session runs on, in one
// e.mu hold, and moves its record from
// creating to open, from which it admits downloads. It refuses a page
// whose close was received, and a connection that has ended: that
// connection's retirement may already have claimed its pages, and a later
// one would never report this page.
func (e *hostedEngine) bind(pageID ids.PageHandle, targetID target.ID, conn context.Context, driver pageDriver, profile profileHandle) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if conn.Err() != nil {
		return errors.New("browser: the connection to the pane browser was lost")
	}
	if err := e.pages.bound(pageID); err != nil {
		return err
	}
	e.bindLocked(pageID, targetID, conn)
	if cdp, ok := driver.(*cdpPage); ok {
		e.drivers[pageID] = cdp
	}
	e.pageProfile[pageID] = profile
	return nil
}

// bindLocked is bind's pairing for a caller that holds e.mu and has
// checked conn. It changes no page's record: a popup's opened at its
// report.
func (e *hostedEngine) bindLocked(pageID ids.PageHandle, targetID target.ID, conn context.Context) {
	e.targetByPage[pageID] = targetID
	e.pageByTarget[targetID] = pageID
	e.pageConn[pageID] = conn
}

func (e *hostedEngine) pageForTarget(targetID target.ID) (ids.PageHandle, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.targetPageLocked(targetID)
}

// targetPageLocked answers the page whose CDP target targetID is: a bound
// page's, or a popup's the Manager has not listed. The caller holds e.mu.
func (e *hostedEngine) targetPageLocked(targetID target.ID) (ids.PageHandle, bool) {
	if pageID, ok := e.pageByTarget[targetID]; ok {
		return pageID, true
	}
	pageID, ok := e.popupByTarget[targetID]
	return pageID, ok
}

// closePage receives the engine's own close of a page, for the Manager's
// close or discard or a create that failed, and asks the launcher to
// destroy the controller. A page with a record seals now and retires on its
// lane after the reports received before it (retirePage), which ends its
// downloads still running: the launcher cancels them with the page and
// reports nothing. A listed page's the Manager ended as it closed it. The
// launcher answers `closed`, which is a no-op by then: the close a page's
// own Close already accounted for must not be reported to the Manager
// twice. A page with no record (a create that failed before its bind, a
// popup refused at its report) is forgotten at once. It answers the
// receipt whose wait ends once the page is retired; a page already closing
// retires once, and its answer is its first close's. The launcher is asked
// to close the page every time: a close of a page it closed already
// changes nothing.
func (e *hostedEngine) closePage(pageID ids.PageHandle) receipt {
	defer e.dispatch(webview2host.Directive{Op: webview2host.OpClose, PageID: string(pageID)})
	discarded := e.pages.receive(pageID, receivedDiscard())
	if discarded.outcome == receiptNoPage {
		e.mu.Lock()
		e.endCloseFallbackLocked(pageID)
		e.forgetPopupLocked(pageID)
		e.forgetPageLocked(pageID)
		e.mu.Unlock()
	}
	return discarded
}

// awaitHostReport waits for one report, the caller's cancellation, or the
// bound. Every path returns; a launcher that never answers is an error,
// never a hang.
func awaitHostReport(ctx context.Context, waiter chan hostReport, timeout time.Duration) (hostReport, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case report, ok := <-waiter:
		if !ok {
			return hostReport{}, errors.New("pane host was interrupted")
		}
		return report, nil
	case <-ctx.Done():
		return hostReport{}, ctx.Err()
	case <-timer.C:
		return hostReport{}, fmt.Errorf("pane host did not answer within %s", timeout)
	}
}

func newHostedPageID() ids.PageHandle {
	return ids.PageHandle(strings.ReplaceAll(uuid.NewString(), "-", ""))
}

// hostedPage is one launcher-hosted controller. Every operation is the CDP
// driver's; only destruction differs, because detaching chromedp leaves a
// real window on the user's screen.
type hostedPage struct {
	pageDriver
	engine    *hostedEngine
	id        ids.PageHandle
	closeOnce sync.Once
}

// Handle is the PAGE id, not the CDP target id: it is what the launcher's
// reports name and what every directive addresses, so it is the one handle
// both halves of this engine agree on.
func (p *hostedPage) Handle() ids.PageHandle { return p.id }

// OwnsFrame answers for the page id too, which is the frame a launcher
// download report names (hosted_downloads.go).
func (p *hostedPage) OwnsFrame(frame frameID) bool {
	return frame == frameID(p.id) || p.pageDriver.OwnsFrame(frame)
}

func (p *hostedPage) Close() {
	p.pageDriver.Close()
	p.closeOnce.Do(func() { p.engine.closePage(p.id) })
}

// Listed is the Manager's listing of an adopted popup (listingDriver): its
// later reports run on its own lane.
func (p *hostedPage) Listed() { p.engine.pages.listed(p.id) }

// TakeReceivedDownloads is receivingDriver: the page's leave lists what the
// launcher reported of it that its lane has not admitted yet.
func (p *hostedPage) TakeReceivedDownloads(listed func(downloadID) bool) []downloadStart {
	return p.engine.pages.takeReceived(p.id, listed)
}

var (
	_ listingDriver   = (*hostedPage)(nil)
	_ receivingDriver = (*hostedPage)(nil)
)
