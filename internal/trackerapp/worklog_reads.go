package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/trackerdash"
)

// The reads of the logged time of each connection's verified account
// that the dashboards count (trackerdash worklogs.go). What must hold:
//   - One run at a time reads a connection's verified account from its
//     current time source. Every read takes one gate, the dashboards' and
//     RefreshWorklogs' (the Time screen, an agent's ReadTime) alike, which
//     is the connection's for the life of the process and never replaced
//     (worklogConn.turn).
//   - The record is of one identity: an account and a time source
//     generation (TrackerConnection.TimeSourceGen, which only grows). It
//     never moves back: an evaluation of an older generation changes
//     nothing (worklogConn.current).
//   - A read starts on its own only from an evaluation in which a number
//     needed the window (Board.Worklogs, wantWorklogs): it queues the days
//     the evaluation found not read but those a read under the identity
//     already took (handed), and the newest worklogSpan days, taken or
//     not, once worklogRereadEvery has passed since they were last
//     queued. A person's Refresh or Read again (ReadWorklogDays) queues
//     every day of the window.
//   - A day handed out ends stored, with a failure shown, or with the
//     record reset: no day sits handed and unread with nothing reading
//     it and nothing said.
//   - One run reads what is queued, newest first, worklogSpan days a read,
//     back to back, and tells the connection's boards once when it ends
//     (BoardEvent.Logged), so their dashboards count again. A screen that
//     stops showing does not stop it: the reads are stored for the next.
//     Turning the connection off, the service's Stop, and a change of the
//     account or time source do.
//   - After a run's read failed, nothing is read on its own until a person
//     asks or the account or time source changes; the failure is shown
//     (trackerdash.WorklogState) until then.
//   - Each read checks, on the connection row it reads with, that the
//     account and time source are the ones its run reads for; one that
//     moved ends the run and forgets what it queued and handed.

// worklogRereadEvery is the least time between two automatic queues of
// the newest worklogSpan days of a connection's verified account: a
// dashboard shown for an hour reads them at most 12 times.
const worklogRereadEvery = 5 * time.Minute

// worklogReads owns the reads. Its lock guards the map and every
// worklogConn's fields but turn; it is held for that work alone, never
// while another lock is taken or a turn awaited. stopUsing takes it under
// opMu.
type worklogReads struct {
	mu   sync.Mutex
	byID map[string]*worklogConn
	// halted is set by the service's Stop: no run starts after it.
	halted bool
}

// worklogConn is one connection's reads.
type worklogConn struct {
	// turn is the gate: a read holds it (take) from before it reads to
	// after it wrote what it read.
	turn chan struct{}
	// author and gen are the account and time source generation
	// (TrackerConnection.TimeSourceGen) the state below is of.
	author string
	gen    int64
	// queued is the days to read, at most the window's; handed is the
	// days a read under this identity took, read or in flight.
	queued map[string]bool
	handed map[string]bool
	// run is the run going, nil when none is.
	run *worklogRun
	// failure is why the last run's read failed, "" when none did.
	failure string
	// newest is when the newest days were last queued, on the steady
	// clock.
	newest time.Time
}

// worklogRun is one run's identity and its cancel.
type worklogRun struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// conn is connection id's reads, made on first use. The caller holds mu.
func (r *worklogReads) conn(id string) *worklogConn {
	c := r.byID[id]
	if c == nil {
		if r.byID == nil {
			r.byID = map[string]*worklogConn{}
		}
		c = &worklogConn{turn: make(chan struct{}, 1), queued: map[string]bool{}, handed: map[string]bool{}}
		r.byID[id] = c
	}
	return c
}

// gate is connection id's turn, for a read of any author.
func (r *worklogReads) gate(id string) *worklogConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.conn(id)
}

// reset makes c's state author's under gen, with nothing queued, handed
// or failed, stopping its run. The caller holds mu.
func (c *worklogConn) reset(author string, gen int64) {
	if c.run != nil {
		c.run.cancel()
		c.run = nil
	}
	c.author, c.gen, c.failure, c.newest = author, gen, "", time.Time{}
	clear(c.queued)
	clear(c.handed)
}

// current makes c's state author's under gen when they are newer than
// its own, and reports whether they are c's now: false for a generation
// older than c's, whose demand is late. The caller holds mu.
func (c *worklogConn) current(author string, gen int64) bool {
	switch {
	case gen < c.gen:
		return false
	case gen > c.gen || author != c.author:
		c.reset(author, gen)
	}
	return true
}

