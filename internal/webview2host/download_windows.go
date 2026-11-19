//go:build windows

package webview2host

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The launcher's half of a pane page's downloads (download.go). Every
// function here runs on the UI thread: WebView2 raises DownloadStarting and
// the operation's events there, and directives reach it through OnMain.

// hostDownload is one download a page started, from its DownloadStarting
// until it ends or its page goes.
type hostDownload struct {
	id    string
	page  *hostPage
	op    *iCoreWebView2DownloadOperation
	bytes *iDownloadEventHandler
	state *iDownloadEventHandler
	// bytesToken and stateToken remove the two subscriptions.
	bytesToken, stateToken eventRegistrationToken
	gate                   progressGate
	// uri and path are the snapshot's address and file; received and total
	// its bytes as last read from an operation.
	uri             downloadAddress
	path            string
	received, total int64
	// cancelRequested is the backend's cancel, applied to a resume too.
	// cancelBound numbers the cancels that started a bound (settleCancel).
	cancelRequested bool
	cancelBound     uint64
	// cancel is what came of the backend's last cancel attempt; every
	// snapshot carries it.
	cancel CancelOutcome
	// snapshots counts the snapshots taken, numbering each (Seq).
	snapshots uint64
	// stale holds the operations resumes replaced (retireOperation).
	stale []downloadOp
}

// newDownloadToken is the part of a download id unique to one host.
func newDownloadToken() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on Windows; a fixed token only loses
		// uniqueness across launcher runs.
		return "host"
	}
	return hex.EncodeToString(b[:])
}

// downloadID is the launcher's id for its n-th download: host is a token
// unique to the host, so no two launcher runs reuse an id.
func downloadID(host string, n uint64) string {
	return "dl-" + host + "-" + strconv.FormatUint(n, 10)
}

// watchDownloads takes every download the page starts, so WebView2's own
// download dialog never opens for it. A runtime without DownloadStarting
// keeps WebView2's dialog and the pane lists nothing.
func (h *Host) watchDownloads(page *hostPage, view *iCoreWebView2) error {
	view4 := view.queryWebView4()
	if view4 == nil {
		return errors.New("the runtime has no DownloadStarting")
	}
	defer view4.release()
	handler := newDownloadStartingHandler(func(args *iDownloadStartingEventArgs) { h.downloadStarting(page, args) })
	page.downloadStarting = handler
	_, err := view4.addDownloadStarting(handler)
	handler.ref.release()
	if err != nil {
		return fmt.Errorf("download subscription: %w", err)
	}
	return nil
}

// downloadStarting takes one download: WebView2's dialog stays closed, the
// operation is kept under a launcher id, and the backend hears its first
// snapshot.
//
// WebView2 resumes a download whose connection dropped by starting it
// again: a new DownloadStarting with a new operation carrying the
// interruption's reason, while the old operation stays in progress, reports
// nothing more and ignores a cancel. Measured on the real runtime, the old
// operation then reads as dropped (in progress with that reason) while a
// healthy download that never resumed reads 0, and the new operation saves
// to "name (N)" whether it continues from the bytes it has or restarts.
// Such a start is the dropped download's resume when exactly one running
// download from its address reads as dropped (resumedDownload): it takes
// the old operation's place under the same id, saving to the file the
// download started with, so one click is one row, one file and one end.
// Otherwise it is a new download saving where WebView2 chose.
func (h *Host) downloadStarting(page *hostPage, args *iDownloadStartingEventArgs) {
	if args == nil {
		return
	}
	if err := args.putHandled(true); err != nil {
		h.config.Logf("browser host: page %s: take a download: %v", page.id, err)
	}
	op, err := args.downloadOperation()
	if err != nil {
		h.config.Logf("browser host: page %s: a download has no operation: %v", page.id, err)
		return
	}
	rawURI, err := op.uri()
	if err != nil {
		// Reported without an address, and never taken for a resume.
		h.config.Logf("browser host: page %s: a starting download: %v", page.id, err)
	}
	path, uri := args.resultFilePath(), newDownloadAddress(rawURI)
	reason, err := op.interruptReason()
	if err != nil {
		// Read as no resume: a download of its own, nothing running touched.
		h.config.Logf("browser host: page %s: a starting download: %v", page.id, err)
	}
	if reason != 0 {
		if d := page.resumedDownload(uri); d != nil {
			h.resumeDownload(d, args, op, path, reason)
			return
		}
	}
	if page.popupDownloadRefused() {
		h.config.Logf("browser host: page %s: cancel a download past the %d a popup may start before it is reported", page.id, maxHeldPopupDownloads)
		if err := op.cancel(); err != nil {
			h.config.Logf("browser host: page %s: cancel a download: %v", page.id, err)
		}
		op.release()
		return
	}
	h.downloadSeq++
	d := &hostDownload{id: downloadID(h.downloadToken, h.downloadSeq), page: page, path: path, uri: uri}
	h.attachOperation(d, op)
	if page.downloads == nil {
		page.downloads = make(map[string]*hostDownload)
	}
	page.downloads[d.id] = d
	h.report(page, d.snapshot())
	// One that ended before its subscriptions were made reports nothing
	// more on its own.
	h.reconcileDownload(d, sampleChange)
}

