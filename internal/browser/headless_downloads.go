package browser

import (
	"context"
	"fmt"
	"sync"
	"time"

	"agent-overflow/internal/browser/destination"

	cdpbrowser "github.com/chromedp/cdproto/browser"
)

// downloadTracker translates the two browser-level download events of one
// browser connection into the seam's vocabulary, and owns what Chromium
// writes for a download until its writer has stopped. Headless Chromium's
// alone: the hosted engine's downloads are the launcher's
// (internal/webview2host/download.go).
//
// downloadWillBegin and downloadProgress reach it in the order Chromium
// sent them, on the connection's one ordered input (headlessProfile.
// subscribe), so a download's start is handled before its progress. Each
// download the connection starts is kept from its start until its end
// (trackedState), with the claim on every name Chromium writes for it (its
// partial and its final file, headlessProfile.removeDownloadFile). The
// claim is settled once the writer has stopped: at the end Chromium
// reports, where an admitted download's file becomes the Manager's
// (handed off) and a refused one's is removed, or once the browser process
// has exited (stop), where every claim still held is removed, since no
// download it ends with finished. Nothing is dropped for room: what is
// kept is what Chromium is still downloading. Progress of a download whose
// start this connection never saw is no one's and is dropped.
//
// The tracker decides under its lock and calls out of it after: the
// Manager's admission and progress, and a claim's removal, are reports it
// queues in its hold (post) and runs in that order once the hold ends
// (flush), so one download's slow report never holds the lock another's
// cancel needs (canceling), and each download's reports reach the Manager
// in the order they were decided.
//
// A start the Manager answers downloadUnowned may be a frame of a page the
// Manager has not listed yet, or a popup's whose opener is not listed yet:
// with a lifetime, it is offered again, on a goroutine of its own, until
// it is answered as other than unowned, for up to ownerWait, and refused
// after. Its newest progress is held meanwhile. A connection that ends
// while it waits refuses it, with no cancel to send; its claim stays until
// the browser process has exited (stop).
type downloadTracker struct {
	events engineEvents
	cancel func(id downloadID)
	// remove removes the files Chromium writes download id to; nil when
	// there are none to remove.
	remove func(id downloadID)
	// popup answers, for a download's frame, the popup the frame is the
	// main frame of, as downloadStart's Popup, Openers and Profile, or an
	// empty start. Nil names none.
	popup func(frame frameID) downloadStart
	// lifetime is the connection's, which ends a start's wait for its
	// owner; nil waits for none. ownerWait bounds the wait, requestOwnerWait
	// when zero.
	lifetime  context.Context
	ownerWait time.Duration

	mu        sync.Mutex
	downloads map[downloadID]*trackedDownload
	stopped   bool
	// out is the reports decided under mu and not run yet, in order, and
	// flushing whether a caller is running them (flush).
	out      []func()
	flushing bool
}

// trackedState is where one download of the connection stands.
type trackedState int

const (
	// trackedAwaiting: no page owns its frame yet; its start is offered
	// (offerLocked), and its newest progress is held for the answer.
	trackedAwaiting trackedState = iota + 1
	// trackedAdmitted: the Manager's; its progress goes to the Manager
	// until its end, and Chromium's "canceled" is a failure.
	trackedAdmitted
	// trackedCanceling: the Manager's, and AO sent its cancel (canceling):
	// CDP names a cancel and a failure alike ("canceled"), so this is what
	// tells them apart. A failure that beats the cancel to Chromium still
	// reads as the cancel.
	trackedCanceling
	// trackedRefused: canceled; its files are removed at its end, or once
	// the browser has stopped.
	trackedRefused
)

// trackedDownload is one download of the connection. claim is every file
// Chromium writes for it, settled once its writer has stopped. offering is
// whether an offer of its start to the Manager is queued or running, and
// waiting whether its wait for an owner (awaitOwner) has begun.
type trackedDownload struct {
	state    trackedState
	held     *downloadProgress
	claim    *destination.Claim
	offering bool
	waiting  bool
}

// to moves d to next: an awaiting download is admitted or refused, an
// admitted one is canceling and back when Chromium refused the cancel, and
// nothing else moves.
func (d *trackedDownload) to(next trackedState) {
	legal := false
	switch d.state {
	case trackedAwaiting:
		legal = next == trackedAdmitted || next == trackedRefused
	case trackedAdmitted:
		legal = next == trackedCanceling
	case trackedCanceling:
		legal = next == trackedAdmitted
	case trackedRefused:
	}
	if !legal {
		panic(fmt.Sprintf("browser: a download moved from %d to %d", d.state, next))
	}
	d.state = next
}

func newDownloadTracker(events engineEvents, cancel, remove func(id downloadID)) *downloadTracker {
	return &downloadTracker{events: events, cancel: cancel, remove: remove, downloads: make(map[downloadID]*trackedDownload)}
}

