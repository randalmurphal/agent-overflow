package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// Writes. Every edit goes to the tracker first, through one path
// (Service.write), which owns, in order:
//
//   - the hold of the connection, taken as a tick takes one (start),
//     interactive: a person or an agent waits on every write;
//   - the provider's write, and tracker.Result.Check before anything acts
//     on its result;
//   - the refresh: every resource a part that is applied or unknown names
//     is read again through the read sync uses and stored, under the root
//     and the fences the hold captured (write_refresh.go);
//   - the settlement of the connection's problem by the last tracker call
//     made;
//   - the change funnel, so a subscriber hears of a write as of a sync,
//     and the settlement of the open tickets the refresh read, whose
//     views are built before the write returns;
//   - the view of the outcomes, with what could not be refreshed beside
//     them.
//
// A write method (write_methods.go) converts its JSON request
// (write_inputs.go), or takes Attach's files as they are, and names its
// provider call; nothing else.

// ErrBadResult is a write whose provider returned a result no provider
// may return (tracker.Result.Check): it is an error, never a partial
// success. The write may have happened; nothing was read again.
var ErrBadResult = errors.New("trackerapp: the tracker's provider answered a write with a result it may not")

// writeOp is one write as Service.write runs it.
type writeOp struct {
	// call is the provider's write; nil for a refresh alone
	// (RefreshWorklogs).
	call func(ctx context.Context, p tracker.Provider) tracker.Result
	// board is the board a create was made from: an issue the create made
	// is read through it, and a create of unknown outcome runs its tick.
	board tracker.BoardID
	// also are resources read again besides those the result names.
	also []tracker.Resource
	// unable, when set, refuses op on the provider the hold found, before
	// anything is sent.
	unable func(p tracker.Provider) error
}

// write makes op's write on connection id and refreshes what it may have
// changed. A connection that is off, removed or a Tempo connection is
// refused (ErrDisabled, ErrNotFound, ErrNoProvider), as is one turned off
// or removed during the write when no part of it may have happened; one
// turned off when a part may have happened answers the parts with Ended
// and refreshes nothing, since the mirror was cleared with it.
func (s *Service) write(ctx context.Context, id string, op writeOp) (WriteView, error) {
	r, err := s.interact(ctx, id)
	if err != nil {
		return WriteView{}, err
	}
	finished := false
	finish := func() {
		if !finished {
			finished = true
			r.finish()
		}
	}
	defer finish()
	if op.unable != nil {
		if err := op.unable(r.p); err != nil {
			return WriteView{}, err
		}
	}
	var res tracker.Result
	resources := op.also
	if op.call != nil {
		res = op.call(r.ctx, r.p)
		if err := res.Check(); err != nil {
			return WriteView{}, fmt.Errorf("%w: %w", ErrBadResult, err)
		}
		res = timeAnsweredParts(r.timeBy, res)
		if !sentNothing(res) {
			r.calls++
		}
		resources = slices.Concat(resources, res.Refresh())
		if created, ok := res.Created(); ok && op.board != "" {
			resources = append(resources, tracker.RowOf(op.board, created))
		}
	}
	view := WriteView{Parts: partViews(r.row.Kind, res, millis(s.now())), CreateUnknown: res.CreateUnknown()}
	if created, ok := res.Created(); ok {
		view.Created = string(created)
	}
	if !r.inUse() {
		if !happened(res) {
			return WriteView{}, s.endedNow(id)
		}
		view.Ended = true
		return view, nil
	}
	writeCalls := r.calls
	var created newIssue
	if id, ok := res.Created(); ok && op.board != "" {
		created = newIssue{issue: id, board: op.board}
	}
	f := r.refresh(resources, created)
	// The mirror ended with the connection: what the refresh stored before
	// was told as it was stored (run.told), as a tick's is, and the
	// outcome answers for every resource (Ended).
	ended := f.ended || !r.inUse()
	if !ended && r.ctx.Err() == nil {
		callErr := f.lastCallError()
		if callErr == nil && r.calls == writeCalls {
			callErr = writeProblem(r.row.Kind, res)
		}
		// A settlement Stop overtook changes nothing: the outcome stands.
		_ = r.settleCalls(callErr)
	}
	if len(f.pumps) > 0 {
		// The open tickets settle with the outcome of their reads and their
		// views are built from what the refresh stored; one it could not
		// read is due on the connection's retry schedule.
		s.settleTickets(ctx, id, f.pumps, f.began, f.reads[:len(f.pumps)], f.details, f.detailsErr, millis(s.now()))
	}
	if ended {
		view.Ended = true
		if !happened(res) {
			return WriteView{}, s.endedNow(id)
		}
		return view, nil
	}
	view.Stale = s.staleViews(r.row.Kind, f.stale)
	view.NotOnBoard = string(f.notOnBoard)
	finish()
	if view.CreateUnknown && op.board != "" {
		if err := s.tickNow(ctx, Scope{Connection: id, Board: op.board}); err != nil {
			view.Stale = append(view.Stale, s.staleView(r.row.Kind, []ResourceView{{Kind: resourceDelta, Board: string(op.board)}}, err))
		}
	}
	return view, nil
}

