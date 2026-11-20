//go:build windows

package webview2host

import "time"

// Where a download stands, read from its operation, and the backend's
// cancels. Every function here runs on the UI thread. reconcileDownload is
// the one place a download's end is decided: it reads the operation
// (observe) and carries out the verdict (downloadoutcome.go).

// downloadCancelBound is how long after the backend's cancel the launcher
// waits for WebView2 to end the download before reading it at the bound
// (settleCancel). On the real runtime WebView2 ends a running download, or
// a resume, within a few milliseconds of its cancel (4 ms and 2 ms
// measured), and a dropped download's old operation never; the bound is
// three orders of magnitude past the first and still clears a stuck row
// promptly.
const downloadCancelBound = 5 * time.Second

// observe reads d's operation.
func (h *Host) observe(d *hostDownload) downloadObservation {
	state, err := d.op.state()
	if err != nil {
		return observeUnread(err)
	}
	switch state {
	case downloadStateCompleted:
		return observeCompleted(d.op.resultFilePath())
	case downloadStateInterrupted:
		reason, err := d.op.interruptReason()
		if err != nil {
			h.config.Logf("browser host: download %s: %v", d.id, err)
		}
		return observeInterrupted(reason)
	default:
		return observeRunning()
	}
}

// reconcileDownload reads d's operation and does what the read and why
// call for (downloadObservation.verdict): keep d running, wait another
// cancel bound, or end it with its terminal snapshot. A download that went
// meanwhile is left alone.
//
// WebView2 resumes a download as a new DownloadStarting (downloadStarting),
// never as this operation coming back to in progress, and raises no
// interruption before it. Measured on the real runtime for a dropped
// connection, a body shorter than its Content-Length and a refused Range:
// the reasons that arrived were the length mismatch (24), at the end of
// WebView2's own retries, and the user's cancel; FILE_TOO_SHORT,
// FILE_HASH_MISMATCH and SERVER_NO_RANGE, which WebView2 documents as
// resumed by itself, never did.
func (h *Host) reconcileDownload(d *hostDownload, why downloadSample) {
	if d.op == nil || d.page.downloads[d.id] != d {
		return
	}
	seen := h.observe(d)
	if seen.unread != nil {
		h.config.Logf("browser host: download %s: %v", d.id, seen.unread)
	}
	verdict := seen.verdict(why)
	switch verdict.kind {
	case verdictKeep:
	case verdictWaitAnotherBound:
		h.armCancelBound(d, d.cancelBound)
	case verdictEnd:
		if verdict.path != "" {
			d.path = verdict.path
		}
		if verdict.cancelFirst {
			if err := d.op.cancel(); err != nil {
				h.config.Logf("browser host: cancel download %s as it ends: %v", d.id, err)
			}
		}
		h.endDownload(d, verdict)
	default:
		panic("webview2host: a download verdict of unknown kind")
	}
}

// answerDownload answers a directive naming one of the page's downloads
// with its snapshot, marked as an answer. The download is read first
// (reconcileDownload), and one that ended since its last report ends now,
// its terminal snapshot the answer. A cancel attempt past the last one
// taken (either directive's Cancel) cancels a running download
// (cancelDownload), and its end follows from the operation's state change,
// or from settleCancel when WebView2 reports none; an attempt already taken
// is a repeat, answered with that attempt's outcome. An ended download's
// answer is its terminal snapshot again; an id with no record gets
// DownloadUnknown.
func (h *Host) answerDownload(page *hostPage, id string, attempt CancelAttempt) {
	d := page.downloads[id]
	if d == nil {
		h.report(page, page.ended.answer(id))
		return
	}
	h.reconcileDownload(d, sampleStatus)
	if page.downloads[id] != d {
		return
	}
	if attempt > d.cancel.Attempt() && d.op != nil && !h.cancelDownload(d, attempt) {
		return
	}
	answer := d.snapshot()
	answer.Answer = true
	h.report(page, answer)
}

// cancelDownload makes the backend's cancel attempt on d's operation, kept
// for a resume that replaces the operation, and starts the bound on the
// first (settleCancel). A cancel WebView2 refuses withdraws the request, so
// no bound ends a download that runs on as canceled, and the refusal rides
// every snapshot until an attempt is taken: d is read again, and one still
// running is answered with it. It answers whether WebView2 took the cancel.
func (h *Host) cancelDownload(d *hostDownload, attempt CancelAttempt) bool {
	if err := d.op.cancel(); err != nil {
		h.config.Logf("browser host: cancel download %s: %v", d.id, err)
		d.cancelRequested = false
		d.cancel = CancelRefused(attempt, err)
		h.reconcileDownload(d, sampleStatus)
		if d.page.downloads[d.id] == d {
			refused := d.snapshot()
			refused.Answer = true
			h.report(d.page, refused)
		}
		return false
	}
	d.cancel = CancelTaken(attempt)
	if !d.cancelRequested {
		d.cancelRequested = true
		d.cancelBound++
		h.armCancelBound(d, d.cancelBound)
	}
	return true
}

// armCancelBound has settleCancel(d, bound) run downloadCancelBound from
// now, on the UI thread.
func (h *Host) armCancelBound(d *hostDownload, bound uint64) {
	h.after(downloadCancelBound, func() { h.config.OnMain(func() { h.settleCancel(d, bound) }) })
}

// settleCancel reads a download the backend canceled that WebView2 has not
// ended (reconcileDownload at the cancel bound): the operation of a dropped
// download whose resume became a row of its own stays in progress and never
// ends, so its row would run for good. Measured on the real runtime: the
// old operation and its resume are one download in WebView2, so the cancel
// ends that resume too, at once and by WebView2's own report, and once the
// resume finished the cancel and the release leave its file as it is. A
// download that finishes after the bound is ended untracked, which the
// pane lists as failed, and its file stays in the Downloads folder, listed
// nowhere; its late state change reaches no handler. bound is the cancel that started
// this bound: one whose cancel was refused since, or that a later cancel
// restarted, is not settled by it.
func (h *Host) settleCancel(d *hostDownload, bound uint64) {
	if d.page.downloads[d.id] != d || !d.cancelRequested || d.cancelBound != bound {
		// Ended by WebView2's own report, its page went, or its cancel was
		// refused.
		return
	}
	h.reconcileDownload(d, sampleCancelBound)
}
