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
	s    *Service
	row  store.TrackerConnection
	p    tracker.Provider
	root int64
	gen  uint64 // root's mirror generation (genLocked)
	at   stamp
	ctx  context.Context
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
	// pass is the reconcile pass the tick goes on with, its pump's; nil
	// for a tick no pump runs, whose pass lasts the tick.
	pass *reconcilePass
	// lifetimes are the lifetimes of the boards a write's refresh reads
	// through, captured before its reads (refreshTargets); a tick names its
	// own board's from state.
	lifetimes map[tracker.BoardID]int64
	stop      func()
}

// connHold is what ticks of a connection reuse until a change to it made
// through the service (Service.holds): the row, the mirror root, the
// provider and the clients it was built over.
type connHold struct {
	row  store.TrackerConnection
	root int64
	gen  uint64
	p    tracker.Provider
	c    *clients
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
// a Tempo connection, ErrStopped after Stop, and the context's error when
// ctx has ended. The caller ends the run with finish.
func (s *Service) start(ctx context.Context, id string, u Urgency) (*run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.enter(); err != nil {
		return nil, err
	}
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
	if h := s.holds[id]; h != nil {
		s.mu.Lock()
		inUse := s.clients[id] == h.c
		s.mu.Unlock()
		if inUse {
			return &run{s: s, row: h.row, root: h.root, gen: h.gen, p: h.p, at: s.begin(h.c)}, nil
		}
		delete(s.holds, id)
	}
	row, err := s.get(id)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, fmt.Errorf("%w: %w", errEnded, err)
	case err != nil:
		return nil, err
	case row.Kind == store.TrackerKindTempo:
		return nil, fmt.Errorf("%w: %s", ErrNoProvider, id)
	case !row.Enabled:
		return nil, fmt.Errorf("%w: %w: %s", errEnded, ErrDisabled, id)
	}
	root, _, err := s.store.GetOrCreateMirrorRoot(id)
	switch {
	case errors.Is(err, store.ErrTrackerCacheGone), errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("%w: %w", errEnded, err)
	case err != nil:
		return nil, fmt.Errorf("trackerapp: the mirror of connection %s: %w", id, err)
	}
	r := &run{s: s, row: row, root: root, gen: s.gen(id)}
	p, c, err := s.providerLocked(row)
	if _, ok := errors.AsType[*ClientError](err); ok {
		// No client means no call: the connection's problem is why.
		r.startErr = call(err)
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	r.p, r.at = p, s.begin(c)
	s.holds[id] = &connHold{row: row, root: root, gen: r.gen, p: p, c: c}
	return r, nil
}

// rehold looks connection id's mirror root up again after the store
// reported gone, the error, the root stale a run of generation gen held,
// and keeps the new one. A run belongs to the mirror generation it began
// in: when the connection's generation moved since (turned off, removed,
// or off and on again), the root is the old connection's mirror and so
// are the run's provider and settlement stamp, and the run ends
// (errEnded). Only a root gone within the generation (a restore emptied
// the mirror) is replaced. A connection turned off meanwhile ends the run
// the same way.
func (s *Service) rehold(id string, stale int64, gen uint64, gone error) (int64, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.gen(id) != gen {
		return 0, fmt.Errorf("%w: the connection's mirror ended since the run began: %w", errEnded, gone)
	}
	root, _, err := s.store.GetOrCreateMirrorRoot(id)
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

// window is the start of the history window now.
func (r *run) window() time.Time { return r.s.now().Add(-HistoryWindow) }

// readAt is now as the store orders reads: the time a read begins, taken
// before its request.
func (r *run) readAt() int64 { return r.s.now().UnixMilli() }

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
// A run that made none and did not fail leaves the problem as it is. A
// run whose connection was turned off or removed meanwhile (its client
// closed, so its calls failed) has its outcome dropped by settle.
func (r *run) settleCalls(callErr error) error {
	if r.calls == 0 && callErr == nil {
		return nil
	}
	r.s.opMu.Lock()
	moved, err := r.s.settle(r.row.ID, r.row.Kind, r.at, callErr, nil)
	r.s.opMu.Unlock()
	if moved {
		r.s.changed()
	}
	return err
}
