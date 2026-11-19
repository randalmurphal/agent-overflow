package browser

import (
	"context"
	"fmt"
	"sync"

	"agent-overflow/internal/webview2host"
)

// The launcher's reports are acknowledged apart from their processing. The
// launcher delivers its reports one at a time, each RPC waiting for the
// last (webview2host.ReportQueue), so a report whose processing is slow (a
// finished download's path conversion, the Manager's publication of it)
// would hold every later report of every page, a popup's announcement
// among them, which the navigation policy waits for. So Report accepts a
// page's download snapshot, close or process failure in one e.mu hold
// (acceptedWork), which is the receipt the RPC answers, and the work runs
// afterwards on its page's lane (reportLane), one at a time, in the order
// accepted. A pending popup's work runs on its nearest listed opener's
// lane, so a popup's download ends land before its opener's close, as the
// launcher sent them. A popup's announcement is processed at receipt
// (popupReported), so it never waits behind a lane.

// hostedPageID is a launcher page id: a hosted page's, or a popup's the
// launcher reported. The report path converts it once, at Report.
type hostedPageID string

// acceptedKind is the report an acceptedWork came from.
type acceptedKind int

const (
	// acceptedSnapshot is a download snapshot, recorded under its download.
	acceptedSnapshot acceptedKind = iota + 1
	// acceptedClose is the launcher's close of a page.
	acceptedClose
	// acceptedCrash is a page's process failure that ended it.
	acceptedCrash
)

// acceptedWork is one launcher report the engine accepted and has not
// processed yet. Only acceptSnapshotLocked, acceptClose and acceptCrash make
// one, so a snapshot's work always has its download's record.
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
func (w *acceptedWork) progressOf() (string, bool) {
	if w.kind != acceptedSnapshot || w.snapshot.State != webview2host.DownloadRunning || w.snapshot.Answer {
		return "", false
	}
	return w.snapshot.ID, true
}

// reportLane runs one page tree's accepted work, one at a time, in the
// order accepted, on a goroutine that exists only while work waits. Guarded
// by hostedEngine.mu.
type reportLane struct {
	key  hostedPageID
	jobs []*acceptedWork
	// progress is each waiting running snapshot a newer one replaces in its
	// place, by download id.
	progress map[string]*acceptedWork
	// pages are the pages whose work the lane holds; each keeps the lane
	// until it goes idle.
	pages []hostedPageID
	busy  bool
}

// reportLanes are the engine's lanes of one run, until Stop. Guarded by
// hostedEngine.mu.
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
// already has work on, else the lane of its nearest opener that has one or
// is no pending popup. The caller holds e.mu.
func (e *hostedEngine) laneLocked(page hostedPageID) *reportLane {
	lanes := e.lanes
	if lane := lanes.pages[page]; lane != nil {
		return lane
	}
	key := page
	var lane *reportLane
	// Each opener is reported before its popups, so the chain ends; the
	// bound stops a cycle all the same.
	for range len(e.popups) + 1 {
		popup := e.popups[string(key)]
		if popup == nil || popup.opener == "" {
			break
		}
		key = hostedPageID(popup.opener)
		if lane = lanes.pages[key]; lane != nil {
			break
		}
	}
	if lane == nil {
		lane = lanes.byKey[key]
	}
	if lane == nil {
		lane = &reportLane{key: key, progress: make(map[string]*acceptedWork)}
		lanes.byKey[key] = lane
	}
	lanes.pages[page] = lane
	lane.pages = append(lane.pages, page)
	return lane
}

// submitLocked queues accepted work on its page's lane and starts the
// lane's goroutine if it is idle. A running snapshot sent unasked replaces
// the same download's still waiting in its place. The caller holds e.mu.
func (e *hostedEngine) submitLocked(w *acceptedWork) {
	lanes := e.lanes
	lane := e.laneLocked(w.page)
	if id, ok := w.progressOf(); ok {
		if waiting := lane.progress[id]; waiting != nil {
			if w.snapshot.Seq > waiting.snapshot.Seq {
				waiting.snapshot = w.snapshot
			}
			return
		}
		lane.progress[id] = w
	}
	lane.jobs = append(lane.jobs, w)
	if lane.busy {
		return
	}
	lane.busy = true
	lanes.workers.Add(1)
	go e.runLane(lanes, lane)
}

// runLane processes lane's work in order until none waits, or until Stop
// ends the lanes it belongs to, then releases the lane and its pages.
func (e *hostedEngine) runLane(lanes *reportLanes, lane *reportLane) {
	defer lanes.workers.Done()
	for {
		e.mu.Lock()
		if len(lane.jobs) == 0 || lanes.stopped {
			lane.busy = false
			lane.jobs = nil
			delete(lanes.byKey, lane.key)
			for _, page := range lane.pages {
				if lanes.pages[page] == lane {
					delete(lanes.pages, page)
				}
			}
			e.mu.Unlock()
			return
		}
		w := lane.jobs[0]
		lane.jobs[0] = nil
		lane.jobs = lane.jobs[1:]
		if id, ok := w.progressOf(); ok && lane.progress[id] == w {
			delete(lane.progress, id)
		}
		e.mu.Unlock()
		e.process(lanes.ctx, w)
	}
}

// process runs one accepted report.
func (e *hostedEngine) process(ctx context.Context, w *acceptedWork) {
	switch w.kind {
	case acceptedSnapshot:
		e.applySnapshot(ctx, w.page, w.snapshot, w.download)
	case acceptedClose:
		e.retirePage(string(w.page))
	case acceptedCrash:
		e.events.PageCrashed(string(w.page), w.crash)
	default:
		panic(fmt.Sprintf("browser: accepted work of unknown kind %d", w.kind))
	}
}

// acceptClose accepts the launcher's close of page, which runs after the
// page's work already accepted (retirePage), and ends every download of the
// page before any snapshot accepted after it runs.
func (e *hostedEngine) acceptClose(page hostedPageID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.submitLocked(&acceptedWork{kind: acceptedClose, page: page})
}

// acceptCrash accepts the process failure that ended page, which runs
// after the page's work already accepted (engineEvents.PageCrashed).
func (e *hostedEngine) acceptCrash(page hostedPageID, reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.submitLocked(&acceptedWork{kind: acceptedCrash, page: page, crash: reason})
}

// stopLanesLocked ends the lanes of this run: work still waiting is
// dropped, a path conversion under way ends, and the next report starts
// new lanes. It answers the wait for the lanes' goroutines, which the
// caller makes once it releases e.mu. The caller holds e.mu.
func (e *hostedEngine) stopLanesLocked() func() {
	lanes := e.lanes
	lanes.stopped = true
	lanes.cancel()
	e.lanes = newReportLanes()
	return lanes.workers.Wait
}
