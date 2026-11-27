// Package activity records when the person gave input to this computer's
// app and on which thread, for the time draft, and answers it per day.
//
// What it keeps true:
//
//   - A minute counts when there was input in the Window before it. A
//     screen reports its input at most once a minute, with how long ago
//     its last input was, and at once when the thread it shows changes
//     (Report); the recorder keeps one open span per connection in memory,
//     and input on the same thread within Window of the last extends it. A
//     span ends at its last input plus Window, or sooner: at input on
//     another thread, at the connection's end (End), or at the flush that
//     finds it idle. Each open span is tied to its connection, and its end
//     unties it.
//   - The recorder records only while it is on (SetOn): while a Jira
//     connection is on, which the app tells it after each change of the
//     connections. Turning it off ends every open span then and writes
//     what came before; while off, input and turn starts record nothing.
//   - Nothing is written per report or per turn start. One flush every
//     FlushEvery writes what changed (a span inserted once, its end moved
//     after) and the days of the threads that started a turn since (Ran),
//     and Close flushes once more.
//   - A flush that fails keeps what it could not write for the next one,
//     at most maxUnwritten closed spans, and the earliest time not written
//     is the recorder's Status until a flush writes again.
//   - The recorder owns the prune of what it measured: once a day, the
//     spans and thread days older than Keep, and nothing else.
//   - Days answers the person's time per local day, never the spans: what
//     was written and what is still in memory, read beside no flush, each
//     instant counted once. A day starts at its midnight, or at the moment
//     after a midnight its zone skips (DayRange).
package activity

import (
	"cmp"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/store"
)

// Window is how long after input a minute still counts as active.
const Window = 3 * time.Minute

// FlushEvery is how often the recorder writes the spans that changed.
const FlushEvery = 5 * time.Minute

// Keep is how long the measured activity is kept.
const Keep = 92 * 24 * time.Hour

// maxUnwritten bounds the closed spans, and the thread days, a failing
// recorder keeps for its next flush; past it the oldest spans and the
// newest days go, and Status still says since when.
const maxUnwritten = 1000

// Store is what the recorder writes and reads.
type Store interface {
	SaveActivity(spans []store.ActivitySpan, days []store.ThreadDay) ([]int64, error)
	PruneActivity(beforeMs int64, beforeDay string) (spans, days int64, err error)
	ActivitySpans(fromMs, toMs int64) ([]store.ActivitySpan, error)
	ThreadDays(from, to string) ([]store.ThreadDay, error)
}

// Status is whether the recorder is writing what it measures: Since is
// the earliest activity not written (Unix ms), 0 while the last flush
// wrote, and Error why it was not.
type Status struct {
	Since int64  `json:"since"`
	Error string `json:"error,omitempty"`
}

// Config is a recorder's clock and flush interval; zero values are the
// product's (time.Now, FlushEvery). Changed tells the app each time
// Status changes.
type Config struct {
	Now        func() time.Time
	FlushEvery time.Duration
	Changed    func(Status)
}

// span is a span of one connection's input: last is its last input,
// saved the end the store holds (0 before it is inserted), and untie
// removes its tie to its connection (nil for none).
type span struct {
	store.ActivitySpan
	last  int64
	saved int64
	untie func()
}

// Recorder is the one recorder of an app.
type Recorder struct {
	st         Store
	now        func() time.Time
	window     int64
	flushEvery time.Duration
	changed    func(Status)

	// flushMu serializes flushes: a flush reads the spans, writes them
	// without mu, and settles them.
	flushMu sync.Mutex
	mu      sync.Mutex
	open    map[string]*span
	closed  []*span
	// ran is the thread days not yet written, each with its earliest turn
	// start (Unix ms).
	ran     map[store.ThreadDay]int64
	status  Status
	pruned  string
	on      bool
	started bool
	stopped bool

	stop chan struct{}
	done chan struct{}
}

// New is a recorder of st that flushes once Start runs, and records once
// SetOn turns it on.
func New(st Store, cfg Config) *Recorder {
	r := &Recorder{st: st, now: cfg.Now, window: int64(Window / time.Millisecond), flushEvery: cfg.FlushEvery, changed: cfg.Changed,
		open: map[string]*span{}, ran: map[store.ThreadDay]int64{}, stop: make(chan struct{}), done: make(chan struct{})}
	if r.now == nil {
		r.now = time.Now
	}
	if r.flushEvery <= 0 {
		r.flushEvery = FlushEvery
	}
	return r
}

