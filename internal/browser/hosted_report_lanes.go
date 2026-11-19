package browser

import (
	"context"
	"fmt"
	"sync"

	"agent-overflow/internal/browser/popupadmit"
	"agent-overflow/internal/webview2host"
)

// The launcher's reports are acknowledged apart from their processing. The
// launcher delivers its reports one at a time, each RPC waiting for the
// last (webview2host.ReportQueue), so a report whose processing is slow (a
// finished download's path conversion, the Manager's publication of it)
// would hold every later report of every page, a popup's announcement
// among them, which the navigation policy waits for. So Report receives a
// page's download snapshot, close, discard or process failure in one
// hostedPages hold (hosted_pages.go), which is the receipt the RPC
// answers, and the work runs afterwards on its page's lane (reportLane),
// one at a time, in the order received. A popup the Manager has not listed
// has its work on its nearest listed opener's lane, so its download ends
// land before its opener's close, as the launcher sent them; once listed,
// its later work runs on its own lane after what it already has drains. A
// popup's announcement is processed at receipt (popupReported), so it
// never waits behind a lane.

// hostedPageID is a launcher page id: a hosted page's, or a popup's the
// launcher reported. The report path converts it once, at Report.
type hostedPageID string

// acceptedKind is the report an acceptedWork came from.
type acceptedKind int

const (
	// acceptedSnapshot is a download snapshot, recorded under its download.
	acceptedSnapshot acceptedKind = iota + 1
	// acceptedClose is the launcher's close of a page, which the Manager
	// hears after the page's downloads settle.
	acceptedClose
	// acceptedDiscard is the engine's own close of a page (the Manager's
	// close or discard, a failed create), which settles the page's
	// downloads and is not reported.
	acceptedDiscard
	// acceptedCrash is a page's process failure that ended it.
	acceptedCrash
)

// acceptedWork is one report the engine received and has not processed
// yet. Only hostedPages makes one (acceptSnapshot, acceptEnd), so a
// snapshot's work always has its download's record.
type acceptedWork struct {
	kind     acceptedKind
	page     hostedPageID
	snapshot webview2host.DownloadReport
	download *hostedDownload
	crash    string
}

// progressOf names the download w is a running snapshot of, sent unasked:
// the one kind of work a newer snapshot of its download replaces while it
// waits. The download applies only its newest snapshot (Seq), so the
// replaced one carries nothing the newer does not.
func (w *acceptedWork) progressOf() (hostedDownloadKey, bool) {
	if w.kind != acceptedSnapshot || w.snapshot.State != webview2host.DownloadRunning || w.snapshot.Answer {
		return hostedDownloadKey{}, false
	}
	return w.download.key, true
}

// reportLane runs one page tree's received work, one at a time, in the
// order received, on a goroutine that exists only while work waits.
// Guarded by hostedPages.mu.
type reportLane struct {
	key  hostedPageID
	jobs []*acceptedWork
	// progress is each waiting running snapshot a newer one replaces in its
	// place, by download.
	progress map[hostedDownloadKey]*acceptedWork
	// pages are the pages whose work the lane holds; each keeps the lane
	// until it goes idle.
	pages []hostedPageID
	busy  bool
}

// reportLanes are the lanes of one run, until Stop. Guarded by
// hostedPages.mu.
type reportLanes struct {
	byKey map[hostedPageID]*reportLane
	// pages is the lane each page's work runs on while that lane is busy, so
	// a popup listed meanwhile keeps its order behind what it already sent.
	pages map[hostedPageID]*reportLane
	// ctx ends at Stop, which ends a path conversion under way; workers
	// counts the lane goroutines, which Stop joins.
	ctx     context.Context
	cancel  context.CancelFunc
	workers *sync.WaitGroup
	stopped bool
}

func newReportLanes() *reportLanes {
	ctx, cancel := context.WithCancel(context.Background())
	return &reportLanes{
		byKey:   make(map[hostedPageID]*reportLane),
		pages:   make(map[hostedPageID]*reportLane),
		ctx:     ctx,
		cancel:  cancel,
		workers: new(sync.WaitGroup),
	}
}

// laneLocked answers the lane page's work runs on: the busy lane the page
// already has work on, else, for a popup the Manager has not listed, the
// lane of its nearest opener that has one or is no such popup, else its
// own. The caller holds p.mu.
func (p *hostedPages) laneLocked(page hostedPageID) *reportLane {
	lanes := p.lanes
	if lane := lanes.pages[page]; lane != nil {
		return lane
	}
	key := page
	var lane *reportLane
	// Each opener is reported before its popups, so the chain ends; the
	// bound stops a cycle all the same.
	for range len(p.records) + 1 {
		r := p.records[key]
		if r == nil || !r.state.pendingPopup() || r.opener == "" {
			break
		}
		key = r.opener
		if lane = lanes.pages[key]; lane != nil {
			break
		}
	}
	if lane == nil {
		lane = lanes.byKey[key]
	}
	if lane == nil {
		lane = &reportLane{key: key, progress: make(map[hostedDownloadKey]*acceptedWork)}
		lanes.byKey[key] = lane
	}
	lanes.pages[page] = lane
	lane.pages = append(lane.pages, page)
	return lane
}

