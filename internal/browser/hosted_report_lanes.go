package browser

import (
	"context"
	"fmt"
	"sync"

	"agent-overflow/internal/browser/ids"
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
// its later work runs on its own lane after its own work already received
// has finished, whatever else its opener's lane still has (gateLocked). A
// popup's announcement is processed at receipt (popupReported), so it
// never waits behind a lane.

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
	page     ids.PageHandle
	snapshot webview2host.DownloadReport
	download *hostedDownload
	crash    string
	// releases are the lanes held until this work finishes (gateLocked).
	// Guarded by hostedPages.mu.
	releases []*reportLane
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

// reportLane runs the received work of the pages whose lane key is key
// (laneKeyLocked), one at a time, in the order received, on a goroutine
// that exists only while work waits and the lane is not held. Guarded by
// hostedPages.mu.
type reportLane struct {
	key  ids.PageHandle
	jobs []*acceptedWork
	// current is the work being processed.
	current *acceptedWork
	// progress is each waiting running snapshot a newer one replaces in its
	// place, by download.
	progress map[hostedDownloadKey]*acceptedWork
	// held counts the work on other lanes this lane runs after (gateLocked).
	held int
	busy bool
}

// reportLanes are the lanes of one run, until Stop. Guarded by
// hostedPages.mu.
type reportLanes struct {
	byKey map[ids.PageHandle]*reportLane
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
		byKey:   make(map[ids.PageHandle]*reportLane),
		ctx:     ctx,
		cancel:  cancel,
		workers: new(sync.WaitGroup),
	}
}

// laneKeyLocked answers the key of the lane page's work runs on: for a
// popup the Manager has not listed, its nearest opener that is no such
// popup, or that has no record; else page itself. The caller holds p.mu.
func (p *hostedPages) laneKeyLocked(page ids.PageHandle) ids.PageHandle {
	key := page
	// Each opener is reported before its popups, so the chain ends; the
	// bound stops a cycle all the same.
	for range len(p.records) + 1 {
		r := p.records[key]
		if r == nil || !r.state.pendingPopup() || r.opener == "" {
			break
		}
		key = r.opener
	}
	return key
}

// laneLocked answers the lane of key, made if it has none. The caller
// holds p.mu.
func (p *hostedPages) laneLocked(key ids.PageHandle) *reportLane {
	lane := p.lanes.byKey[key]
	if lane == nil {
		lane = &reportLane{key: key, progress: make(map[hostedDownloadKey]*acceptedWork)}
		p.lanes.byKey[key] = lane
	}
	return lane
}

// gateLocked holds key's lane after its key changed for some pages (a
// popup the Manager listed, whose lane key turns from its opener's to its
// own, or the record of one it did not list gone, whose popups' key turns
// to its page id): it runs once their work received before, waiting or
// under way on any other lane, has finished, and not after that lane's
// other work. The caller holds p.mu.
func (p *hostedPages) gateLocked(key ids.PageHandle) {
	var gate *reportLane
	hold := func(w *acceptedWork) {
		if w == nil || p.laneKeyLocked(w.page) != key {
			return
		}
		if gate == nil {
			gate = p.laneLocked(key)
		}
		gate.held++
		w.releases = append(w.releases, gate)
	}
	for laneKey, lane := range p.lanes.byKey {
		if laneKey == key {
			continue
		}
		hold(lane.current)
		for _, w := range lane.jobs {
			hold(w)
		}
	}
}

// submitLocked queues received work on its page's lane and starts the
// lane's goroutine if it is idle and not held. A running snapshot sent
// unasked replaces the same download's still waiting in its place. The
// caller holds p.mu.
func (p *hostedPages) submitLocked(w *acceptedWork) {
	lanes := p.lanes
	lane := p.laneLocked(p.laneKeyLocked(w.page))
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
	if lane.busy || lane.held > 0 {
		return
	}
	lane.busy = true
	lanes.workers.Add(1)
	go p.runLane(lanes, lane)
}

// runLane processes lane's work in order until none waits, or until Stop
// ends the lanes it belongs to, then releases the lane.
func (p *hostedPages) runLane(lanes *reportLanes, lane *reportLane) {
	defer lanes.workers.Done()
	for {
		p.mu.Lock()
		if done := lane.current; done != nil {
			lane.current = nil
			p.releaseLocked(lanes, done)
		}
		if len(lane.jobs) == 0 || lanes.stopped {
			lane.busy = false
			lane.jobs = nil
			if lanes.byKey[lane.key] == lane && lane.held == 0 {
				delete(lanes.byKey, lane.key)
			}
			p.mu.Unlock()
			return
		}
		w := lane.jobs[0]
		lane.jobs[0] = nil
		lane.jobs = lane.jobs[1:]
		lane.current = w
		if key, ok := w.progressOf(); ok && lane.progress[key] == w {
			delete(lane.progress, key)
		}
		p.mu.Unlock()
		p.process(lanes.ctx, w)
	}
}

// releaseLocked lets go of the lanes finished work w held, starting each
// whose last hold it was and which has work, and dropping one with none.
// The caller holds p.mu.
func (p *hostedPages) releaseLocked(lanes *reportLanes, w *acceptedWork) {
	for _, gate := range w.releases {
		gate.held--
		switch {
		case gate.held > 0 || lanes.stopped || gate.busy:
		case len(gate.jobs) == 0:
			if lanes.byKey[gate.key] == gate {
				delete(lanes.byKey, gate.key)
			}
		default:
			gate.busy = true
			lanes.workers.Add(1)
			go p.runLane(lanes, gate)
		}
	}
	w.releases = nil
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
		e.events.PageCrashed(w.page, w.crash)
	default:
		panic(fmt.Sprintf("browser: accepted work of unknown kind %d", w.kind))
	}
}

// acceptClose receives the launcher's close of page (hostedPages.acceptEnd):
// a popup the Manager has not listed is recorded closed in the Manager's
// admission, so its listing is refused, its admission seals and its attach
// is signaled to end, at once; the page retires on its lane after its work
// already received (retirePage).
func (e *hostedEngine) acceptClose(page ids.PageHandle) {
	e.pages.acceptEnd(page, acceptedClose, "")
}

// acceptCrash receives the process failure that ended page, which the
// Manager hears after the page's work already received
// (engineEvents.PageCrashed); a popup the Manager has not listed is
// recorded crashed in its admission at once.
func (e *hostedEngine) acceptCrash(page ids.PageHandle, reason string) {
	e.pages.acceptEnd(page, acceptedCrash, reason)
}
