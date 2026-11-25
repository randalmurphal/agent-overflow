package trackerapp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// run is one tick's hold on a connection, taken when it starts: the
// connection's row, its provider, the mirror root every write names, the
// stamp its outcome is settled with, and the context its tracker calls
// run under. A run is used by one goroutine.
type run struct {
	s   *Service
	row store.TrackerConnection
	p   tracker.Provider
	// timeBy is the kind of the tracker that answers for p's time
	// (providerLocked), which names it in a failure; every worklog
	// replace names row.TimeSourceGen.
	timeBy string
	root   int64
	gen    uint64 // root's mirror generation (genLocked)
	at     stamp
	ctx    context.Context
	// startErr is a client that could not be built: the tick's failure,
	// with no call made.
	startErr error
	// calls counts the tracker calls the run made. A run that made none
	// and did not fail leaves the connection's problem as it is.
	calls int
	// scope is the tick's scope.
	scope Scope
	// state is the scope's sync state as the tick read it at its start
	// (run.readState): the cursor and the board's lifetime every board
	// write names come from it. stored reports that the mirror holds the
	// board's structure, which a sync state found or a structure stored
	// shows. structureDue reports that the board's structure is to be read
	// (run.cascade): the state said so, or a row write since the run's
	// last structure read did.
	state        store.MirrorSync
	stored       bool
	structureDue bool
	// walking reports that the tick's delta stopped with more of its walk
	// to read, which the tick's reconcile waits for (run.reconcile).
	walking bool
	// pass is the reconcile pass the tick goes on with, its pump's; nil for
	// a run no pump makes, which does not reconcile (syncBoard).
	pass *reconcilePass
	// reconciled is how the tick's reconcile ended (run.reconcile), which
	// the tick records (run.record); lastRead is the read the run began
	// last (beginRead), the failing call's when a call failed.
	reconciled reconcileOutcome
	lastRead   store.MirrorRead
	// lifetimes are the lifetimes of the boards a write's refresh reads
	// through, captured before its reads (refreshTargets); a tick names its
	// own board's from state.
	lifetimes map[tracker.BoardID]int64
	// origin is the pump the run's changes are for (tickOrigin): a tick's
	// pump; nil for none. tell, when set, collects what each write stored,
	// which told has published already.
	origin *tickOrigin
	tell   func(Changes)
	stop   func()
}

// told passes w, what one store write of the run committed, through
// the change funnel as the write returns, before any later call of the
// run can fail or wait: the one rule of every path's publication (a
// tick's units, a write's refresh, a read of tickets). The links it moved
// go to Deps.LinksChanged, whether or not a view of the connection is
// open; its changes go to the views. board is the board the write's read
// went through, none for a read of cards or tickets; every write of a
// board's tick is its board's (ownOf).
func (r *run) told(board tracker.BoardID, w written) {
	if len(w.links) > 0 {
		r.s.linksChanged(w.links)
	}
	if r.scope.Board != "" {
		board = r.scope.Board
	}
	ch := w.Changes
	r.s.publish(r.root, r.gen, r.row.ID, ch, ownOf(Scope{Connection: r.row.ID, Board: board}, ch), r.origin)
	if r.tell != nil {
		r.tell(ch)
	}
}

// connHold is what ticks of a connection reuse until a change to it made
// through the service (Service.holds): the row, the mirror root, the
// provider and the clients it was built over.
type connHold struct {
	row  store.TrackerConnection
	root int64
	gen  uint64
	p    tracker.Provider
	// timeBy is the kind of the tracker that answers for p's time
	// (providerLocked).
	timeBy string
	c      *clients
}

// forgetHolds drops every connection's hold. The caller holds opMu.
// Dropping them all on any change keeps a Jira connection's hold, whose
// provider holds its Tempo connection's client, from outliving a change
// to the Tempo connection.
func (s *Service) forgetHolds() { clear(s.holds) }

// callError marks a failed tracker call, so a unit tells the tracker's
// failures, which are outcomes, from the store's, which are errors.
type callError struct{ err error }

func (e *callError) Error() string { return e.err.Error() }
func (e *callError) Unwrap() error { return e.err }

// call marks err, the error of a provider call, as the tracker's.
func call(err error) error {
	if err == nil {
		return nil
	}
	return &callError{err: err}
}

// errEnded is a run whose connection is off or gone at its start.
var errEnded = errors.New("trackerapp: the connection's mirror ended")

