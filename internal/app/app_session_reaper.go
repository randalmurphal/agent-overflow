package app

import (
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

// reapIdleSessions walks a.sessions, picks candidates whose
// lastActivity is older than the threshold and whose activeTurns
// counter is zero, then for each candidate confirms (a) triage holds
// no user-blocking live state (pending approvals, pending user-input
// requests, queued flush items, or pending sends), (b) no pending
// harness wakeup is still due to fire (ScheduleWakeup timers are
// in-process CLI state a close would silently kill), and (c) no
// running background tool calls exist in the store, before closing the
// session. The triage check ensures sessions that the user perceives
// as active or blocked-on-user are never reaped. The two-phase split
// keeps a.mu untouched during the triage query, the SQLite probe,
// and the close call, all of which can take meaningful time.
// idleCloseSession re-checks the per-session guards under the lock so
// a user send between the sweep snapshot and the close cannot lose a
// turn.
//
// Errors are logged, not surfaced. The reaper is opportunistic — a
// failed close just means the subprocess sticks around until the next
// sweep or an explicit StopSession. A failed bg-items query is treated
// as "skip this thread for now" so a transient SQLite error can't
// cause us to kill a session with active background work.
func (a *App) reapIdleSessions(now time.Time) {
	cutoffNano := now.Add(-idleReapThreshold).UnixNano()

	for _, threadID := range a.sessionManager().idleCandidates(cutoffNano) {
		if a.shuttingDown.Load() {
			return
		}
		if a.triage.HasPendingWork(threadID) {
			continue
		}
		// A pending ScheduleWakeup timer lives inside the CLI process
		// with no task lifecycle — the session looks fully idle until
		// the harness fires the stored prompt as a fresh turn. Closing
		// the process would silently kill the timer, so a future fire
		// time (plus firing-latency grace) blocks the reap.
		if wakeAt, ok := a.triage.PendingWakeupAt(threadID); ok && now.Before(wakeAt.Add(wakeupReapGrace)) {
			continue
		}
		running, err := a.store.ListRunningBackgroundToolCalls(threadID)
		if err != nil {
			log.Printf("app: idle reaper: list running background tool calls for %s: %v", threadID, err)
			continue
		}
		if len(running) > 0 {
			continue
		}
		if err := a.idleCloseSession(threadID, cutoffNano); err != nil {
			log.Printf("app: idle reaper: idle close %s: %v", threadID, err)
		}
	}
}

// idleCloseSession removes the session entry under the lock, then runs
// the shared teardown via teardownAndCloseSession. Skipped if:
//   - The entry is gone (the user beat us to StopSession, or
//     unregisterSession fired from readLoop's defer).
//   - activeTurns has gone above zero (a Codex EventTurnStart landed
//     between sweep and close). Claude never increments activeTurns
//     because its provider never emits EventTurnStart — see the comment
//     in recordSessionActivity for why that's safe.
//   - lastActivityUnixNano has advanced past the sweep cutoff. This
//     covers the gap between sendToProvider's pre-stdin-write bump and
//     the eventual EventTurnStart from the wire: a user send in that
//     window leaves activeTurns at 0 but moves lastActivity forward,
//     and the activity floor must protect the in-flight send.
//
// cutoffNano comes from reapIdleSessions so the floor matches the
// sweep that selected this candidate, not a fresh "now" that could
// have advanced under load.
func (a *App) idleCloseSession(threadID string, cutoffNano int64) error {
	sess, ok := a.sessionManager().takeIdle(threadID, cutoffNano)
	if !ok {
		return nil
	}

	return a.teardownAndCloseSession(threadID, sess)
}