// Start runs the flush every FlushEvery until Close.
func (r *Recorder) Start() {
	r.mu.Lock()
	if r.started || r.stopped {
		r.mu.Unlock()
		return
	}
	r.started = true
	r.mu.Unlock()
	go func() {
		defer close(r.done)
		t := time.NewTicker(r.flushEvery)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-t.C:
				r.Flush()
			}
		}
	}()
}

// Close stops the flushes, ends every open span now and writes what is
// left; its error is the activity that last write could not record. A
// recorder that was never started is closed as well.
func (r *Recorder) Close() error {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return nil
	}
	r.stopped = true
	started := r.started
	now := r.now().UnixMilli()
	for conn := range r.open {
		r.endLocked(conn, now)
	}
	r.mu.Unlock()
	close(r.stop)
	if started {
		<-r.done
	}
	r.Flush()
	if st := r.Status(); st.Since != 0 {
		return fmt.Errorf("activity since %s is not recorded: %s", time.UnixMilli(st.Since).Format(time.DateTime), st.Error)
	}
	return nil
}

// SetOn is whether the recorder records: while a Jira connection is on.
// Turning it off ends every open span now and writes what came before;
// while it is off, Report and Ran record nothing.
func (r *Recorder) SetOn(on bool) {
	r.mu.Lock()
	if r.on == on || r.stopped {
		r.mu.Unlock()
		return
	}
	r.on = on
	if !on {
		now := r.now().UnixMilli()
		for conn := range r.open {
			r.endLocked(conn, now)
		}
	}
	r.mu.Unlock()
	if !on {
		r.Flush()
	}
}

// Report is input on connection conn's screen sinceInput ago (clamped to 0
// to Window), on thread (empty when the screen shows none), in the
// compact layout when phone. A span it opens is first tied to the
// connection's end: tie, when set, binds end (End of conn) to run then,
// inside the recorder's lock, so an end that runs at once finds the span,
// and answers what unties it; a tie that fails opens nothing and is
// Report's error.
func (r *Recorder) Report(conn, thread string, phone bool, sinceInput time.Duration, tie func(end func()) (untie func(), err error)) error {
	at := r.now().Add(-min(max(sinceInput, 0), Window)).UnixMilli()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped || !r.on {
		return nil
	}
	s := r.open[conn]
	if s != nil {
		at = max(at, s.last)
		if s.ThreadID == thread && s.Phone == phone && at-s.last <= r.window && at-s.StartMs < store.MaxActivitySpanMs-r.window {
			s.last = at
			return nil
		}
	}
	r.endLocked(conn, at)
	next := &span{ActivitySpan: store.ActivitySpan{ThreadID: thread, Phone: phone, StartMs: at, EndMs: at}, last: at}
	if tie != nil {
		untie, err := tie(func() { r.End(conn) })
		if err != nil {
			return err
		}
		next.untie = untie
	}
	r.open[conn] = next
	return nil
}

// Ran is a turn start of thread at atMs: the thread is among those that
// started a turn on that local day. The next flush writes it.
func (r *Recorder) Ran(thread string, atMs int64) {
	day := store.ThreadDay{Day: time.UnixMilli(atMs).Format(time.DateOnly), ThreadID: thread}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch at, ok := r.ran[day]; {
	case r.stopped, !r.on:
	case ok:
		r.ran[day] = min(at, atMs)
	case len(r.ran) < maxUnwritten:
		r.ran[day] = atMs
	}
}

// End is connection conn's end: its open span ends now.
func (r *Recorder) End(conn string) {
	now := r.now().UnixMilli()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endLocked(conn, now)
}

// endLocked closes conn's open span at now, or at its last input plus
// the window when that is sooner, and unties it from its connection.
func (r *Recorder) endLocked(conn string, now int64) {
	s := r.open[conn]
	if s == nil {
		return
	}
	delete(r.open, conn)
	if s.untie != nil {
		s.untie()
	}
	s.EndMs = max(s.StartMs, min(s.last+r.window, now))
	r.closed = append(r.closed, s)
	if over := len(r.closed) - maxUnwritten; over > 0 {
		r.closed = slices.Delete(r.closed, 0, over)
	}
}

// OpenSpans is how many connections have a span open.
func (r *Recorder) OpenSpans() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.open)
}

