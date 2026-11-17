package browser

import (
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/webview2host"
)

// awaitDownloads waits until the thread lists downloads in the states want,
// newest first.
func (l *popupLauncher) awaitDownloads(access Access, want ...string) {
	l.t.Helper()
	deadline := time.Now().Add(hostedTestDeadline)
	for {
		var got []string
		for _, d := range l.manager.CompanionState(access).Downloads {
			got = append(got, d.State)
		}
		if slices.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			l.t.Fatalf("the thread's downloads are %v, want %v", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// pageDownloads answers the states of the downloads listed in the tab
// whose engine page is handle, oldest first.
func (l *popupLauncher) pageDownloads(threadID, handle string) []string {
	p := l.tabs(threadID)[handle]
	if p == nil {
		return nil
	}
	p.downloadMu.Lock()
	defer p.downloadMu.Unlock()
	var out []string
	for _, d := range p.downloads {
		out = append(out, d.State)
	}
	return out
}

// A popup that finishes a download and closes before the Manager lists it
// leaves the finished download in its opener's tab, and the one it left
// running ends there.
func TestBP4CompletedDownloadOfAPopupClosedBeforeItsListing(t *testing.T) {
	l := newPopupLauncher(t)
	access := popupTestAccess(t, "thread")
	_, opener := l.open(access)
	release := l.gate("PT-1")
	defer release()
	popupID, _ := l.popup(opener, "https://example.test/popup")
	l.awaitAttach("PT-1")
	reportDownload(t, l.engine, popupID, snapshot("dl-1", webview2host.DownloadRunning, 10))
	reportDownload(t, l.engine, popupID, snapshot("dl-1", webview2host.DownloadCompleted, 100))
	reportDownload(t, l.engine, popupID, snapshot("dl-2", webview2host.DownloadRunning, 10))
	l.report(popupID, webview2host.ReportClosed, "")
	l.awaitDownloads(access, downloadStateCanceled, downloadStateCompleted)
	if got := l.pageDownloads(access.ThreadID, opener); !slices.Equal(got, []string{downloadStateCompleted, downloadStateCanceled}) {
		t.Fatalf("the opener's tab lists %v", got)
	}
	release()
	l.awaitTabGone(access.ThreadID, popupID)
	l.settledEngine()
}

// A download's terminal snapshot reaches the popup's tab however many
// snapshots came before it while the popup was adopted.
func TestBP4TerminalDownloadSurvivesHeldReportBound(t *testing.T) {
	l := newPopupLauncher(t)
	access := popupTestAccess(t, "thread")
	_, opener := l.open(access)
	release := l.gate("PT-1")
	defer release()
	popupID, _ := l.popup(opener, "https://example.test/popup")
	l.awaitAttach("PT-1")
	for i := range 200 {
		reportDownload(t, l.engine, popupID, snapshot("dl-1", webview2host.DownloadRunning, int64(i%100)))
	}
	reportDownload(t, l.engine, popupID, snapshot("dl-1", webview2host.DownloadCompleted, 100))
	release()
	l.awaitTab(access.ThreadID, popupID, true)
	l.awaitDownloads(access, downloadStateCompleted)
	if got := l.pageDownloads(access.ThreadID, popupID); !slices.Equal(got, []string{downloadStateCompleted}) {
		t.Fatalf("the popup's tab lists %v", got)
	}
	l.settledEngine()
}

// A popup opened by a popup the Manager is still attaching becomes a tab
// after it, and a download it started meanwhile is in its own tab.
func TestBP4NestedHostedPopupBeforeParentListed(t *testing.T) {
	l := newPopupLauncher(t)
	access := popupTestAccess(t, "thread")
	_, opener := l.open(access)
	release := l.gate("PT-1")
	defer release()
	parent, _ := l.popup(opener, "https://example.test/parent")
	l.awaitAttach("PT-1")
	child, _ := l.popup(parent, "https://example.test/child")
	reportDownload(t, l.engine, child, snapshot("dl-1", webview2host.DownloadRunning, 10))
	if l.closed(child) {
		t.Fatal("the child was closed while its opener was attached")
	}
	release()
	l.awaitTab(access.ThreadID, parent, true)
	l.awaitTab(access.ThreadID, child, true)
	l.awaitDownloads(access, downloadStateRunning)
	if got := l.pageDownloads(access.ThreadID, child); !slices.Equal(got, []string{downloadStateRunning}) {
		t.Fatalf("the child's tab lists %v", got)
	}
	l.settledEngine()
}