// stop forgets connection id's reads but its gate and the generation it
// has seen: its run stops, and the next starts with nothing queued,
// handed or failed.
func (r *worklogReads) stop(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.byID[id]; c != nil {
		c.reset("", c.gen)
	}
}

// halt stops every run and lets none start.
func (r *worklogReads) halt() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.halted = true
	for _, c := range r.byID {
		c.reset("", c.gen)
	}
}

// take waits for the turn, or for ctx to end.
func (c *worklogConn) take(ctx context.Context) error {
	select {
	case c.turn <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("trackerapp: wait to read the logged time: %w", ctx.Err())
	}
}

func (c *worklogConn) give() { <-c.turn }

// wantWorklogs is Board.Worklogs of a board of connection id, whose
// verified account was author under time source generation gen when the
// board was read: it queues what need finds not read but the days a read
// already took, which an evaluation that copied the board before they
// were stored finds not read, and, once worklogRereadEvery has passed,
// the newest days; starts a run when one is due; and answers the state of
// the reads. An evaluation of an older generation queues nothing. It
// waits on nothing.
func (s *Service) wantWorklogs(id, author string, gen int64, need trackerdash.WorklogNeed) trackerdash.WorklogState {
	r := &s.worklogs
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.conn(id)
	if c.current(author, gen) && c.failure == "" {
		for _, d := range need.Unread {
			if !c.handed[d] {
				c.queued[d] = true
			}
		}
		if now := s.steady(); c.newest.IsZero() || now.Sub(c.newest) >= worklogRereadEvery {
			if today, err := tracker.ParseDate(need.Today); err == nil {
				for i := range worklogSpan {
					c.queued[dayTime(today).AddDate(0, 0, -i).Format(time.DateOnly)] = true
				}
				c.newest = now
			} else {
				s.logf("trackerapp: the newest logged time of %s is not read again: %v", id, err)
			}
		}
		s.startWorklogsLocked(id, c)
	}
	return trackerdash.WorklogState{Reading: c.run != nil, Failure: c.failure}
}

// ReadWorklogDays is a person's Refresh or Read again of the logged time
// the dashboards count of connection id's verified account: every day of
// the window, from loggedFrom to today in timeZone, is read again now,
// and a failure that stopped the reads is forgotten. It starts the reads
// and returns; the boards are told when they end.
func (s *Service) ReadWorklogDays(id, timeZone string) error {
	loc, err := time.LoadLocation(timeZone)
	if err != nil || timeZone == "" {
		return fmt.Errorf("%w: %q is not a time zone", ErrInvalidRequest, timeZone)
	}
	first, err := s.loggedFrom()
	if err != nil {
		return err
	}
	last, err := tracker.DateOf(s.now().In(loc))
	if err != nil {
		return fmt.Errorf("trackerapp: today in %s: %w", timeZone, err)
	}
	if err := s.enter(); err != nil {
		return err
	}
	defer s.leave()
	row, author, err := s.timeAuthor(id)
	if err != nil {
		return err
	}
	r := &s.worklogs
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.conn(id)
	if !c.current(author, row.TimeSourceGen) {
		return fmt.Errorf("trackerapp: the time source of %s is generation %d, older than the %d its reads of logged time are of", id, row.TimeSourceGen, c.gen)
	}
	c.failure, c.newest = "", s.steady()
	for d := dayTime(first); !d.After(dayTime(last)); d = d.AddDate(0, 0, 1) {
		c.queued[d.Format(time.DateOnly)] = true
	}
	s.startWorklogsLocked(id, c)
	return nil
}

// startWorklogsLocked starts c's run when days are queued and none runs.
// The caller holds worklogs.mu and is inside the service (enter), so Stop
// waits for the run it starts.
func (s *Service) startWorklogsLocked(id string, c *worklogConn) {
	if c.run != nil || len(c.queued) == 0 || s.worklogs.halted {
		return
	}
	ctx, cancel := context.WithCancel(s.life)
	run := &worklogRun{ctx: ctx, cancel: cancel}
	c.run = run
	s.workers.Add(1)
	go s.runWorklogs(id, c, run, c.author, c.gen)
}

