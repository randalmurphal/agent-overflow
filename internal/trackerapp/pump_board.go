package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"agent-overflow/internal/pump"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// boardPump is a board's pump: one goroutine and one timer for every
// handle on the board's scope, plus the retry timer while a tick fails.
// Its core and the fields below it are under subMu.
type boardPump struct {
	core  pump.Core[struct{}]
	scope Scope
	// every is the delta interval of the connection's kind.
	every time.Duration
	// exited closes when the goroutine has exited, or at once when it
	// never starts (launchLocked).
	exited chan struct{}

	// pending is what the next event carries; sync is the board's sync
	// view as the last tick recorded it, and failure the failure the
	// subscribers were last told of.
	pending boardPending
	sync    SyncView
	failure *FailureView
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
// view: the pump's first tick reads it, and its event says so. PollEvery
// is the pump's delta interval in milliseconds: while no failure is
// reported, the board was read within it, so the last read time needs no
// event of its own.
type BoardSubscription struct {
	ID        string     `json:"id"`
	Seq       uint64     `json:"seq"`
	Loaded    bool       `json:"loaded"`
	View      *BoardView `json:"view,omitempty"`
	PollEvery int64      `json:"poll_every"`
}

// SubscribeBoard subscribes to scope's board for project projectID, which
// must list it (ErrNotListed), with the first page of each of sections in
// the answer. The answer comes from the mirror alone; the board's pump,
// made on the first handle, reads the tracker after it, its first tick
// interactive. A connection turned off or removed is refused (ErrDisabled,
// ErrNotFound), as is a subscribe over the cap (ErrTooManySubscriptions).
func (s *Service) SubscribeBoard(projectID string, scope Scope, sections []Section) (BoardSubscription, error) {
	if err := scope.boardOnly(); err != nil {
		return BoardSubscription{}, err
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
	listed, err := s.store.ListProjectTrackerBoards(projectID)
	if err != nil {
		return BoardSubscription{}, fmt.Errorf("trackerapp: read the boards of project %s: %w", projectID, err)
	}
	if !slices.ContainsFunc(listed, func(b store.ProjectTrackerBoard) bool {
		return b.ConnectionID == scope.Connection && b.BoardID == string(scope.Board)
	}) {
		return BoardSubscription{}, fmt.Errorf("%w: project %s, board %s of %s", ErrNotListed, projectID, scope.Board, scope.Connection)
	}
	id, p, start, seq, err := s.takeBoardHandle(scope)
	if err != nil {
		return BoardSubscription{}, err
	}
	answer := BoardSubscription{ID: id, Seq: seq, PollEvery: p.every.Milliseconds()}
	view, err := s.Board(scope, sections)
	switch {
	case err == nil:
		answer.Loaded, answer.View = true, &view
	case !errors.Is(err, ErrNotMirrored):
		s.ReleaseSubscription(id)
		answer = BoardSubscription{}
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

// takeBoardHandle takes a handle on scope's pump, making the pump when
// there is none; start reports that the caller launches it. seq is the
// answer's sequence number. The connection is read under opMu, so a turn
// off orders entirely before (refused) or after (the pump ends).
func (s *Service) takeBoardHandle(scope Scope) (id string, p *boardPump, start bool, seq uint64, err error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	kind, err := s.subscribableLocked(scope.Connection)
	if err != nil {
		return "", nil, false, 0, err
	}
	s.subMu.Lock()
	defer s.subMu.Unlock()
	switch {
	case s.subs.halted:
		return "", nil, false, 0, ErrStopped
	case s.subs.boardHandles >= maxBoardSubscriptions:
		return "", nil, false, 0, fmt.Errorf("%w: %d board subscriptions", ErrTooManySubscriptions, maxBoardSubscriptions)
	}
	p = s.subs.boards[scope]
	if p == nil {
		if s.subs.boards == nil {
			s.subs.boards = map[Scope]*boardPump{}
		}
		p = &boardPump{core: s.newCore(), scope: scope, every: s.cadence.delta(kind), exited: make(chan struct{})}
		s.subs.boards[scope] = p
		start = true
	}
	id = s.takeHandleLocked(&handle{board: p}, &p.core)
	return id, p, start, s.nextSeqLocked(), nil
}

// subscribableLocked is the kind of connection id, refusing a subscribe
// when it is turned off, removed or a Tempo connection. A connection a tick holds is
// on: any change to it drops the hold. The caller holds opMu.
func (s *Service) subscribableLocked(id string) (kind string, err error) {
	if h := s.holds[id]; h != nil {
		s.mu.Lock()
		inUse := s.clients[id] == h.c
		s.mu.Unlock()
		if inUse {
			return h.row.Kind, nil
		}
	}
	row, err := s.get(id)
	switch {
	case err != nil:
		return "", err
	case row.Kind == store.TrackerKindTempo:
		return "", fmt.Errorf("%w: %s", ErrNoProvider, id)
	case !row.Enabled:
		return "", fmt.Errorf("%w: %s", ErrDisabled, id)
	}
	return row.Kind, nil
}

// RefreshSubscription has handle id's pump read now, as a person's
// action: a board runs structure, delta, reconcile and history, a ticket
// reads its detail, each interactive. It returns when the read ran;
// what it found reaches the subscribers as events. A subscription that
// ends first fails with ErrSubscriptionEnded.
func (s *Service) RefreshSubscription(ctx context.Context, id string) error {
	s.subMu.Lock()
	h := s.subs.handles[id]
	var core *pump.Core[struct{}]
	if h != nil {
		core = h.core()
	}
	s.subMu.Unlock()
	if core == nil {
		return fmt.Errorf("%w: %s", ErrUnknownSubscription, id)
	}
	return core.Request(ctx, struct{}{})
}

// runBoardPump is a board pump's goroutine: an interactive first tick,
// then a tick every Cadence.Delta while a handle shows the board, at once
// again while a tick reports More, on a person's request, and on the
// retry schedule while ticks fail. It exits when the core stops.
func (s *Service) runBoardPump(p *boardPump) {
	defer s.workers.Done()
	defer close(p.exited)
	next := time.NewTimer(p.every)
	defer next.Stop()
	retry := pump.Retry{Base: s.cadence.RetryBase, Interval: p.every}
	missed := false
	// step runs a tick and re-arms the timers; again reports a tick to run
	// at once (More while shown), over that the pump ended.
	step := func(refresh bool, u Urgency) (again, over bool, err error) {
		missed = false
		res := s.boardTick(p, refresh, u)
		retry.Arm(res.failed, res.retryAt)
		next.Reset(p.every)
		if res.over {
			return false, true, res.err
		}
		if res.more && !res.failed && p.core.Paused() {
			missed = true
		}
		return res.more && !res.failed && !p.core.Paused(), false, res.err
	}
	serve := func(req *pump.Request[struct{}]) (again, over bool) {
		again, over, err := step(true, Interactive)
		if over {
			err = ErrSubscriptionEnded
		}
		req.Finish(err)
		return again, over
	}
	again, over, _ := step(false, Interactive)
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

// tickResult is how a pump's tick ended, for its schedule.
type tickResult struct {
	more, failed, over bool
	retryAt            time.Time
	err                error
}

// boardTick runs one tick of p's board: what is due by the board's sync
// state, or for a refresh every unit. It settles the outcome into the
// pump: the sync view, a failure that started, changed or cleared, the
// event, and the pump's end when the board's mirror ended.
func (s *Service) boardTick(p *boardPump, refresh bool, u Urgency) tickResult {
	origin := &tickOrigin{board: p}
	due := s.boardDue
	if refresh {
		due = func(*run) BoardDue { return BoardDue{Structure: true, Delta: true, Reconcile: true, History: true} }
	}
	out, err := s.syncBoardPlanned(p.core.Context(), p.scope, u, due, origin)
	res := tickResult{more: out.More(), retryAt: out.RetryAt, err: err}
	stopped := err != nil && p.core.Context().Err() != nil
	res.failed = out.Failure != nil || (err != nil && !stopped)
	if err != nil && !stopped && !errors.Is(err, ErrStopped) {
		s.logf("trackerapp: the sync of board %s of %s failed: %v", p.scope.Board, p.scope.Connection, err)
	}
	s.subMu.Lock()
	defer s.subMu.Unlock()
	if p.core.DeadLocked() {
		res.over = true
		return res
	}
	if origin.recorded {
		p.sync = origin.sync
	}
	if !sameFailure(p.failure, p.sync.Failure) {
		p.failure = p.sync.Failure
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
// every unit for a board never read; otherwise the delta, the reconcile
// and the structure when their interval passed since they last ran (the
// structure also when the store says it is due), and history when an
// issue is due one.
func (s *Service) boardDue(r *run) BoardDue {
	st := r.state
	if !r.stored || st.ReadAt == 0 {
		return BoardDue{Structure: true, Delta: true, Reconcile: true, History: true}
	}
	now := s.now()
	passed := func(at int64, every time.Duration) bool { return now.Sub(time.UnixMilli(at)) >= every }
	return BoardDue{
		Structure: r.structureDue || st.StructureReadAt == 0 || passed(st.StructureReadAt, s.cadence.Structure),
		Delta:     true,
		Reconcile: passed(st.ReconciledAt, s.cadence.Reconcile),
		History:   st.HistoryDue,
	}
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
		Truncated: pending.truncated, ReadAgain: pending.readAgain, Sync: p.sync,
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
		Truncated: pending.truncated, ReadAgain: pending.readAgain, Sync: p.sync, Ended: true,
	})
	p.core.StopLocked()
	if s.subs.boards[p.scope] == p {
		delete(s.subs.boards, p.scope)
	}
	s.endHandlesLocked(&p.core)
}
