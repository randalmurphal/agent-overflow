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
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/jiraprovider"
	"agent-overflow/internal/tracker/trackerapi"
)

// Planning poker. The votes are the add-on's, held outside the tracker's
// data (tracker.Poker), so nothing here touches the mirror:
//
//   - PokerSupport says whether a connection's site has the add-on, from
//     the client's listing kept 30 minutes (jira/poker.go), with no
//     invoke. A screen asks it when a ticket of the connection opens.
//   - An open ticket's handle follows the votes (FollowPoker) while its
//     poker panel is open. While a handle both follows and shows the
//     ticket, one poker run reads them every Cadence.Poker on a goroutine
//     of its own (runPoker), apart from the ticket's pump, so a slow
//     add-on never holds a refresh or a view; otherwise no run exists and
//     nothing is read. A change is told as IssueEvent.Poker and read with
//     Poker. When the last follower goes, the votes held are dropped.
//   - PokerVote, RevealPoker and ClearPoker write through Service.write,
//     so they answer as every write does; the following pump of the issue
//     reads the votes at once after one that may have happened.
//
// A poker failure is told with the votes, never as the ticket's or the
// connection's failure: when the add-on's calls fail, only poker shows
// it. Nothing writes an issue's estimate from a vote.

// PokerEvery is how often a followed ticket's votes are read while a
// person has its poker panel open: a teammate's vote or reveal shows
// within it. It costs one invoke per open panel per interval.
const PokerEvery = 5 * time.Second

// PokerView is a ticket's votes as its followers were last told: Loaded
// once a read answered, whether they are revealed, each vote (Value set
// only for the caller's own vote until revealed), when the last read that
// answered began (Unix milliseconds by this computer's wall clock), and
// the failure of the latest read, nil while reads succeed. A failure
// leaves the last votes read in place.
type PokerView struct {
	Loaded   bool            `json:"loaded"`
	Revealed bool            `json:"revealed,omitempty"`
	Votes    []PokerVoteView `json:"votes,omitempty"`
	ReadAt   int64           `json:"read_at,omitempty"`
	Failure  *FailureView    `json:"failure,omitempty"`
}

// PokerVoteView is one person's vote: Value is absent while hidden.
type PokerVoteView struct {
	Voter UserView `json:"voter"`
	Value *float64 `json:"value,omitempty"`
	Mine  bool     `json:"mine,omitempty"`
}

// PokerSupport is whether a connection's site has planning poker:
// Available, or the failure of the read that would say, with Available
// false.
type PokerSupport struct {
	Available bool         `json:"available"`
	Failure   *FailureView `json:"failure,omitempty"`
}

// PokerVoteRequest is a vote on an issue: a number from tracker's
// MinPokerVote to MaxPokerVote. A request with no vote, or a null one,
// is refused (PokerVote); zero is a vote.
type PokerVoteRequest struct {
	Issue string   `json:"issue"`
	Vote  *float64 `json:"vote"`
}

// pokerFollow is an open ticket's poker while a handle follows it, under
// subMu: the handles that follow, the view they were last told, when the
// next read is due, the retry schedule's step, whether the next read is
// a person's (a follow's first, or after a write), whether the next event
// tells a change, and the run that reads, nil while no handle both
// follows and shows the ticket (admitPokerLocked).
type pokerFollow struct {
	followers map[*handle]struct{}
	view      PokerView
	due       time.Time
	delay     time.Duration
	asked     bool
	changed   bool
	run       *pokerRun
}

// pokerRun is one goroutine reading a ticket's votes (runPoker): its
// context's cancel, a nudge to look at the read's due time again, and
// done, closed once it returned.
type pokerRun struct {
	cancel context.CancelFunc
	wake   chan struct{}
	done   chan struct{}
}

// noPoker is the refusal of poker on a tracker that cannot have it.
func noPoker(kind string) error {
	return fmt.Errorf("%w: %s has no planning poker", tracker.ErrUnsupported, trackerName(kind))
}

// PokerSupport reports whether connection id's site has the planning
// poker add-on. A Linear or Tempo connection has none, answered with no
// request. A Jira connection asks its client, which reads the site's
// extension listing at most once in 30 minutes; a read that fails
// answers its failure with Available false. It settles nothing of the
// connection: the listing is poker's.
func (s *Service) PokerSupport(ctx context.Context, id string) (PokerSupport, error) {
	if id == "" {
		return PokerSupport{}, fmt.Errorf("%w: no connection named", ErrInvalidRequest)
	}
	if err := s.enter(); err != nil {
		return PokerSupport{}, err
	}
	defer s.leave()
	row, err := s.get(id)
	switch {
	case err != nil:
		return PokerSupport{}, err
	case row.Kind != store.TrackerKindJira:
		return PokerSupport{}, nil
	}
	r, err := s.start(ctx, id, Interactive)
	switch {
	case errors.Is(err, errEnded):
		return PokerSupport{}, endedError(id, err)
	case err != nil:
		return PokerSupport{}, err
	}
	defer r.finish()
	if r.startErr != nil {
		return PokerSupport{}, r.startErr
	}
	ok, err := r.p.Poker().Support(r.ctx)
	if err != nil {
		f := ClassifyFailure(r.row.Kind, err)
		return PokerSupport{Failure: &FailureView{Kind: string(f.Kind), Message: f.Message, At: millis(s.now())}}, nil
	}
	return PokerSupport{Available: ok}, nil
}