// start takes a hold on connection id for one tick or read, or fails:
// with errEnded for a connection turned off or removed, ErrNoProvider for
// a Tempo connection, ErrStopped after Stop, the context's error when ctx
// has ended, and the error of the delete of an earlier process's open
// tickets when that failed (dropEarlierTickets). It first tries again the
// drops of closed tickets that failed (payOwedDrops). The caller ends the
// run with finish.
func (s *Service) start(ctx context.Context, id string, u Urgency) (*run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.enter(); err != nil {
		return nil, err
	}
	if err := s.dropEarlierTickets(); err != nil {
		s.leave()
		return nil, err
	}
	s.payOwedDrops("the next run tries it again")
	r, err := s.hold(id)
	if err != nil {
		s.leave()
		return nil, err
	}
	callCtx, cancel := context.WithCancel(ctx)
	stopLife := context.AfterFunc(s.life, cancel)
	if u == Interactive {
		callCtx = trackerapi.WithInteractive(callCtx)
	}
	r.ctx = callCtx
	r.stop = func() {
		stopLife()
		cancel()
		s.leave()
	}
	return r, nil
}

// hold reads connection id, its mirror root and its provider under opMu,
// so a change to the connection orders entirely before or after it: a
// run that starts after a turn off sees the connection off, and one that
// started before holds the root the turn off detaches. What it reads is
// kept (Service.holds) while the connection's client stays in use, so the
// next tick reads none of it again.
func (s *Service) hold(id string) (*run, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if h := s.heldLocked(id); h != nil {
		return &run{s: s, row: h.row, root: h.root, gen: h.gen, p: h.p, timeBy: h.timeBy, at: s.begin(h.c)}, nil
	}
	delete(s.holds, id)
	row, err := s.get(id)
	if err == nil {
		err = usable(row)
	}
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrDisabled):
		return nil, fmt.Errorf("%w: %w", errEnded, err)
	case err != nil:
		return nil, err
	}
	gen := s.gen(id)
	root, err := s.mirrorRootLocked(id, gen)
	switch {
	case errors.Is(err, store.ErrTrackerCacheGone), errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("%w: %w", errEnded, err)
	case err != nil:
		return nil, fmt.Errorf("trackerapp: the mirror of connection %s: %w", id, err)
	}
	r := &run{s: s, row: row, root: root, gen: gen}
	p, c, timeBy, err := s.providerLocked(row)
	if _, ok := errors.AsType[*ClientError](err); ok {
		// No client means no call: the connection's problem is why.
		r.startErr = call(err)
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	r.p, r.timeBy, r.at = p, timeBy, s.begin(c)
	s.holds[id] = &connHold{row: row, root: root, gen: r.gen, p: p, timeBy: timeBy, c: c}
	return r, nil
}

// dropEarlierTickets deletes the open-ticket rows an earlier process left
// (store DropMirrorTicketsOfOtherSessions), once per process: every run
// passes it as it starts, and every bind of an open ticket is made in a
// run (bindTickets), so the delete has completed before any row of this
// process is written and never removes one. Every read of a view passes
// it too (readRoot), so no view shows an issue that only an earlier
// process's row keeps. Concurrent first runs wait for one delete. A delete
// that fails fails the run or the read that tried it, which is retried as
// a failure outside the tracker is, and the next one tries it again.
func (s *Service) dropEarlierTickets() error {
	s.earlier.Lock()
	defer s.earlier.Unlock()
	if s.earlier.done {
		return nil
	}
	if _, err := s.store.DropMirrorTicketsOfOtherSessions(s.session); err != nil {
		return fmt.Errorf("trackerapp: drop the open tickets an earlier process left: %w", err)
	}
	s.earlier.done = true
	return nil
}

// heldLocked is connection id's hold while the client it was built over
// is still the connection's, nil otherwise. The caller holds opMu.
func (s *Service) heldLocked(id string) *connHold {
	h := s.holds[id]
	if h == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clients[id] != h.c {
		return nil
	}
	return h
}

