package browser

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/webview2host"

	"github.com/chromedp/cdproto/target"
)

// awaitDownloads waits until the thread lists downloads in the states want,
// newest first.
func (l *popupLauncher) awaitDownloads(access Access, want ...string) {
	l.t.Helper()
	deadline := time.Now().Add(hostedTestDeadline)
	for {
		var got []string
		for _, d := range l.manager.CompanionState(access.Caller()).Downloads {
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
func TestCompletedDownloadOfAPopupClosedBeforeItsListing(t *testing.T) {
	l := newPopupLauncher(t)
	access := l.access("thread")
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

// A popup the launcher reports with no target (it went before the launcher
// read it) is never offered for adoption. The engine keeps it as the owner
// of the downloads the launcher reports for it until its close, which lists
// them in its opener's tab, as they are, and the engine keeps nothing of it
// after.
func TestHostedTargetlessPopupsDownloadsGoToItsOpenerAtItsClose(t *testing.T) {
	l := newPopupLauncher(t)
	access := l.access("thread")
	_, opener := l.open(access)
	offered := make(chan string, 1)
	adopt := l.engine.events.PopupOpened
	l.engine.events.PopupOpened = func(popup enginePopup) {
		select {
		case offered <- popup.Handle:
		default:
		}
		adopt(popup)
	}
	const popupID = "popup-gone"
	l.reportPopup(popupID, webview2host.PopupReport{Opener: opener, URL: "https://example.test/file.bin"})
	reportDownload(t, l.engine, popupID, snapshot("dl-1", webview2host.DownloadRunning, 10))
	reportDownload(t, l.engine, popupID, snapshot("dl-1", webview2host.DownloadCompleted, 100))
	reportDownload(t, l.engine, popupID, snapshot("dl-2", webview2host.DownloadRunning, 10))
	l.report(popupID, webview2host.ReportClosed, "")
	l.awaitDownloads(access, downloadStateCanceled, downloadStateCompleted)
	if got := l.pageDownloads(access.ThreadID, opener); !slices.Equal(got, []string{downloadStateCompleted, downloadStateCanceled}) {
		t.Fatalf("the opener's tab lists %v", got)
	}
	select {
	case handle := <-offered:
		t.Fatalf("the targetless popup %s was offered for adoption", handle)
	default:
	}
	l.settledEngine()
	l.engine.mu.Lock()
	_, profiled := l.engine.pageProfile[popupID]
	_, ended := l.engine.downloads.ended[popupID]
	l.engine.mu.Unlock()
	if profiled || ended {
		t.Fatalf("the engine keeps the closed popup: profile %v, ended downloads %v", profiled, ended)
	}
}

// A page's target can go before the launcher's ordered reports of its
// downloads' ends and of its close arrive. The launcher's close ends the
// page, so a download it finished after the target went ends completed.
func TestHostedDownloadEndAfterTheTargetWentIsKept(t *testing.T) {
	for _, popup := range []bool{false, true} {
		t.Run(fmt.Sprintf("popup %v", popup), func(t *testing.T) {
			l := newPopupLauncher(t)
			access := l.access("thread")
			_, page := l.open(access)
			targetID := "T-" + page
			if popup {
				page, targetID = l.popup(page, "https://example.test/popup")
				l.awaitTab(access.ThreadID, page, true)
				l.settledEngine()
			}
			reportDownload(t, l.engine, page, snapshot("dl-1", webview2host.DownloadRunning, 10))
			l.awaitDownloads(access, downloadStateRunning)
			l.engine.targetDestroyed(l.engine.currentTargets(), target.EventTargetDestroyed{TargetID: target.ID(targetID)})
			// Long enough for an early retire to settle the row canceled.
			time.Sleep(50 * time.Millisecond)
			reportDownload(t, l.engine, page, snapshot("dl-1", webview2host.DownloadCompleted, 100))
			l.report(page, webview2host.ReportClosed, "")
			l.awaitTabGone(access.ThreadID, page)
			l.awaitDownloads(access, downloadStateCompleted)
			l.engine.mu.Lock()
			waiting := len(l.engine.closeFallbacks)
			l.engine.mu.Unlock()
			if waiting != 0 {
				t.Fatalf("the engine still waits for %d closes", waiting)
			}
		})
	}
}

// A popup that closes while its attach is under way ends the attach at
// once, and what it downloaded settles in its opener's tab; an attach that
// never answers ends at the engine's attach bound.
func TestHostedPopupAttachEndsAtItsCloseOrItsBound(t *testing.T) {
	t.Run("closed", func(t *testing.T) {
		l := newPopupLauncher(t)
		access := l.access("thread")
		_, opener := l.open(access)
		release := l.gate("PT-1")
		defer release()
		popupID, _ := l.popup(opener, "https://example.test/popup")
		l.awaitAttach("PT-1")
		reportDownload(t, l.engine, popupID, snapshot("dl-1", webview2host.DownloadRunning, 10))
		l.report(popupID, webview2host.ReportClosed, "")
		// The attach is still held: only its cancel ends the adoption,
		// which discards the popup.
		l.awaitTab(access.ThreadID, popupID, false)
		l.awaitDownloads(access, downloadStateCanceled)
		if got := l.pageDownloads(access.ThreadID, opener); !slices.Equal(got, []string{downloadStateCanceled}) {
			t.Fatalf("the opener's tab lists %v", got)
		}
		l.settledEngine()
	})
	t.Run("bound", func(t *testing.T) {
		l := newPopupLauncher(t)
		l.engine.attachTimeout = 200 * time.Millisecond
		access := l.access("thread")
		_, opener := l.open(access)
		release := l.gate("PT-1")
		defer release()
		popupID, _ := l.popup(opener, "https://example.test/popup")
		l.awaitAttach("PT-1")
		l.awaitTab(access.ThreadID, popupID, false)
		l.settledEngine()
	})
}

// A download's terminal snapshot reaches the popup's tab however many
// snapshots came before it while the popup was adopted.
func TestTerminalDownloadSurvivesHeldReportBound(t *testing.T) {
	l := newPopupLauncher(t)
	access := l.access("thread")
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
func TestNestedHostedPopupBeforeParentListed(t *testing.T) {
	l := newPopupLauncher(t)
	access := l.access("thread")
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

// A download's end still being applied when its page's close arrives
// reaches the Manager first: the close is acknowledged at once, the row
// stays running while the path lookup runs, and ends completed, never
// canceled for the close, whether the popup is listed or still being
// adopted.
func TestHostedPopupCloseWaitsForTerminalProgress(t *testing.T) {
	for _, listed := range []bool{false, true} {
		name := "during-adoption"
		if listed {
			name = "after-listing"
		}
		t.Run(name, func(t *testing.T) {
			l := newPopupLauncher(t)
			access := l.access("thread")
			_, opener := l.open(access)
			entered, releasePath := make(chan struct{}), make(chan struct{})
			l.engine.backendPath = func(ctx context.Context, _ string) (string, error) {
				close(entered)
				select {
				case <-releasePath:
				case <-ctx.Done():
				}
				return "", errors.New("no test download file")
			}
			releaseAttach := func() {}
			if !listed {
				releaseAttach = l.gate("PT-1")
				defer releaseAttach()
			}
			popup, _ := l.popup(opener, "https://example.test/popup")
			if listed {
				l.awaitTab(access.ThreadID, popup, true)
				l.settledEngine()
			} else {
				l.awaitAttach("PT-1")
			}
			reportDownload(t, l.engine, popup, snapshot("dl-1", webview2host.DownloadRunning, 10))
			terminal := make(chan struct{})
			go func() {
				defer close(terminal)
				reportDownload(t, l.engine, popup, snapshot("dl-1", webview2host.DownloadCompleted, 100))
			}()
			<-entered
			closed := make(chan struct{})
			go func() {
				defer close(closed)
				l.report(popup, webview2host.ReportClosed, "")
			}()
			waitClosed(t, closed, "the close's acknowledgment while the terminal snapshot is applied")
			time.Sleep(50 * time.Millisecond)
			// A popup still being adopted has its download parked, unlisted.
			for _, row := range l.manager.CompanionState(access.Caller()).Downloads {
				if row.State != downloadStateRunning {
					t.Errorf("the close passed the pending terminal snapshot: row %+v", row)
				}
			}
			close(releasePath)
			<-terminal
			releaseAttach()
			l.awaitDownloads(access, downloadStateCompleted)
		})
	}
}