// FollowPoker makes handle id, a ticket's, follow its issue's votes or
// stop following them, and answers the votes as last told. The first
// follower's read is due at once; it runs while a handle that follows
// shows the ticket. A handle that is not a ticket's fails with
// ErrInvalidRequest, and a ticket of a tracker with no planning poker
// with ErrUnsupported.
func (s *Service) FollowPoker(id string, follow bool) (PokerView, error) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	h := s.subs.handles[id]
	switch {
	case h == nil:
		return PokerView{}, fmt.Errorf("%w: %s", ErrUnknownSubscription, id)
	case h.ticket == nil:
		return PokerView{}, fmt.Errorf("%w: subscription %s is a board's, not a ticket's", ErrInvalidRequest, id)
	case follow && h.ticket.kind != store.TrackerKindJira:
		return PokerView{}, noPoker(h.ticket.kind)
	}
	tp := h.ticket
	switch {
	case follow && !h.poker:
		h.poker = true
		if len(tp.poker.followers) == 0 {
			tp.poker.followers = map[*handle]struct{}{}
			tp.poker.due, tp.poker.asked = s.now(), true
		}
		tp.poker.followers[h] = struct{}{}
	case !follow && h.poker:
		s.unfollowPokerLocked(h)
	}
	s.admitPokerLocked(tp)
	return tp.poker.viewCopy(), nil
}

// unfollowPokerLocked ends h's following; the last follower's end ends
// the run and drops the votes. The caller holds subMu and then calls
// admitPokerLocked.
func (s *Service) unfollowPokerLocked(h *handle) {
	if !h.poker {
		return
	}
	h.poker = false
	tp := h.ticket
	delete(tp.poker.followers, h)
	if len(tp.poker.followers) == 0 {
		if tp.poker.run != nil {
			tp.poker.run.cancel()
		}
		tp.poker = pokerFollow{}
	}
}

// Poker is the votes of handle id's ticket as its followers were last
// told, from memory: after an IssueEvent with Poker set.
func (s *Service) Poker(id string) (PokerView, error) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	h := s.subs.handles[id]
	if h == nil || h.ticket == nil {
		return PokerView{}, fmt.Errorf("%w: ticket %s", ErrUnknownSubscription, id)
	}
	return h.ticket.poker.viewCopy(), nil
}

func (p *pokerFollow) viewCopy() PokerView {
	v := p.view
	v.Votes = slices.Clone(v.Votes)
	return v
}

// admitPokerLocked gives tp a poker run while it is admitted, and none
// otherwise: admitted while tp is live, the service is not stopping, and
// a handle that follows its votes also shows it (pump.Handle's
// VisibleLocked, the one record of a handle's visibility). A run it ends
// is cancelled, its request in flight with it, and nothing it reads
// after is applied. Its callers are every change to what admits it:
// FollowPoker (a follow or an unfollow), SetSubscriptionVisible (a ticket
// handle shown or hidden), ReleaseSubscription (a handle released) and
// retireTicketLocked (the ticket's end: its last release, its connection
// turned off or removed, its generation ended, Stop). The caller holds
// subMu.
func (s *Service) admitPokerLocked(tp *ticketPump) {
	admitted := !tp.over && !tp.core.DeadLocked() && !s.subs.halted
	if admitted {
		admitted = false
		for h := range tp.poker.followers {
			if h.ref.VisibleLocked() {
				admitted = true
				break
			}
		}
	}
	switch run := tp.poker.run; {
	case admitted && run == nil:
		ctx, cancel := context.WithCancel(tp.core.Context())
		run = &pokerRun{cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{})}
		tp.poker.run = run
		if tp.pokerRuns == nil {
			tp.pokerRuns = map[*pokerRun]struct{}{}
		}
		tp.pokerRuns[run] = struct{}{}
		s.workers.Add(1)
		go s.runPoker(ctx, tp, run)
	case !admitted && run != nil:
		run.cancel()
		tp.poker.run = nil
	}
}

