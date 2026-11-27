// Package activity records when the person gave input to this computer's
// app and on which thread, for the time draft, and answers it per day.
//
// What it keeps true:
//
//   - A minute counts when there was input in the Window before it. A
//     screen reports its input at most once a minute and at once when its
//     focused thread changes (Report); the recorder keeps one open span per
//     connection in memory, and a report on the same thread within Window
//     of the last one extends it. A span ends at its last report plus
//     Window, or sooner: at the next report on another thread, at the
//     connection's end (End), or at the flush that finds it idle.
//   - Nothing is written per report. One flush every FlushEvery writes
//     what changed (a span inserted once, its end moved after), and Close
//     flushes once more. Only while a Jira connection is on is a span
//     inserted (store.SaveActivitySpans); a span no write took is dropped.
//   - A flush that fails keeps what it could not write for the next one,
//     at most maxUnwritten closed spans, and the earliest time not written
//     is the recorder's Status until a flush writes again.
//   - The recorder owns the prune of what it measured: once a day, the
//     spans and thread days older than Keep, and nothing else.
//   - Days answers the person's minutes per day, never the spans.
package activity

import (
	"log"
	"slices"
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

// maxUnwritten bounds the closed spans a failing recorder keeps for its
// next flush; past it the oldest go, and Status still says since when.
const maxUnwritten = 1000

// Store is what the recorder writes and reads.
type Store interface {
	SaveActivitySpans(spans []store.ActivitySpan) ([]int64, error)
	PruneActivity(beforeMs int64, beforeDay string) (spans, days int64, err error)
	ActivitySpans(fromMs, toMs int64) ([]store.ActivityRow, error)
	ThreadDays(from, to string) ([]store.ThreadDay, error)
}

// Status is whether the recorder is writing what it measures: Since is
// the earliest activity not written (Unix ms), 0 while the last flush
// wrote, and Error why it was not.
type Status struct {
	Since int64  `json:"since"`
	Error string `json:"error,omitempty"`
}

// Config is a recorder's clock, window and flush interval; zero values
// are the product's (Window, FlushEvery, time.Now). Changed tells the
// app each time Status changes.
type Config struct {
	Now        func() time.Time
	Window     time.Duration
	FlushEvery time.Duration
	Changed    func(Status)
}

// span is a span of one connection's input: last is its last report, and
// saved the end the store holds (0 before it is inserted).
type span struct {
	store.ActivitySpan
	last  int64
	saved int64
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
	status  Status
	pruned  string
	started bool
	stopped bool

	stop chan struct{}
	done chan struct{}
}

// New is a recorder of st that flushes once Start runs.
func New(st Store, cfg Config) *Recorder {
	r := &Recorder{st: st, now: cfg.Now, window: int64(cfg.Window / time.Millisecond), flushEvery: cfg.FlushEvery, changed: cfg.Changed,
		open: map[string]*span{}, stop: make(chan struct{}), done: make(chan struct{})}
	if r.now == nil {
		r.now = time.Now
	}
	if r.window <= 0 {
		r.window = int64(Window / time.Millisecond)
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
// left. A recorder that was never started is closed as well.
func (r *Recorder) Close() {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
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
}

// Report is input on connection conn's screen at now, on thread (empty
// when no thread was focused), in the compact layout when phone.
func (r *Recorder) Report(conn, thread string, phone bool) {
	now := r.now().UnixMilli()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	if s := r.open[conn]; s != nil {
		if s.ThreadID == thread && s.Phone == phone && now-s.last <= r.window && now-s.StartMs < store.MaxActivitySpanMs-r.window {
			s.last = max(s.last, now)
			return
		}
		r.endLocked(conn, now)
	}
	r.open[conn] = &span{ActivitySpan: store.ActivitySpan{ThreadID: thread, Phone: phone, StartMs: now, EndMs: now}, last: now}
}

// End is connection conn's end: its open span ends now.
func (r *Recorder) End(conn string) {
	now := r.now().UnixMilli()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endLocked(conn, now)
}

// endLocked closes conn's open span at now, or at its last report plus
// the window when that is sooner.
func (r *Recorder) endLocked(conn string, now int64) {
	s := r.open[conn]
	if s == nil {
		return
	}
	delete(r.open, conn)
	s.EndMs = max(s.StartMs, min(s.last+r.window, now))
	r.closed = append(r.closed, s)
	if over := len(r.closed) - maxUnwritten; over > 0 {
		r.closed = slices.Delete(r.closed, 0, over)
	}
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
		if s.ID == 0 || s.EndMs != s.saved {
			write = append(write, s)
		}
	}
	write = append(write, r.closed...)
	spans := make([]store.ActivitySpan, len(write))
	for i, s := range write {
		spans[i] = s.ActivitySpan
	}
	r.mu.Unlock()

	ids, err := r.st.SaveActivitySpans(spans)

	r.mu.Lock()
	before := r.status
	if err != nil {
		since := r.status.Since
		for _, s := range write {
			from := s.StartMs
			if s.ID != 0 {
				from = s.saved
			}
			if since == 0 || from < since {
				since = from
			}
		}
		r.status = Status{Since: since, Error: err.Error()}
	} else {
		for i, s := range write {
			s.ID, s.saved = ids[i], spans[i].EndMs
		}
		// What a write did not take (no Jira connection on) is not kept:
		// a closed span is done either way.
		r.closed = slices.DeleteFunc(r.closed, func(s *span) bool { return slices.Contains(write, s) })
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
