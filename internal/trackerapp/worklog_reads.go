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
//   - One read at a time per connection: the dashboards' reads,
//     RefreshWorklogs (the Time screen, an agent's ReadTime) take one gate,
//     which is the connection's for the life of the process and never
//     replaced (worklogConn.turn).
//   - A read starts on its own only from an evaluation in which a widget
//     counted the logged time (Board.Worklogs, wantWorklogs): it queues the
//     days the evaluation found not read, and the newest worklogSpan days
//     once worklogRereadEvery has passed since they were last queued. A
//     person's Refresh or Read again (ReadWorklogDays) queues every day of
//     the window.
//   - One run reads what is queued, newest first, worklogSpan days a read,
//     back to back, and tells the connection's boards once when it ends
//     (BoardEvent.Logged), so their dashboards count again. A screen that
//     stops showing does not stop it: the reads are stored for the next.
//     Turning the connection off, the service's Stop, and a change of the
//     account or time source do.
//   - After a run's read failed, nothing is read on its own until a person
//     asks or the account or time source changes; the failure is shown
//     (trackerdash.WorklogState) until then.
//   - Before each read the run checks that the connection's account and
//     time source are still the ones it reads for.

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
	// queued is the days to read, at most the window's; reading is the
	// first and last day of the read in flight, "" when none is.
	queued  map[string]bool
	reading [2]string
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
		c = &worklogConn{turn: make(chan struct{}, 1), queued: map[string]bool{}}
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

// reset makes c's state author's under gen, with nothing queued, failed
// or read, stopping its run. The caller holds mu.
func (c *worklogConn) reset(author string, gen int64) {
	if c.run != nil {
		c.run.cancel()
		c.run = nil
	}
	c.author, c.gen, c.failure, c.newest, c.reading = author, gen, "", time.Time{}, [2]string{}
	clear(c.queued)
}

// stop forgets connection id's reads but its gate: its run stops, and the
// next starts with nothing queued, failed or read.
func (r *worklogReads) stop(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.byID[id]; c != nil {
		c.reset("", 0)
	}
}

// halt stops every run and lets none start.
func (r *worklogReads) halt() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.halted = true
	for _, c := range r.byID {
		c.reset("", 0)
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
// board was read: it queues what need finds not read and, once
// worklogRereadEvery has passed, the newest days, but the days of the
// read in flight, which an evaluation that copied the board before it
// stored finds not read; starts a run when one is due; and answers the
// state of the reads. It waits on nothing.
func (s *Service) wantWorklogs(id, author string, gen int64, need trackerdash.WorklogNeed) trackerdash.WorklogState {
	r := &s.worklogs
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.conn(id)
	if c.author != author || c.gen != gen {
		c.reset(author, gen)
	}
	if c.failure == "" && !r.halted {
		queue := func(d string) {
			if d < c.reading[0] || d > c.reading[1] {
				c.queued[d] = true
			}
		}
		for _, d := range need.Unread {
			queue(d)
		}
		if now := s.steady(); c.newest.IsZero() || now.Sub(c.newest) >= worklogRereadEvery {
			if today, err := tracker.ParseDate(need.Today); err == nil {
				for i := range worklogSpan {
					queue(dayTime(today).AddDate(0, 0, -i).Format(time.DateOnly))
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
	if c.author != author || c.gen != row.TimeSourceGen {
		c.reset(author, row.TimeSourceGen)
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

// readNextWorklogs takes the turn and reads the newest span of what c
// queues for run: at most worklogSpan days ending at the newest queued
// day and starting no earlier than the oldest. It reads nothing, with
// errWorklogsDone, once nothing is queued or run is not c's.
func (s *Service) readNextWorklogs(id string, c *worklogConn, run *worklogRun, author string, gen int64) (*FailureView, error) {
	if err := c.take(run.ctx); err != nil {
		return nil, err
	}
	defer c.give()
	s.worklogs.mu.Lock()
	var days []string
	if c.run == run {
		for d := range c.queued {
			days = append(days, d)
		}
	}
	if len(days) == 0 {
		s.worklogs.mu.Unlock()
		return nil, errWorklogsDone
	}
	slices.Sort(days)
	last, err := tracker.ParseDate(days[len(days)-1])
	if err != nil {
		s.worklogs.mu.Unlock()
		return nil, err
	}
	start := max(days[0], dayTime(last).AddDate(0, 0, 1-worklogSpan).Format(time.DateOnly))
	for _, d := range days {
		if d >= start {
			delete(c.queued, d)
		}
	}
	c.reading = [2]string{start, days[len(days)-1]}
	s.worklogs.mu.Unlock()
	defer func() {
		s.worklogs.mu.Lock()
		if c.run == run {
			c.reading = [2]string{}
		}
		s.worklogs.mu.Unlock()
	}()
	first, err := tracker.ParseDate(start)
	if err != nil {
		return nil, err
	}
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.leave()
	row, now, err := s.timeAuthor(id)
	switch {
	case err != nil:
		return nil, err
	case now != author || row.TimeSourceGen != gen:
		// Another account or time source: the next evaluation starts its
		// reads over (wantWorklogs).
		return nil, errWorklogsDone
	}
	return s.readWorklogSpan(run.ctx, id, author, first, last)
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
