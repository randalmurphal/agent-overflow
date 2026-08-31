package providerlifecycleapp

import (
	"context"
	"sync"
	"time"
)

type usageProbeGate struct {
	probe func(context.Context) error
	ctxFn func() context.Context

	now       func() time.Time
	afterFunc func(time.Duration, func())

	mu        sync.Mutex
	lastStart time.Time
	inFlight  bool
	trailing  bool
	timerSet  bool
}

func newUsageProbeGate(
	probe func(context.Context) error,
	ctxFn func() context.Context,
) *usageProbeGate {
	return &usageProbeGate{
		probe: probe,
		ctxFn: ctxFn,
		now:   time.Now,
		afterFunc: func(delay time.Duration, callback func()) {
			time.AfterFunc(delay, callback)
		},
	}
}

func (g *usageProbeGate) Request() { g.request(false) }

func (g *usageProbeGate) request(fromTimer bool) {
	g.mu.Lock()
	if fromTimer {
		g.timerSet = false
	}
	if g.inFlight {
		g.trailing = true
		g.mu.Unlock()
		return
	}
	if wait := g.holdLocked(g.now()); wait > 0 {
		g.scheduleLocked(wait)
		g.mu.Unlock()
		return
	}
	g.inFlight = true
	g.lastStart = g.now()
	g.mu.Unlock()

	_ = g.probe(g.ctxFn())

	g.mu.Lock()
	g.inFlight = false
	if g.trailing {
		g.trailing = false
		g.scheduleLocked(g.holdLocked(g.now()))
	}
	g.mu.Unlock()
}

func (g *usageProbeGate) holdLocked(now time.Time) time.Duration {
	return probeMinInterval - now.Sub(g.lastStart)
}

func (g *usageProbeGate) scheduleLocked(wait time.Duration) {
	if g.timerSet {
		return
	}
	g.timerSet = true
	if wait < 0 {
		wait = 0
	}
	g.afterFunc(wait, func() { g.request(true) })
}

func (s *Service) claudeUsageGate() *usageProbeGate {
	s.claudeGateOnce.Do(func() {
		s.claudeGate = newUsageProbeGate(s.probeClaudeRateLimits, s.context)
	})
	return s.claudeGate
}

func (s *Service) codexUsageGate() *usageProbeGate {
	s.codexGateOnce.Do(func() {
		s.codexGate = newUsageProbeGate(s.probeCodexRateLimits, s.context)
	})
	return s.codexGate
}

func (s *Service) RequestClaudeUsage() { s.claudeUsageGate().Request() }

func (s *Service) RequestCodexUsage() { s.codexUsageGate().Request() }
