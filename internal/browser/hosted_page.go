package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

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
	waiter, err := e.watch(pageID)
	if err != nil {
		return nil, err
	}
	defer e.unwatch(pageID)

	if !e.dispatch(webview2host.Directive{Op: webview2host.OpCreate, PageID: pageID, ProfileID: profile.id, Ephemeral: profile.ephemeral}) {
		return nil, errors.New("browser: pane host refused the create directive")
	}

	report, err := awaitHostReport(ctx, waiter, e.createTimeout)
	if err != nil {
		// The launcher may still be mid-create; a close for a page it has
		// not finished is harmless, and skipping it would leak a controller.
		e.closePage(pageID)
		return nil, fmt.Errorf("browser: create pane page: %w", err)
	}
	if report.kind != webview2host.ReportCreated {
		// No controller is left to close, but a find-unavailable report may
		// have preceded the failure.
		e.mu.Lock()
		delete(e.noFind, pageID)
		e.mu.Unlock()
		return nil, fmt.Errorf("browser: pane host reported %s: %s", report.kind, report.detail)
	}
	targetID := strings.TrimSpace(report.detail)
	if targetID == "" {
		e.closePage(pageID)
		return nil, errors.New("browser: pane host reported a page with no CDP target")
	}

	browserCtx, err := e.ensureBrowser()
	if err != nil {
		e.closePage(pageID)
		return nil, err
	}
	pageCtx, pageCancel := chromedp.NewContext(browserCtx, chromedp.WithTargetID(target.ID(targetID)))
	driver, err := startCDPPage(browserCtx, pageCtx, pageCancel, hooks)
	if err != nil {
		e.closePage(pageID)
		return nil, err
	}
	if err := e.bind(pageID, targetID, browserCtx); err != nil {
		driver.Close()
		e.closePage(pageID)
		return nil, err
	}
	e.mu.Lock()
	if cdp, ok := driver.(*cdpPage); ok {
		e.drivers[pageID] = cdp
	}
	e.pageProfile[pageID] = profile.id
	held, hasHeld := e.heldInfo.take(targetID)
	e.mu.Unlock()
	if hasHeld {
		// An info change that reached the engine before the page was bound.
		e.events.PageInfoChanged(pageID, held.url, held.title, held.order)
	}
	return &hostedPage{pageDriver: driver, engine: e, id: pageID}, nil
}

// DownloadSaving: WebView2 saves to the Windows user's Downloads folder
// (hosted_downloads.go).
func (e *hostedEngine) DownloadSaving() downloadSaving { return downloadsSavedByEngine }

// DiscardPage closes a controller the Manager declined to adopt. The handle
// is this engine's page id, because that is what pageDriver.Handle answers.
func (e *hostedEngine) DiscardPage(handle string) {
	e.closePage(handle)
}