// refusedDownloadCanceler is the tracker's cancel for a download the Manager
// refused: no caller waits on it, so a failure is logged.
func refusedDownloadCanceler(cancel func(id downloadID) error, logf func(string, ...any)) func(id downloadID) {
	return func(id downloadID) {
		if err := cancel(id); err != nil {
			logf("browser: cancel refused download %s: %v", id, err)
		}
	}
}

// post queues report, decided under t.mu, to run after the hold in the
// order posted. The caller holds t.mu.
func (t *downloadTracker) post(report func()) {
	t.out = append(t.out, report)
}

// flush runs the queued reports, in order and outside t.mu, unless another
// caller already is, which then runs these too. Every entry point calls it
// after its hold.
func (t *downloadTracker) flush() {
	t.mu.Lock()
	if t.flushing {
		t.mu.Unlock()
		return
	}
	t.flushing = true
	for len(t.out) > 0 {
		report := t.out[0]
		t.out[0] = nil
		t.out = t.out[1:]
		t.mu.Unlock()
		report()
		t.mu.Lock()
	}
	t.out = nil
	t.flushing = false
	t.mu.Unlock()
}

func (t *downloadTracker) willBegin(event cdpbrowser.EventDownloadWillBegin) {
	defer t.flush()
	t.mu.Lock()
	defer t.mu.Unlock()
	guid := downloadID(event.GUID)
	if t.stopped || t.downloads[guid] != nil {
		return
	}
	d := &trackedDownload{state: trackedAwaiting, claim: t.claim(guid)}
	t.downloads[guid] = d
	t.offerLocked(event, d)
}

// offerLocked queues the offer of an awaiting download's start to the
// Manager, whose answer decide applies. The caller holds t.mu.
func (t *downloadTracker) offerLocked(event cdpbrowser.EventDownloadWillBegin, d *trackedDownload) {
	d.offering = true
	start := t.startLocked(event)
	t.post(func() {
		admission := t.events.DownloadStarted(start)
		t.mu.Lock()
		defer t.mu.Unlock()
		t.decideLocked(event, d, admission)
	})
}

// decideLocked applies the Manager's answer to the offer of d's start,
// unless d was settled meanwhile (stop). The caller holds t.mu.
func (t *downloadTracker) decideLocked(event cdpbrowser.EventDownloadWillBegin, d *trackedDownload, admission downloadAdmission) {
	guid := downloadID(event.GUID)
	d.offering = false
	if t.downloads[guid] != d || d.state != trackedAwaiting {
		return
	}
	switch admission {
	case downloadAdmitted:
		t.admitLocked(guid, d)
	case downloadRefused:
		t.refuseLocked(guid, d)
	case downloadUnowned:
		if t.lifetime == nil {
			t.refuseLocked(guid, d)
			return
		}
		// The first answer starts the wait; a later one, with the wait
		// under way, leaves it to go on.
		if !d.waiting {
			d.waiting = true
			go t.awaitOwner(event)
		}
	default:
		panic(fmt.Sprintf("browser: unknown download admission %d", admission))
	}
}

// claim is the claim on the files Chromium writes download id to.
func (t *downloadTracker) claim(id downloadID) *destination.Claim {
	if t.remove == nil {
		return destination.NewClaim(nil)
	}
	return destination.NewClaim(func() { t.remove(id) })
}

// startLocked is the start event names. The caller holds t.mu.
func (t *downloadTracker) startLocked(event cdpbrowser.EventDownloadWillBegin) downloadStart {
	var start downloadStart
	if t.popup != nil {
		start = t.popup(frameID(event.FrameID))
	}
	start.Frame, start.ID, start.URL, start.SuggestedName = frameID(event.FrameID), downloadID(event.GUID), event.URL, event.SuggestedFilename
	return start
}

// admitLocked makes d the Manager's, and the progress held for it goes to
// the Manager. The caller holds t.mu.
func (t *downloadTracker) admitLocked(guid downloadID, d *trackedDownload) {
	d.to(trackedAdmitted)
	held := d.held
	d.held = nil
	if held != nil {
		t.deliverLocked(guid, d, *held)
	}
}

// deliverLocked queues progress of the Manager's download d, and at its
// end settles d: the file it ended with is the Manager's now. The caller
// holds t.mu.
func (t *downloadTracker) deliverLocked(guid downloadID, d *trackedDownload, progress downloadProgress) {
	t.post(func() { t.events.DownloadProgress(progress) })
	if !progress.State.running() {
		delete(t.downloads, guid)
		t.post(d.claim.HandOff)
	}
}

// refuseLocked cancels d, which keeps the claim on its files until its
// end, at once if its end was already held. The caller holds t.mu.
func (t *downloadTracker) refuseLocked(guid downloadID, d *trackedDownload) {
	d.to(trackedRefused)
	go t.cancel(guid)
	if held := d.held; held != nil && !held.State.running() {
		t.releaseLocked(guid, d)
	}
}

// releaseLocked forgets a download whose writer has stopped and queues the
// removal of its files. The caller holds t.mu.
func (t *downloadTracker) releaseLocked(guid downloadID, d *trackedDownload) {
	delete(t.downloads, guid)
	t.post(d.claim.Release)
}

