package browser

import (
	"context"
	"fmt"
	"sync"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
)

// downloadTracker translates the two browser-level download events of one
// browser connection into the seam's vocabulary, and settles what Chromium
// writes for each download in the process's own directory (files, a
// downloadRoom). Headless Chromium's alone: the hosted engine's downloads
// are the launcher's (internal/webview2host/download.go).
//
// downloadWillBegin and downloadProgress reach it in the order Chromium
// sent them, on the connection's one ordered input (headlessProfile.
// subscribe), so a download's start is handled before its progress. Each
// download the connection starts is kept from its start until its end
// (trackedState), as one state machine whose every transition is made
// under mu. Its end settles its files: an admitted download's finished
// file is handed over to the Manager and anything else of it is removed.
// Once the browser process has exited (stop), whatever is still in the
// directory goes, a download whose start was never processed included.
// Nothing is dropped for room: what is kept is what Chromium is still
// downloading. Progress of a download whose start this connection never
// saw is no one's and is dropped.
//
// The tracker decides under its lock and calls out of it after: the
// Manager's admission and progress, and the file work, are reports it
// queues in its hold (post) and runs in that order once the hold ends
// (flush), so one download's slow report never holds the lock another's
// cancel needs (canceling), and each download's reports reach the Manager
// in the order they were decided. An offer to the Manager is marked in
// flight by the report that makes it, in the hold that checks the download
// still wants it, so nothing settled while the offer waited in the queue
// is offered.
//
// A start the Manager answers downloadUnowned may be a frame of a page the
// Manager has not listed yet, or a popup's whose opener is not listed yet:
// with a lifetime, it is offered again, on a goroutine of its own, until
// it is answered as other than unowned, for up to ownerWait, and refused
// after. Its newest progress is held meanwhile. A connection that ends
// while it waits refuses it, with no cancel to send. A wait that ends while
// an offer is answered leaves the decision to the answer: an admission
// stands, and only an answer of unowned meets the ended wait.
//
// A download's end is the Manager's from its receipt: its start's
// EndReported answers true from then until its end report has run, so a
// page that leaves meanwhile leaves the row to that report
// (pageLeftLocked). A download an AO cancel reached, accepted or not
// answered yet, that finished all the same is the Manager's only as far as
// its end report took it: what the report leaves of its file is withdrawn.
type downloadTracker struct {
	events engineEvents
	// cancel starts a cancel no caller waits on, of a download the tracker
	// refused, and returns at once: it runs as a report, after those
	// decided before it.
	cancel func(id downloadID)
	files  downloadFiles
	// popup answers, for a download's frame, the popup the frame is the
	// main frame of, as downloadStart's Popup, Openers and Profile, or an
	// empty start. Nil names none.
	popup func(frame frameID) downloadStart
	// lifetime is the connection's, which ends a start's wait for its
	// owner; nil waits for none. ownerWait bounds the wait, requestOwnerWait
	// when zero, and ownerPoll spaces its offers, requestOwnerPoll when
	// zero.
	lifetime  context.Context
	ownerWait time.Duration
	ownerPoll time.Duration

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
	// trackedAwaiting: no page owns its frame yet, and no offer of its
	// start is queued or under way; its wait (awaitOwner) offers it again.
	trackedAwaiting trackedState = iota + 1
	// trackedOffered: an offer of its start is queued (offerLocked) and not
	// made yet.
	trackedOffered
	// trackedOffering: the Manager is answering the offer of its start.
	trackedOffering
	// trackedAdmitted: the Manager's; its progress goes to the Manager
	// until its end, and Chromium's "canceled" is a failure.
	trackedAdmitted
	// trackedCanceling: the Manager's, with an AO cancel Chromium accepted
	// or has not answered yet: CDP names a cancel and a failure alike
	// ("canceled"), so this is what tells them apart. A failure that beats
	// the cancel to Chromium still reads as the cancel. A refusal of the
	// cancel makes it admitted again.
	trackedCanceling
	// trackedRefused: canceled; its files are removed at its end, or once
	// the browser has stopped.
	trackedRefused
	// trackedEnding: the Manager's, ended; its end report, which settles
	// its files first, is queued or under way, and its start's EndReported
	// answers true.
	trackedEnding
)

func (s trackedState) String() string {
	switch s {
	case trackedAwaiting:
		return "awaiting"
	case trackedOffered:
		return "offered"
	case trackedOffering:
		return "offering"
	case trackedAdmitted:
		return "admitted"
	case trackedCanceling:
		return "canceling"
	case trackedRefused:
		return "refused"
	case trackedEnding:
		return "ending"
	}
	return fmt.Sprintf("trackedState(%d)", int(s))
}

