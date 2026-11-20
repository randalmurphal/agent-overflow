package browser

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/webview2host"
)

// Downloads on the hosted engine are the launcher's: it takes every download
// a pane page starts, so WebView2's dialog does not open for it, lets
// WebView2 save it to the Windows user's Downloads folder, and reports each
// one as whole snapshots (ReportDownload, internal/webview2host/download.go),
// each numbered in the order the launcher took it. Report receives each
// under its page and id (hostedPages.acceptSnapshot) and its page's lane
// applies it (applySnapshot): one download's snapshots one at a time, each
// only if it is newer than the last it applied. A snapshot of a download
// it has no row for starts the row (a lost first report heals on the
// next), a repeat or an older one changes nothing, and nothing after a
// terminal snapshot reopens it. The Manager's cancel is OpCancelDownload,
// and a running download the launcher has not reported for
// hostedDownloadQuiet is asked about with OpDownloadStatus, so a lost end
// settles with no one acting. Both are answered with a snapshot marked as
// an answer, which settles or advances a row and never creates one;
// DownloadUnknown, the answer for an id the launcher has no record of,
// ends only a row still running. Each cancel is numbered, and every
// snapshot after it names the last the launcher took and WebView2's
// refusal of it, which withdraws the Manager's cancel
// (downloadCancelRefused) on whichever snapshot first carries it;
// a status question names the last cancel sent, so the launcher makes one
// it never got. Nothing about downloads crosses CDP. A popup is a pane page
// too: a download it starts before the Manager lists its tab names it and
// its openers, for the Manager to list (popup_parked.go). A page admits
// downloads from its bind or report until its close or discard is
// received, and one still running when its lane retires it ends there
// (retirePage).

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

// acceptDownload receives one launcher snapshot of a download of page
// (hostedPages.acceptSnapshot). A report naming a download id another page
// owns is refused and logged.
func (e *hostedEngine) acceptDownload(page ids.PageHandle, detail string) {
	report, err := webview2host.ParseDownloadReport(detail)
	if err != nil {
		e.logf("browser: pane host download report for page %s: %v", page, err)
		return
	}
	receipt := e.pages.acceptSnapshot(page, report)
	switch receipt {
	case snapshotAccepted, snapshotNoPage, snapshotEnded, snapshotUnasked, snapshotSealed:
		// A page no record has went or was never this engine's, and a
		// sealed page's downloads the launcher cancels itself.
	case snapshotOtherPage:
		e.logf("browser: pane host page %s reported download %s, which another page owns; refused", page, report.ID)
	default:
		panic(fmt.Sprintf("browser: snapshot receipt %d", receipt))
	}
}

// applySnapshot applies one received snapshot of download d, on its page's
// lane. A finished download's path conversion ends with ctx (Stop).
func (e *hostedEngine) applySnapshot(ctx context.Context, report webview2host.DownloadReport, d *hostedDownload) {
	// The page is the frame its downloads name (hostedPage.OwnsFrame).
	frame, id := frameID(d.key.page), d.key.id
	live, admission, cancels := e.pages.live(d, report.Cancel.Attempt())
	if !live {
		// Ended, refused, or its page retired, before this snapshot's turn.
		return
	}
	admitted := admission == hostedAdmitted
	if report.State == webview2host.DownloadUnknown {
		// Created by no answer, so the download is admitted. The launcher
		// lost it, so how it ended is unknown.
		e.events.DownloadProgress(downloadProgress{ID: id, State: downloadUntracked, Error: report.Reason})
		e.pages.end(d)
		return
	}
	if report.Seq <= d.applied {
		return
	}
	if refusal, refused := report.Cancel.Refusal(); refused && admitted && report.Cancel.Attempt() == cancels && d.refused != cancels {
		// Every snapshot after a refusal carries it, so the newest applied
		// has it: the Manager's cancel stands until the refusal of the last
		// attempt sent arrives, and hears it once.
		d.refused = cancels
		e.events.DownloadProgress(downloadProgress{ID: id, State: downloadCancelRefused, Error: refusal})
	}
	if !admitted {
		name := report.Name
		if name == "" {
			name = windowsBase(report.Path)
		}
		start := e.pages.popupStart(d.key.page)
		start.Frame, start.ID, start.URL, start.SuggestedName = frame, id, report.URI, name
		if e.events.DownloadStarted(start) != downloadAdmitted {
			// The Manager refused it: nothing later reaches a row.
			e.pages.end(d)
			if !report.Terminal() {
				if err := e.sendDownloadCancel(d); err != nil {
					e.logf("%v", err)
				}
			}
			return
		}
		if e.pages.admit(d) {
			// No caller waits on this cancel any more.
			if err := e.sendDownloadCancel(d); err != nil {
				e.logf("%v", err)
			}
		}
	}
	d.applied = report.Seq
	progress := downloadProgress{ID: id, Received: float64(report.Received), Total: float64(report.Total), State: downloadInProgress}
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
		pathCtx, cancel := context.WithTimeout(ctx, hostedPathTimeout)
		path, err := e.backendPath(pathCtx, report.Path)
		cancel()
		if ctx.Err() != nil {
			// The engine stopped; the Manager settled the page's rows first.
			return
		}
		if err != nil {
			progress.Error = fmt.Sprintf("the file was saved to %s, which this backend cannot reach, so it is not opened from here: %v", report.Path, err)
		} else {
			progress.FilePath = path
		}
	case webview2host.DownloadInterrupted:
		progress.State, progress.Error = hostedDownloadEnd(report.End), report.Reason
	}
	e.events.DownloadProgress(progress)
	if report.Terminal() {
		e.pages.end(d)
	}
}

