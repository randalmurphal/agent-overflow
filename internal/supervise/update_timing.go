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
		// One line per phase, also for errors.Join's newline-separated text.
		fmt.Fprintf(&line, " failed: %s", strings.ReplaceAll(err.Error(), "\n", "; "))
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
// branch (handoff), the steps before the swap, the framework's Restart up
// to the host's shutdown callback (swap), this process's shutdown from
// that callback (shutdown), then one summary. The framework's helper
// replaces the executable and relaunches it after this process exits, and
// logs no durations.
//
// The framework's Restart quits the host before it returns, so the host's
// shutdown, and the end of the app loop that calls End, can run before the
// code that called Restart resumes. Swapping, Quitting and End therefore
// share one lock: each transition happens once, a late one does nothing,
// and End closes whichever phase is open before the summary, which is
// always the last line.
//
// A nil *SwapTiming is valid and does nothing. Safe for concurrent use.
type SwapTiming struct {
	phases  *PhaseLog
	now     func() time.Time
	started time.Time

	mu    sync.Mutex
	stage swapStage
	// open is the phase in progress: swap while swapping, shutdown while
	// quitting, otherwise none.
	open Phase
}

type swapStage int

const (
	swapPreparing swapStage = iota
	swapSwapping
	swapQuitting
	swapEnded
)

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

// Swapping begins swap, just before the framework's Restart. It does
// nothing once the shutdown began or the swap ended.
func (s *SwapTiming) Swapping() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stage == swapPreparing {
		s.stage = swapSwapping
		s.open = s.phases.Begin("swap")
	}
}

// Quitting ends swap and begins shutdown. The host calls it from its
// shutdown callback, where the shutdown starts; the caller of Restart calls
// it again once Restart returns, for a host whose callback did not run.
// Only the first call does anything, and none after End.
func (s *SwapTiming) Quitting() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stage >= swapQuitting {
		return
	}
	if s.stage == swapSwapping {
		s.open.End(nil)
	}
	s.stage = swapQuitting
	s.open = s.phases.Begin("shutdown")
}

// End ends the swap once: the open phase, swap or shutdown, with err, then
// the summary with outcome swapped, or error when err is not nil. A swap
// that failed before the quit passes its error, and this process keeps
// running.
func (s *SwapTiming) End(err error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stage == swapEnded {
		return
	}
	if s.stage != swapPreparing {
		s.open.End(err)
	}
	s.stage = swapEnded
	s.open = Phase{}
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
