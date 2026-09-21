package app

import (
	"log"
	"time"
)

// Keep the waits separate from history work so a slow fork can be diagnosed
// from the affected machine's logs without recording conversation content.
type forkTiming struct {
	source       string
	operation    string
	started      time.Time
	phase        string
	phaseStarted time.Time
	phases       []forkPhaseDuration
}

type forkPhaseDuration struct {
	Name     string
	Duration time.Duration
}

func startForkTiming(source, operation string) *forkTiming {
	now := time.Now()
	return &forkTiming{source: source, operation: operation, started: now, phaseStarted: now, phase: "action_lock"}
}

func (f *forkTiming) next(phase string) {
	now := time.Now()
	f.phases = append(f.phases, forkPhaseDuration{f.phase, now.Sub(f.phaseStarted).Round(time.Millisecond)})
	f.phase, f.phaseStarted = phase, now
}

func (f *forkTiming) finish() {
	f.next("")
	log.Printf("fork: source=%s operation=%s total=%s phases=%v", f.source, f.operation, time.Since(f.started).Round(time.Millisecond), f.phases)
}