// Report routes one launcher answer. Called from the App's
// BrowserHostReport binding, which is the backend end of the RPC the
// launcher posts over the notification bridge.
func (e *hostedEngine) Report(pageID string, kind webview2host.ReportKind, detail string) {
	switch kind {
	case webview2host.ReportCreated:
		if !e.deliver(pageID, hostReport{kind: kind, detail: detail}) {
			// Nobody is waiting: the create already timed out or was
			// abandoned. The controller is real, so close it rather than
			// leaving an orphan window in the launcher.
			e.closePage(pageID)
		}
	case webview2host.ReportCreateFailed:
		e.deliver(pageID, hostReport{kind: kind, detail: detail})
	case webview2host.ReportProcessFailed:
		// A process death can land on a page still being created, which
		// fails its creation, OR on a live one, whose tab turns crashed
		// (crash.go) when the page's own process ended.
		if e.deliver(pageID, hostReport{kind: kind, detail: detail}) {
			return
		}
		if !processFailureCrashes(detail) {
			e.logf("browser: pane host page %s reported %s; the page keeps running", pageID, detail)
			return
		}
		// Inline, as retirePage reports a close: this is the launcher's
		// report path, an RPC goroutine of its own.
		e.pageCrashed(pageID, detail)
	case webview2host.ReportClosed:
		if !e.deliver(pageID, hostReport{kind: kind, detail: detail}) {
			e.retirePage(pageID)
		}
	case webview2host.ReportAccelerator:
		// The launcher already matched and swallowed the chord; the Manager
		// re-matches (same set) and routes it to the owning thread.
		var pressed keybindings.Accelerator
		if err := json.Unmarshal([]byte(detail), &pressed); err != nil {
			e.logf("browser: pane host accelerator for page %s is unreadable: %v", pageID, err)
			return
		}
		e.events.KeyChord(pageID, pressed, KeyPress{})
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
		e.events.PageFocusReported(pageID, report.Mount, report.Intent, failure)
	case webview2host.ReportDownload:
		e.downloadReport(pageID, detail)
	case webview2host.ReportPopup:
		e.popupReported(pageID, detail)
	case webview2host.ReportCleared, webview2host.ReportClearFailed:
		// A clear names a correlation id, not a page: there is no controller
		// to close and no page to retire. Nobody waiting means ClearSiteData
		// already gave up on its own bound and has reported that.
		if !e.deliver(pageID, hostReport{kind: kind, detail: detail}) {
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

// retirePage drops the engine's bookkeeping and tells the Manager the page
// is gone. The Manager alone decides what that means for its registry.
//
// PageClosed is called inline here because every SYNCHRONOUS caller is the
// launcher's report path, which is an RPC goroutine of its own. The one
// caller that reads CDP events starts this on a goroutine instead
// (targetDestroyed above).
func (e *hostedEngine) retirePage(pageID string) {
	e.mu.Lock()
	if e.retirePendingLocked(pageID) {
		// A popup the Manager has not listed: its close waits for its
		// listing (popupListed), or goes with its discard.
		e.mu.Unlock()
		return
	}
	targetID, known := e.targetByPage[pageID]
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
	e.forgetDownloadsLocked(pageID)
	e.mu.Unlock()
	if !known {
		return
	}
	e.events.PageClosed(pageID)
}

func (e *hostedEngine) watch(pageID string) (chan hostReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, exists := e.pending[pageID]; exists {
		return nil, fmt.Errorf("browser: pane page %s is already being created", pageID)
	}
	waiter := make(chan hostReport, 1)
	e.pending[pageID] = waiter
	return waiter, nil
}

func (e *hostedEngine) unwatch(pageID string) {
	e.mu.Lock()
	delete(e.pending, pageID)
	e.mu.Unlock()
}

// deliver hands one report to a pending create. Reports whether a waiter
// took it, which is what tells the caller a late or unmatched report needs
// its own handling.
func (e *hostedEngine) deliver(pageID string, report hostReport) bool {
	e.mu.Lock()
	waiter, ok := e.pending[pageID]
	if ok {
		delete(e.pending, pageID)
	}
	e.mu.Unlock()
	if !ok {
		return false
	}
	waiter <- report
	return true
}

// bind pairs a page with its CDP target and with conn, the browser
// connection its session runs on. It refuses a connection that has ended:
// that connection's retirement may already have claimed its pages, and a
// later one would never report this page.
func (e *hostedEngine) bind(pageID, targetID string, conn context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if conn.Err() != nil {
		return errors.New("browser: the connection to the pane browser was lost")
	}
	e.targetByPage[pageID] = targetID
	e.pageByTarget[targetID] = pageID
	e.pageConn[pageID] = conn
	return nil
}

func (e *hostedEngine) pageForTarget(targetID string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.targetPageLocked(targetID)
}

// targetPageLocked answers the page whose CDP target targetID is: a bound
// page's, or a popup's the Manager has not listed. The caller holds e.mu.
func (e *hostedEngine) targetPageLocked(targetID string) (string, bool) {
	if pageID, ok := e.pageByTarget[targetID]; ok {
		return pageID, true
	}
	pageID, ok := e.popupByTarget[targetID]
	return pageID, ok
}

// closePage retires the bookkeeping and asks the launcher to destroy the
// controller. The launcher answers `closed`, which is a no-op by then —
// the close a page's own Close already accounted for must not be reported
// to the Manager twice.
func (e *hostedEngine) closePage(pageID string) {
	e.mu.Lock()
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
	e.forgetPopupLocked(pageID)
	e.forgetDownloadsLocked(pageID)
	e.mu.Unlock()
	e.dispatch(webview2host.Directive{Op: webview2host.OpClose, PageID: pageID})
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

func newHostedPageID() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}

// hostedPage is one launcher-hosted controller. Every operation is the CDP
// driver's; only destruction differs, because detaching chromedp leaves a
// real window on the user's screen.
type hostedPage struct {
	pageDriver
	engine    *hostedEngine
	id        string
	closeOnce sync.Once
}

// Handle is the PAGE id, not the CDP target id: it is what the launcher's
// reports name and what every directive addresses, so it is the one handle
// both halves of this engine agree on.
func (p *hostedPage) Handle() string { return p.id }

// OwnsFrame answers for the page id too, which is the frame a launcher
// download report names (hosted_downloads.go).
func (p *hostedPage) OwnsFrame(frame string) bool {
	return frame == p.id || p.pageDriver.OwnsFrame(frame)
}

// Adopted is adoptedPage: a popup's tab is listed, so what it reported
// meanwhile is delivered (popupListed).
func (p *hostedPage) Adopted() { p.engine.popupListed(p.id) }

func (p *hostedPage) Close() {
	p.pageDriver.Close()
	p.closeOnce.Do(func() { p.engine.closePage(p.id) })
}