// preAdmission reports a state the Manager has not answered yet.
func (s trackedState) preAdmission() bool {
	return s == trackedAwaiting || s == trackedOffered || s == trackedOffering
}

// waitEnd is why a download's wait for its owner ended while an offer of
// its start was under way, which the offer's answer then applies.
type waitEnd int

const (
	waitGoing waitEnd = iota
	// waitExpired: ownerWait passed; an answer of unowned is refused with a
	// cancel, and an admission stands.
	waitExpired
	// waitLost: the connection ended, so there is no cancel to send.
	waitLost
)

// cancelMark is one AO cancel of a download, so the undo of a cancel
// Chromium refused withdraws that cancel and no other.
type cancelMark struct{}

// trackedDownload is one download of the connection. held is its newest
// progress while the Manager has not admitted it, and cancel the AO cancel
// it carries while Chromium has accepted it or not answered yet: one
// CancelDownload made before its admission, kept for the admission, or the
// one that made it canceling. waiting is whether its wait for an owner
// (awaitOwner) has begun, and ended why that wait ended while an offer was
// under way. canceled is whether an AO cancel reached it before its end
// report ran, which then withdraws what the Manager did not take of its
// file. endReceived is whether Chromium's end of it was received, set at
// that receipt whatever its state, held or delivered; d is forgotten once
// its end report returns.
type trackedDownload struct {
	state       trackedState
	held        *cdpbrowser.EventDownloadProgress
	cancel      *cancelMark
	waiting     bool
	ended       waitEnd
	canceled    bool
	endReceived bool
}

// to moves d to next along the edges of the state machine, which panics
// on any other.
func (d *trackedDownload) to(next trackedState) {
	legal := false
	switch d.state {
	case trackedAwaiting:
		legal = next == trackedOffered || next == trackedRefused
	case trackedOffered:
		legal = next == trackedOffering || next == trackedRefused
	case trackedOffering:
		legal = next == trackedAwaiting || next == trackedAdmitted || next == trackedRefused
	case trackedAdmitted:
		legal = next == trackedCanceling || next == trackedEnding
	case trackedCanceling:
		legal = next == trackedAdmitted || next == trackedEnding
	case trackedRefused, trackedEnding:
	}
	if !legal {
		panic(fmt.Sprintf("browser: a download moved from %s to %s", d.state, next))
	}
	d.state = next
}

func newDownloadTracker(events engineEvents, cancel func(id downloadID), files downloadFiles) *downloadTracker {
	return &downloadTracker{events: events, cancel: cancel, files: files, downloads: make(map[downloadID]*trackedDownload)}
}

// trackerCanceler is the tracker's cancel: it runs on a goroutine of its
// own, so a Chromium slow to answer holds no report behind it, and no
// caller waits on it, so a failure is logged.
func trackerCanceler(cancel func(id downloadID) error, logf func(string, ...any)) func(id downloadID) {
	return func(id downloadID) {
		go func() {
			if err := cancel(id); err != nil {
				logf("browser: cancel download %s: %v", id, err)
			}
		}()
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
	d := &trackedDownload{state: trackedAwaiting}
	t.downloads[guid] = d
	t.offerLocked(event, d)
}

// offerLocked queues the offer of an awaiting download's start. The offer
// is made only if d is still offered when its turn comes, and decide
// applies the Manager's answer. The caller holds t.mu.
func (t *downloadTracker) offerLocked(event cdpbrowser.EventDownloadWillBegin, d *trackedDownload) {
	d.to(trackedOffered)
	guid := downloadID(event.GUID)
	t.post(func() {
		t.mu.Lock()
		if t.downloads[guid] != d || d.state != trackedOffered {
			t.mu.Unlock()
			return
		}
		d.to(trackedOffering)
		start := t.startLocked(event, d)
		t.mu.Unlock()
		admission := t.events.DownloadStarted(start)
		t.mu.Lock()
		defer t.mu.Unlock()
		t.decideLocked(event, d, admission)
	})
}

// decideLocked applies the Manager's answer to the offer of d's start,
// with whatever happened to d while the Manager answered: a cancel it
// kept, an end held, a wait that ended. A download the browser's stop
// settled meanwhile has no file left and nothing more to report; one the
// Manager admitted ends with its page, which ends with the browser. The
// caller holds t.mu.
func (t *downloadTracker) decideLocked(event cdpbrowser.EventDownloadWillBegin, d *trackedDownload, admission downloadAdmission) {
	guid := downloadID(event.GUID)
	if t.downloads[guid] != d {
		return
	}
	if t.stopped && admission != downloadAdmitted {
		// The browser is gone, so there is nothing to cancel: the end the
		// stop kept for this answer (reportsEnd) goes with its file.
		delete(t.downloads, guid)
		t.post(func() { t.files.discard(guid) })
		t.closeIfLastLocked()
		return
	}
	switch admission {
	case downloadAdmitted:
		t.admitLocked(guid, d)
	case downloadRefused:
		t.refuseLocked(guid, d, d.cancel == nil && d.ended != waitLost)
	case downloadUnowned:
		switch {
		case t.lifetime == nil || d.ended == waitExpired:
			t.refuseLocked(guid, d, d.cancel == nil)
		case d.ended == waitLost:
			t.refuseLocked(guid, d, false)
		default:
			d.to(trackedAwaiting)
			// The first answer starts the wait; a later one, with the wait
			// under way, leaves it to go on.
			if !d.waiting {
				d.waiting = true
				go t.awaitOwner(event)
			}
		}
	default:
		panic(fmt.Sprintf("browser: unknown download admission %d", admission))
	}
}

// startLocked is the start event names, for d. The caller holds t.mu.
func (t *downloadTracker) startLocked(event cdpbrowser.EventDownloadWillBegin, d *trackedDownload) downloadStart {
	var start downloadStart
	if t.popup != nil {
		start = t.popup(frameID(event.FrameID))
	}
	guid := downloadID(event.GUID)
	start.Frame, start.ID, start.URL, start.SuggestedName = frameID(event.FrameID), guid, event.URL, event.SuggestedFilename
	start.EndReported = func() bool { return t.endReported(guid, d) }
	return start
}

// endReported is the EndReported of d's start: whether its end was
// received (endReceived), in whatever state, and its end report has not
// returned, which forgets d.
func (t *downloadTracker) endReported(guid downloadID, d *trackedDownload) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.downloads[guid] == d && d.endReceived
}

