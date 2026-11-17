package browser

import (
	"context"
	"fmt"
	"time"

	"agent-overflow/internal/webview2host"
)

// Downloads on the hosted engine are the launcher's: it takes every download
// a pane page starts, so WebView2's own dialog never opens, lets WebView2
// save it to the Windows user's Downloads folder, and reports each one as
// whole snapshots (ReportDownload, internal/webview2host/download.go). This
// engine applies them idempotently: a snapshot of a download it has no row
// for starts the row (a lost first report heals on the next), a repeat
// changes nothing, and nothing after a terminal snapshot reopens it. The
// Manager's cancel is OpCancelDownload, and a running download the launcher
// has not reported for hostedDownloadQuiet is asked about with
// OpDownloadStatus, so a lost end settles with no one acting. Both are
// answered with a snapshot. Nothing about downloads crosses CDP. A
// download started in a window the launcher does not own (a popup WebView2
// opened itself) is never reported and keeps WebView2's own handling.

const (
	// hostedPathTimeout bounds the `wslpath -u` of a finished download's
	// path.
	hostedPathTimeout = 10 * time.Second
	// hostedDownloadQuiet is how long a running download may go without a
	// snapshot before the engine asks the launcher for one. The launcher
	// sends one every DownloadProgressInterval while bytes arrive, so a
	// download this quiet has stalled or its report was lost; one small
	// directive and answer per quiet download per interval is the cost,
	// and the interval bounds how long a row whose end was lost stays
	// running.
	hostedDownloadQuiet = 10 * time.Second
	// maxHostedEndedDownloads bounds the ended ids the engine keeps per
	// page, so a late snapshot of one cannot reopen its row; past it the
	// oldest goes, and a snapshot naming it would start a row again. None
	// comes: the launcher sends an ended download's snapshot only in answer
	// to this engine, which asks only about running ones.
	maxHostedEndedDownloads = webview2host.MaxEndedDownloads
)

// hostedDownloads is the engine's record of the launcher's downloads.
// Guarded by hostedEngine.mu.
type hostedDownloads struct {
	// running is each running download by the launcher's id.
	running map[string]*hostedDownload
	// ended is each page's ended download ids, oldest first.
	ended map[string][]string
	// watching is set while the status loop runs; stop ends it.
	watching bool
	stop     chan struct{}
}

type hostedDownload struct {
	page string
	// heard is when the last snapshot arrived, or the last status was asked.
	heard time.Time
}

func newHostedDownloads() hostedDownloads {
	return hostedDownloads{running: make(map[string]*hostedDownload), ended: make(map[string][]string)}
}

func (d *hostedDownloads) endedIn(page, id string) bool {
	for _, ended := range d.ended[page] {
		if ended == id {
			return true
		}
	}
	return false
}

func (d *hostedDownloads) noteEnded(page, id string) {
	delete(d.running, id)
	ended := append(d.ended[page], id)
	if len(ended) > maxHostedEndedDownloads {
		ended = ended[len(ended)-maxHostedEndedDownloads:]
	}
	d.ended[page] = ended
}

