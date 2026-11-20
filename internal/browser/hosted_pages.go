package browser

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/webview2host"
)

// The hosted engine's pages as the launcher's reports see them
// (hostedPages): each page's lifecycle, its admission of downloads, its
// downloads, and the lanes their reports run on (hosted_report_lanes.go).
// A report is received and processed apart. Receipt (receive) records
// what the Manager's own actions meanwhile must find, takes no engine lock
// and waits for nothing (a create's or clear's answer goes to its waiter
// under hostWaiters' own lock). Processing, on the page's lane in the
// order received, does the effects:
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
	// retired is closed once the page's lane retired it and the
	// retirement's callbacks returned, or as a page being created is
	// abandoned. Stop does not close it (receipt.stopped).
	retired chan struct{}
	// stopAttach signals a popup's attach under way to end (attaching); it
	// waits for nothing, and the attach joins its own teardown.
	stopAttach context.CancelFunc
}

// pageDebt is what the engine owes a page in callbacks: a retirement under
// way, until its last callback returned (retire), each received end the
// page's lane took, until the lane delivered it (takeEnd, end), and each
// one Stop took from the lane, until Stop delivered it (stopAll). It is
// the one record a close or discard of a page Stop forgot reads (receive),
// so the discard succeeds only once every callback owed to the page
// returned, and hears it was interrupted until then, whatever the page
// was doing when Stop took over and whenever the discard comes.
type pageDebt struct {
	owed int
	// paid closes when owed reaches zero, and stopped once Stop forgot the
	// page's record.
	paid    chan struct{}
	stopped <-chan struct{}
}

// oweLocked adds n callbacks to what the engine owes page. The caller
// holds p.mu.
func (p *hostedPages) oweLocked(page ids.PageHandle, n int) {
	if n == 0 {
		return
	}
	d := p.debts[page]
	if d == nil {
		d = &pageDebt{paid: make(chan struct{})}
		p.debts[page] = d
	}
	d.owed += n
}

// payLocked records one owed callback to page returned. The caller holds
// p.mu.
func (p *hostedPages) payLocked(page ids.PageHandle) {
	d := p.debts[page]
	if d == nil {
		panic(fmt.Sprintf("browser: paid a callback page %s was not owed", page))
	}
	if d.owed--; d.owed == 0 {
		close(d.paid)
		delete(p.debts, page)
	}
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
	// debts are the callbacks the engine owes each page and has not
	// returned from (pageDebt).
	debts map[ids.PageHandle]*pageDebt
	// owners is the page of each download id a record holds, running or
	// ended, so a report naming another page's id is refused.
	owners map[downloadID]ids.PageHandle
	lanes  *reportLanes
	// process runs one accepted report on its lane; watch starts the
	// status loop, which runs until stop closes or no download runs;
	// observe records a page's received close or crash with the Manager
	// (engineEvents.PageEndReceived), with a pending popup's openers
	// (pendingOpeners), called with no lock of the engine's.
	process  func(context.Context, *acceptedWork)
	watch    func(stop <-chan struct{})
	observe  func(page ids.PageHandle, closed bool, crash string, openers []ids.PageHandle)
	watching bool
	stop     chan struct{}
	// receivedDownloads numbers the downloads in the order their first
	// snapshot was received (hostedDownload.order).
	receivedDownloads uint64
}

