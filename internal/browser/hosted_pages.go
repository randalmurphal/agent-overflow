package browser

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/browser/popupadmit"
	"agent-overflow/internal/webview2host"
)

// The hosted engine's pages as the launcher's reports see them
// (hostedPages): each page's lifecycle, its admission of downloads, its
// downloads, and the lanes their reports run on (hosted_report_lanes.go).
// A report is received and processed apart. Receipt, in Report, takes no
// engine lock (a create's or clear's answer goes to its waiter under
// hostWaiters' own lock) and does three things in one hold of this
// record's lock, waiting for nothing: a
// pending popup's close or crash is recorded in the Manager's popup
// admission (engineEvents.PageEndReceived), first, so no listing commits
// after the page is sealed; a close or a discard seals the page's
// admission, so nothing the launcher reports after it starts a download;
// and the page's attach, if one runs, is signaled to end, its teardown
// joined by the attach's own caller.
// Processing, on the page's lane in the order received, does the effects:
// a snapshot's path conversion and the Manager's download events, and a
// close's or discard's settlement of the page's downloads, which runs after
// every report accepted before it. Whether a download is gone is decided
// by lane order: one whose record left it is gone.
//
// A download is the launcher's by page and id (hostedDownloadKey): a report
// is applied only to the download its page owns, and one naming an id
// another page owns is refused.

// hostedDownloadKey is a hosted download's identity: the page that
// reported it and the launcher's id for it.
type hostedDownloadKey struct {
	page ids.PageHandle
	id   downloadID
}

// hostedPageState is where a hosted page stands in the launcher's reports.
type hostedPageState int

const (
	// hostedPageCreating is a page the engine is creating, from its create
	// directive until its bind: it admits nothing, and its close, received
	// once the launcher reported it created, ends it before its bind, which
	// then fails.
	hostedPageCreating hostedPageState = iota + 1
	// hostedPopupPending is a popup the launcher reported that the Manager
	// has not listed: it admits downloads, its work runs on its opener's
	// lane, and its downloads name it as a popup.
	hostedPopupPending
	// hostedPageOpen is a page the engine created and bound, or a popup the
	// Manager listed: it admits downloads and its work runs on its own lane.
	hostedPageOpen
	// hostedPopupClosing is a pending popup whose close or discard was
	// received: sealed, its work still on its opener's lane.
	hostedPopupClosing
	// hostedPageClosing is a page being created or open whose close or
	// discard was received: sealed. A record stays closing until its lane's
	// retirement of it has returned (retire).
	hostedPageClosing
)

func (s hostedPageState) String() string {
	switch s {
	case hostedPageCreating:
		return "creating"
	case hostedPopupPending:
		return "pending popup"
	case hostedPageOpen:
		return "open"
	case hostedPopupClosing:
		return "closing pending popup"
	case hostedPageClosing:
		return "closing"
	default:
		panic(fmt.Sprintf("browser: hosted page state %d", int(s)))
	}
}

// admits reports whether the page admits a download's first snapshot.
func (s hostedPageState) admits() bool {
	switch s {
	case hostedPopupPending, hostedPageOpen:
		return true
	case hostedPageCreating, hostedPopupClosing, hostedPageClosing:
		return false
	default:
		panic(fmt.Sprintf("browser: hosted page state %d", int(s)))
	}
}

// pendingPopup reports whether the page is a popup the Manager has not
// listed: its work runs on its opener's lane and its downloads name it.
func (s hostedPageState) pendingPopup() bool {
	switch s {
	case hostedPopupPending, hostedPopupClosing:
		return true
	case hostedPageCreating, hostedPageOpen, hostedPageClosing:
		return false
	default:
		panic(fmt.Sprintf("browser: hosted page state %d", int(s)))
	}
}

// hostedPageEvent is what moves a page's state.
type hostedPageEvent int

const (
	// pageBound: the engine bound the page it was creating.
	pageBound hostedPageEvent = iota + 1
	// pageListed: the Manager listed the popup.
	pageListed
	// pageEnding: its close or discard was received.
	pageEnding
)