// hostedDownloadEnd is the state of a download the launcher ended for end,
// which ParseDownloadReport admits only as one of these.
func hostedDownloadEnd(end webview2host.DownloadEnd) downloadState {
	switch end {
	case webview2host.DownloadEndCanceled:
		return downloadCanceled
	case webview2host.DownloadEndFailed:
		return downloadFailed
	case webview2host.DownloadEndUntracked:
		return downloadUntracked
	case webview2host.DownloadEndPageGone:
		return downloadPageGone
	}
	panic(fmt.Sprintf("browser: the pane host ended a download for %q", end))
}

// cancelDownload asks the launcher to cancel a running download; its answer
// settles it. One the engine no longer has has ended, and its terminal
// report is out. The Manager lists a download's row before its admission
// answers, so a cancel can come while it is in flight: it is kept, and sent
// as the admission ends (applySnapshot).
func (e *hostedEngine) cancelDownload(id downloadID) error {
	d, admitted, ok := e.pages.cancel(id)
	if !ok {
		return errDownloadEnded
	}
	if !admitted {
		return nil
	}
	return e.sendDownloadCancel(d)
}

// sendDownloadCancel sends the launcher the cancel of d, numbered after the
// last one sent. A cancel that could not be sent gives its number back, so
// no status question makes it later.
func (e *hostedEngine) sendDownloadCancel(d *hostedDownload) error {
	attempt := e.pages.nextCancel(d)
	if !e.dispatch(webview2host.Directive{Op: webview2host.OpCancelDownload, PageID: string(d.key.page), Download: string(d.key.id), Cancel: attempt}) {
		e.pages.unsent(d, attempt)
		return fmt.Errorf("browser: the pane host refused to cancel download %s", d.key.id)
	}
	return nil
}

// askQuietDownloads is the engine's one status loop: each interval it asks
// the launcher for the snapshot of every running download it has not heard
// from for that long, or whose last cancel no snapshot has named yet
// (hostedPages.quiet). It ends with the last running download, or at Stop.
func (e *hostedEngine) askQuietDownloads(stop <-chan struct{}) {
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
		ask, keep := e.pages.quiet(stop, time.Now(), interval)
		if !keep {
			return
		}
		for _, directive := range ask {
			e.dispatch(directive)
		}
	}
}

// retirePage ends page on its lane, after every report received before its
// close or discard: the engine forgets it, ends its downloads the Manager
// admitted that still run as canceled for why (the launcher cancels a
// going page's downloads and reports nothing of them), and, for the
// launcher's close, tells the Manager the page is gone. A listed page's
// rows the Manager ended as it closed the page take nothing from this; a
// popup's it never listed settle here. Whoever waits for the page to go
// (DiscardPage) is woken last.
func (e *hostedEngine) retirePage(page ids.PageHandle, why string, closed bool) {
	running, finish := e.pages.retire(page)
	defer finish()
	pageID := page
	e.mu.Lock()
	e.endCloseFallbackLocked(pageID)
	cancelAttach := e.forgetPopupLocked(pageID)
	e.forgetPageLocked(pageID)
	e.mu.Unlock()
	cancelAttach()
	for _, id := range running {
		e.events.DownloadProgress(downloadProgress{ID: id, State: downloadPageGone, Error: why})
	}
	if closed {
		e.events.PageClosed(pageID)
	}
}

// errDiscardPending is DiscardPage's answer when its caller's context ended
// before the page's lane retired the page: the discard stands, and the
// lane finishes it after the reports received before it.
var errDiscardPending = errors.New("the discard is still pending behind the page's received reports, which finish it")

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
