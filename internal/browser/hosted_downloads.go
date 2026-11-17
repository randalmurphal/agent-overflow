package browser

import (
	"context"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/webview2host"
)

// Downloads on the hosted engine are the launcher's: it takes every download
// a pane page starts, so WebView2's own dialog never opens, lets WebView2
// save it to the Windows user's Downloads folder, and reports its life as
// ReportDownload (internal/webview2host/download.go). This engine turns
// those reports into the Manager's download events and its cancel into
// OpCancelDownload; nothing about downloads crosses CDP. A download started
// in a window the launcher does not own (a popup WebView2 opened itself) is
// never reported and keeps WebView2's own handling.

// hostedPathTimeout bounds the `wslpath -u` of a finished download's path.
const hostedPathTimeout = 10 * time.Second

// downloadReport routes one launcher download report of page pageID.
func (e *hostedEngine) downloadReport(pageID, detail string) {
	report, err := webview2host.ParseDownloadReport(detail)
	if err != nil {
		e.logf("browser: pane host download report for page %s: %v", pageID, err)
		return
	}
	switch report.State {
	case webview2host.DownloadStarted:
		e.mu.Lock()
		_, live := e.targetByPage[pageID]
		if live {
			e.downloads[report.ID] = pageID
		}
		e.mu.Unlock()
		allowed := live && e.events.DownloadStarted(downloadStart{
			Frame: pageID, ID: report.ID, URL: report.URI, SuggestedName: windowsBase(report.Path),
		})
		if !allowed {
			// No page of the Manager's started it, or that page is leaving.
			// The launcher cancels a closed page's downloads itself.
			if live {
				if err := e.cancelDownload(report.ID); err != nil {
					e.logf("browser: cancel refused download %s: %v", report.ID, err)
				}
			}
			return
		}
		if report.Received > 0 {
			e.events.DownloadProgress(downloadProgress{ID: report.ID, Received: float64(report.Received), Total: float64(report.Total), State: downloadInProgress})
		}
	case webview2host.DownloadProgress:
		e.events.DownloadProgress(downloadProgress{ID: report.ID, Received: float64(report.Received), Total: float64(report.Total), State: downloadInProgress})
	case webview2host.DownloadCompleted:
		e.forgetDownload(report.ID)
		progress := downloadProgress{ID: report.ID, Received: float64(report.Received), Total: float64(report.Total), State: downloadCompleted}
		ctx, cancel := context.WithTimeout(context.Background(), hostedPathTimeout)
		path, err := e.backendPath(ctx, report.Path)
		cancel()
		if err != nil {
			progress.State, progress.Error = downloadCanceled, fmt.Sprintf("the download was saved to %s, which this backend cannot reach: %v", report.Path, err)
		} else {
			progress.FilePath = path
		}
		e.events.DownloadProgress(progress)
	case webview2host.DownloadInterrupted:
		e.forgetDownload(report.ID)
		e.events.DownloadProgress(downloadProgress{ID: report.ID, Received: float64(report.Received), Total: float64(report.Total), State: downloadCanceled, Error: report.Reason})
	}
}

// cancelDownload asks the launcher to cancel a running download. One the
// engine no longer tracks has ended, and its terminal report is out or on
// the way.
func (e *hostedEngine) cancelDownload(id string) error {
	e.mu.Lock()
	pageID, ok := e.downloads[id]
	e.mu.Unlock()
	if !ok {
		return errDownloadEnded
	}
	if !e.dispatch(webview2host.Directive{Op: webview2host.OpCancelDownload, PageID: pageID, Download: id}) {
		return fmt.Errorf("browser: the pane host refused to cancel download %s", id)
	}
	return nil
}

func (e *hostedEngine) forgetDownload(id string) {
	e.mu.Lock()
	delete(e.downloads, id)
	e.mu.Unlock()
}

// forgetDownloadsLocked drops a page's downloads as the page goes; the
// launcher cancels them with the page. The caller holds e.mu.
func (e *hostedEngine) forgetDownloadsLocked(pageID string) {
	for id, page := range e.downloads {
		if page == pageID {
			delete(e.downloads, id)
		}
	}
}

// windowsBase is the file name of a Windows path, whichever separator it
// uses.
func windowsBase(path string) string {
	return path[strings.LastIndexAny(path, `\/`)+1:]
}