// next answers the state event moves s to, and false for an event that
// does not apply to s: a page bound after its close or not being created,
// a page listed twice, or ending twice.
func (s hostedPageState) next(event hostedPageEvent) (hostedPageState, bool) {
	switch event {
	case pageBound:
		switch s {
		case hostedPageCreating:
			return hostedPageOpen, true
		case hostedPopupPending, hostedPageOpen, hostedPopupClosing, hostedPageClosing:
			return s, false
		}
	case pageListed:
		switch s {
		case hostedPopupPending:
			return hostedPageOpen, true
		case hostedPopupClosing:
			return hostedPageClosing, true
		case hostedPageCreating, hostedPageOpen, hostedPageClosing:
			return s, false
		}
	case pageEnding:
		switch s {
		case hostedPageCreating, hostedPageOpen:
			return hostedPageClosing, true
		case hostedPopupPending:
			return hostedPopupClosing, true
		case hostedPopupClosing, hostedPageClosing:
			return s, false
		}
	}
	panic(fmt.Sprintf("browser: hosted page event %d in state %d", int(event), int(s)))
}

// hostedPageRecord is one page from its create directive or popup report
// until its lane's retirement of it has returned. Guarded by
// hostedPages.mu.
type hostedPageRecord struct {
	state hostedPageState
	// opener and profile are a popup's.
	opener  ids.PageHandle
	profile profileHandle
	// downloads are the page's running downloads, admitted or with their
	// admission in flight; ended its ended or refused ids, oldest first.
	downloads map[downloadID]*hostedDownload
	ended     []downloadID
	// retired is closed once the page's lane retired it, the retirement's
	// callbacks returned, or at Stop.
	retired chan struct{}
	// stopAttach signals a popup's attach under way to end (attaching); it
	// waits for nothing, and the attach joins its own teardown.
	stopAttach context.CancelFunc
}

// to moves the record's state by event, its one writer, and answers
// whether it moved.
func (r *hostedPageRecord) to(event hostedPageEvent) bool {
	next, ok := r.state.next(event)
	r.state = next
	return ok
}

// hostedPages is the engine's record of its pages and their downloads, and
// the lanes their reports run on. Its fields are written only by its
// methods, under mu; the engine's own lock may be held while one is
// called, never the reverse.
type hostedPages struct {
	mu      sync.Mutex
	records map[ids.PageHandle]*hostedPageRecord
	// owners is the page of each download id a record holds, running or
	// ended, so a report naming another page's id is refused.
	owners map[downloadID]ids.PageHandle
	lanes  *reportLanes
	// process runs one accepted report on its lane; watch starts the
	// status loop, which runs until stop closes or no download runs;
	// observe records a pending popup's end in the Manager's admission
	// (engineEvents.PageEndReceived) and takes no lock of the engine's.
	process  func(context.Context, *acceptedWork)
	watch    func(stop <-chan struct{})
	observe  func(ids.PageHandle, popupadmit.Observation)
	watching bool
	stop     chan struct{}
}

func newHostedPages(process func(context.Context, *acceptedWork), watch func(stop <-chan struct{}), observe func(ids.PageHandle, popupadmit.Observation)) *hostedPages {
	return &hostedPages{
		records: make(map[ids.PageHandle]*hostedPageRecord),
		owners:  make(map[downloadID]ids.PageHandle),
		lanes:   newReportLanes(),
		process: process,
		watch:   watch,
		observe: observe,
	}
}

func newPageRecord(state hostedPageState, opener ids.PageHandle, profile profileHandle) *hostedPageRecord {
	return &hostedPageRecord{state: state, opener: opener, profile: profile, downloads: make(map[downloadID]*hostedDownload), retired: make(chan struct{})}
}

// errHostedPageEnded refuses a step of a page being created whose close
// was received, or that has no record.
var errHostedPageEnded = errors.New("browser: the pane host closed the page before it was ready")

// addCreating records page, which the engine is about to ask the launcher
// to create, as creating, and answers false when page has a record.
func (p *hostedPages) addCreating(page ids.PageHandle) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.records[page] != nil {
		return false
	}
	p.records[page] = newPageRecord(hostedPageCreating, "", "")
	return true
}