// Status is whether the recorder is writing what it measures.
func (r *Recorder) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// Flush writes the spans that changed since the last flush and, once a
// day, prunes what is older than Keep.
func (r *Recorder) Flush() {
	r.flushMu.Lock()
	defer r.flushMu.Unlock()
	now := r.now().UnixMilli()
	r.mu.Lock()
	for conn, s := range r.open {
		if now-s.last > r.window {
			r.endLocked(conn, now)
		}
	}
	var write []*span
	for _, s := range r.open {
		s.EndMs = max(s.StartMs, min(s.last+r.window, now))
	}
	for _, s := range append(slices.Collect(maps.Values(r.open)), r.closed...) {
		if s.ID == 0 || s.EndMs != s.saved {
			write = append(write, s)
		}
	}
	spans := make([]store.ActivitySpan, len(write))
	for i, s := range write {
		spans[i] = s.ActivitySpan
	}
	// The closed spans and thread days this flush settles; what comes
	// while it writes waits for the next.
	taken := make(map[*span]bool, len(r.closed))
	for _, s := range r.closed {
		taken[s] = true
	}
	days := slices.Collect(maps.Keys(r.ran))
	r.mu.Unlock()

	ids, err := r.st.SaveActivity(spans, days)

	r.mu.Lock()
	before := r.status
	if err != nil {
		since := r.status.Since
		froms := make([]int64, 0, len(write)+len(days))
		for _, s := range write {
			from := s.StartMs
			if s.ID != 0 {
				from = s.saved
			}
			froms = append(froms, from)
		}
		for _, d := range days {
			froms = append(froms, r.ran[d])
		}
		for _, from := range froms {
			if since == 0 || from < since {
				since = from
			}
		}
		r.status = Status{Since: since, Error: err.Error()}
	} else {
		for i, s := range write {
			s.ID, s.saved = ids[i], spans[i].EndMs
		}
		// A closed span is done once written, and so is a thread day.
		r.closed = slices.DeleteFunc(r.closed, func(s *span) bool { return taken[s] })
		for _, d := range days {
			delete(r.ran, d)
		}
		r.status = Status{}
	}
	after := r.status
	day := time.UnixMilli(now).Format(time.DateOnly)
	prune := r.pruned != day
	r.mu.Unlock()

	if after.Since != before.Since && (before.Since == 0 || after.Since == 0) {
		if after.Since != 0 {
			log.Printf("activity: activity not recorded since %s: %s", time.UnixMilli(after.Since).Format(time.RFC3339), after.Error)
		} else {
			log.Printf("activity: recording again")
		}
		if r.changed != nil {
			r.changed(after)
		}
	}
	if prune {
		cut := time.UnixMilli(now).Add(-Keep)
		if _, _, err := r.st.PruneActivity(cut.UnixMilli(), cut.Format(time.DateOnly)); err != nil {
			log.Printf("activity: prune the activity before %s: %v", cut.Format(time.DateOnly), err)
			return
		}
		r.mu.Lock()
		r.pruned = day
		r.mu.Unlock()
	}
}

// Day is what the recorder measured of one local day: the time with
// input, the part of it in the compact layout, the part on each focused
// thread ("" for none), and the threads that started a turn that day.
type Day struct {
	Day     string
	Active  time.Duration
	Phone   time.Duration
	Threads map[string]time.Duration
	Ran     []string
}

// Days is what the recorder measured of each local day from from to to,
// both YYYY-MM-DD and included, for the days that hold any. Where spans
// of two connections overlap, each instant counts once, on the span that
// started later: the screen the person turned to. The store and memory are
// read under the flush's lock, so no flush writes and retires a span
// between the two reads.
func (r *Recorder) Days(from, to string) ([]Day, error) {
	first, end, err := DayRange(from, to)
	if err != nil {
		return nil, err
	}
	r.flushMu.Lock()
	stored, err := r.st.ActivitySpans(first.UnixMilli(), end.UnixMilli())
	if err != nil {
		r.flushMu.Unlock()
		return nil, err
	}
	ran, err := r.st.ThreadDays(from, to)
	if err != nil {
		r.flushMu.Unlock()
		return nil, err
	}
	spans, ran := r.withUnwritten(stored, ran, from, to)
	r.flushMu.Unlock()
	byDay := map[string]*Day{}
	day := func(name string) *Day {
		d := byDay[name]
		if d == nil {
			d = &Day{Day: name, Threads: map[string]time.Duration{}}
			byDay[name] = d
		}
		return d
	}
	var stuck error
	count(spans, first.UnixMilli(), end.UnixMilli(), func(s store.ActivitySpan, from, to int64) {
		// The local days the stretch reaches, each its own part.
		for from < to && stuck == nil {
			t := time.UnixMilli(from)
			next := min(to, nextDayStart(t).UnixMilli())
			if next <= from {
				stuck = fmt.Errorf("activity: the day after %s starts at %s, not after it", t.Format(time.RFC3339), time.UnixMilli(next).Format(time.RFC3339))
				return
			}
			d := day(t.Format(time.DateOnly))
			part := time.Duration(next-from) * time.Millisecond
			d.Active += part
			if s.Phone {
				d.Phone += part
			}
			d.Threads[s.ThreadID] += part
			from = next
		}
	})
	if stuck != nil {
		return nil, stuck
	}
	for _, td := range ran {
		d := day(td.Day)
		d.Ran = append(d.Ran, td.ThreadID)
	}
	out := make([]Day, 0, len(byDay))
	for _, d := range byDay {
		out = append(out, *d)
	}
	slices.SortFunc(out, func(a, b Day) int { return strings.Compare(a.Day, b.Day) })
	return out, nil
}

