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
// with err when it is not nil.
func (p Phase) End(err error, fields ...any) {
	if p.log == nil {
		return
	}
	took := p.log.record(p, err)
	var line strings.Builder
	fmt.Fprintf(&line, "phase=%s duration=%s", p.name, took)
	for i := 0; i+1 < len(fields); i += 2 {
		fmt.Fprintf(&line, " %v=%v", fields[i], fields[i+1])
	}
	if err != nil {
		fmt.Fprintf(&line, " failed: %v", err)
	}
	p.log.logf("%s: %s", p.log.prefix, line.String())
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

func (l *PhaseLog) record(p Phase, err error) time.Duration {
	took := roundPhase(l.now().Sub(p.started))
	entry := p.name + ":" + took.String()
	if err != nil {
		entry += "(failed)"
	}
	l.mu.Lock()
	l.phases = append(l.phases, entry)
	l.mu.Unlock()
	return took
}

func roundPhase(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d.Round(time.Millisecond)
}
