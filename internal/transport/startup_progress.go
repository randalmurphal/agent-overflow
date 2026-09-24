package transport

import (
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"agent-overflow/internal/startupprogress"
)

// startupHeartbeatInterval is how often an open boot phase advances
// AliveAt and looks for watched files changing size. Clients judge a
// stall against both (the Windows launcher fails after 30 s without an
// advance), so it must stay well under that.
const startupHeartbeatInterval = time.Second

// SetStartupProgress replaces the progress a readiness-gated
// /bootstrap.json reports until MarkReady. A server that never receives
// progress keeps answering the bare 503.
func (s *Server) SetStartupProgress(p startupprogress.Progress) {
	s.startupProgress.Store(&p)
}

// writeStartupProgress answers a not-ready bootstrap request. It reports
// false when no progress has ever been set, leaving the bare 503 to the
// caller.
func (s *Server) writeStartupProgress(w http.ResponseWriter) bool {
	p := s.startupProgress.Load()
	if p == nil {
		return false
	}
	startupprogress.Write(w, *p)
	return true
}

// BackendStartingError reports that a backend answered its bootstrap with
// a starting report. Carriers return it so the hop can answer with the
// same report.
type BackendStartingError struct {
	Progress startupprogress.Progress
}

func (e *BackendStartingError) Error() string {
	return "backend is starting: " + e.Progress.Phase
}

// StartupReporter turns boot phases into startup progress on one server.
// Phases nest: ending an inner phase restores its parent's report.
//
// UpdatedAt advances only on observed progress: a phase beginning or
// ending, a new detail or step, or a watched file (WatchBootFiles)
// changing size since the previous heartbeat. A long CREATE INDEX or table
// rebuild grows the WAL and counts; a statement that hangs does not.
// AliveAt advances on every heartbeat. The heartbeat runs every interval
// while any phase is open and stops when the outermost phase ends, so a
// backend wedged between phases stops advancing both.
//
// All methods are safe for concurrent use and on a nil receiver, which
// reports nothing.
type StartupReporter struct {
	srv      *Server
	now      func() time.Time
	interval time.Duration

	mu      sync.Mutex
	current startupprogress.Progress
	open    []startupPhase
	stop    chan struct{}
	stopped chan struct{}
	// watched maps each WatchBootFiles path to its size at the last look:
	// -1 while it does not exist, statUnknown until a look could read it.
	watched map[string]int64
	// statFailed holds paths whose stat failure was already logged.
	statFailed map[string]bool
}

type startupPhase struct {
	phase, detail string
	step, steps   int
}

// NewStartupReporter publishes an initial "starting" report on srv and
// returns the reporter for the boot's phases. updatingTo names the
// version an in-app update is being finished to, or is empty.
func NewStartupReporter(srv *Server, updatingTo string) *StartupReporter {
	return newStartupReporter(srv, updatingTo, startupHeartbeatInterval, time.Now)
}

func newStartupReporter(srv *Server, updatingTo string, interval time.Duration, now func() time.Time) *StartupReporter {
	r := &StartupReporter{srv: srv, now: now, interval: interval, watched: map[string]int64{}, statFailed: map[string]bool{}}
	started := now().UnixMilli()
	r.current = startupprogress.Progress{
		Phase:      "starting",
		Detail:     "Starting",
		StartedAt:  started,
		UpdatedAt:  started,
		AliveAt:    started,
		UpdatingTo: updatingTo,
	}
	srv.SetStartupProgress(r.current)
	return r
}

// WatchBootFiles adds files whose size changes count as progress, such as
// the database and its WAL. A file that does not exist yet counts when it
// appears.
func (r *StartupReporter) WatchBootFiles(paths ...string) {
	if r == nil {
		return
	}
	sizes := make([]int64, len(paths))
	for i, path := range paths {
		sizes[i] = r.fileSize(path)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, path := range paths {
		r.watched[path] = sizes[i]
	}
}

// BeginBootPhase reports that phase began. The returned func ends it and
// is safe to call more than once.
func (r *StartupReporter) BeginBootPhase(phase, detail string) (end func()) {
	if r == nil {
		return func() {}
	}
	r.mu.Lock()
	r.open = append(r.open, startupPhase{phase: phase, detail: detail})
	depth := len(r.open)
	r.publishLocked()
	if depth == 1 {
		r.stop = make(chan struct{})
		r.stopped = make(chan struct{})
		go r.heartbeat(r.stop, r.stopped)
	}
	r.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { r.endPhase(depth) }) }
}

// BootPhaseDetail reports progress inside the innermost open phase. It
// is ignored when no phase is open.
func (r *StartupReporter) BootPhaseDetail(detail string, step, steps int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.open) == 0 {
		return
	}
	inner := &r.open[len(r.open)-1]
	inner.detail, inner.step, inner.steps = detail, step, steps
	r.publishLocked()
}

// endPhase closes the phase opened at depth and every phase opened inside
// it. Closing the outermost phase stops the heartbeat and waits for it.
func (r *StartupReporter) endPhase(depth int) {
	r.mu.Lock()
	if len(r.open) < depth {
		r.mu.Unlock()
		return
	}
	r.open = r.open[:depth-1]
	if len(r.open) > 0 {
		r.publishLocked()
		r.mu.Unlock()
		return
	}
	stop, stopped := r.stop, r.stopped
	r.stop, r.stopped = nil, nil
	r.mu.Unlock()
	close(stop)
	<-stopped
}

func (r *StartupReporter) heartbeat(stop <-chan struct{}, stopped chan<- struct{}) {
	defer close(stopped)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			r.beat()
		}
	}
}

// beat stamps the heartbeat, and progress when a watched file changed
// size since the last look. Files are read outside the lock so a slow
// filesystem cannot hold up a report.
func (r *StartupReporter) beat() {
	r.mu.Lock()
	paths := make([]string, 0, len(r.watched))
	for path := range r.watched {
		paths = append(paths, path)
	}
	r.mu.Unlock()
	sizes := make([]int64, len(paths))
	for i, path := range paths {
		sizes[i] = r.fileSize(path)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now().UnixMilli()
	for i, path := range paths {
		if sizes[i] == statUnknown {
			continue
		}
		if prev := r.watched[path]; prev != sizes[i] {
			r.watched[path] = sizes[i]
			if prev != statUnknown {
				r.current.UpdatedAt = now
			}
		}
	}
	r.current.AliveAt = now
	r.srv.SetStartupProgress(r.current)
}

// statUnknown is a size fileSize could not read. It is no evidence either
// way, so the file keeps its last known size.
const statUnknown = -2

// fileSize is path's size, -1 when it does not exist, or statUnknown when
// it cannot be read. A read failure is logged once per path.
func (r *StartupReporter) fileSize(path string) int64 {
	info, err := os.Stat(path)
	switch {
	case err == nil:
		return info.Size()
	case errors.Is(err, fs.ErrNotExist):
		return -1
	}
	r.mu.Lock()
	first := !r.statFailed[path]
	r.statFailed[path] = true
	r.mu.Unlock()
	if first {
		log.Printf("startup progress: cannot read %s, its writes will not count as progress: %v", path, err)
	}
	return statUnknown
}

func (r *StartupReporter) publishLocked() {
	inner := r.open[len(r.open)-1]
	r.current.Phase = inner.phase
	r.current.Detail = inner.detail
	r.current.Step = inner.step
	r.current.Steps = inner.steps
	now := r.now().UnixMilli()
	r.current.UpdatedAt = now
	r.current.AliveAt = now
	r.srv.SetStartupProgress(r.current)
}