// runPoker is a poker run's goroutine: it reads tp's votes each time a
// read is due until its context ends, at its admission's end or the
// ticket's. The ticket's pump waits for it before it exits
// (awaitPokerRuns), and Stop through s.workers.
func (s *Service) runPoker(ctx context.Context, tp *ticketPump, run *pokerRun) {
	defer s.workers.Done()
	defer func() {
		s.subMu.Lock()
		delete(tp.pokerRuns, run)
		s.subMu.Unlock()
		close(run.done)
	}()
	defer run.cancel()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		s.subMu.Lock()
		due := tp.poker.due
		s.subMu.Unlock()
		timer.Reset(max(due.Sub(s.now()), 0))
		select {
		case <-ctx.Done():
			return
		case <-run.wake:
			continue
		case <-timer.C:
		}
		s.readPoker(ctx, tp, run)
	}
}

// awaitPokerRuns ends tp's poker runs and waits for each to return, the
// last of a ticket's pump's exit, so a pump that exited leaves no run.
func (s *Service) awaitPokerRuns(tp *ticketPump) {
	s.subMu.Lock()
	runs := make([]*pokerRun, 0, len(tp.pokerRuns))
	for run := range tp.pokerRuns {
		run.cancel()
		runs = append(runs, run)
	}
	s.subMu.Unlock()
	for _, run := range runs {
		<-run.done
	}
}

// readPoker reads tp's votes when the read is due and run is tp's run,
// and tells the followers what changed. A read that fails keeps the
// votes last read, tells its failure and is due again on the retry
// schedule. A read a person asked for while this one was in flight is
// due at once, and any read waits for a rate limit to lift. A read whose
// run was ended meanwhile is dropped.
func (s *Service) readPoker(ctx context.Context, tp *ticketPump, run *pokerRun) {
	s.subMu.Lock()
	if tp.poker.run != run || tp.poker.due.After(s.now()) {
		s.subMu.Unlock()
		return
	}
	u := Background
	if tp.poker.asked {
		u = Interactive
	}
	tp.poker.asked = false
	s.subMu.Unlock()

	began := millis(s.now())
	read, release, err := s.readPokerRound(ctx, tp.key, u)
	if ctx.Err() != nil {
		return
	}
	s.subMu.Lock()
	defer s.subMu.Unlock()
	ps := &tp.poker
	if ps.run != run || tp.over {
		return
	}
	next := ps.view
	if err == nil {
		next = read
		next.ReadAt = began
		ps.delay, ps.due = 0, s.now().Add(s.cadence.poker())
	} else {
		next.Failure = s.pokerFailureView(tp.kind, err)
		ps.delay = pump.NextDelay(ps.delay, s.cadence.RetryBase, s.cadence.poker())
		ps.due = s.now().Add(ps.delay)
	}
	if ps.asked {
		// A write or a new follow asked for a read while this one was in
		// flight; this read may have begun before the write landed.
		ps.due = s.now()
	}
	if release.After(ps.due) {
		ps.due = release
	}
	if !samePoker(ps.view, next) {
		ps.changed, tp.pending = true, true
	}
	ps.view = next
	s.flushTicketLocked(tp, false)
}

// pokerFailureView is err, a failed poker read on a connection of kind,
// as the followers see it: a tracker call's (callError) as ClassifyFailure
// sorts it, and any other, a failure outside the tracker, logged.
func (s *Service) pokerFailureView(kind string, err error) *FailureView {
	at := millis(s.now())
	c, ok := errors.AsType[*callError](err)
	if !ok {
		s.logf("trackerapp: read planning poker: %v", err)
		return internalFailure(at)
	}
	f := ClassifyFailure(kind, c.err)
	return &FailureView{Kind: string(f.Kind), Message: f.Message, At: at}
}

// readPokerRound reads key's votes through its connection's provider,
// urgency u, as the view shows them. release is when a rate limit that
// failed the read lifts.
func (s *Service) readPokerRound(ctx context.Context, key ticketKey, u Urgency) (_ PokerView, release time.Time, _ error) {
	r, err := s.start(ctx, key.connection, u)
	if err != nil {
		return PokerView{}, time.Time{}, err
	}
	defer r.finish()
	if r.startErr != nil {
		return PokerView{}, time.Time{}, r.startErr
	}
	// A follow is refused on any tracker but Jira, whose provider has a
	// poker (FollowPoker).
	round, err := r.p.Poker().Read(r.ctx, key.issue)
	if err != nil {
		if limited, ok := errors.AsType[*trackerapi.RateLimitedError](err); ok {
			release = limited.Until
		}
		return PokerView{}, release, call(err)
	}
	names := catalogNames{users: map[string]tracker.User{}}
	for _, u := range round.Users {
		names.users[string(u.ID)] = u
	}
	v := PokerView{Loaded: true, Revealed: round.Revealed, Votes: make([]PokerVoteView, len(round.Votes))}
	for i, vote := range round.Votes {
		v.Votes[i] = PokerVoteView{Voter: names.user(string(vote.Voter)), Value: vote.Value, Mine: vote.Mine}
	}
	return v, time.Time{}, nil
}