func newHostedPages(process func(context.Context, *acceptedWork), watch func(stop <-chan struct{}), observe func(page ids.PageHandle, closed bool, crash string, openers []ids.PageHandle)) *hostedPages {
	return &hostedPages{
		records: make(map[ids.PageHandle]*hostedPageRecord),
		debts:   make(map[ids.PageHandle]*pageDebt),
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

// received is one report the engine receives for a page's lane: a download
// snapshot, the launcher's close, the engine's own discard, or the process
// failure that ended the page.
type received struct {
	kind     acceptedKind
	snapshot webview2host.DownloadReport
	crash    string
}

func receivedSnapshot(report webview2host.DownloadReport) received {
	return received{kind: acceptedSnapshot, snapshot: report}
}

func receivedClose() received   { return received{kind: acceptedClose} }
func receivedDiscard() received { return received{kind: acceptedDiscard} }

func receivedCrash(reason string) received {
	return received{kind: acceptedCrash, crash: reason}
}

// receiptOutcome is what receive did with a report.
type receiptOutcome int

const (
	// receiptQueued: queued on its page's lane.
	receiptQueued receiptOutcome = iota + 1
	// receiptNoPage: no record has the page (never this engine's, or
	// retired).
	receiptNoPage
	// receiptOtherPage: a snapshot naming a download id another page owns.
	receiptOtherPage
	// receiptDownloadEnded: a snapshot of a download that ended here.
	receiptDownloadEnded
	// receiptUnasked: an answer about a download the page does not have,
	// which creates nothing.
	receiptUnasked
	// receiptSealed: a first snapshot on a page that admits no download.
	receiptSealed
	// receiptEnding: a close or discard of a page whose close or discard
	// was received before, which that one retires.
	receiptEnding
)

// receipt is what receive answers: its outcome, and for a page's end,
// retired, which closes once the page's lane retired it and the
// retirement's last callback returned, and stopped, which closes at Stop,
// after which a retirement not under way never runs; both nil for a page
// with no record.
type receipt struct {
	outcome receiptOutcome
	retired <-chan struct{}
	stopped <-chan struct{}
}

// errDiscardInterrupted is the answer of a wait for a page's retirement
// that Stop ended first: the engine forgot the page, and its retirement
// either never runs or finishes after the wait returned.
var errDiscardInterrupted = errors.New("the engine stopped before the page's lane retired it")

// wait waits within ctx for the retirement r names: nil once it returned
// or for a page with no record, errDiscardInterrupted when Stop came
// first, and errDiscardPending past ctx.
func (r receipt) wait(ctx context.Context) error {
	if r.retired == nil {
		return nil
	}
	select {
	case <-r.retired:
		return nil
	default:
	}
	select {
	case <-r.retired:
		return nil
	case <-r.stopped:
		return errDiscardInterrupted
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", errDiscardPending, ctx.Err())
	}
}

// receive is the receipt of every report of a page that its lane processes
// (hosted_report_lanes.go). It records what each Manager action that can
// come before that processing must find (hosted_receipt_test.go walks each
// pair):
//
//   - A close or crash goes to the Manager first (observe, which is
//     engineEvents.PageEndReceived), for every page: a pending popup's
//     listing and a created or restored page's listing read it
//     (popupadmit, creation_end.go), so neither lists a page whose close
//     was received, and one whose crash was is listed crashed. It is made
//     outside p.mu, which the Manager takes under its own lock, and before
//     the seal, so whoever sees the page sealed finds it recorded.
//   - A close or a discard seals the page's admission, signals its attach
//     to end and queues its retirement behind the work already received,
//     once; a crash queues the Manager's report of it.
//   - A snapshot checks the download's owner and starts a download its page
//     admits, with its address and name, numbered in the order received,
//     and keeps the counts of the newest numbered one: the page's leave
//     lists it from that record (takeReceived) when the lane has not
//     admitted it yet, so a download received before its tab closed, was
//     suspended or the browser closed is listed in that tab, and a row the
//     leave cancels keeps those counts (downloadStart.NewestCounts). A
//     popup's received close hands its downloads to its nearest listed
//     opener's leave the same way: the Manager holds that ownership from
//     the close's record, which names the popup's openers, until it
//     settles the popup's parked downloads (popupCarrier). A terminal
//     snapshot, or the launcher's
//     DownloadUnknown, marks the download's end received, which then
//     settles its row (downloadStart.EndReported) whatever left the page
//     meanwhile.
//
// A report the engine processes in full at receipt (a create's or clear's
// answer, a popup, a key chord, a focus) does not come here. Nothing here
// takes the engine's lock or waits.
func (p *hostedPages) receive(page ids.PageHandle, in received) receipt {
	switch in.kind {
	case acceptedClose:
		p.observe(page, true, "", p.pendingOpeners(page))
	case acceptedCrash:
		p.observe(page, false, in.crash, nil)
	case acceptedSnapshot, acceptedDiscard:
	default:
		panic(fmt.Sprintf("browser: received a report of kind %d", in.kind))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.records[page]
	if r == nil {
		// A page Stop forgot whose callbacks are still owed.
		if d := p.debts[page]; d != nil && in.kind != acceptedSnapshot && in.kind != acceptedCrash {
			return receipt{outcome: receiptEnding, retired: d.paid, stopped: d.stopped}
		}
		return receipt{outcome: receiptNoPage}
	}
	if in.kind == acceptedSnapshot {
		return receipt{outcome: p.snapshotLocked(page, r, in.snapshot)}
	}
	ended := receipt{outcome: receiptQueued, retired: r.retired, stopped: p.lanes.ctx.Done()}
	if in.kind != acceptedCrash {
		pending := r.state == hostedPopupPending
		if !r.to(pageEnding) {
			ended.outcome = receiptEnding
			return ended
		}
		if pending {
			p.popupEndingLocked(r, in.kind)
		}
		if r.stopAttach != nil {
			r.stopAttach()
			r.stopAttach = nil
		}
	}
	p.submitLocked(&acceptedWork{kind: in.kind, page: page, crash: in.crash})
	return ended
}

// popupEndingLocked records the end of r, a popup the Manager has not
// listed whose close or discard of kind was received, as the end of each
// of its downloads whose own end was not received: the launcher cancels
// them with the page, and its retirement delivers that (retire), so the
// leave of the page that takes them waits for it (downloadStart.EndReported)
// and it reads as the popup's close or discard whatever left meanwhile.
// The caller holds p.mu.
func (p *hostedPages) popupEndingLocked(r *hostedPageRecord, kind acceptedKind) {
	why := pageClosedReason
	if kind == acceptedDiscard {
		why = popupDiscardedReason
	}
	for _, d := range r.downloads {
		if d.ending == endNotReceived {
			d.ending, d.pageEnd = endReceived, why
		}
	}
}

// snapshotLocked receives one snapshot of a download of page, whose record
// is r: it checks the download's owner, starts a download its page admits,
// and queues the snapshot on the page's lane. The caller holds p.mu.
func (p *hostedPages) snapshotLocked(page ids.PageHandle, r *hostedPageRecord, report webview2host.DownloadReport) receiptOutcome {
	id := downloadID(report.ID)
	if owner, known := p.owners[id]; known && owner != page {
		return receiptOtherPage
	}
	d := r.downloads[id]
	if d == nil {
		switch {
		case r.endedHas(id):
			return receiptDownloadEnded
		case report.Answer:
			return receiptUnasked
		case !r.state.admits():
			return receiptSealed
		}
		p.receivedDownloads++
		d = &hostedDownload{key: hostedDownloadKey{page: page, id: id}, admission: hostedAnnouncing, order: p.receivedDownloads}
		r.downloads[id] = d
		p.owners[id] = page
		p.watchLocked()
	}
	if d.url == "" {
		d.url = report.URI
	}
	if d.name == "" {
		d.name = report.Name
		if d.name == "" {
			d.name = windowsBase(report.Path)
		}
	}
	d.heard = time.Now()
	if report.Seq > d.seq {
		d.seq, d.received, d.total = report.Seq, report.Received, report.Total
	}
	if endsTracking(report) && d.ending == endNotReceived {
		d.ending = endReceived
	}
	p.submitLocked(&acceptedWork{kind: acceptedSnapshot, page: page, snapshot: report, download: d})
	return receiptQueued
}

// retire ends page's record on its lane, after every report accepted
// before its close or discard: it answers the ends, for why, of the
// downloads the Manager admitted that still run, at their newest counts,
// but for one whose received page end Stop took and delivers (stopAll),
// and finish, which the caller calls once it has delivered them. The
// record stays, closing and with no download, until finish drops it and
// wakes whoever waits for the page to go, so a close received meanwhile
// joins this one.
func (p *hostedPages) retire(page ids.PageHandle, why string) (gone []downloadProgress, finish func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.records[page]
	if r == nil {
		return nil, func() {}
	}
	p.oweLocked(page, 1)
	for id, d := range r.downloads {
		switch {
		case d.admission != hostedAdmitted:
		case d.pageEnd == "":
			gone = append(gone, pageGoneLocked(d, why))
		case d.ending == endReceived:
			d.ending = endTaken
			gone = append(gone, pageGoneLocked(d, d.pageEnd))
		}
		p.dropOwnerLocked(id, page)
	}
	for _, id := range r.ended {
		p.dropOwnerLocked(id, page)
	}
	r.downloads, r.ended = make(map[downloadID]*hostedDownload), nil
	return gone, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		close(r.retired)
		p.payLocked(page)
		// Stop may have forgotten it first.
		if p.records[page] == r {
			delete(p.records, page)
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
// the lanes (Stop): work still waiting is dropped, a retirement still
// queued never runs, a path conversion under way ends, and whoever waits
// for a page to go is told it was interrupted (receipt.stopped); a
// retirement under way still closes its record's retired when its last
// callback returns. It answers the wait for the lanes' goroutines, which
// the caller makes once it holds no lock, and the received ends its lanes
// have not delivered, in each lane's order, the one under way first,
// which the Manager may be waiting for (downloadStart.EndReported): one a
// lane already took is that lane's to deliver (takeEnd). A download's
// admission may still be in flight on its lane, so an end is taken
// whether or not it was admitted; the Manager drops the end of a download
// it has no row for. Each end is owed to its page (pageDebt) until the
// caller, having delivered it, calls paid with it, so a close or discard
// of the page reads the debt as a retirement under way does.
func (p *hostedPages) stopAll() (join func(), ends []stopEnd, paid func(stopEnd)) {
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
	}
	take := func(w *acceptedWork) {
		if w == nil || w.kind != acceptedSnapshot || w.download.ending != endReceived || !endsTracking(w.snapshot) {
			return
		}
		w.download.ending = endTaken
		end := stopEnd{page: w.download.key.page, progress: receivedProgress(w.snapshot)}
		if w.snapshot.State == webview2host.DownloadUnknown {
			end.progress = untrackedLocked(w.download, w.snapshot.Reason)
		}
		ends = append(ends, end)
	}
	for _, lane := range p.lanes.byKey {
		take(lane.current)
		for _, w := range lane.jobs {
			take(w)
		}
	}
	// The received end of a pending popup's page, whose retirement no lane
	// runs now.
	for _, r := range p.records {
		for _, d := range r.downloads {
			if d.pageEnd != "" && d.ending == endReceived {
				d.ending = endTaken
				ends = append(ends, stopEnd{page: d.key.page, progress: pageGoneLocked(d, d.pageEnd)})
			}
		}
	}
	for _, end := range ends {
		p.oweLocked(end.page, 1)
	}
	stopped := p.lanes.ctx.Done()
	for _, d := range p.debts {
		d.stopped = stopped
	}
	p.records = make(map[ids.PageHandle]*hostedPageRecord)
	p.owners = make(map[downloadID]ids.PageHandle)
	return p.stopLanesLocked(), ends, func(end stopEnd) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.payLocked(end.page)
	}
}

// stopEnd is a received end Stop took from its page's lane, which it
// delivers in the lane's stead.
type stopEnd struct {
	page     ids.PageHandle
	progress downloadProgress
}