// runWorklogs reads what c queues for author under gen, newest first, one
// span at a time under the turn, until nothing is queued, a read fails or
// the run is stopped, then tells the connection's boards.
func (s *Service) runWorklogs(id string, c *worklogConn, run *worklogRun, author string, gen int64) {
	defer s.workers.Done()
	defer run.cancel()
	for {
		failure, err := s.readNextWorklogs(id, c, run, author, gen)
		if errors.Is(err, errWorklogsDone) || run.ctx.Err() != nil {
			break
		}
		if err == nil && failure == nil {
			continue
		}
		msg := ""
		if failure != nil {
			msg = failure.Message
		} else {
			msg = err.Error()
		}
		s.logf("trackerapp: read the logged time of %s on %s: %s", author, id, msg)
		s.worklogs.mu.Lock()
		if c.run == run {
			c.failure = msg
			clear(c.queued)
		}
		s.worklogs.mu.Unlock()
		break
	}
	s.worklogs.mu.Lock()
	mine := c.run == run
	if mine {
		c.run = nil
	}
	s.worklogs.mu.Unlock()
	if mine {
		s.tellLogged(id)
	}
}

// errWorklogsDone is a run with nothing left to read.
var errWorklogsDone = errors.New("trackerapp: no logged time is queued")

// errWorklogsMoved is a read whose connection's account or time source
// is no longer its run's.
var errWorklogsMoved = errors.New("trackerapp: the account or time source of the logged time moved")

// readNextWorklogs takes the turn and reads the newest span of what c
// queues for run (handOut). It reads nothing, with errWorklogsDone, once
// nothing is queued or run is not c's, and when the connection row the
// read runs with is of another account or time source than the run's,
// forgetting then what c queued and handed for the run.
func (s *Service) readNextWorklogs(id string, c *worklogConn, run *worklogRun, author string, gen int64) (*FailureView, error) {
	if err := c.take(run.ctx); err != nil {
		return nil, err
	}
	defer c.give()
	first, last, err := s.handOut(c, run)
	if err != nil {
		return nil, err
	}
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.leave()
	if _, _, err := s.timeAuthor(id); err != nil {
		return nil, err
	}
	failure, err := s.readWorklogSpan(run.ctx, id, author, first, last, func(row store.TrackerConnection) error {
		if row.VerifiedAccountID != author || row.TimeSourceGen != gen {
			return errWorklogsMoved
		}
		return nil
	})
	if errors.Is(err, errWorklogsMoved) {
		s.worklogs.mu.Lock()
		if c.run == run {
			clear(c.queued)
			clear(c.handed)
		}
		s.worklogs.mu.Unlock()
		return nil, errWorklogsDone
	}
	return failure, err
}

// handOut takes the newest span of what c queues for run off the queue
// and hands it out: at most worklogSpan days ending at the newest queued
// day and starting no earlier than the oldest. errWorklogsDone is nothing
// queued, or run not c's.
func (s *Service) handOut(c *worklogConn, run *worklogRun) (first, last tracker.Date, err error) {
	s.worklogs.mu.Lock()
	defer s.worklogs.mu.Unlock()
	var days []string
	if c.run == run {
		for d := range c.queued {
			days = append(days, d)
		}
	}
	if len(days) == 0 {
		return first, last, errWorklogsDone
	}
	slices.Sort(days)
	if last, err = tracker.ParseDate(days[len(days)-1]); err != nil {
		return first, last, err
	}
	if first, err = tracker.ParseDate(max(days[0], dayTime(last).AddDate(0, 0, 1-worklogSpan).Format(time.DateOnly))); err != nil {
		return first, last, err
	}
	for d := dayTime(first); !d.After(dayTime(last)); d = d.AddDate(0, 0, 1) {
		day := d.Format(time.DateOnly)
		delete(c.queued, day)
		c.handed[day] = true
	}
	return first, last, nil
}

// tellLogged tells every board pump of connection id, on the board's
// event, that the logged time its dashboards count was read
// (BoardEvent.Logged).
func (s *Service) tellLogged(id string) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	told := map[*boardPump]bool{}
	for _, h := range s.subs.handles {
		p := h.board
		if p == nil || told[p] || p.scope.Connection != id || p.over || p.core.DeadLocked() {
			continue
		}
		told[p] = true
		p.pending.logged = true
		p.pending.due = true
		s.flushBoardLocked(p)
	}
}

// worklogsOf is the Board.Worklogs of a board of connection row: nil for
// a connection with no time tracking or no verified account.
func (s *Service) worklogsOf(row store.TrackerConnection) func(trackerdash.WorklogNeed) trackerdash.WorklogState {
	if !hasTimeTracking(row.Kind) || row.VerifiedAccountID == "" {
		return nil
	}
	return func(need trackerdash.WorklogNeed) trackerdash.WorklogState {
		return s.wantWorklogs(row.ID, row.VerifiedAccountID, row.TimeSourceGen, need)
	}
}