// rehold looks connection id's mirror root up again after the store
// reported gone, the error, the root stale a run of generation gen held,
// and keeps the new one. A run belongs to the mirror generation it began
// in: when the connection's generation moved since (turned off, removed,
// or off and on again), the root is the old connection's mirror and so
// are the run's provider and settlement stamp, and the run ends
// (errEnded). Only a root gone within the generation (a restore emptied
// the mirror) is replaced, and the open tickets are kept again under the
// new one (mirrorRootLocked). A connection turned off meanwhile ends the
// run the same way.
func (s *Service) rehold(id string, stale int64, gen uint64, gone error) (int64, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.gen(id) != gen {
		return 0, fmt.Errorf("%w: the connection's mirror ended since the run began: %w", errEnded, gone)
	}
	root, err := s.mirrorRootLocked(id, gen)
	switch {
	case errors.Is(err, store.ErrTrackerCacheGone), errors.Is(err, sql.ErrNoRows):
		delete(s.holds, id)
		return 0, fmt.Errorf("%w: %w", errEnded, err)
	case err != nil:
		return 0, fmt.Errorf("trackerapp: the mirror of connection %s: %w", id, err)
	}
	if h := s.holds[id]; h != nil && h.root == stale {
		h.root = root
	}
	return root, nil
}

// mirrorRootLocked is connection id's mirror root, made when it has none
// (store GetOrCreateMirrorRoot), for a run of generation gen. A root made
// while open tickets of gen are kept replaces one a restore emptied, with
// their rows: they are kept again under it (keepTicketsAgainLocked). The
// first root of a connection, or one made after it was turned on again,
// has no kept ticket of its generation. The caller holds opMu.
func (s *Service) mirrorRootLocked(id string, gen uint64) (int64, error) {
	root, created, err := s.store.GetOrCreateMirrorRoot(id)
	if err == nil && created {
		s.keepTicketsAgainLocked(id, gen, root)
	}
	return root, err
}

// gen is connection id's mirror generation (genLocked). Read under opMu
// with the mirror root, it pairs them: a turn off moves it, under opMu,
// before it detaches the root.
func (s *Service) gen(id string) uint64 {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	return s.genLocked(id)
}

// readState reads the scope's sync state, the tick's one read before its
// tracker calls: the cursor, the board's lifetime, whether the store
// shows the board, whether its structure is due a read and whether an
// issue's history is due one now. That last is how a pump learns that an
// issue's wait for a final read is over, on its next tick, with no timer
// and no statement of its own. A root the store reports gone is looked
// up again once within the run's generation (rehold); the run keeps its
// generation, and one that outlived it ends. A board scope whose board no
// project lists has no lifetime: the tick ends.
func (r *run) readState() error {
	board := string(r.scope.Board)
	st, shown, err := r.s.store.GetMirrorSync(r.root, board, r.steadyAt())
	if errors.Is(err, store.ErrTrackerCacheGone) {
		if r.root, err = r.s.rehold(r.row.ID, r.root, r.gen, err); err != nil {
			return err
		}
		st, shown, err = r.s.store.GetMirrorSync(r.root, board, r.steadyAt())
	}
	if err != nil {
		return fmt.Errorf("trackerapp: read the sync state of %s board %q: %w", r.row.ID, board, err)
	}
	if board != "" && st.Lifetime == 0 {
		return fmt.Errorf("%w: board %s of %s is not listed", store.ErrTrackerCacheUnlisted, board, r.row.ID)
	}
	r.state, r.stored, r.structureDue = st, shown, board != "" && st.StructureDueAt > 0
	return nil
}

// lifetime is the lifetime of board the run captured, which every write
// of the board names: a tick's own board's, or one a write's refresh
// captured.
func (r *run) lifetime(board tracker.BoardID) (int64, error) {
	if board == r.scope.Board && r.state.Lifetime != 0 {
		return r.state.Lifetime, nil
	}
	if l := r.lifetimes[board]; l != 0 {
		return l, nil
	}
	return 0, fmt.Errorf("trackerapp: board %s is not a board the run captured (its own is %q)", board, r.scope.Board)
}

func (r *run) finish() { r.stop() }

// window is the start of the history window now (Service.window).
func (r *run) window() time.Time { return r.s.window() }

// beginRead begins a read of the tracker, before its request, on the
// root's read clock (store BeginMirrorRead): every write of what a read
// returned takes the read it began, so the store orders reads made
// through every path, and every process, on one clock.
func (r *run) beginRead() (store.MirrorRead, error) {
	read, err := r.s.store.BeginMirrorRead(r.root, millis(r.s.now()))
	if err != nil {
		return store.MirrorRead{}, fmt.Errorf("trackerapp: begin a read of %s: %w", r.row.ID, err)
	}
	r.lastRead = read
	return read, nil
}

