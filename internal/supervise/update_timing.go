package supervise

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// PhaseLog writes how long each phase of an in-app update took, one line
// per phase, in the log the code running the phase already writes
// (docs/specs/app-update.md, "What an update logs"):
//
//	<prefix>: phase=<name> duration=<d>[ <key>=<value>...][ failed: <reason>]
//	<prefix>: phase=<name> skipped: <why>
//
// duration is a Go duration rounded to the millisecond. A process that waits
// for a phase another process runs records its wait with Waited, which
// writes no line; Summary lists every phase this log saw.
//
// A nil *PhaseLog is valid and does nothing. Safe for concurrent use.
type PhaseLog struct {
	logf   func(string, ...any)
	prefix string
	now    func() time.Time

	mu     sync.Mutex
	phases []string
}

// NewPhaseLog returns a log whose lines start with prefix. nil logf is
// silent; nil now is time.Now.
func NewPhaseLog(logf func(string, ...any), prefix string, now func() time.Time) *PhaseLog {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if now == nil {
		now = time.Now
	}
	return &PhaseLog{logf: logf, prefix: prefix, now: now}
}

// Phase is one phase that began.
type Phase struct {
	log     *PhaseLog
	name    string
	started time.Time
}

// Begin starts timing phase name.
func (l *PhaseLog) Begin(name string) Phase {
	if l == nil {
		return Phase{}
	}
	return l.BeginAt(name, l.now())
}

// BeginAt times phase name from started, for a phase that began before
// the code that ends it could, such as a process's own start.
func (l *PhaseLog) BeginAt(name string, started time.Time) Phase {
	if l == nil {
		return Phase{}
	}
	return Phase{log: l, name: name, started: started}
}

// End logs the phase's line with fields, key and value pairs, and failed
// with err when it is not nil. It returns the time the phase ended, from
// the log's clock, so a phase that follows can begin exactly there; zero
// for a phase of a nil log.
func (p Phase) End(err error, fields ...any) (ended time.Time) {
	if p.log == nil {
		return time.Time{}
	}
	took, ended := p.log.record(p, err)
	var line strings.Builder
	fmt.Fprintf(&line, "phase=%s duration=%s", p.name, took)
	for i := 0; i+1 < len(fields); i += 2 {
		fmt.Fprintf(&line, " %v=%v", fields[i], fields[i+1])
	}
	if err != nil {
		fmt.Fprintf(&line, " failed: %v", err)
	}
	p.log.logf("%s: %s", p.log.prefix, line.String())
	return ended
}

// Waited records the caller's wait for a phase whose own line another
// process or component writes, for Summary only.
func (p Phase) Waited(err error) {
	if p.log != nil {
		p.log.record(p, err)
	}
}

// Skip logs that phase name did not run, and why.
func (l *PhaseLog) Skip(name, why string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.phases = append(l.phases, name+":skipped")
	l.mu.Unlock()
	l.logf("%s: phase=%s skipped: %s", l.prefix, name, why)
}

// Summary logs one line listing every phase in the order it ended, with
// the total and fields:
//
//	<prefix>: summary total=<d> phases=<name>:<d>[(failed)],<name>:skipped...[ <key>=<value>...]
func (l *PhaseLog) Summary(total time.Duration, fields ...any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	phases := strings.Join(l.phases, ",")
	l.mu.Unlock()
	var line strings.Builder
	fmt.Fprintf(&line, "summary total=%s phases=%s", roundPhase(total), phases)
	for i := 0; i+1 < len(fields); i += 2 {
		fmt.Fprintf(&line, " %v=%v", fields[i], fields[i+1])
	}
	l.logf("%s: %s", l.prefix, line.String())
}

func (l *PhaseLog) record(p Phase, err error) (took time.Duration, ended time.Time) {
	ended = l.now()
	took = roundPhase(ended.Sub(p.started))
	entry := p.name + ":" + took.String()
	if err != nil {
		entry += "(failed)"
	}
	l.mu.Lock()
	l.phases = append(l.phases, entry)
	l.mu.Unlock()
	return took, ended
}

// SwapTiming times an update that skips the trial and takes the
// framework's swap, in the old version's process: the choice of that
// branch (handoff), the steps before the swap, the swap's start (swap) and
// this process's shutdown from the quit it requests (shutdown), then one
// summary. The framework's helper replaces the executable and relaunches
// it after this process exits, and logs no durations.
//
// A nil *SwapTiming is valid and does nothing. Safe for concurrent use.
type SwapTiming struct {
	phases  *PhaseLog
	now     func() time.Time
	started time.Time

	mu       sync.Mutex
	shutdown Phase
	ended    bool
}

// NewSwapTiming starts timing the swap to version to. nil logf is silent;
// nil now is time.Now.
func NewSwapTiming(logf func(string, ...any), to string, now func() time.Time) *SwapTiming {
	prefix := "updater: swap"
	if to != "" {
		prefix += " to " + to
	}
	phases := NewPhaseLog(logf, prefix, now)
	return &SwapTiming{phases: phases, now: phases.now, started: phases.now()}
}

// Phases is the log of the swap's phases.
func (s *SwapTiming) Phases() *PhaseLog {
	if s == nil {
		return nil
	}
	return s.phases
}

// Declined ends handoff, the trial's handoff begun on another log, as the
// branch taken: the target cannot run the trial, so its time is logged
// here with branch=swap and never as a failure.
func (s *SwapTiming) Declined(handoff Phase) {
	if s == nil {
		return
	}
	s.phases.BeginAt("handoff", handoff.started).End(nil, "branch", "swap")
}

// Quitting begins shutdown: the swap's helper is running and this process
// has asked to quit.
func (s *SwapTiming) Quitting() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ended {
		s.shutdown = s.phases.Begin("shutdown")
	}
}

// End ends the swap once: the shutdown, when it began, with err, then the
// summary with outcome swapped, or error when err is not nil. A swap that
// failed before the quit passes its error, and this process keeps running.
func (s *SwapTiming) End(err error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	s.ended = true
	if s.shutdown.log != nil {
		s.shutdown.End(err)
	}
	outcome := "swapped"
	if err != nil {
		outcome = "error"
	}
	s.phases.Summary(s.now().Sub(s.started), "outcome", outcome)
}

func roundPhase(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d.Round(time.Millisecond)
}
