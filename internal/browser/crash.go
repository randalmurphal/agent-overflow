package browser

import (
	"fmt"
	"log"
	"strings"

	"github.com/chromedp/cdproto/target"
)

// Crashed pages. When a page's renderer or web content process ends while
// the page is open, its engine reports it (engineEvents.PageCrashed):
// WebView2's ProcessFailed for the page's renderer or browser process, CDP's
// Target.targetCrashed on the hosted and headless engines, WebKitGTK's
// web-process-terminated and WKWebView's
// webViewWebContentProcessDidTerminate:. A browser lost whole reports each of
// its pages the same way: a headless Chromium that ended without its profile
// ending it (reportLost), and a hosted CDP connection that ended without
// Stop (hostedEngine.watchBrowser). The tab stays, as a suspended
// record carrying the failure: the engine page is closed, the tab shows the
// failure with a Reload, and the Reload restores it at its address through
// the ordinary restore path (ReloadCompanionPage, resolvePage). Selecting
// the tab does not restore it; a tool call on it does, as on any suspended
// page.

// maxCrashReasonBytes bounds the engine's reason on the tab.
const maxCrashReasonBytes = 128

// crashFailure is the failure a crashed tab shows.
func crashFailure(reason string) string {
	reason = truncateUTF8(strings.TrimSpace(reason), maxCrashReasonBytes)
	if reason == "" {
		reason = "unknown reason"
	}
	return "the page's process ended (" + reason + ")"
}

// cdpCrashReason names a Target.targetCrashed for the tab.
func cdpCrashReason(event target.EventTargetCrashed) string {
	status := strings.TrimSpace(event.Status)
	if status == "" {
		status = "crashed"
	}
	if event.ErrorCode != 0 {
		return fmt.Sprintf("%s, code %d", status, event.ErrorCode)
	}
	return status
}

// pageCrashed is engineEvents.PageCrashed. The page's operations are
// cancelled first, so one waiting on a dead renderer gives up the page's
// lock; then, in one m.mu hold, the page leaves its scope and its record
// takes its place, as a suspension does (suspendPage). A page closed or
// suspended meanwhile, or one whose records were cleared since the report
// (the browser turned off, its site data cleared), is left as it is.
func (m *Manager) pageCrashed(handle, reason string) {
	m.mu.Lock()
	p := m.pageByHandleLocked(handle)
	cleared := m.recordsCleared
	m.mu.Unlock()
	if p == nil {
		return
	}
	log.Printf("browser: page %s of thread %s crashed: %s", p.id, p.owner, reason)
	p.cancelOps()
	p.mu.Lock()
	m.mu.Lock()
	var scope *jarScope
	for _, candidate := range m.scopes {
		if candidate.pages[p.id] == p {
			scope = candidate
			break
		}
	}
	if scope == nil || m.closed || m.recordsCleared != cleared {
		m.mu.Unlock()
		p.mu.Unlock()
		return
	}
	ended := m.pageLeftLocked(scope, p, "the page's process ended")
	// The cache, not a read: a page whose process ended answers none, and
	// its last reports stand.
	rec := recordOf(p, p.cachedInfo())
	rec.failure = crashFailure(reason)
	m.suspended[p.id] = rec
	var released *jarScope
	if m.releaseScopeLocked(scope) {
		released = scope
	}
	m.mu.Unlock()
	m.publishDownloads(ended.notes...)
	p.driver.Close()
	m.endFindLocked(p)
	p.mu.Unlock()
	m.disposeReleased(ended, released, p.owner, p.id)
	m.emitThreadState(p.owner)
	// Hides the dead view; the pane shows the failure in its place.
	m.syncPanePresentation(p.owner)
	if err := m.persistThreads([]string{p.owner}); err != nil {
		// The tab is in memory; only a restart before the next save loses
		// it, as it would have lost the live page.
		log.Printf("browser: page %s crashed: save the thread's pages: %v", p.id, err)
	}
}