// submitLocked queues received work on its page's lane and starts the
// lane's goroutine if it is idle. A running snapshot sent unasked replaces
// the same download's still waiting in its place. The caller holds p.mu.
func (p *hostedPages) submitLocked(w *acceptedWork) {
	lanes := p.lanes
	lane := p.laneLocked(w.page)
	if key, ok := w.progressOf(); ok {
		if waiting := lane.progress[key]; waiting != nil {
			if w.snapshot.Seq > waiting.snapshot.Seq {
				waiting.snapshot = w.snapshot
			}
			return
		}
		lane.progress[key] = w
	}
	lane.jobs = append(lane.jobs, w)
	if lane.busy {
		return
	}
	lane.busy = true
	lanes.workers.Add(1)
	go p.runLane(lanes, lane)
}

// runLane processes lane's work in order until none waits, or until Stop
// ends the lanes it belongs to, then releases the lane and its pages.
func (p *hostedPages) runLane(lanes *reportLanes, lane *reportLane) {
	defer lanes.workers.Done()
	for {
		p.mu.Lock()
		if len(lane.jobs) == 0 || lanes.stopped {
			lane.busy = false
			lane.jobs = nil
			delete(lanes.byKey, lane.key)
			for _, page := range lane.pages {
				if lanes.pages[page] == lane {
					delete(lanes.pages, page)
				}
			}
			p.mu.Unlock()
			return
		}
		w := lane.jobs[0]
		lane.jobs[0] = nil
		lane.jobs = lane.jobs[1:]
		if key, ok := w.progressOf(); ok && lane.progress[key] == w {
			delete(lane.progress, key)
		}
		p.mu.Unlock()
		p.process(lanes.ctx, w)
	}
}

// stopLanesLocked ends the lanes of this run: work still waiting is
// dropped, a path conversion under way ends, and the next report starts
// new lanes. It answers the wait for the lanes' goroutines. The caller
// holds p.mu.
func (p *hostedPages) stopLanesLocked() func() {
	lanes := p.lanes
	lanes.stopped = true
	lanes.cancel()
	p.lanes = newReportLanes()
	return lanes.workers.Wait
}

// process runs one received report on its lane.
func (e *hostedEngine) process(ctx context.Context, w *acceptedWork) {
	switch w.kind {
	case acceptedSnapshot:
		e.applySnapshot(ctx, w.snapshot, w.download)
	case acceptedClose:
		e.retirePage(w.page, pageClosedReason, true)
	case acceptedDiscard:
		e.retirePage(w.page, popupDiscardedReason, false)
	case acceptedCrash:
		e.events.PageCrashed(string(w.page), w.crash)
	default:
		panic(fmt.Sprintf("browser: accepted work of unknown kind %d", w.kind))
	}
}

// acceptClose receives the launcher's close of page: its admission seals
// now, and a popup the Manager has not listed is recorded closed in the
// Manager's admission, so its listing is refused; the page retires on its
// lane after its work already received (retirePage). Its attach and its
// wait for the close end now.
func (e *hostedEngine) acceptClose(page hostedPageID) {
	receipt := e.pages.acceptEnd(page, acceptedClose, "")
	if !receipt.accepted {
		return
	}
	e.endPageWaits(page)
	if receipt.pendingPopup {
		e.events.PageEndReceived(string(page), popupadmit.Closed())
	}
}

// acceptCrash receives the process failure that ended page, which the
// Manager hears after the page's work already received
// (engineEvents.PageCrashed); a popup the Manager has not listed is
// recorded crashed in its admission now.
func (e *hostedEngine) acceptCrash(page hostedPageID, reason string) {
	receipt := e.pages.acceptEnd(page, acceptedCrash, reason)
	if receipt.accepted && receipt.pendingPopup {
		e.events.PageEndReceived(string(page), popupadmit.Crashed(reason))
	}
}

// endPageWaits ends what page waits for as it goes: its popup attach and
// its wait for the launcher's close (awaitLauncherClose).
func (e *hostedEngine) endPageWaits(page hostedPageID) {
	e.mu.Lock()
	e.endCloseFallbackLocked(string(page))
	var cancelAttach func()
	if popup := e.popups[string(page)]; popup != nil {
		cancelAttach = popup.takeAttachCancelLocked()
	}
	e.mu.Unlock()
	if cancelAttach != nil {
		cancelAttach()
	}
}
