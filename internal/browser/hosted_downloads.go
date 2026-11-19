package browser

import (
	"context"
	"fmt"
	"sync"
	"time"

	"agent-overflow/internal/webview2host"
)

// Downloads on the hosted engine are the launcher's: it takes every download
// a pane page starts, so WebView2's dialog does not open for it, lets
// WebView2 save it to the Windows user's Downloads folder, and reports each
// one as whole snapshots (ReportDownload, internal/webview2host/download.go),
// each numbered in the order the launcher took it. Reports arrive on RPC
// goroutines of their own, so they can overtake each other; this engine
// applies one download's snapshots one at a time (hostedDownload.apply,
// held across the Manager's callbacks), each only if it is newer than the
// last it applied: a snapshot of a download it has no row for starts the
// row (a lost first report heals on the next), a repeat or an older one
// changes nothing, and nothing after a terminal snapshot reopens it. The
// Manager's cancel is OpCancelDownload, and a running download the launcher
// has not reported for hostedDownloadQuiet is asked about with
// OpDownloadStatus, so a lost end settles with no one acting. Both are
// answered with a snapshot marked as an answer, which settles or advances a
// row and never creates one; DownloadUnknown, the answer for an id the
// launcher has no record of, ends only a row still running, and a running
// answer with a reason is a cancel WebView2 refused, which withdraws the
// Manager's cancel (downloadProgress.CancelRefused). Nothing about
// downloads crosses CDP. A popup is a pane page too: a download it starts
// before the Manager lists its tab names it and its openers, for the
// Manager to list (popup_adoption.go), and one still running when such a
// popup goes ends here (joinDownloadReports).

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
	// page, so a late snapshot of one cannot reopen its row. Past it the
	// oldest goes: an answer naming it still creates nothing, and only a
	// report the launcher sent unasked, delayed past this many later ends
	// of the page's downloads, would start its row again.
	maxHostedEndedDownloads = webview2host.MaxEndedDownloads
)

// hostedDownloads is the engine's record of the launcher's downloads.
// Guarded by hostedEngine.mu.
type hostedDownloads struct {
	// running is each download by the launcher's id from its first snapshot
	// until it ends or is refused, admitted or with its admission in flight.
	running map[string]*hostedDownload
	// ended is each page's ended or refused download ids, oldest first.
	ended map[string][]string
	// watching is set while the status loop runs; stop ends it.
	watching bool
	stop     chan struct{}
}

type hostedDownload struct {
	page string
	// heard is when the last snapshot arrived, or the last status was asked.
	heard time.Time
	// admitted is set once the Manager took the download: only then is it
	// known to the Manager, to cancel or to ask about. gone is set as it
	// leaves running (ended, refused, or its page or the engine went).
	admitted, gone bool
	// apply is held while one snapshot is applied, across the Manager's
	// callbacks, so the download's snapshots reach the Manager one at a time.
	apply sync.Mutex
	// applied is the number of the last snapshot applied. Guarded by apply.
	applied uint64
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
	if known := d.running[id]; known != nil {
		known.gone = true
	}
	delete(d.running, id)
	ended := append(d.ended[page], id)
	if len(ended) > maxHostedEndedDownloads {
		ended = ended[len(ended)-maxHostedEndedDownloads:]
	}
	d.ended[page] = ended
}