// admitLocked makes d the Manager's, whatever its wait did meanwhile: a
// cancel it kept makes it canceling, and the progress held for it goes to
// the Manager. The caller holds t.mu.
func (t *downloadTracker) admitLocked(guid downloadID, d *trackedDownload) {
	d.to(trackedAdmitted)
	if d.cancel != nil {
		d.to(trackedCanceling)
	}
	held := d.held
	d.held = nil
	if held != nil {
		t.deliverLocked(guid, d, *held)
	}
}

// deliverLocked queues progress of the Manager's download d. At its end,
// the report settles d's files first, its finished file handed over and
// anything else removed, then reaches the Manager; then, for a download an
// AO cancel reached, what the Manager did not take of the handed-over file
// is withdrawn, and d is forgotten. The caller holds t.mu.
func (t *downloadTracker) deliverLocked(guid downloadID, d *trackedDownload, event cdpbrowser.EventDownloadProgress) {
	progress := cdpDownloadProgress(event, d.state == trackedCanceling)
	if progress.State.running() {
		t.post(func() { t.events.DownloadProgress(progress) })
		return
	}
	d.canceled = d.canceled || d.state == trackedCanceling
	d.to(trackedEnding)
	t.post(func() {
		handed := false
		if progress.State == downloadCompleted {
			path, err := t.files.handOver(guid)
			if err != nil {
				progress.State, progress.Error = downloadFailed, err.Error()
			} else {
				progress.FilePath, handed = path, true
			}
		}
		if progress.State != downloadCompleted {
			t.files.discard(guid)
		}
		t.events.DownloadProgress(progress)
		t.mu.Lock()
		withdraw := handed && d.canceled
		delete(t.downloads, guid)
		last := t.stopped && !t.endingLocked()
		t.mu.Unlock()
		if withdraw {
			t.files.withdraw(guid)
		}
		if last {
			t.files.close()
		}
	})
}

// endingLocked reports whether an end report of the Manager's is still
// queued or under way. The caller holds t.mu.
func (t *downloadTracker) endingLocked() bool {
	for _, d := range t.downloads {
		if d.reportsEnd() {
			return true
		}
	}
	return false
}

// reportsEnd is whether the tracker still reports d's received end: it is
// being reported, or is held for the answer of an admission in flight,
// which reports it if the answer admits d.
func (d *trackedDownload) reportsEnd() bool {
	return d.state == trackedEnding || (d.state == trackedOffering && d.endReceived)
}

// closeIfLastLocked queues the directory's close once the browser has
// stopped and no end is still reported. The caller holds t.mu.
func (t *downloadTracker) closeIfLastLocked() {
	if t.stopped && !t.endingLocked() {
		t.post(t.files.close)
	}
}

// refuseLocked refuses d, sending its cancel when send says one is due,
// and keeps it until its end, at which its files go, at once if its end
// was already held. The caller holds t.mu.
func (t *downloadTracker) refuseLocked(guid downloadID, d *trackedDownload, send bool) {
	d.to(trackedRefused)
	if send {
		t.post(func() { t.cancel(guid) })
	}
	if held := d.held; held != nil && held.State != cdpbrowser.DownloadProgressStateInProgress {
		t.releaseLocked(guid)
	}
}