// resumedDownload answers the running download a resume from uri
// continues: the one running download from uri whose operation reads as
// dropped, in progress with a nonzero interrupt reason. A download that
// never resumed reads 0 while healthy, so it is never taken. A resume names
// nothing else of its download, so where none or several read as dropped
// it answers nil and the resume is a download of its own, every running
// download left as it is. Several read so when two downloads from one
// address dropped at once, or when one dropped beside a download that
// already resumed once, whose operation (a resume's) reads its reason for
// good, dropped or not. An operation from uri whose state cannot be read
// makes the answer as uncertain, so it is nil too, and a resume or a
// download whose address the runtime did not answer matches nothing.
func (p *hostPage) resumedDownload(uri downloadAddress) *hostDownload {
	var dropped *hostDownload
	for _, d := range p.downloads {
		if d.op == nil || !d.uri.same(uri) {
			continue
		}
		state, err := d.op.state()
		if err != nil {
			return nil
		}
		reason, err := d.op.interruptReason()
		if err != nil {
			return nil
		}
		if state != downloadStateInProgress || reason == 0 {
			continue
		}
		if dropped != nil {
			return nil
		}
		dropped = d
	}
	return dropped
}

// resumeDownload makes op, which WebView2 started to resume d after an
// interruption for reason, d's operation. It saves to d's file: WebView2
// chose path while d's own file name was still taken, and on the real
// runtime a resume set back to it completes there byte for byte, whether
// it continues from the bytes it has or restarts. Should the runtime refuse
// the path, the download names the file it does write.
func (h *Host) resumeDownload(d *hostDownload, args *iDownloadStartingEventArgs, op *iCoreWebView2DownloadOperation, path string, reason uint32) {
	h.config.Logf("browser host: download %s: WebView2 resumes it after %s", d.id, downloadInterruptReason(reason))
	if !strings.EqualFold(path, d.path) {
		if err := args.putResultFilePath(d.path); err != nil {
			h.config.Logf("browser host: download %s: keep its file %s: %v", d.id, d.path, err)
			d.path = path
		}
	}
	h.retireOperation(d)
	h.attachOperation(d, op)
	if d.cancelRequested && !h.cancelDownload(d, d.cancel.Attempt()) {
		return
	}
	h.reconcileDownload(d, sampleChange)
}

// snapshot is the running download's whole state as a report, numbered
// after the last one taken.
func (d *hostDownload) snapshot() DownloadReport {
	return d.take(DownloadRunning, "")
}

// take is the download's whole state as a report in state, with reason,
// numbered after the last one taken: snapshot's running one, or the
// terminal one endDownload makes from an end verdict.
func (d *hostDownload) take(state, reason string) DownloadReport {
	if d.op != nil {
		d.received, d.total = d.op.bytesReceived(), d.op.totalBytes()
	}
	d.snapshots++
	return DownloadReport{
		ID: d.id, Seq: d.snapshots, State: state, URI: d.uri.shown, Name: windowsFileName(d.path), Path: d.path,
		Received: d.received, Total: d.total, Reason: reason, Cancel: d.cancel,
	}
}

// windowsFileName is the last element of a Windows path.
func windowsFileName(path string) string {
	return path[strings.LastIndexAny(path, `\/`)+1:]
}

// downloadOp is one operation a download had, with its subscriptions.
type downloadOp struct {
	op                     *iCoreWebView2DownloadOperation
	bytes, state           *iDownloadEventHandler
	bytesToken, stateToken eventRegistrationToken
}