// awaitOwner offers an unowned start again until the Manager answers it
// otherwise, once popup names its frame or a listed page owns it, and
// refuses it when ownerWait passes first or the connection ends.
func (t *downloadTracker) awaitOwner(event cdpbrowser.EventDownloadWillBegin) {
	wait := t.ownerWait
	if wait <= 0 {
		wait = requestOwnerWait
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	poll := time.NewTicker(requestOwnerPoll)
	defer poll.Stop()
	for {
		expired := false
		select {
		case <-poll.C:
		case <-deadline.C:
			expired = true
		case <-t.lifetime.Done():
			t.connectionEnded(downloadID(event.GUID))
			return
		}
		if t.offerAgain(event, expired) {
			return
		}
	}
}

// connectionEnded refuses the awaiting download guid once its connection
// has ended: no page will own it, and with the connection gone there is
// no cancel to send. Unless its end was already held, its writer may run
// until the browser process exits, so its claim stays for stop.
func (t *downloadTracker) connectionEnded(guid downloadID) {
	defer t.flush()
	t.mu.Lock()
	defer t.mu.Unlock()
	d := t.downloads[guid]
	if d == nil || d.state != trackedAwaiting {
		return
	}
	d.to(trackedRefused)
	if held := d.held; held != nil && !held.State.running() {
		t.releaseLocked(guid, d)
	}
}

// offerAgain offers an awaiting start to the Manager once more, unless an
// offer is still under way, or refuses it once its wait expired, and
// answers whether it is settled. A download settled meanwhile is not
// offered.
func (t *downloadTracker) offerAgain(event cdpbrowser.EventDownloadWillBegin, expired bool) bool {
	defer t.flush()
	t.mu.Lock()
	defer t.mu.Unlock()
	guid := downloadID(event.GUID)
	d := t.downloads[guid]
	switch {
	case d == nil || d.state != trackedAwaiting:
		return true
	case expired:
		t.refuseLocked(guid, d)
		return true
	case !d.offering:
		t.offerLocked(event, d)
	}
	return false
}

func (t *downloadTracker) progress(event cdpbrowser.EventDownloadProgress) {
	defer t.flush()
	t.mu.Lock()
	defer t.mu.Unlock()
	guid := downloadID(event.GUID)
	d := t.downloads[guid]
	if d == nil {
		return
	}
	progress := cdpDownloadProgress(event, d.state == trackedCanceling)
	switch d.state {
	case trackedAwaiting:
		d.held = &progress
	case trackedAdmitted, trackedCanceling:
		t.deliverLocked(guid, d, progress)
	case trackedRefused:
		if !progress.State.running() {
			t.releaseLocked(guid, d)
		}
	default:
		panic(fmt.Sprintf("browser: a download in unknown state %d", d.state))
	}
}

// canceling marks the admitted download guid as canceled by AO, before
// its CDP cancel is sent, and answers the undo for a cancel Chromium
// refused, which withdraws only the mark this call made. A tracker of no
// connection (nil) marks nothing.
func (t *downloadTracker) canceling(guid downloadID) (undo func()) {
	if t == nil {
		return func() {}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	d := t.downloads[guid]
	if d == nil || d.state != trackedAdmitted {
		return func() {}
	}
	d.to(trackedCanceling)
	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.downloads[guid] == d && d.state == trackedCanceling {
			d.to(trackedAdmitted)
		}
	}
}

// stop settles every download the connection still has, once its browser
// process has exited and writes nothing more: none of them finished, so
// the files of each, the Manager's included, are removed. Later events
// are dropped.
func (t *downloadTracker) stop() {
	defer t.flush()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = true
	for guid, d := range t.downloads {
		t.releaseLocked(guid, d)
	}
}

// retained reports how many downloads the tracker keeps.
func (t *downloadTracker) retained() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.downloads)
}

// cdpDownloadProgress is a CDP download event in the seam's terms: its
// "canceled" is AO's cancel when AO sent one (canceled), and a failure
// otherwise. A state the pinned protocol does not name fails the download
// with it as the reason, so a row never stays running on a report nothing
// here understands.
func cdpDownloadProgress(event cdpbrowser.EventDownloadProgress, canceled bool) downloadProgress {
	progress := downloadProgress{ID: downloadID(event.GUID), Received: event.ReceivedBytes, Total: event.TotalBytes, FilePath: event.FilePath}
	switch event.State {
	case cdpbrowser.DownloadProgressStateInProgress:
		progress.State = downloadInProgress
	case cdpbrowser.DownloadProgressStateCompleted:
		progress.State = downloadCompleted
	case cdpbrowser.DownloadProgressStateCanceled:
		progress.State = downloadFailed
		if canceled {
			progress.State = downloadCanceled
		}
	default:
		progress.State, progress.Error = downloadFailed, fmt.Sprintf("Chromium reported the download as %q", event.State)
	}
	return progress
}
