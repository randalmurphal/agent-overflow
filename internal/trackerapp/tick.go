package trackerapp

import (
	"context"
	"errors"
	"fmt"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// step is one unit of a tick.
type step struct {
	unit Unit
	body func(*run) (UnitResult, error)
	// records marks a unit whose success the tick records in the scope's
	// sync state as a read, or with reconcile as a reconcile, at the time
	// the unit began; with whole only when it finished (not More).
	records, reconcile, whole bool
	// history marks a unit that holds the connection's history lock.
	history bool
}

// SyncBoard runs the units due names on scope's board, in the order
// structure, delta, reconcile, history, one at a time, on one hold of the
// connection, and returns which units ran and what each stored. Delta
// runs before reconcile so a first sync reads the rows the walk returns
// once: reconcile then reads only the rows the walk's window leaves out.
//
// A tick first reads the scope's sync state, which carries the cursor
// and the board's lifetime every board write names; a board no project
// lists ends the tick. It stops at the first unit that fails, then
// records in one write the successes of the units that ran without
// failing (delta a read, a reconcile that read every missing row a
// reconcile), which clear an older failure, and the failure that
// followed them. Structure and history record nothing: a failure they
// left stands until a read or reconcile succeeds. An idle delta tick is
// that one read, its tracker request and that one write. A tick with
// nothing due does nothing: no client is built, nothing is read or
// written.
//
// A second tick of the same scope waits for the one running to end, then
// runs: a person's refresh made during a pump's tick reads after the
// change it was made for. The history unit reads the connection's due
// issues, not the board's, and waits likewise for the history unit of
// any other scope of the connection.
//
// The units:
//
//   - structure reads the board's structure and stores it.
//   - delta reads the board's changed rows from the cursor the mirror
//     holds, a walk with no cursor starting at the history window, and
//     stores each page with the cursor it returned in one transaction, so
//     a restart resumes after the last stored page; up to deltaPages
//     pages a tick (More). A cursor the provider refuses (an encoding it
//     no longer reads) starts the walk over.
//   - reconcile lists the issues on the board, removes the rows of those
//     no longer listed when the listing is complete (an incomplete one
//     only adds), and reads the rows of listed issues that have none on
//     the board, up to reconcileBatches batches a tick (More).
//   - history reads the status, sprint, estimate and assignee changes of
//     the connection's issues that are due (store ListMirrorHistoryDue),
//     historyBatch issues a read and historyBatches reads a tick (More).
//
// Reconcile and delta read the board's structure first when the mirror
// lacks it, and after their rows when it is due (run.cascade): the sync
// state the tick read says so, or a row write since the tick's last
// structure read found a status, user or sprint the mirror lacks that no
// complete structure read marked unexplained. The store keeps both.
func (s *Service) SyncBoard(ctx context.Context, scope Scope, due BoardDue, u Urgency) (Outcome, error) {
	if err := scope.boardOnly(); err != nil {
		return Outcome{}, err
	}
	steps := boardSteps(scope.Board, due)
	if len(steps) == 0 {
		return Outcome{Ran: []UnitResult{}}, nil
	}
	return s.tick(ctx, scope, u, func(*run) []step { return steps }, nil)
}

// syncBoardPlanned is SyncBoard with the units chosen by due from the
// tick's sync state once the tick has read it, so choosing them reads
// nothing more; origin is the pump that runs it.
func (s *Service) syncBoardPlanned(ctx context.Context, scope Scope, u Urgency, due func(*run) BoardDue, origin *tickOrigin) (Outcome, error) {
	if err := scope.boardOnly(); err != nil {
		return Outcome{}, err
	}
	return s.tick(ctx, scope, u, func(r *run) []step { return boardSteps(scope.Board, due(r)) }, origin)
}

// boardSteps are the steps of the units due names on board b.
func boardSteps(b tracker.BoardID, due BoardDue) []step {
	var steps []step
	if due.Structure {
		steps = append(steps, step{unit: UnitStructure, body: func(r *run) (UnitResult, error) { return r.structureUnit(b) }})
	}
	if due.Delta {
		steps = append(steps, step{unit: UnitDelta, body: func(r *run) (UnitResult, error) { return r.delta(b) }, records: true})
	}
	if due.Reconcile {
		steps = append(steps, step{unit: UnitReconcile, body: func(r *run) (UnitResult, error) { return r.reconcile(b) },
			records: true, reconcile: true, whole: true})
	}
	if due.History {
		steps = append(steps, step{unit: UnitHistory, body: (*run).history, history: true})
	}
	return steps
}

// SyncLinked runs the units due names on connection id's linked-issues
// scope (the issues threads this computer owns are linked to), as
// SyncBoard runs a board's: in order, one at a time, recorded once.
//
//   - cards reads the cards of the linked issues no shown board has a
//     row of (store ListMirrorOffBoardLinkedIssues), every page of the
//     listing, readBatch issues a read, and records a read.
//   - history is the board tick's history unit, under the same lock.
//
// The issues nothing keeps any more, which a thread's unlink, delete,
// retire or transfer leaves, are removed by FinishMirrorCleanup.
func (s *Service) SyncLinked(ctx context.Context, id string, due LinkedDue, u Urgency) (Outcome, error) {
	if id == "" {
		return Outcome{}, fmt.Errorf("%w: a linked-issues tick needs a connection", ErrInvalidRequest)
	}
	var steps []step
	if due.Cards {
		steps = append(steps, step{unit: UnitCards, body: (*run).linkedCards, records: true})
	}
	if due.History {
		steps = append(steps, step{unit: UnitHistory, body: (*run).history, history: true})
	}
	if len(steps) == 0 {
		return Outcome{Ran: []UnitResult{}}, nil
	}
	return s.tick(ctx, Scope{Connection: id}, u, func(*run) []step { return steps }, nil)
}

// tick runs the steps plan chooses on scope under its lock and one hold
// of its connection, then settles and records the outcome once. plan runs
// after the tick read the scope's sync state; a plan with no step ends
// the tick there. What the tick stored passes through the change funnel
// (publish) before it returns, whatever its outcome; origin is the pump
// that runs it, nil for any other caller.
func (s *Service) tick(ctx context.Context, scope Scope, u Urgency, plan func(*run) []step, origin *tickOrigin) (out Outcome, err error) {
	out = Outcome{Ran: []UnitResult{}}
	unlock, err := s.lock(ctx, lockKey{scope: scope})
	if err != nil {
		return out, err
	}
	defer unlock()
	r, err := s.start(ctx, scope.Connection, u)
	if errors.Is(err, errEnded) {
		return ended(out), nil
	}
	if err != nil {
		return out, err
	}
	defer r.finish()
	defer func() {
		ch := out.Changes()
		s.publish(r.root, r.gen, scope.Connection, ch, ownOf(scope, ch), origin)
	}()
	r.scope = scope
	if err := r.readState(); errors.Is(err, errEnded) || endsMirror(err) {
		return ended(out), nil
	} else if err != nil {
		return out, err
	}
	steps := plan(r)
	if len(steps) == 0 {
		return out, nil
	}
	done := 0
	// began is when each unit of out.Ran began, the time it records.
	began := make([]int64, 0, len(steps))
	err = r.startErr
	if err != nil {
		out.Ran = append(out.Ran, UnitResult{Unit: steps[0].unit})
		began = append(began, r.readAt())
	}
	for _, st := range steps {
		if err != nil {
			break
		}
		began = append(began, r.readAt())
		var res UnitResult
		res, err = r.runStep(st)
		res.Unit = st.unit
		out.Ran = append(out.Ran, res)
		if err == nil {
			done++
		}
	}
	callErr, stop, err := r.failed(&out, err)
	if err != nil || stop {
		return out, err
	}
	if err := r.settleCalls(callErr); err != nil {
		return out, err
	}
	a, err := r.record(steps[:done], began, out)
	if endsMirror(err) {
		return ended(out), nil
	}
	if err == nil && origin != nil {
		origin.sync, origin.recorded = syncAfter(r.state, a), true
	}
	return out, err
}

// tickOrigin is the pump a tick runs for, which emits what the tick
// changed of its own scope together with its failure, and what the
// tick's record left of the scope's sync state; or the ticket pumps a
// read of tickets settles itself.
type tickOrigin struct {
	board   *boardPump
	tickets []*ticketPump
	// sync is the scope's sync view after the tick's record, set when
	// recorded.
	sync     SyncView
	recorded bool
}

// syncAfter is the sync view of a scope whose state was st after a record
// of a, as the store applies it: times only move forward, a success clears
// a failure no later than it, and a's failure is the scope's.
func syncAfter(st store.MirrorSync, a store.MirrorSyncAttempt) SyncView {
	v := SyncView{ReadAt: max(st.ReadAt, a.ReadAt), ReconciledAt: max(st.ReconciledAt, a.ReconciledAt)}
	switch f := st.Failure; {
	case a.Failure.At > 0:
		v.Failure = &FailureView{Kind: a.Failure.Kind, Message: a.Failure.Message, At: a.Failure.At}
	case f.Kind != "" && f.At > max(a.ReadAt, a.ReconciledAt):
		v.Failure = &FailureView{Kind: f.Kind, Message: f.Message, At: f.At}
	}
	return v
}

// runStep runs one unit, under the connection's history lock when it
// takes one.
func (r *run) runStep(st step) (UnitResult, error) {
	if st.history {
		unlock, err := r.s.lock(r.ctx, lockKey{scope: Scope{Connection: r.scope.Connection}, history: true})
		if err != nil {
			return UnitResult{}, err
		}
		defer unlock()
	}
	return st.body(r)
}

// record writes the tick's outcome in the scope's sync state in one
// statement: the success of each unit of done (the units that ran without
// failing) that records one, at the time it began, and the tick's failure
// at the time its unit began, so a failure is never older than a success
// it follows. A tick with nothing to record writes nothing, and the store
// writes nothing when the attempt changes nothing.
func (r *run) record(done []step, began []int64, out Outcome) (store.MirrorSyncAttempt, error) {
	a := store.MirrorSyncAttempt{Lifetime: r.state.Lifetime}
	for i, st := range done {
		switch {
		case !st.records || st.whole && out.Ran[i].More:
		case st.reconcile:
			a.ReconciledAt = began[i]
		default:
			a.ReadAt = began[i]
		}
	}
	if out.Failure != nil {
		a.Failure = store.MirrorSyncFailure{At: began[len(out.Ran)-1], Kind: string(out.Failure.Kind), Message: out.Failure.Message}
	}
	if a.ReadAt == 0 && a.ReconciledAt == 0 && a.Failure.At == 0 {
		return a, nil
	}
	if _, err := r.s.store.RecordMirrorSyncAttempt(r.root, string(r.scope.Board), a); err != nil {
		return a, fmt.Errorf("trackerapp: record the sync outcome of %s board %q: %w", r.row.ID, r.scope.Board, err)
	}
	return a, nil
}

// structureUnit is the structure unit: the read.
func (r *run) structureUnit(board tracker.BoardID) (UnitResult, error) {
	ch, err := r.structure(board)
	return UnitResult{Changes: ch}, err
}

// lockKey names a lock a tick takes: a scope's, or with history set the
// history lock of the scope's connection.
type lockKey struct {
	scope   Scope
	history bool
}

// scopeLock is one lock of the service's lock map: held is full while a
// tick holds it, and refs counts the ticks holding or waiting for it.
type scopeLock struct {
	held chan struct{}
	refs int
}

// lock takes key's lock, waiting until the tick holding it ends or ctx
// does. The map holds a lock only while a tick holds or waits for it, so
// it is bounded by the ticks in flight; the last to leave removes it.
func (s *Service) lock(ctx context.Context, key lockKey) (unlock func(), err error) {
	s.mu.Lock()
	l := s.locks[key]
	if l == nil {
		l = &scopeLock{held: make(chan struct{}, 1)}
		s.locks[key] = l
	}
	l.refs++
	s.mu.Unlock()
	select {
	case l.held <- struct{}{}:
		return func() {
			<-l.held
			s.unref(key, l)
		}, nil
	case <-ctx.Done():
		s.unref(key, l)
		return nil, fmt.Errorf("trackerapp: wait for the sync of %s: %w", key.scope.Connection, context.Cause(ctx))
	}
}

func (s *Service) unref(key lockKey, l *scopeLock) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l.refs--; l.refs == 0 {
		delete(s.locks, key)
	}
}