// retireOperation stops d's operation from reporting and keeps it until d
// ends: a resume's operation shares the download with the one it replaces,
// and letting the old one go meanwhile stops the new one's state reports
// (measured on the real runtime).
func (h *Host) retireOperation(d *hostDownload) {
	if d.op == nil {
		return
	}
	d.bytes.changed.detach()
	d.state.changed.detach()
	d.stale = append(d.stale, downloadOp{op: d.op, bytes: d.bytes, state: d.state, bytesToken: d.bytesToken, stateToken: d.stateToken})
	d.op = nil
}

// attachOperation makes op d's operation, subscribed to its bytes and state.
// d takes the caller's reference.
func (h *Host) attachOperation(d *hostDownload, op *iCoreWebView2DownloadOperation) {
	d.op = op
	d.bytes = newDownloadEventHandler(func() { h.downloadBytes(d) })
	var err error
	if d.bytesToken, err = op.addBytesReceivedChanged(d.bytes); err != nil {
		h.config.Logf("browser host: download %s progress subscription: %v", d.id, err)
	}
	d.bytes.ref.release()
	d.state = newDownloadEventHandler(func() { h.reconcileDownload(d, sampleChange) })
	if d.stateToken, err = op.addStateChanged(d.state); err != nil {
		h.config.Logf("browser host: download %s state subscription: %v", d.id, err)
	}
	d.state.ref.release()
}

// detachOperation lets d's operation go: both subscriptions removed, their
// callbacks detached, the operation released.
func (h *Host) detachOperation(d *hostDownload) {
	if d.op == nil {
		return
	}
	if err := d.op.removeBytesReceivedChanged(d.bytesToken); err != nil {
		h.config.Logf("browser host: download %s: remove progress subscription: %v", d.id, err)
	}
	if err := d.op.removeStateChanged(d.stateToken); err != nil {
		h.config.Logf("browser host: download %s: remove state subscription: %v", d.id, err)
	}
	d.bytes.changed.detach()
	d.state.changed.detach()
	d.op.release()
	d.op = nil
}

// downloadBytes reports a download's snapshot as its bytes change, at most
// once per DownloadProgressInterval.
func (h *Host) downloadBytes(d *hostDownload) {
	if d.op == nil || !d.gate.due(time.Now()) {
		return
	}
	h.report(d.page, d.snapshot())
}

// report sends one snapshot of a page's download.
func (h *Host) report(page *hostPage, r DownloadReport) {
	if h.holdDownload(page, r) {
		return
	}
	detail, err := r.Encode()
	if err != nil {
		h.config.Logf("browser host: download %s: %v", r.ID, err)
		return
	}
	h.config.Report(page.id, ReportDownload, detail)
}

// endDownload ends a download as reconcileDownload's end verdict says:
// every operation it had is let go, its terminal snapshot is kept for a
// directive that names the download later, and the backend hears it.
func (h *Host) endDownload(d *hostDownload, end downloadVerdict) {
	if end.kind != verdictEnd {
		panic("webview2host: a download ended by a verdict that keeps it")
	}
	terminal := d.take(end.state, end.reason)
	h.releaseOperations(d)
	delete(d.page.downloads, d.id)
	d.page.ended.add(terminal)
	h.report(d.page, terminal)
	if d.page.closeAfterDownloads && len(d.page.downloads) == 0 {
		// The page asked to close while this ran (windowCloseRequested).
		h.closeAsked(d.page)
	}
}

// releaseOperations lets go of every operation d had.
func (h *Host) releaseOperations(d *hostDownload) {
	h.detachOperation(d)
	for _, stale := range d.stale {
		d.op, d.bytes, d.state, d.bytesToken, d.stateToken = stale.op, stale.bytes, stale.state, stale.bytesToken, stale.stateToken
		h.detachOperation(d)
	}
	d.stale = nil
}

// retireDownloads cancels and lets go every download of a page that is
// going, with every terminal snapshot it kept. The backend settles them as
// its page leaves; nothing is reported.
func (h *Host) retireDownloads(page *hostPage) {
	if page.downloadStarting != nil {
		page.downloadStarting.starting.detach()
	}
	for _, d := range page.downloads {
		if d.op != nil {
			if err := d.op.cancel(); err != nil {
				h.config.Logf("browser host: cancel download %s of a closing page: %v", d.id, err)
			}
		}
		h.releaseOperations(d)
	}
	page.downloads = nil
	page.ended = endedDownloads{}
}