// interact holds connection id for a call a person waits on: a write, or
// a read a write form needs. A connection that is off or removed is
// refused with ErrDisabled or ErrNotFound; a client that could not be
// built is settled as the connection's problem and returned. The caller
// ends the run with finish.
func (s *Service) interact(ctx context.Context, id string) (*run, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: no connection named", ErrInvalidRequest)
	}
	r, err := s.start(ctx, id, Interactive)
	switch {
	case errors.Is(err, errEnded):
		return nil, endedError(id, err)
	case err != nil:
		return nil, err
	}
	r.scope = Scope{Connection: id}
	if r.startErr != nil {
		settleErr := r.settleCalls(r.startErr)
		r.finish()
		var c *callError
		errors.As(r.startErr, &c)
		return nil, errors.Join(callFailure(r.row.Kind, c.err), settleErr)
	}
	return r, nil
}

// endedError is err, a run that found connection id off or gone, as the
// caller's error: ErrNotFound for a connection removed, ErrDisabled for
// one turned off or whose mirror was cleared.
func endedError(id string, err error) error {
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w %s", ErrNotFound, id)
	}
	return fmt.Errorf("%w: %s", ErrDisabled, id)
}

// endedNow is the error of a write whose connection stopped being used
// while it was made: ErrStopped after Stop, ErrNotFound when the
// connection was removed, ErrDisabled otherwise.
func (s *Service) endedNow(id string) error {
	s.mu.Lock()
	stopped := s.stopped
	s.mu.Unlock()
	if stopped {
		return ErrStopped
	}
	if _, err := s.store.GetTrackerConnection(id); err != nil {
		return endedError(id, fmt.Errorf("%w: %w", ErrNotFound, err))
	}
	return fmt.Errorf("%w: %s was turned off while the write was sent", ErrDisabled, id)
}

// inUse reports whether the run's connection is still used through the
// client the run began on: not turned off, removed or stopped since.
func (r *run) inUse() bool {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	return !r.s.stopped && r.s.clients[r.row.ID] == r.at.c
}

// happened reports a result some part of which may have happened:
// applied, or sent and not answered.
func happened(res tracker.Result) bool {
	t := res.Tally()
	return t.Applied+t.Unknown > 0
}

// sentNothing reports a result every part of which was refused before
// anything was sent (input the provider's checks refused, or a write the
// tracker cannot make) or stopped by its own file, which ended before the
// tracker had it whole. Such a write says nothing of the connection.
func sentNothing(res tracker.Result) bool {
	for _, p := range res.Parts {
		if _, cut := errors.AsType[*trackerapi.SourceError](p.Outcome.Err()); cut && p.Outcome.State() == tracker.StateNotSent {
			continue
		}
		r, ok := p.Outcome.Refusal()
		if !ok || r.Reason != tracker.ReasonInvalid && r.Reason != tracker.ReasonUnsupported {
			return false
		}
	}
	return true
}

