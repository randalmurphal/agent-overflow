package transport

import (
	"net/http"
	"sync"
	"time"

	"agent-overflow/internal/startupprogress"
)

// startupHeartbeatInterval is how often an open boot step advances
// UpdatedAt. Clients judge a stall against it (the Windows launcher fails
// after 30 s without an advance), so it must stay well under that.
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
// Phases nest: ending an inner phase restores its parent's report. While
// any phase is open a heartbeat advances UpdatedAt; it stops when the
// outermost phase ends, so a backend wedged between phases stops
// advancing. All methods are safe for concurrent use and on a nil
// receiver, which reports nothing.
type StartupReporter struct {
	srv      *Server
	now      func() time.Time
	interval time.Duration

	mu      sync.Mutex
	current startupprogress.Progress
	open    []startupPhase
	stop    chan struct{}
	stopped chan struct{}
	observe func(p startupprogress.Progress, liveness bool)
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
	r := &StartupReporter{srv: srv, now: now, interval: interval}
	started := now().UnixMilli()
	r.current = startupprogress.Progress{
		Phase:      "starting",
		Detail:     "Starting",
		StartedAt:  started,
		UpdatedAt:  started,
		UpdatingTo: updatingTo,
	}
	srv.SetStartupProgress(r.current)
	return r
}

// Observe installs fn to receive every report as it is published, starting
// with the current one, and returns r. liveness is true for a heartbeat,
// which only advances UpdatedAt; every other report is a real step. fn runs
// under the reporter's lock and must not block. A nil fn observes nothing.
func (r *StartupReporter) Observe(fn func(p startupprogress.Progress, liveness bool)) *StartupReporter {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observe = fn
	if fn != nil {
		fn(r.current, false)
	}
	return r
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
			r.mu.Lock()
			r.current.UpdatedAt = r.now().UnixMilli()
			r.srv.SetStartupProgress(r.current)
			if r.observe != nil {
				r.observe(r.current, true)
			}
			r.mu.Unlock()
		}
	}
}

func (r *StartupReporter) publishLocked() {
	inner := r.open[len(r.open)-1]
	r.current.Phase = inner.phase
	r.current.Detail = inner.detail
	r.current.Step = inner.step
	r.current.Steps = inner.steps
	r.current.UpdatedAt = r.now().UnixMilli()
	r.srv.SetStartupProgress(r.current)
	if r.observe != nil {
		r.observe(r.current, false)
	}
}
