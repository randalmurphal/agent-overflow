package trackerapp

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"agent-overflow/internal/tracker"
)

// worklogRereadEvery is the least time between two automatic reads of the
// newest worklogSpan days of a connection's verified account
// (ReadWorklogDays with WorklogReadChanged): a dashboard shown for an hour
// while its board changes makes at most 12 of them.
const worklogRereadEvery = 5 * time.Minute

// The reasons of a ReadWorklogDays call.
const (
	// WorklogReadShown is a call because logged-time widgets show: it
	// reads the days of the window never read, each at most once on its
	// own, and the days a person asked to read again.
	WorklogReadShown = "shown"
	// WorklogReadChanged is WorklogReadShown on a board that changed since
	// its last such call: the newest worklogSpan days are read again
	// once worklogRereadEvery has passed since they last were.
	WorklogReadChanged = "changed"
	// WorklogReadPerson is a person asking (a dashboard's Refresh or a
	// widget's Read again): every day of the window is read again now.
	WorklogReadPerson = "person"
)

// worklogReads owns the reads of the logged time of each connection's
// verified account on this computer: one read at a time per connection,
// and what the automatic reads have done. Its lock guards the map alone,
// is held for map work only and never with another lock taken while it
// is held; stopUsing takes it under opMu to drop a connection's turn.
type worklogReads struct {
	mu   sync.Mutex
	byID map[string]*worklogTurn
}

// worklogTurn is the reads of one connection's logged time by one author
// under one time source generation (TrackerConnection.TimeSourceGen). A
// read holds the turn (take) from before it reads which days are read to
// after it wrote what it read, so two reads never read the same days at
// once. Its fields are touched only while holding the turn.
type worklogTurn struct {
	author string
	gen    int64
	turn   chan struct{}
	// tried are the window's days read on their own (WorklogReadShown)
	// since a person last asked: a day still unread after it (its read
	// failed) is not read again on its own. It holds only days of the
	// window: each call drops the rest.
	tried map[string]bool
	// owed are the days to read again whether or not they were read: the
	// window's days after a person asked, the newest worklogSpan days
	// when a board changed. Bounded by the window as tried is.
	owed map[string]bool
	// reread is when the newest days were last owed by a board's change,
	// on the service's steady clock.
	reread time.Time
}

// of is the turn of connection id's reads of author's logged time under
// time source generation gen: a new one when the connection has none or
// its turn is another author's or generation's.
func (r *worklogReads) of(id, author string, gen int64) *worklogTurn {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t := r.byID[id]; t != nil && t.author == author && t.gen == gen {
		return t
	}
	if r.byID == nil {
		r.byID = map[string]*worklogTurn{}
	}
	t := &worklogTurn{author: author, gen: gen, turn: make(chan struct{}, 1), tried: map[string]bool{}, owed: map[string]bool{}}
	r.byID[id] = t
	return t
}

// drop forgets connection id's turn: a read holding it finishes, and the
// next read starts a new one with nothing tried or owed.
func (r *worklogReads) drop(id string) {
	r.mu.Lock()
	delete(r.byID, id)
	r.mu.Unlock()
}

// take waits for the turn, or for ctx to end.
func (t *worklogTurn) take(ctx context.Context) error {
	select {
	case t.turn <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("trackerapp: wait to read the logged time: %w", ctx.Err())
	}
}

func (t *worklogTurn) give() { <-t.turn }

// WorklogRead is what one ReadWorklogDays did. Read reports that it read
// days from the tracker, so the counts of logged time may have changed.
// More reports that days are left to read now: the caller calls again
// while the widgets still show. Failure is why the read failed; nothing
// more is read on its own until a person asks. RereadIn is how long, in
// ms, until a board's change can have the newest days read again: the
// caller asks again with WorklogReadChanged then, if the widgets still
// show.
type WorklogRead struct {
	Read     bool         `json:"read,omitempty"`
	More     bool         `json:"more,omitempty"`
	Failure  *FailureView `json:"failure,omitempty"`
	RereadIn int64        `json:"rereadIn,omitempty"`
}