// creating answers errHostedPageEnded unless page is still being created.
func (p *hostedPages) creating(page ids.PageHandle) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r := p.records[page]; r == nil || r.state != hostedPageCreating {
		return errHostedPageEnded
	}
	return nil
}

// bound moves page, being created, to open, from which it admits
// downloads, and refuses with errHostedPageEnded a page whose close was
// received or that has no record.
func (p *hostedPages) bound(page ids.PageHandle) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r := p.records[page]; r == nil || !r.to(pageBound) {
		return errHostedPageEnded
	}
	return nil
}

// abandon drops page, being created, whose create the launcher answered
// with no page; a record past creating stays for its lane.
func (p *hostedPages) abandon(page ids.PageHandle) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r := p.records[page]; r != nil && r.state == hostedPageCreating {
		delete(p.records, page)
		close(r.retired)
	}
}

// addPopup records page, a popup opener opened in profile, as pending,
// and answers false when page has a record.
func (p *hostedPages) addPopup(page, opener ids.PageHandle, profile profileHandle) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.records[page] != nil {
		return false
	}
	p.records[page] = newPageRecord(hostedPopupPending, opener, profile)
	return true
}

// admits reports whether page has a record that admits downloads: a page
// whose close or discard was received opens no popup either.
func (p *hostedPages) admits(page ids.PageHandle) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.records[page]
	return r != nil && r.state.admits()
}

// attaching records stop, which ends the attach of pending popup page, for
// the page's close or discard to signal, and answers false, stop unkept,
// for a page that no longer admits: its close or discard was received.
func (p *hostedPages) attaching(page ids.PageHandle, stop context.CancelFunc) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.records[page]
	if r == nil || !r.state.admits() {
		return false
	}
	r.stopAttach = stop
	return true
}

// attached forgets page's attach, which returned.
func (p *hostedPages) attached(page ids.PageHandle) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r := p.records[page]; r != nil {
		r.stopAttach = nil
	}
}

// listed records the Manager's listing of popup page: its later work runs
// on its own lane, once the work it already has drains, and its downloads
// are its tab's.
func (p *hostedPages) listed(page ids.PageHandle) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r := p.records[page]; r != nil && r.to(pageListed) {
		p.gateLocked(page)
	}
}

// snapshotReceipt is what acceptSnapshot did with a snapshot.
type snapshotReceipt int

const (
	// snapshotAccepted: queued on its page's lane.
	snapshotAccepted snapshotReceipt = iota + 1
	// snapshotNoPage: no record has the page (never bound, or retired).
	snapshotNoPage
	// snapshotOtherPage: another page owns the download id.
	snapshotOtherPage
	// snapshotEnded: the download ended here.
	snapshotEnded
	// snapshotUnasked: an answer about a download the page does not have,
	// which creates nothing.
	snapshotUnasked
	// snapshotSealed: a first snapshot on a page that admits no download.
	snapshotSealed
)

// acceptSnapshot receives one snapshot of a download of page: in one hold
// it checks the download's owner, starts a download its page admits, and
// queues the snapshot on the page's lane.
func (p *hostedPages) acceptSnapshot(page ids.PageHandle, report webview2host.DownloadReport) snapshotReceipt {
	id := downloadID(report.ID)
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.records[page]
	if r == nil {
		return snapshotNoPage
	}
	if owner, known := p.owners[id]; known && owner != page {
		return snapshotOtherPage
	}
	d := r.downloads[id]
	if d == nil {
		switch {
		case r.endedHas(id):
			return snapshotEnded
		case report.Answer:
			return snapshotUnasked
		case !r.state.admits():
			return snapshotSealed
		}
		d = &hostedDownload{key: hostedDownloadKey{page: page, id: id}, admission: hostedAnnouncing}
		r.downloads[id] = d
		p.owners[id] = page
		p.watchLocked()
	}
	d.heard = time.Now()
	p.submitLocked(&acceptedWork{kind: acceptedSnapshot, page: page, snapshot: report, download: d})
	return snapshotAccepted
}