// writeProblem is the failure among res's parts that is the connection's
// rather than the write's: a credential refused, a tracker that could not
// be reached or that is limiting requests. A part the tracker otherwise
// judged, applied or refused, shows the connection works.
func writeProblem(kind string, res tracker.Result) error {
	for _, p := range res.Parts {
		err := p.Outcome.Err()
		if err == nil || p.Outcome.State() == tracker.StateApplied {
			continue
		}
		switch ClassifyFailure(kind, err).Kind {
		case FailureSetup, FailureTransient, FailureRateLimited:
			return err
		case FailureForbidden, FailureTracker, FailureNotFound, FailureInternal, FailureUpload:
			// Forbidden and tracker are the tracker judging the write, and
			// upload the client that sent the file. ClassifyFailure gives
			// neither not found nor internal.
		}
	}
	return nil
}

// tickNow runs the tick scope's board is due now, interactive: through
// its pump when one is live, so the board's ticks have one sequence and
// its subscribers hear the outcome with the pump's sync state, and
// otherwise as a delta of its own. It returns the tracker's failure as a
// *failureError.
func (s *Service) tickNow(ctx context.Context, scope Scope) error {
	s.subMu.Lock()
	p := s.subs.boards[scope]
	live := p != nil && !p.over && !p.core.DeadLocked()
	s.subMu.Unlock()
	if live {
		err := p.core.Request(ctx, readRequest{now: true})
		if !errors.Is(err, ErrSubscriptionEnded) {
			return err
		}
	}
	out, err := s.syncDelta(ctx, scope, Interactive)
	switch {
	case err != nil:
		return err
	case out.Failure != nil:
		return &failureError{f: *out.Failure}
	}
	return nil
}

// staleViews are stale as views; kind names the tracker in a failure's
// sentence.
func (s *Service) staleViews(kind string, stale []staleRead) []StaleView {
	var out []StaleView
	for _, st := range stale {
		views := make([]ResourceView, len(st.resources))
		for i, res := range st.resources {
			views[i] = resourceView(res)
		}
		out = append(out, s.staleView(kind, views, st.err))
	}
	return out
}

// refreshStopped is the failure of resources a refresh did not read
// because the write's context ended.
const refreshStopped = "The refresh stopped before it read this; reading it again shows the change."

// staleView is resources left stale by err: the write's context ending,
// the tracker's failure as ClassifyFailure sorts it, and any other as a
// failure outside the tracker, which is logged.
func (s *Service) staleView(kind string, resources []ResourceView, err error) StaleView {
	at := millis(s.now())
	var c *callError
	var fe *failureError
	var refused *requestRefused
	switch {
	case errors.Is(err, context.Canceled):
		// The write's caller went away, or the service stopped, before the
		// read was made or answered.
		return StaleView{Resources: resources, Failure: FailureView{Kind: string(FailureTransient), Message: refreshStopped, At: at}}
	case errors.As(err, &fe):
		return StaleView{Resources: resources, Failure: FailureView{Kind: string(fe.f.Kind), Message: fe.f.Message, At: at}}
	case errors.As(err, &refused):
		return StaleView{Resources: resources, Failure: FailureView{Kind: string(refused.f.Kind), Message: refused.f.Message, At: at}}
	case errors.As(err, &c):
		f := ClassifyFailure(kind, c.err)
		return StaleView{Resources: resources, Failure: FailureView{Kind: string(f.Kind), Message: f.Message, At: at}}
	}
	s.logf("trackerapp: the refresh after a write left %d resources stale: %v", len(resources), err)
	return StaleView{Resources: resources, Failure: *internalFailure(at)}
}
