package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agent-overflow/internal/pump"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// boardPump is a board's pump: one goroutine and one timer for every
// handle on the board's scope, plus the retry timer while a tick fails.
// Its core and the fields below it are under subMu.
type boardPump struct {
	core  pump.Core[readRequest]
	scope Scope
	// gen is the mirror generation the pump shows (genLocked).
	gen uint64
	// every is the delta interval of the connection's kind.
	every time.Duration
	// exited closes when the goroutine has exited, or at once when it
	// never starts (launchLocked).
	exited chan struct{}
	// pass is the reconcile pass the pump's ticks go on with (run.pass),
	// read and written only by those ticks, under their scope's lock. It
	// holds at most the candidates of one listing, and goes with the pump:
	// only a pump's tick reconciles (syncBoard).
	pass reconcilePass

	// pending is what the next event carries; sync is the board's sync
	// view as the last tick recorded it; internal is the failure of the
	// last tick when it was not the tracker's (a store error), which the
	// sync state cannot record; failure is the failure the subscribers
	// were last told of (told).
	pending  boardPending
	sync     SyncView
	internal *FailureView
	failure  *FailureView
	// over is set once the pump ended and its subscribers were told, or
	// were not to be.
	over bool
}

// boardPending is what an event of the board will carry.
type boardPending struct {
	changed, removed     []tracker.IssueID
	readAgain, truncated bool
	// due reports something to send: a change or a failure that started,
	// changed or cleared.
	due bool
}

// overflows reports that adding changed and removed would pass an
// event's bound while it already holds rows, so what it holds is to be
// sent first and the event stays one a patch reads. Rows that fit by
// their counts fit; only past that are the unions counted.
func (b *boardPending) overflows(changed, removed []tracker.IssueID) bool {
	held := len(b.changed) + len(b.removed)
	if b.truncated || held == 0 || held+len(changed)+len(removed) <= maxEventIssues {
		return false
	}
	return len(unite(b.changed, changed))+len(unite(b.removed, removed)) > maxEventIssues
}

// add merges changed and removed rows of the board, and readAgain.
func (b *boardPending) add(changed, removed []tracker.IssueID, readAgain bool) {
	if len(changed)+len(removed) == 0 && !readAgain {
		return
	}
	b.due = true
	b.readAgain = b.readAgain || readAgain
	if b.truncated {
		return
	}
	b.changed, b.removed = unite(b.changed, changed), unite(b.removed, removed)
	if len(b.changed)+len(b.removed) > maxEventIssues {
		b.changed, b.removed, b.truncated, b.readAgain = nil, nil, true, true
	}
}

// BoardSubscription is a board subscribe's answer: the handle, the
// sequence number the answer is as of, and the board view of the sections
// asked for when the mirror holds the board's structure (Loaded), or no
// view: the pump's first tick reads it, and its event says so. Failure is
// the board's failure as its subscribers were last told of it, which the
// view's sync state carries too. PollEvery is the pump's delta interval in
// milliseconds: while no failure is reported, the board was read within
// it, so the last read time needs no event of its own.
type BoardSubscription struct {
	ID        string       `json:"id"`
	Seq       uint64       `json:"seq"`
	Loaded    bool         `json:"loaded"`
	View      *BoardView   `json:"view,omitempty"`
	Failure   *FailureView `json:"failure,omitempty"`
	PollEvery int64        `json:"poll_every"`
}

