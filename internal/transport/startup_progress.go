package transport

import (
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/startupprogress"
)

// startupHeartbeatInterval is how often an open boot phase advances
// AliveAt and looks for evidence of progress. Clients judge a stall
// against both (the Windows launcher fails after 30 s without an
// advance), so it must stay well under that.
const startupHeartbeatInterval = time.Second

// maxBootFailureErrorRunes bounds the error a boot failure carries, since
// every hello repeats it.
const maxBootFailureErrorRunes = 500

// BootFailure is a boot phase that failed without stopping the boot, such
// as a sweep of the previous instance's residue. Every hello names each
// one, so a client shows it while this process runs.
type BootFailure struct {
	// Phase is the boot phase id (the `boot: phase=` log name).
	Phase string `json:"phase"`
	// Detail is the phase's sentence, as its progress report read.
	Detail string `json:"detail"`
	Error  string `json:"error"`
}

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

// bootFailureList is what every hello reports as the boot's failures.
func (s *Server) bootFailureList() []BootFailure {
	if f := s.bootFailures.Load(); f != nil {
		return *f
	}
	return nil
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
// ending, a new detail or step, or, between two heartbeats, a watched
// file (WatchBootFiles) changing size or this process doing work: CPU
// time or storage I/O over startupprogress.Sampler's thresholds. A sort, a
// foreign key check, a cold read or a rebuild counts; a statement blocked
// on a lock does not. AliveAt advances on every heartbeat. The heartbeat
// runs every interval while any phase is open and stops when the
// outermost phase ends, so a backend wedged between phases stops
// advancing both.
//
// All methods are safe for concurrent use and on a nil receiver, which
// reports nothing.
type StartupReporter struct {
	srv      *Server
	now      func() time.Time
	interval time.Duration

	mu       sync.Mutex
	current  startupprogress.Progress
	open     []startupPhase
	failures []BootFailure
	stop     chan struct{}
	stopped  chan struct{}
	observe  func(p startupprogress.Progress)
	// sampler finds progress between two heartbeats. It is set before the
	// reporter is shared and never replaced.
	sampler *startupprogress.Sampler
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
	r := &StartupReporter{
		srv: srv, now: now, interval: interval,
		sampler: startupprogress.NewSampler(startupprogress.SamplerOptions{Interval: interval}),
	}
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

// Observe installs fn to receive every report as it is published, heartbeats
// included, starting with the current one, and returns r. A report's
// UpdatedAt and AliveAt say what it proves. fn runs under the reporter's
// lock and must not block. A nil fn observes nothing.
func (r *StartupReporter) Observe(fn func(p startupprogress.Progress)) *StartupReporter {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observe = fn
	if fn != nil {
		fn(r.current)
	}
	return r
}

// WatchBootFiles adds files whose size changes count as progress, such as
// the database and its WAL. A file that does not exist yet counts when it
// appears.
func (r *StartupReporter) WatchBootFiles(paths ...string) {
	if r == nil {
		return
	}
	r.sampler.Watch(paths...)
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

// BootPhaseFailed reports that the phase the report names, the innermost
// open one or with none open the last one reported, failed without
// stopping the boot. The failure reaches every hello from then on, after
// MarkReady too.
func (r *StartupReporter) BootPhaseFailed(err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = append(r.failures, BootFailure{
		Phase: r.current.Phase, Detail: r.current.Detail,
		Error: clampRunes(err.Error(), maxBootFailureErrorRunes),
	})
	published := append([]BootFailure(nil), r.failures...)
	r.srv.bootFailures.Store(&published)
}

// clampRunes cuts s to at most n runes, marking a cut with an ellipsis.
func clampRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
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

// beat stamps the heartbeat, and progress when the sampler found a watched
// file changed size or the process did enough work since the last look.
// The sampler reads outside the lock so a slow read cannot hold up a
// report.
func (r *StartupReporter) beat() {
	progressed := r.sampler.Sample()
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now().UnixMilli()
	if progressed {
		r.current.UpdatedAt = now
	}
	r.current.AliveAt = now
	r.srv.SetStartupProgress(r.current)
	if r.observe != nil {
		r.observe(r.current)
	}
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
	if r.observe != nil {
		r.observe(r.current)
	}
}