// releaseLocked forgets a refused download whose writer has stopped and
// queues the removal of its files. The caller holds t.mu.
func (t *downloadTracker) releaseLocked(guid downloadID) {
	delete(t.downloads, guid)
	t.post(func() { t.files.discard(guid) })
}

// awaitOwner offers an unowned start again until the Manager answers it
// otherwise, once popup names its frame or a listed page owns it, and
// refuses it when ownerWait passes first or the connection ends.
func (t *downloadTracker) awaitOwner(event cdpbrowser.EventDownloadWillBegin) {
	wait, every := t.ownerWait, t.ownerPoll
	if wait <= 0 {
		wait = requestOwnerWait
	}
	if every <= 0 {
		every = requestOwnerPoll
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	poll := time.NewTicker(every)
	defer poll.Stop()
	for {
		end := waitGoing
		select {
		case <-poll.C:
		case <-deadline.C:
			end = waitExpired
		case <-t.lifetime.Done():
			end = waitLost
		}
		if t.offerAgain(event, end) {
			return
		}
	}
}

// offerAgain applies one turn of a start's wait for its owner and answers
// whether the wait is over: an awaiting start is offered again while its
// wait goes on, and refused once it ended, with a cancel unless the
// connection is gone. A wait that ends while an offer is queued refuses
// the start, whose offer then is not made; one that ends while an offer is
// under way is recorded for its answer to apply. A download settled
// meanwhile ends the wait.
func (t *downloadTracker) offerAgain(event cdpbrowser.EventDownloadWillBegin, end waitEnd) bool {
	defer t.flush()
	t.mu.Lock()
	defer t.mu.Unlock()
	guid := downloadID(event.GUID)
	d := t.downloads[guid]
	switch {
	case d == nil || !d.state.preAdmission():
		return true
	case end == waitGoing:
		if d.state == trackedAwaiting {
			t.offerLocked(event, d)
		}
		return false
	case d.state == trackedOffering:
		d.ended = end
	default:
		t.refuseLocked(guid, d, end == waitExpired && d.cancel == nil)
	}
	return true
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
	if event.State != cdpbrowser.DownloadProgressStateInProgress {
		// Its end is the Manager's from here, held or delivered
		// (EndReported).
		d.endReceived = true
	}
	switch d.state {
	case trackedAwaiting, trackedOffered, trackedOffering:
		d.held = &event
	case trackedAdmitted, trackedCanceling:
		t.deliverLocked(guid, d, event)
	case trackedRefused:
		if event.State != cdpbrowser.DownloadProgressStateInProgress {
			t.releaseLocked(guid)
		}
	case trackedEnding:
		// Its end was reported; Chromium reports nothing after it.
	default:
		panic(fmt.Sprintf("browser: a download in unknown state %d", d.state))
	}
}

// canceling marks download guid as canceled by AO, before its CDP cancel
// is sent, and answers the undo for a cancel Chromium refused, which
// withdraws only the mark this call made. An admitted download is
// canceling from here; one the Manager has not answered yet keeps the mark
// for its admission (admitLocked). One whose end was received, held or
// delivered, answers ended, with no cancel to send: its end report settles
// it, and what that report leaves of its file is withdrawn. Any other
// marks nothing.
func (t *downloadTracker) canceling(guid downloadID) (undo func(), ended bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	d := t.downloads[guid]
	switch {
	case d != nil && d.endReceived:
		d.canceled = true
		return func() {}, true
	case d == nil || d.cancel != nil:
		return func() {}, false
	case d.state == trackedAdmitted:
		d.to(trackedCanceling)
	case d.state.preAdmission():
	default:
		return func() {}, false
	}
	mark := &cancelMark{}
	d.cancel = mark
	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.downloads[guid] != d || d.cancel != mark {
			return
		}
		d.cancel = nil
		if d.state == trackedCanceling {
			d.to(trackedAdmitted)
		}
	}, false
}

// stop settles every download the connection still has, once its browser
// process has exited and writes nothing more: none of them finished, so
// everything in the process's directory goes, the Manager's running
// downloads' files included, apart from those of the ends still reported
// (reportsEnd), whose last report removes what is left (deliverLocked,
// decideLocked). Later events are dropped.
func (t *downloadTracker) stop() {
	defer t.flush()
	t.mu.Lock()
	t.stopped = true
	keep := make(map[downloadID]bool)
	for guid, d := range t.downloads {
		if d.reportsEnd() {
			keep[guid] = true
			continue
		}
		delete(t.downloads, guid)
	}
	t.mu.Unlock()
	if len(keep) == 0 {
		t.files.close()
		return
	}
	t.files.empty(keep)
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