// ReadWorklogDays reads at most worklogSpan days of the logged time of
// connection id's verified account for a dashboard whose logged-time
// widgets show, its day being today: the newest owed days (owed), else
// the newest days of the window never read nor tried. The window is the
// days from the day before the history window's start (as the dashboard
// export's logged time starts) to today. reason is one of
// WorklogReadShown, WorklogReadChanged and WorklogReadPerson. It waits
// its turn with RefreshWorklogs and the other reads of the connection's
// logged time, so two calls never read the same days at once. today must
// be within a day of the service's own date (a time zone's date).
func (s *Service) ReadWorklogDays(ctx context.Context, id, today, reason string) (WorklogRead, error) {
	last, err := parseDay(today)
	if err != nil {
		return WorklogRead{}, err
	}
	switch reason {
	case WorklogReadShown, WorklogReadChanged, WorklogReadPerson:
	default:
		return WorklogRead{}, fmt.Errorf("%w: a logged-time read's reason is shown, changed or person, not %q", ErrInvalidRequest, reason)
	}
	if off := dayTime(last).Sub(s.now().UTC().Truncate(24 * time.Hour)); off < -24*time.Hour || off > 24*time.Hour {
		return WorklogRead{}, fmt.Errorf("%w: %s is not today in any time zone", ErrInvalidRequest, today)
	}
	first, err := tracker.DateOf(s.window().UTC().AddDate(0, 0, -1))
	if err != nil {
		return WorklogRead{}, fmt.Errorf("trackerapp: the window's first day: %w", err)
	}
	if err := s.enter(); err != nil {
		return WorklogRead{}, err
	}
	defer s.leave()
	row, author, err := s.timeAuthor(id)
	if err != nil {
		return WorklogRead{}, err
	}
	t := s.worklogs.of(id, author, row.TimeSourceGen)
	if err := t.take(ctx); err != nil {
		return WorklogRead{}, err
	}
	defer t.give()

	window := make([]string, 0, daysBetween(first, last)+1)
	for d := dayTime(first); !d.After(dayTime(last)); d = d.AddDate(0, 0, 1) {
		window = append(window, d.Format(time.DateOnly))
	}
	maps.DeleteFunc(t.tried, func(d string, _ bool) bool { return d < window[0] || d > today })
	maps.DeleteFunc(t.owed, func(d string, _ bool) bool { return d < window[0] || d > today })
	var out WorklogRead
	switch now := s.steady(); {
	case reason == WorklogReadPerson:
		clear(t.tried)
		for _, d := range window {
			t.owed[d] = true
		}
		t.reread = now
	case reason == WorklogReadChanged && now.Sub(t.reread) < worklogRereadEvery:
		out.RereadIn = max(1, (worklogRereadEvery - now.Sub(t.reread)).Milliseconds())
	case reason == WorklogReadChanged:
		for _, d := range window[max(0, len(window)-worklogSpan):] {
			t.owed[d] = true
		}
		t.reread = now
	}
	read, err := s.store.ReadMirrorWorklogReadDays(id, author, window[0], today)
	if err != nil {
		return WorklogRead{}, fmt.Errorf("trackerapp: read which days of %s's logged time were read: %w", author, err)
	}
	// left are the days to read, newest first.
	left := func() []string {
		var days []string
		for _, d := range slices.Backward(window) {
			if _, done := slices.BinarySearch(read, d); t.owed[d] || (!done && !t.tried[d]) {
				days = append(days, d)
			}
		}
		return days
	}
	todo := left()
	if len(todo) == 0 {
		return out, nil
	}
	end, err := parseDay(todo[0])
	if err != nil {
		return WorklogRead{}, err
	}
	start := max(window[0], dayTime(end).AddDate(0, 0, 1-worklogSpan).Format(time.DateOnly))
	from, err := parseDay(start)
	if err != nil {
		return WorklogRead{}, err
	}
	for _, d := range window {
		if d >= start && d <= todo[0] {
			delete(t.owed, d)
			t.tried[d] = true
		}
	}
	out.Read = true
	out.Failure, err = s.readWorklogSpan(ctx, id, author, from, end)
	if err != nil {
		return WorklogRead{}, err
	}
	if out.Failure != nil {
		clear(t.owed)
		return out, nil
	}
	out.More = len(left()) > 0
	return out, nil
}