// SubscribeBoard subscribes to scope's board for project projectID, which
// must list it (ErrNotListed), with the first page of each of sections in
// the answer (none asks for the Board screen's, as Board reads them),
// read with the status groups the project gives the board's site
// (StatusGroups), which the handle's later reads apply too. The handle
// starts with visible as its vote (SetSubscriptionVisible). The answer
// comes from the mirror alone; the board's pump, made on the first handle,
// reads the tracker after it, its first tick interactive, once a handle
// shows it (pump.Core.AwaitShown). A connection turned off or removed is
// refused (ErrDisabled, ErrNotFound), as is a subscribe over the cap
// (ErrTooManySubscriptions) and one whose project's setup file cannot be
// read (ErrSetupUnreadable).
// tie ties the handle to its client before any pump is made for it; its
// refusal is the subscribe's.
func (s *Service) SubscribeBoard(projectID string, scope Scope, sections []Section, visible bool, tie Tie) (BoardSubscription, error) {
	if err := scope.boardOnly(); err != nil {
		return BoardSubscription{}, err
	}
	if tie == nil {
		return BoardSubscription{}, fmt.Errorf("%w: a board subscribe ties its handle to its client", ErrInvalidRequest)
	}
	if projectID == "" || len(sections) > maxViewSections {
		return BoardSubscription{}, fmt.Errorf("%w: a board subscribe names its project and at most %d sections", ErrInvalidRequest, maxViewSections)
	}
	for _, sec := range sections {
		if _, err := sec.mirror(); err != nil {
			return BoardSubscription{}, err
		}
	}
	if err := s.enter(); err != nil {
		return BoardSubscription{}, err
	}
	defer s.leave()
	slug, listed, err := s.store.ProjectTrackerBoardListed(projectID, scope.Connection, string(scope.Board))
	switch {
	case err != nil:
		return BoardSubscription{}, fmt.Errorf("trackerapp: read the boards of project %s: %w", projectID, err)
	case !listed:
		return BoardSubscription{}, fmt.Errorf("%w: project %s, board %s of %s", ErrNotListed, projectID, scope.Board, scope.Connection)
	}
	id, p, start, seq, told, key, err := s.takeBoardHandle(projectID, scope, slug, visible, tie)
	if err != nil {
		return BoardSubscription{}, err
	}
	answer := BoardSubscription{ID: id, Seq: seq, PollEvery: p.every.Milliseconds()}
	var view BoardView
	groups, err := s.boardStatusGroups(key)
	if err == nil {
		view, err = s.Board(scope, groups, sections)
	}
	switch {
	case err == nil:
		answer.Loaded, answer.View = true, &view
		if start {
			answer.Failure = view.Sync.Failure
		} else {
			// The subscribers were told the sync state the pump holds, as of
			// seq.
			answer.View.Sync, answer.Failure = told, told.Failure
		}
	case !errors.Is(err, ErrNotMirrored):
		s.ReleaseSubscription(id)
		answer = BoardSubscription{}
	default:
		answer.Failure = told.Failure
	}
	s.subMu.Lock()
	defer s.subMu.Unlock()
	if start {
		if answer.Loaded {
			p.sync = view.Sync
			p.failure = view.Sync.Failure
		}
		s.launchLocked(&p.core, p.exited, func() { s.runBoardPump(p) })
	}
	if answer.ID == "" {
		return answer, err
	}
	return answer, nil
}

// takeBoardHandle takes a handle on scope's pump for the project slug
// names, tied by tie and voting visible, making the pump when there is
// none; start reports that the caller launches it. seq is the answer's
// sequence number, told the board's sync state as the pump's subscribers
// were last told of it then, and key the status groups the handle reads
// with. The connection is read under opMu, so a turn off orders entirely
// before (refused) or after (the pump ends).
func (s *Service) takeBoardHandle(projectID string, scope Scope, slug string, visible bool, tie Tie) (id string, p *boardPump, start bool, seq uint64,
	told SyncView, key setupKey, err error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	row, err := s.subscribableLocked(scope.Connection)
	if err != nil {
		return "", nil, false, 0, SyncView{}, setupKey{}, err
	}
	key = setupKey{slug: slug, site: siteKey{row.Kind, row.Site}}
	s.subMu.Lock()
	defer s.subMu.Unlock()
	switch {
	case s.subs.halted:
		return "", nil, false, 0, SyncView{}, setupKey{}, ErrStopped
	case s.subs.boardHandles >= maxBoardSubscriptions:
		return "", nil, false, 0, SyncView{}, setupKey{}, fmt.Errorf("%w: %d board subscriptions", ErrTooManySubscriptions, maxBoardSubscriptions)
	}
	id = newHandleID()
	untie, err := tie(id)
	if err != nil {
		return "", nil, false, 0, SyncView{}, setupKey{}, err
	}
	p = s.subs.boards[scope]
	if p == nil {
		if s.subs.boards == nil {
			s.subs.boards = map[Scope]*boardPump{}
		}
		p = &boardPump{core: s.newCore(), scope: scope, gen: s.genLocked(scope.Connection), every: s.cadence.delta(row.Kind), exited: make(chan struct{})}
		s.subs.boards[scope] = p
		start = true
	}
	h := &handle{board: p, untie: untie, setup: key, project: projectID}
	s.takeHandleLocked(id, h, &p.core)
	h.ref.VoteLocked(visible)
	return id, p, start, s.nextSeqLocked(), p.toldLocked(), key, nil
}