// endReceipt is what acceptEnd did with a page's close, discard or crash.
type endReceipt struct {
	// accepted: queued on the page's lane; a close or discard sealed it.
	accepted bool
	// pendingPopup: the page is a popup the Manager has not listed.
	pendingPopup bool
	// retired closes once the page's lane retired it and the retirement's
	// callbacks returned; nil for a page with no record, which is not this
	// engine's or has retired.
	retired <-chan struct{}
}

// acceptEnd receives a page's end of kind (acceptedClose, acceptedDiscard
// or acceptedCrash). A pending popup's close or crash is recorded in the
// Manager's admission first, in the hold that seals the page, so the
// listing's commit reads it whenever the page is seen sealed. A close or
// discard then seals its admission, signals its attach to end and queues
// its retirement behind its work already accepted, once; a crash queues
// the Manager's report of it.
func (p *hostedPages) acceptEnd(page ids.PageHandle, kind acceptedKind, crash string) endReceipt {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.records[page]
	if r == nil {
		return endReceipt{}
	}
	receipt := endReceipt{pendingPopup: r.state.pendingPopup(), retired: r.retired}
	switch kind {
	case acceptedClose:
		if receipt.pendingPopup && r.state.admits() {
			p.observe(page, popupadmit.Closed())
		}
	case acceptedCrash:
		if receipt.pendingPopup {
			p.observe(page, popupadmit.Crashed(crash))
		}
	case acceptedDiscard:
	case acceptedSnapshot:
		panic("browser: a snapshot is no page end")
	default:
		panic(fmt.Sprintf("browser: a page end of kind %d", kind))
	}
	switch kind {
	case acceptedClose, acceptedDiscard:
		if !r.to(pageEnding) {
			return receipt
		}
		if r.stopAttach != nil {
			r.stopAttach()
			r.stopAttach = nil
		}
	case acceptedCrash:
	case acceptedSnapshot:
		panic("browser: a snapshot is no page end")
	default:
		panic(fmt.Sprintf("browser: a page end of kind %d", kind))
	}
	receipt.accepted = true
	p.submitLocked(&acceptedWork{kind: kind, page: page, crash: crash})
	return receipt
}

// retire ends page's record on its lane, after every report accepted
// before its close or discard: it answers the downloads the Manager
// admitted that still run, and finish, which the caller calls once it has
// settled them. The record stays, closing and with no download, until
// finish drops it and wakes whoever waits for the page to go, so a close
// received meanwhile joins this one.
func (p *hostedPages) retire(page ids.PageHandle) (running []downloadID, finish func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.records[page]
	if r == nil {
		return nil, func() {}
	}
	for id, d := range r.downloads {
		if d.admission == hostedAdmitted {
			running = append(running, id)
		}
		p.dropOwnerLocked(id, page)
	}
	for _, id := range r.ended {
		p.dropOwnerLocked(id, page)
	}
	r.downloads, r.ended = make(map[downloadID]*hostedDownload), nil
	return running, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		// Stop may have woken its waiters and dropped it first.
		if p.records[page] == r {
			delete(p.records, page)
			close(r.retired)
			if r.state.pendingPopup() {
				// Its popups' lane key turns to its page id.
				p.gateLocked(page)
			}
		}
	}
}

func (p *hostedPages) dropOwnerLocked(id downloadID, page ids.PageHandle) {
	if p.owners[id] == page {
		delete(p.owners, id)
	}
}

func (r *hostedPageRecord) endedHas(id downloadID) bool {
	for _, ended := range r.ended {
		if ended == id {
			return true
		}
	}
	return false
}

// stopAll forgets every page and download and ends the status loop and
// the lanes (Stop): work still waiting is dropped, a path conversion under
// way ends, and whoever waits for a page to go is woken. It answers the
// wait for the lanes' goroutines, which the caller makes once it holds no
// lock.
func (p *hostedPages) stopAll() func() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stop != nil {
		close(p.stop)
		p.stop = nil
	}
	p.watching = false
	for _, r := range p.records {
		if r.stopAttach != nil {
			r.stopAttach()
		}
		close(r.retired)
	}
	p.records = make(map[ids.PageHandle]*hostedPageRecord)
	p.owners = make(map[downloadID]ids.PageHandle)
	return p.stopLanesLocked()
}
