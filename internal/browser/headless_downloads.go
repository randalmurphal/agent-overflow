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
// writes for a download the Manager did not take. Headless Chromium's
// alone: the hosted engine's downloads are the launcher's
// (internal/webview2host/download.go).
//
// downloadWillBegin and downloadProgress reach it in the order Chromium
// sent them, on the connection's one ordered input (headlessProfile.
// subscribe), so a download's start is handled before its progress. Each
// download the connection starts is kept from its start until its end
// (trackedState): one the Manager admitted until its end is delivered to
// the Manager; one it refused, with the claim on its file, until its end
// arrives or the browser has stopped (stopped), and its file is removed
// then, once nothing writes it. Nothing is dropped for room: what is kept
// is what Chromium is still downloading. Progress of a download whose
// start this connection never saw is no one's and is dropped.
//
// A start the Manager answers downloadUnowned may be a frame of a page the
// Manager has not listed yet, or a popup's whose opener is not listed yet:
// with a lifetime, it is offered again, on a goroutine of its own, until
// it is answered as other than unowned, for up to ownerWait, and refused
// after. Its newest progress is held meanwhile, and its file is claimed as
// a refused one's. A connection that ends while it waits refuses it and
// removes its file at once (connectionEnded).
type downloadTracker struct {
	events engineEvents
	cancel func(id string)
	// remove removes the file Chromium writes download id to; nil when
	// there is none to remove.
	remove func(id string)
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
	downloads map[string]*trackedDownload
	stopped   bool
}

// trackedState is where one download of the connection stands.
type trackedState int

const (
	// trackedAwaiting: no page owns its frame yet; its start is offered
	// again (awaitOwner), and its newest progress is held for the answer.
	trackedAwaiting trackedState = iota + 1
	// trackedAdmitted: the Manager's; its progress goes to the Manager
	// until its end, and Chromium's "canceled" is a failure.
	trackedAdmitted
	// trackedCanceling: the Manager's, and AO sent its cancel (canceling):
	// CDP names a cancel and a failure alike ("canceled"), so this is what
	// tells them apart. A failure that beats the cancel to Chromium still
	// reads as the cancel.
	trackedCanceling
	// trackedRefused: canceled; its file is removed at its end, or once
	// the browser has stopped.
	trackedRefused
)

// trackedDownload is one download of the connection. claim is its file,
// released (removed) at its settlement unless the Manager admitted it.
type trackedDownload struct {
	state trackedState
	held  *downloadProgress
	claim *destination.Claim
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

func newDownloadTracker(events engineEvents, cancel, remove func(id string)) *downloadTracker {
	return &downloadTracker{events: events, cancel: cancel, remove: remove, downloads: make(map[string]*trackedDownload)}
}

// refusedDownloadCanceler is the tracker's cancel for a download the Manager
// refused: no caller waits on it, so a failure is logged.
func refusedDownloadCanceler(cancel func(id string) error, logf func(string, ...any)) func(id string) {
	return func(id string) {
		if err := cancel(id); err != nil {
			logf("browser: cancel refused download %s: %v", id, err)
		}
	}
}

func (t *downloadTracker) willBegin(event cdpbrowser.EventDownloadWillBegin) {
	t.mu.Lock()
	defer t.mu.Unlock()
	guid := event.GUID
	if t.stopped || t.downloads[guid] != nil {
		return
	}
	d := &trackedDownload{state: trackedAwaiting, claim: t.claim(guid)}
	t.downloads[guid] = d
	switch admission := t.events.DownloadStarted(t.startLocked(event)); admission {
	case downloadAdmitted:
		t.admitLocked(guid, d)
	case downloadRefused:
		t.refuseLocked(guid, d)
	case downloadUnowned:
		if t.lifetime == nil {
			t.refuseLocked(guid, d)
			return
		}
		go t.awaitOwner(event)
	default:
		panic(fmt.Sprintf("browser: unknown download admission %d", admission))
	}
}

// claim is the claim on the file Chromium writes download id to.
func (t *downloadTracker) claim(id string) *destination.Claim {
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
	start.Frame, start.ID, start.URL, start.SuggestedName = frameID(event.FrameID), event.GUID, event.URL, event.SuggestedFilename
	return start
}

// admitLocked makes d the Manager's: its file is the Manager's now, and
// the progress held for it goes to the Manager. The caller holds t.mu.
func (t *downloadTracker) admitLocked(guid string, d *trackedDownload) {
	d.to(trackedAdmitted)
	d.claim.HandOff()
	held := d.held
	d.held = nil
	if held == nil {
		return
	}
	t.events.DownloadProgress(*held)
	if !held.State.running() {
		delete(t.downloads, guid)
	}
}

// refuseLocked cancels d, which keeps the claim on its file until its end,
// at once if its end was already held. The caller holds t.mu.
func (t *downloadTracker) refuseLocked(guid string, d *trackedDownload) {
	d.to(trackedRefused)
	go t.cancel(guid)
	if held := d.held; held != nil && !held.State.running() {
		t.settleLocked(guid, d)
	}
}

// settleLocked forgets a download that will report nothing more and
// releases the claim on its file. The caller holds t.mu.
func (t *downloadTracker) settleLocked(guid string, d *trackedDownload) {
	delete(t.downloads, guid)
	d.claim.Release()
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
			t.connectionEnded(event.GUID)
			return
		}
		if t.offerAgain(event, expired) {
			return
		}
	}
}