// toldLocked is p's sync view as its subscribers were last told of it:
// the recorded state, with a failure outside the tracker in place of the
// recorded failure. The caller holds subMu.
func (p *boardPump) toldLocked() SyncView {
	v := p.sync
	if p.internal != nil {
		v.Failure = p.internal
	}
	return v
}

// subscribableLocked is connection id's row, refusing a subscribe when it
// is turned off, removed or a Tempo connection. A connection a tick holds is
// on: any change to it drops the hold. The caller holds opMu.
func (s *Service) subscribableLocked(id string) (store.TrackerConnection, error) {
	if h := s.heldLocked(id); h != nil {
		return h.row, nil
	}
	row, err := s.get(id)
	if err == nil {
		err = usable(row)
	}
	if err != nil {
		return store.TrackerConnection{}, err
	}
	return row, nil
}

// RefreshSubscription has handle id's pump read now, as a person's
// action: a board runs structure, delta, reconcile and history, a ticket
// reads its detail, each interactive. It returns when the read ran;
// what it found reaches the subscribers as events, the tracker's failure
// too, and it fails with a failure outside the tracker (a ticket's own,
// such as a view that could not be built) or the read's stop. A
// subscription that ends first fails with ErrSubscriptionEnded.
func (s *Service) RefreshSubscription(ctx context.Context, id string) error {
	s.subMu.Lock()
	h := s.subs.handles[id]
	var core *pump.Core[readRequest]
	if h != nil {
		core = h.core()
	}
	s.subMu.Unlock()
	if core == nil {
		return fmt.Errorf("%w: %s", ErrUnknownSubscription, id)
	}
	return core.Request(ctx, readRequest{})
}

