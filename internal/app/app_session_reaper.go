package app

import (
	"fmt"
	"log"
	"time"
)

// Idle-session reaper. Provider subprocesses are spawned per thread and
// never time out on their own — between turns, both `claude` and
// `codex app-server` sit on stdin waiting for the next message. Without
// eviction, navigating between threads accumulates one live subprocess
// per visited thread (~150–300 MB RSS each for Claude with Opus-class
// models). The reaper sweeps the session map on a slow cadence and
// closes any session that has gone idleReapThreshold without activity,
// is not mid-turn, and has no running background tool calls. The next
// user send (or SwitchThread auto-resume) lazily respawns the
// subprocess and reattaches via the persisted SessionRef.
//
// Reference: t3-code's ProviderSessionReaper at apps/server/src/provider/
// Layers/ProviderSessionReaper.ts (30 min idle, 5 min sweep).

const (
	// idleReapThreshold is the inactivity window after which a session
	// becomes a reap candidate. Treated as a floor: a session that
	// crossed the threshold mid-sweep is reaped on the next tick rather
	// than the boundary tick. 30 min (t3-code's default): each Claude
	// process holds ~288 MB RSS, but reaping a session also ends its
	// harness-backgrounded work (persistent Monitor tasks die at
	// session end), so the window errs toward keeping quiet-but-working
	// sessions alive over reclaiming memory a sweep earlier.
	idleReapThreshold = 30 * time.Minute

	// idleReapInterval is the sweep cadence. Five minutes is long
	// enough to keep the per-tick cost negligible (one map walk + at
	// most one SQLite probe per candidate) and short enough that the
	// reaper resolves the leak before a typical desktop session runs
	// out of memory.
	idleReapInterval = 5 * time.Minute

	// wakeupReapGrace extends a pending harness wakeup's protection past
	// its fire time. When the wakeup fires, the CLI starts a turn whose
	// wire activity bumps lastActivity and protects the session on its
	// own — the grace only has to cover the firing latency between the
	// scheduled instant and the first envelope reaching our read loop.
	wakeupReapGrace = 2 * time.Minute
)

// startIdleSessionReaper kicks off the background sweeper goroutine.
// Idempotent: a second call while a reaper is already running is a
// no-op so test fixtures that exercise ServiceStartup repeatedly can't
// fan out reapers. Shutdown closes idleReaperStop and waits on the
// WaitGroup before the parallel session close runs in Step 4, so the
// reaper can't fire mid-teardown.
//
// Unlike startClaudeRateLimitProbeLoop (which selects on appCtx.Done()
// and can exit on its own), this reaper needs deterministic teardown
// via the idleReaperStop channel + WaitGroup because its close path
// races a.sessions mutation; the rate-limit probe only reads a snapshot.
func (a *App) startIdleSessionReaper() {
	stop, started := a.sessionManager().runtime.StartIdleReaper()
	if !started {
		return
	}

	go func() {
		defer a.sessionManager().runtime.IdleReaperDone()
		ticker := time.NewTicker(idleReapInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if a.shuttingDown.Load() {
					return
				}
				a.reapIdleSessions(a.reaperNow())
			}
		}
	}()
}

// stopIdleSessionReaper signals the reaper goroutine to exit and waits
// for it to return. Safe to call before the reaper has started (no-op)
// and safe to call twice (the nil-then-close guard makes the second
// call a no-op).
func (a *App) stopIdleSessionReaper() {
	a.sessionManager().runtime.StopIdleReaper()
}

// reaperNow returns the reaper's notion of "now," honoring the
// test-only clock override. Exposed for unit tests via the
// idleReaperNowFn field; production callers route through here.
func (a *App) reaperNow() time.Time {
	if a.idleReaperNowFn != nil {
		return a.idleReaperNowFn()
	}
	return time.Now()
}

// reapIdleSessions selects stale sessions. idleCloseSession owns the complete
// eligibility check under the thread locks. Query or teardown failures remain
// observable in the log; a query failure must never authorize eviction.
func (a *App) reapIdleSessions(now time.Time) {
	cutoffNano := now.Add(-idleReapThreshold).UnixNano()

	for _, threadID := range a.sessionManager().idleCandidates(cutoffNano) {
		if a.shuttingDown.Load() {
			return
		}
		if err := a.idleCloseSession(threadID, cutoffNano); err != nil {
			log.Printf("app: idle reaper: idle close %s: %v", threadID, err)
		}
	}
}

// idleCloseSession serializes with execution and queue admission before
// checking live work. The runtime rechecks activity at removal so provider
// events arriving during the store query still prevent eviction.
// cutoffNano belongs to the sweep that selected this session.
func (a *App) idleCloseSession(threadID string, cutoffNano int64) error {
	unlock, err := a.threadLocks().LockCtx(a.lifeCtx(), threadID)
	if err != nil {
		return err
	}
	defer unlock()
	if _, present := a.sessionManager().get(threadID); !present {
		return nil
	}
	unlockMutation, err := a.threadApplication().LockMutable(a.lifeCtx(), threadID)
	if err != nil {
		return err
	}
	defer unlockMutation()
	if a.shuttingDown.Load() {
		return nil
	}
	if a.triage != nil && (a.triage.HasInFlightTurnOrRound(threadID) || a.triage.HasPendingWork(threadID)) {
		return nil
	}
	// Includes messages already handed off to a dispatch worker that is
	// waiting for this action lock, after they left triage's queue.
	if a.pendingFlushWorkCount(threadID) > 0 {
		return nil
	}
	now := time.Unix(0, cutoffNano).Add(idleReapThreshold)
	if wakeAt, ok := a.triage.PendingWakeupAt(threadID); ok && now.Before(wakeAt.Add(wakeupReapGrace)) {
		return nil
	}
	running, err := a.store.ListRunningBackgroundToolCalls(threadID)
	if err != nil {
		return fmt.Errorf("list running background tool calls: %w", err)
	}
	if len(running) > 0 {
		return nil
	}
	sess, ok := a.sessionManager().takeIdle(threadID, cutoffNano)
	if !ok {
		return nil
	}

	log.Printf("provider: idle close thread=%q provider=%q", threadID, sess.Provider)
	return a.teardownAndCloseSession(threadID, sess)
}