// downloadReport applies one launcher snapshot of a download of page
// pageID.
func (e *hostedEngine) downloadReport(pageID, detail string) {
	report, err := webview2host.ParseDownloadReport(detail)
	if err != nil {
		e.logf("browser: pane host download report for page %s: %v", pageID, err)
		return
	}
	e.mu.Lock()
	if e.downloads.endedIn(pageID, report.ID) {
		e.mu.Unlock()
		return
	}
	known := e.downloads.running[report.ID]
	if known == nil {
		if _, live := e.targetByPage[pageID]; !live {
			// No page of the Manager's started it, or that page is leaving;
			// the launcher cancels a closed page's downloads itself.
			e.mu.Unlock()
			return
		}
		known = &hostedDownload{page: pageID}
		e.downloads.running[report.ID] = known
		e.watchDownloadsLocked()
	}
	fresh := known.heard.IsZero()
	known.heard = time.Now()
	if report.Terminal() {
		e.downloads.noteEnded(pageID, report.ID)
	}
	e.mu.Unlock()

	if fresh {
		name := report.Name
		if name == "" {
			name = windowsBase(report.Path)
		}
		if !e.events.DownloadStarted(downloadStart{Frame: pageID, ID: report.ID, URL: report.URI, SuggestedName: name}) {
			// The Manager refused it: nothing later reaches a row.
			e.mu.Lock()
			e.downloads.noteEnded(pageID, report.ID)
			e.mu.Unlock()
			if !report.Terminal() {
				e.dispatch(webview2host.Directive{Op: webview2host.OpCancelDownload, PageID: pageID, Download: report.ID})
			}
			return
		}
	}
	progress := downloadProgress{ID: report.ID, Received: float64(report.Received), Total: float64(report.Total), State: downloadInProgress}
	switch report.State {
	case webview2host.DownloadRunning:
		if fresh && report.Received == 0 {
			return
		}
	case webview2host.DownloadCompleted:
		progress.State = downloadCompleted
		if report.Path == "" {
			// Saved, under a path the launcher could not report.
			progress.Error = report.Reason
			break
		}
		ctx, cancel := context.WithTimeout(context.Background(), hostedPathTimeout)
		path, err := e.backendPath(ctx, report.Path)
		cancel()
		if err != nil {
			progress.Error = fmt.Sprintf("the file was saved to %s, which this backend cannot reach, so it is not opened from here: %v", report.Path, err)
		} else {
			progress.FilePath = path
		}
	case webview2host.DownloadInterrupted:
		progress.State, progress.Error = downloadCanceled, report.Reason
	}
	e.events.DownloadProgress(progress)
}

// cancelDownload asks the launcher to cancel a running download; its answer
// settles it. One the engine no longer tracks has ended, and its terminal
// report is out.
func (e *hostedEngine) cancelDownload(id string) error {
	e.mu.Lock()
	known := e.downloads.running[id]
	page := ""
	if known != nil {
		page = known.page
	}
	e.mu.Unlock()
	if known == nil {
		return errDownloadEnded
	}
	if !e.dispatch(webview2host.Directive{Op: webview2host.OpCancelDownload, PageID: page, Download: id}) {
		return fmt.Errorf("browser: the pane host refused to cancel download %s", id)
	}
	return nil
}

// watchDownloadsLocked starts the status loop unless it runs. The caller
// holds e.mu.
func (e *hostedEngine) watchDownloadsLocked() {
	if e.downloads.watching {
		return
	}
	e.downloads.watching = true
	if e.downloads.stop == nil {
		e.downloads.stop = make(chan struct{})
	}
	go e.askQuietDownloads(e.downloads.stop)
}

// askQuietDownloads is the engine's one status loop: each interval it asks
// the launcher for the snapshot of every running download it has not heard
// from for that long. It ends with the last running download, or at Stop.
func (e *hostedEngine) askQuietDownloads(stop chan struct{}) {
	interval := e.downloadQuiet
	if interval <= 0 {
		interval = hostedDownloadQuiet
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		now := time.Now()
		var ask []webview2host.Directive
		e.mu.Lock()
		if e.downloads.stop != stop {
			// Stop ended this loop and a later download started another.
			e.mu.Unlock()
			return
		}
		if len(e.downloads.running) == 0 {
			e.downloads.watching = false
			e.mu.Unlock()
			return
		}
		for id, d := range e.downloads.running {
			if now.Sub(d.heard) >= interval {
				d.heard = now
				ask = append(ask, webview2host.Directive{Op: webview2host.OpDownloadStatus, PageID: d.page, Download: id})
			}
		}
		e.mu.Unlock()
		for _, directive := range ask {
			e.dispatch(directive)
		}
	}
}

// forgetDownloadsLocked drops a page's downloads as the page goes; the
// launcher cancels them with the page. The caller holds e.mu.
func (e *hostedEngine) forgetDownloadsLocked(pageID string) {
	for id, d := range e.downloads.running {
		if d.page == pageID {
			delete(e.downloads.running, id)
		}
	}
	delete(e.downloads.ended, pageID)
}

// stopDownloadsLocked forgets every download and ends the status loop
// (Stop). The caller holds e.mu.
func (e *hostedEngine) stopDownloadsLocked() {
	if e.downloads.stop != nil {
		close(e.downloads.stop)
	}
	e.downloads = newHostedDownloads()
}

// windowsBase is the file name of a Windows path, whichever separator it
// uses.
func windowsBase(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '\\' || path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}