// connectionEnded refuses the awaiting download guid once its connection
// has ended and removes its file at once: no page will own it, and with
// the connection gone there is no cancel to send. A writer still running
// writes to an unlinked file.
func (t *downloadTracker) connectionEnded(guid string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	d := t.downloads[guid]
	if d == nil || d.state != trackedAwaiting {
		return
	}
	d.to(trackedRefused)
	t.settleLocked(guid, d)
}

// offerAgain offers an awaiting start to the Manager once more, or refuses
// it once its wait expired, and answers whether it is settled. A download
// settled meanwhile (stop) is not offered.
func (t *downloadTracker) offerAgain(event cdpbrowser.EventDownloadWillBegin, expired bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	d := t.downloads[event.GUID]
	if d == nil || d.state != trackedAwaiting {
		return true
	}
	if expired {
		t.refuseLocked(event.GUID, d)
		return true
	}
	switch admission := t.events.DownloadStarted(t.startLocked(event)); admission {
	case downloadAdmitted:
		t.admitLocked(event.GUID, d)
	case downloadRefused:
		t.refuseLocked(event.GUID, d)
	case downloadUnowned:
		return false
	default:
		panic(fmt.Sprintf("browser: unknown download admission %d", admission))
	}
	return true
}

func (t *downloadTracker) progress(event cdpbrowser.EventDownloadProgress) {
	t.mu.Lock()
	defer t.mu.Unlock()
	d := t.downloads[event.GUID]
	if d == nil {
		return
	}
	progress := cdpDownloadProgress(event, d.state == trackedCanceling)
	switch d.state {
	case trackedAwaiting:
		d.held = &progress
	case trackedAdmitted, trackedCanceling:
		t.events.DownloadProgress(progress)
		if !progress.State.running() {
			delete(t.downloads, event.GUID)
		}
	case trackedRefused:
		if !progress.State.running() {
			t.settleLocked(event.GUID, d)
		}
	default:
		panic(fmt.Sprintf("browser: a download in unknown state %d", d.state))
	}
}

// canceling marks the admitted download guid as canceled by AO, before
// its CDP cancel is sent, and answers the undo for a cancel Chromium
// refused, which withdraws only the mark this call made. A tracker of no
// connection (nil) marks nothing.
func (t *downloadTracker) canceling(guid string) (undo func()) {
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
// has stopped and writes nothing more: the file of each the Manager did
// not admit is removed; the Manager settles its own as their pages go.
// Later events are dropped.
func (t *downloadTracker) stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = true
	for guid, d := range t.downloads {
		t.settleLocked(guid, d)
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
	progress := downloadProgress{ID: event.GUID, Received: event.ReceivedBytes, Total: event.TotalBytes, FilePath: event.FilePath}
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