// DayRange is the instants of the local days from from to to, both
// YYYY-MM-DD and included: from the first instant of from to the first of
// the day after to (dayStart).
func DayRange(from, to string) (start, end time.Time, err error) {
	first, err := time.Parse(time.DateOnly, from)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("activity: days from %q: %w", from, err)
	}
	last, err := time.Parse(time.DateOnly, to)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("activity: days to %q: %w", to, err)
	}
	return dayStart(first.Date()), dayStart(last.AddDate(0, 0, 1).Date()), nil
}

// nextDayStart is the first instant of the local day after t's.
func nextDayStart(t time.Time) time.Time {
	y, m, d := t.Date()
	return dayStart(y, m, d+1)
}

// dayStart is the first instant of local date y-m-d (normalized, so d+1
// is the next day): its midnight, or, where the zone skips midnight, the
// first hour that the date has. time.Date may answer a skipped midnight
// with the evening before (America/Santiago, America/Havana).
func dayStart(y int, m time.Month, d int) time.Time {
	y, m, d = time.Date(y, m, d, 12, 0, 0, 0, time.UTC).Date()
	for h := range 24 {
		t := time.Date(y, m, d, h, 0, 0, 0, time.Local)
		if ty, tm, td := t.Date(); ty == y && tm == m && td == d {
			return t
		}
	}
	return time.Date(y, m, d, 12, 0, 0, 0, time.Local)
}

// withUnwritten is stored and ran with what the recorder holds that it
// has not written: an open span as it ends now, a span written before by
// its end as the recorder holds it, and the thread days from from to to
// not yet written, each once.
func (r *Recorder) withUnwritten(stored []store.ActivitySpan, ran []store.ThreadDay, from, to string) ([]store.ActivitySpan, []store.ThreadDay) {
	now := r.now().UnixMilli()
	r.mu.Lock()
	held := make([]store.ActivitySpan, 0, len(r.open)+len(r.closed))
	for _, s := range r.open {
		sp := s.ActivitySpan
		sp.EndMs = max(sp.StartMs, min(s.last+r.window, now))
		held = append(held, sp)
	}
	for _, s := range r.closed {
		held = append(held, s.ActivitySpan)
	}
	for d := range r.ran {
		if d.Day >= from && d.Day <= to && !slices.Contains(ran, d) {
			ran = append(ran, d)
		}
	}
	r.mu.Unlock()
	out := slices.DeleteFunc(slices.Clone(stored), func(st store.ActivitySpan) bool {
		return slices.ContainsFunc(held, func(h store.ActivitySpan) bool { return h.ID != 0 && h.ID == st.ID })
	})
	return append(out, held...), ran
}

// count calls each for every stretch of from to to that spans cover, with
// the span it counts on: of the spans that cover it, the one that started
// last.
func count(spans []store.ActivitySpan, from, to int64, each func(s store.ActivitySpan, from, to int64)) {
	// A span that does not reach from to to has no stretch in it.
	spans = slices.DeleteFunc(slices.Clone(spans), func(s store.ActivitySpan) bool { return max(from, s.StartMs) >= min(to, s.EndMs) })
	slices.SortStableFunc(spans, func(a, b store.ActivitySpan) int { return cmp.Compare(a.StartMs, b.StartMs) })
	edges := make([]int64, 0, 2*len(spans))
	for _, s := range spans {
		edges = append(edges, max(from, s.StartMs), min(to, s.EndMs))
	}
	slices.Sort(edges)
	edges = slices.Compact(edges)
	// covering holds the spans that cover the stretch from the edge on, in
	// the order they started.
	var covering []store.ActivitySpan
	next := 0
	for i := 0; i+1 < len(edges); i++ {
		a, b := edges[i], edges[i+1]
		for next < len(spans) && spans[next].StartMs <= a {
			covering = append(covering, spans[next])
			next++
		}
		covering = slices.DeleteFunc(covering, func(s store.ActivitySpan) bool { return s.EndMs <= a })
		if len(covering) > 0 && a < b {
			each(covering[len(covering)-1], a, b)
		}
	}
}