// The service's elapsed-time decisions, and the clock each reads:
//
//   - Since a read the root stored began (Service.sinceWall): the board's
//     structure and reconcile intervals (boardDue) and a document's age
//     (checkDetails, detailMaxAge). The wall clock against the wall time
//     the stored read began at (store MirrorRead.Wall, stored beside its
//     order time). An interval is due when no wall time is stored (none
//     read, or read before the column existed), when the clock is behind
//     it (it stepped back), or when the interval passed: after a step
//     either way the scope is read once and measured from that read. The
//     order times stay ordered across the step and a restart
//     (BeginMirrorRead), and a reconcile removes no row on its
//     listing's word (store ReconcileMirrorBoard), so a step back removes
//     no row.
//   - The history wait (store MirrorHistory.AnsweredAt and Wait, the now
//     of GetMirrorSync and ListMirrorHistoryDue, historyFinal): the steady
//     clock (Service.steady), monotonic within the process. A wait an
//     earlier process answered is not counted (steadyFrom): it starts
//     over.
//   - A ticket round's due, hold and retry times, a rate limit's, and the
//     pumps' and the linked loop's timers: times held in memory, compared
//     with Deps.Now's; with time.Now they compare its monotonic reading,
//     so a wall step moves none.
//   - Whether a status time is trusted and a history complete (HistoryTo
//     and CompleteTo against the card's update time): the tracker's own
//     clock on both sides.
//
// sinceWall is how long ago wall, the wall time a read the root stored
// began at, was by the wall clock, and false when the clock is behind it.
// An interval is due when it is unknown. None stored is zero, decades ago,
// so it is due.
func (s *Service) sinceWall(wall int64) (time.Duration, bool) {
	since := s.now().Sub(time.UnixMilli(wall))
	return since, since >= 0
}

// steadyAt is now by the clock the history wait is measured on
// (Service.steady), in Unix ms.
func (r *run) steadyAt() int64 { return r.s.steady().UnixMilli() }

// endsMirror reports a store error that ends the scope's sync rather than
// failing it: the root detached, or the board no longer listed or
// retired.
func endsMirror(err error) bool {
	return errors.Is(err, store.ErrTrackerCacheGone) || errors.Is(err, store.ErrTrackerCacheUnlisted)
}

// ended is out as the outcome of a tick whose mirror ended: no failure.
func ended(out Outcome) Outcome {
	out.Ended, out.Failure, out.RetryAt = true, nil, time.Time{}
	return out
}

// failed sorts err, how a tick's units stopped, into the tick's outcome:
//
//   - A write refused because the root is gone or the board is no longer
//     listed ends the tick (Ended).
//   - A tick whose context ended returns the context's error and records
//     nothing: it was stopped, it did not fail.
//   - Any other error that is not a tracker failure is returned.
//   - A tracker failure is the outcome's Failure, with the time the
//     tracker said its rate limit lifts.
//
// It returns the tracker's error for settle, and stop when the tick
// records nothing more.
func (r *run) failed(out *Outcome, err error) (callErr error, stop bool, _ error) {
	var failure *callError
	switch {
	case err == nil:
		return nil, false, nil
	case endsMirror(err):
		*out = ended(*out)
		return nil, true, nil
	case r.ctx.Err() != nil:
		return nil, true, fmt.Errorf("trackerapp: the sync of %s stopped: %w", r.row.ID, context.Cause(r.ctx))
	case !errors.As(err, &failure):
		return nil, true, err
	}
	f := ClassifyFailure(r.row.Kind, failure.err)
	out.Failure = &f
	if limited, ok := errors.AsType[*trackerapi.RateLimitedError](failure.err); ok {
		out.RetryAt = limited.Until
	}
	return failure.err, false, nil
}

// settleCalls settles the outcome of the run's tracker calls as the
// connection's problem: callErr, or a success when the run made a call.
// Changed is told only when the problem started, changed or cleared.
// A run that made none and did not fail leaves the problem as it is. A
// run whose connection was turned off or removed meanwhile (its client
// closed, so its calls failed) has its outcome dropped by settle.
func (r *run) settleCalls(callErr error) error {
	if r.calls == 0 && callErr == nil {
		return nil
	}
	r.s.opMu.Lock()
	_, changed, err := r.s.settle(r.row.ID, r.row.Kind, r.at, callErr, nil)
	r.s.opMu.Unlock()
	if changed {
		r.s.changed()
	}
	return err
}