// runBoardPump is a board pump's goroutine: an interactive first tick once
// a handle shows the board (pump.Core.AwaitShown), then a tick every
// Cadence.Delta while one does, at once again while a tick reports More,
// on a person's request, and on the retry schedule while ticks fail. It
// exits when the core stops.
func (s *Service) runBoardPump(p *boardPump) {
	defer s.workers.Done()
	defer close(p.exited)
	next := time.NewTimer(p.every)
	defer next.Stop()
	retry := pump.Retry{Base: s.cadence.RetryBase, Interval: p.every}
	missed := false
	// step runs a tick and re-arms the timers; again reports a tick to run
	// at once (More while shown), over that the pump ended.
	step := func(refresh bool, u Urgency) (again, over bool, res tickResult) {
		missed = false
		res = s.boardTick(p, refresh, u)
		retry.Arm(res.failed, res.retryAt)
		next.Reset(p.every)
		if res.over {
			return false, true, res
		}
		if res.more && !res.failed && p.core.Paused() {
			missed = true
		}
		return res.more && !res.failed && !p.core.Paused(), false, res
	}
	// serve runs a request: a person's refresh, whose tracker failure
	// reaches the subscribers alone, or the tick due now, whose caller is
	// handed it too.
	serve := func(req *pump.Request[readRequest]) (again, over bool) {
		now := req.Body().now
		again, over, res := step(!now, Interactive)
		err := res.err
		switch {
		case over:
			err = ErrSubscriptionEnded
		case now && err == nil && res.failure != nil:
			err = &failureError{f: *res.failure}
		}
		req.Finish(err)
		return again, over
	}
	var again, over bool
	switch req, stopped := p.core.AwaitShown(); {
	case stopped:
		return
	case req != nil:
		again, over = serve(req)
	default:
		again, over, _ = step(false, Interactive)
	}
	for !over {
		if again {
			select {
			case <-p.core.Done():
				return
			case req := <-p.core.Requests():
				again, over = serve(req)
			default:
				again, over, _ = step(false, Background)
			}
			continue
		}
		select {
		case <-p.core.Done():
			return
		case req := <-p.core.Requests():
			again, over = serve(req)
		case <-p.core.Wake():
			if !p.core.Paused() && missed {
				again, over, _ = step(false, Background)
			}
		case <-retry.C():
			if p.core.Paused() {
				retry.FiredWhilePaused()
				missed = true
				continue
			}
			again, over, _ = step(false, Background)
		case <-next.C:
			// A pump that is hidden or held by a rate limit leaves its timer
			// unarmed: the resume or the release runs the tick it missed.
			if p.core.Paused() {
				missed = true
				continue
			}
			if retry.Held() {
				continue
			}
			again, over, _ = step(false, Background)
		}
	}
}

// tickResult is how a pump's tick ended, for its schedule, with the
// tracker's failure that stopped it.
type tickResult struct {
	more, failed, over bool
	retryAt            time.Time
	failure            *Failure
	err                error
}

// readRequest is a read a caller hands a pump. The zero request is a
// person's refresh: a board's every unit, a ticket in full. now asks a
// board's pump for the tick its timer would run, at once and interactive:
// the delta, and whatever else is due. A write that may have made an
// issue nobody can name asks it (Service.Create).
type readRequest struct{ now bool }

// failureError hands a tracker failure that stopped a read to the caller
// that asked for the read.
type failureError struct{ f Failure }

func (e *failureError) Error() string { return e.f.Message }

// boardTick runs one tick of p's board: what is due by the board's sync
// state, or for a refresh every unit. It settles the outcome into the
// pump: the sync view, a failure that started, changed or cleared (the
// tracker's as recorded, or one outside the tracker that the tick
// returned), the event, and the pump's end when the board's mirror ended.
func (s *Service) boardTick(p *boardPump, refresh bool, u Urgency) tickResult {
	due := s.boardDue
	if refresh {
		due = func(*run) BoardDue { return BoardDue{Structure: true, Delta: true, Reconcile: true, History: true} }
	}
	out, recorded, err := s.syncBoard(p.core.Context(), p, u, due)
	res := tickResult{more: out.More(), retryAt: out.RetryAt, failure: out.Failure, err: err}
	stopped := err != nil && p.core.Context().Err() != nil
	res.failed = out.Failure != nil || (err != nil && !stopped)
	internal := err != nil && !stopped && !errors.Is(err, ErrStopped)
	if internal {
		s.logf("trackerapp: the sync of board %s of %s failed: %v", p.scope.Board, p.scope.Connection, err)
	}
	at := millis(s.now())
	s.subMu.Lock()
	defer s.subMu.Unlock()
	if p.core.DeadLocked() {
		res.over = true
		return res
	}
	if recorded != nil {
		// Every subscriber of a board is told once when its first reconcile
		// is recorded, whether or not a row changed: until then a dashboard
		// says the board's first read has not finished.
		if p.sync.ReconciledAt == 0 && recorded.ReconciledAt != 0 {
			p.pending.due = true
		}
		p.sync = *recorded
	}
	switch {
	case internal:
		if p.internal == nil {
			p.internal = internalFailure(at)
		}
	case err == nil:
		p.internal = nil
	}
	if told := p.toldLocked(); !sameFailure(p.failure, told.Failure) {
		p.failure = told.Failure
		p.pending.due = true
	}
	if out.Ended || errors.Is(err, ErrStopped) {
		s.endBoardLocked(p)
		res.over = true
		return res
	}
	s.flushBoardLocked(p)
	return res
}