// downloadReport applies one launcher snapshot of a download of page
// pageID, in the download's apply section.
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
		if report.Answer {
			// An answer settles or advances a row and never creates one: the
			// engine asks only about downloads it has, so this one ended here
			// and was forgotten, or its page went.
			e.mu.Unlock()
			return
		}
		_, live := e.targetByPage[pageID]
		if _, pending := e.popups[pageID]; !live && !pending {
			// No page of the Manager's started it, or that page is leaving;
			// the launcher cancels a closed page's downloads itself.
			e.mu.Unlock()
			return
		}
		known = &hostedDownload{page: pageID}
		e.downloads.running[report.ID] = known
		e.watchDownloadsLocked()
	}
	known.heard = time.Now()
	e.mu.Unlock()

	known.apply.Lock()
	defer known.apply.Unlock()
	e.mu.Lock()
	gone, admitted := known.gone, known.admitted
	e.mu.Unlock()
	if gone {
		// Ended or refused while this snapshot waited its turn.
		return
	}
	if report.State == webview2host.DownloadUnknown {
		// Created by no answer, so the download is admitted.
		e.events.DownloadProgress(downloadProgress{ID: report.ID, State: downloadCanceled, Error: report.Reason})
		e.mu.Lock()
		e.downloads.noteEnded(pageID, report.ID)
		e.mu.Unlock()
		return
	}
	if admitted && report.CancelRefused() {
		// Whatever its number: a newer snapshot that overtook it carries no
		// refusal.
		e.events.DownloadProgress(downloadProgress{ID: report.ID, State: downloadInProgress, Error: report.Reason, CancelRefused: true})
	}
	if report.Seq <= known.applied {
		return
	}
	if !admitted {
		name := report.Name
		if name == "" {
			name = windowsBase(report.Path)
		}
		e.mu.Lock()
		popup := e.popupOwnersLocked(pageID)
		e.mu.Unlock()
		popup.Frame, popup.ID, popup.URL, popup.SuggestedName = pageID, report.ID, report.URI, name
		if !e.events.DownloadStarted(popup) {
			// The Manager refused it: nothing later reaches a row.
			e.mu.Lock()
			e.downloads.noteEnded(pageID, report.ID)
			e.mu.Unlock()
			if !report.Terminal() {
				e.dispatch(webview2host.Directive{Op: webview2host.OpCancelDownload, PageID: pageID, Download: report.ID})
			}
			return
		}
		e.mu.Lock()
		known.admitted = true
		e.mu.Unlock()
	}
	known.applied = report.Seq
	progress := downloadProgress{ID: report.ID, Received: float64(report.Received), Total: float64(report.Total), State: downloadInProgress}
	switch report.State {
	case webview2host.DownloadRunning:
		if !admitted && report.Received == 0 {
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
		progress.PageGone = report.Reason == webview2host.PopupGoneReason
	}
	e.events.DownloadProgress(progress)
	if report.Terminal() {
		// After the Manager has its end, so a page that goes meanwhile
		// still finds the download running and waits for this snapshot
		// (joinDownloadReports).
		e.mu.Lock()
		e.downloads.noteEnded(pageID, report.ID)
		e.mu.Unlock()
	}
}

// cancelDownload asks the launcher to cancel a running download; its answer
// settles it. One the engine no longer tracks has ended, and its terminal
// report is out.
func (e *hostedEngine) cancelDownload(id string) error {
	e.mu.Lock()
	known := e.downloads.running[id]
	page, admitted := "", false
	if known != nil {
		page, admitted = known.page, known.admitted
	}
	e.mu.Unlock()
	if !admitted {
		// A download whose admission is in flight is not the Manager's yet,
		// so its cancel cannot come from there.
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
			if d.admitted && now.Sub(d.heard) >= interval {
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
			d.gone = true
			delete(e.downloads.running, id)
		}
	}
	delete(e.downloads.ended, pageID)
}

// joinDownloadReports returns once every snapshot being applied for a
// download of pageID, a page that goes, has reached the Manager, so the
// page's close reaches it after them. With why set, pageID is a popup the
// Manager never listed, and its downloads still running then end as
// canceled for why: the launcher cancels a going page's downloads and
// reports nothing, and only a listed page's leaving settles them in the
// Manager. Each ends in its apply section.
func (e *hostedEngine) joinDownloadReports(pageID, why string) {
	e.mu.Lock()
	running := make(map[string]*hostedDownload)
	for id, d := range e.downloads.running {
		if d.page == pageID {
			running[id] = d
		}
	}
	e.mu.Unlock()
	for id, d := range running {
		d.apply.Lock()
		e.mu.Lock()
		end := why != "" && d.admitted && !d.gone
		if end {
			e.downloads.noteEnded(pageID, id)
		}
		e.mu.Unlock()
		if end {
			e.events.DownloadProgress(downloadProgress{ID: id, State: downloadCanceled, Error: why, PageGone: true})
		}
		d.apply.Unlock()
	}
}

// stopDownloadsLocked forgets every download and ends the status loop
// (Stop). The caller holds e.mu.
func (e *hostedEngine) stopDownloadsLocked() {
	if e.downloads.stop != nil {
		close(e.downloads.stop)
	}
	for _, d := range e.downloads.running {
		d.gone = true
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