// samePoker reports two views that show the same: the read time alone
// is no change.
func samePoker(a, b PokerView) bool {
	return a.Loaded == b.Loaded && a.Revealed == b.Revealed && sameFailure(a.Failure, b.Failure) &&
		slices.EqualFunc(a.Votes, b.Votes, func(x, y PokerVoteView) bool {
			return x.Voter == y.Voter && x.Mine == y.Mine && (x.Value == nil) == (y.Value == nil) && (x.Value == nil || *x.Value == *y.Value)
		})
}

// PokerVote casts or changes the caller's vote on in.Issue. A request
// with no vote fails with ErrInvalidRequest before anything is sent, so
// a form sent before a value was picked never replaces a vote. It writes
// no estimate.
func (s *Service) PokerVote(ctx context.Context, id string, in PokerVoteRequest) (WriteView, error) {
	if in.Vote == nil {
		return WriteView{}, fmt.Errorf("%w: a vote needs its value, a number from %d to %d", ErrInvalidRequest, tracker.MinPokerVote, tracker.MaxPokerVote)
	}
	issue, value := tracker.IssueID(in.Issue), *in.Vote
	return s.pokerWrite(ctx, id, issue, func(ctx context.Context, pk tracker.Poker) tracker.Result {
		return pk.Vote(ctx, tracker.PokerVoteInput{Issue: issue, Value: value})
	})
}

// RevealPoker shows every vote on issue.
func (s *Service) RevealPoker(ctx context.Context, id, issue string) (WriteView, error) {
	return s.pokerWrite(ctx, id, tracker.IssueID(issue), func(ctx context.Context, pk tracker.Poker) tracker.Result {
		return pk.Reveal(ctx, tracker.IssueID(issue))
	})
}

// ClearPoker removes every vote on issue.
func (s *Service) ClearPoker(ctx context.Context, id, issue string) (WriteView, error) {
	return s.pokerWrite(ctx, id, tracker.IssueID(issue), func(ctx context.Context, pk tracker.Poker) tracker.Result {
		return pk.Clear(ctx, tracker.IssueID(issue))
	})
}

// pokerWrite makes the poker write w on connection id about issue, as
// every write, refused as a part on a tracker with no planning poker;
// after a write that may have happened, the issue's following pump reads
// the votes at once.
func (s *Service) pokerWrite(ctx context.Context, id string, issue tracker.IssueID, w func(context.Context, tracker.Poker) tracker.Result) (WriteView, error) {
	view, err := s.write(ctx, id, writeOp{call: func(ctx context.Context, p tracker.Provider) tracker.Result {
		pk := p.Poker()
		if pk == nil {
			return tracker.Refuse(tracker.StepPoker, tracker.Refusal{Reason: tracker.ReasonUnsupported,
				Message: "this tracker has no planning poker", Err: tracker.ErrUnsupported}, issue)
		}
		return w(ctx, pk)
	}})
	if err == nil && slices.ContainsFunc(view.Parts, func(p PartView) bool { return p.State != "refused" && p.State != "not_sent" }) {
		s.readPokerNow(id, issue)
	}
	return view, err
}

// readPokerNow makes the following pump of issue on connection id, if
// any, read its votes at once, as a person's read.
func (s *Service) readPokerNow(id string, issue tracker.IssueID) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	tp := s.subs.tickets[ticketKey{connection: id, issue: issue}]
	if tp == nil || len(tp.poker.followers) == 0 {
		return
	}
	tp.poker.due, tp.poker.asked = s.now(), true
	if tp.poker.run != nil {
		nudge(tp.poker.run.wake)
	}
}

// pokerOwn reports an error that is planning poker's own: a site without
// the add-on, or a call the add-on or Atlassian's gateway answered in a
// way only poker shows (jira.PokerError).
func pokerOwn(err error) bool {
	_, ok := errors.AsType[*jira.PokerError](err)
	return ok || errors.Is(err, tracker.ErrPokerAbsent) || errors.Is(err, jira.ErrPokerAbsent)
}

// pokerFailure is the failure of err, which pokerOwn reports: the sentence
// says poker is unavailable here and why, and that nothing else is.
func pokerFailure(err error) Failure {
	if pe, ok := errors.AsType[*jira.PokerError](err); ok {
		return Failure{Kind: FailureTracker, Message: "planning poker is unavailable on this site right now: " + pe.Step + " " + pe.Detail +
			"; the rest of the ticket is unaffected"}
	}
	return Failure{Kind: FailureTracker, Message: jiraprovider.PokerAbsent}
}