// boardDue is what a pump's tick runs, from the sync state the tick read:
// every unit for a board never read; otherwise the delta, the structure
// when its interval passed since it last ran or the store says it is due,
// the reconcile when its interval passed or a reconcile failure is stored,
// and history when an issue is due one. The reconcile is never due while
// it waits for a refusal (refusalWaits), except to go on with the pump's
// pass open in the board's lifetime: a refusal empties the pass and only
// a reconcile that runs opens one, so an open pass is one a reconcile
// admitted (at the interval, a refresh, after another failure) began.
// Each unit that reported More is so run again: the delta always, the
// reconcile by its pass, the history by the issues the store holds due.
func (s *Service) boardDue(r *run) BoardDue {
	st := r.state
	if !r.stored || st.ReadAt == 0 {
		// No refusal is stored here: a reconcile runs after the tick's delta
		// read succeeded and the board's structure is held.
		return BoardDue{Structure: true, Delta: true, Reconcile: true, History: true}
	}
	open, waits := r.pass.openIn(st.Lifetime), s.refusalWaits(st.ReconcileFailure)
	passed := func(wall int64, every time.Duration) bool {
		since, known := s.sinceWall(wall)
		return !known || since >= every
	}
	return BoardDue{
		Structure: r.structureDue || passed(st.StructureWall, s.cadence.Structure),
		Delta:     true,
		Reconcile: open || !waits && (passed(st.ReconciledWall, s.cadence.Reconcile) || st.ReconcileFailure.Kind != ""),
		History:   st.HistoryDue,
	}
}

// refusalWaits reports that the reconcile waits for f, the board's stored
// reconcile failure: the tracker refused the listing's rule less than a
// reconcile interval ago by the wall clock, measured from when the
// refused call returned. A clock behind it ends the wait, as every
// interval whose start is unknown is due (sinceWall), so a step back lists
// once and measures from that listing's outcome.
func (s *Service) refusalWaits(f store.MirrorReconcileFailure) bool {
	if !f.Refused {
		return false
	}
	since, known := s.sinceWall(f.Wall)
	return known && since < s.cadence.Reconcile
}

// sameFailure reports whether a and b are the same failure as a
// subscriber reads it: both none, or the same kind and sentence.
func sameFailure(a, b *FailureView) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Kind == b.Kind && a.Message == b.Message
}

// flushBoardLocked emits p's pending event, if any. The caller holds
// subMu.
func (s *Service) flushBoardLocked(p *boardPump) {
	if !p.pending.due || p.over || p.core.DeadLocked() {
		return
	}
	pending := p.pending
	p.pending = boardPending{}
	s.emitBoard(BoardEvent{
		Scope: p.scope, Seq: s.nextSeqLocked(), Changed: pending.changed, Removed: pending.removed,
		Truncated: pending.truncated, ReadAgain: pending.readAgain, Sync: p.toldLocked(),
	})
}

// endBoardLocked ends p: it stops, it and its handles are forgotten, and
// its subscribers are told once, with what was pending. The caller holds
// subMu.
func (s *Service) endBoardLocked(p *boardPump) {
	if p.over {
		return
	}
	// The end is told before the core stops: stopping refuses the requests
	// that wait on it, and their callers must find the end already told.
	pending := p.pending
	p.over, p.pending = true, boardPending{}
	s.emitBoard(BoardEvent{
		Scope: p.scope, Seq: s.nextSeqLocked(), Changed: pending.changed, Removed: pending.removed,
		Truncated: pending.truncated, ReadAgain: pending.readAgain, Sync: p.toldLocked(), Ended: true,
	})
	p.core.StopLocked()
	if s.subs.boards[p.scope] == p {
		delete(s.subs.boards, p.scope)
	}
	s.endHandlesLocked(&p.core)
}
