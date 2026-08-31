package app

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// TestSessionLivenessBumpActivityStampsClock is the base-case contract
// for the activity stamp the reaper reads. A nil receiver is a no-op
// — the recordSessionActivity guard depends on this being safe so that
// a session entry without liveness wired up (legacy test fixtures)
// can't panic when an event arrives.
func TestSessionLivenessBumpActivityStampsClock(t *testing.T) {
	l := newSessionLiveness(time.Unix(0, 1_000))
	if got := l.LastActivityUnixNano.Load(); got != 1_000 {
		t.Fatalf("initial lastActivity = %d, want 1000", got)
	}
	l.BumpActivity(time.Unix(0, 5_000))
	if got := l.LastActivityUnixNano.Load(); got != 5_000 {
		t.Fatalf("after bump lastActivity = %d, want 5000", got)
	}

	// nil receiver must not panic — guards the recordSessionActivity
	// fast-path where the entry has no liveness.
	var nilLiveness *sessionLiveness
	nilLiveness.BumpActivity(time.Unix(0, 9_000))
}

// TestRecordSessionActivityBumpsOnEveryEventKind feeds each non-status
// event kind through the chokepoint and asserts the timestamp advances.
// We don't enumerate every constant — the function is uniform across
// kinds — but we cover the rendering-row kinds, an interactive kind,
// and EventInit to pin the contract that "any provider event keeps the
// subprocess alive." Also asserts that the non-Turn kinds DO NOT
// mutate the turn counter, which would silently disable the reaper's
// mid-turn skip if a future refactor routed a non-Turn kind into the
// EventTurnStart arm.
func TestRecordSessionActivityBumpsOnEveryEventKind(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-bump")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "tok",
		Liveness: newSessionLiveness(time.Unix(0, 1)),
	})

	kinds := []provider.EventKind{
		provider.EventInit,
		provider.EventTextDelta,
		provider.EventToolStart,
		provider.EventToolComplete,
		provider.EventApprovalRequest,
		provider.EventThinking,
	}
	entry, _ := app.sessionManager().get(thread.ID)
	prev := entry.Liveness.LastActivityUnixNano.Load()
	for i, k := range kinds {
		// Sleep one nanosecond between bumps so the monotonic clock can
		// distinguish them — wall-clock granularity on Linux is much
		// finer than this, but time.Now's monotonic component on a fast
		// loop is not guaranteed to advance every call without a yield.
		time.Sleep(time.Nanosecond)
		app.recordSessionActivity(thread.ID, "tok", k, "")
		got := entry.Liveness.LastActivityUnixNano.Load()
		if got <= prev {
			t.Fatalf("kind[%d]=%s did not advance lastActivity (prev=%d got=%d)", i, k, prev, got)
		}
		prev = got
	}
	if got := entry.Liveness.ActiveTurns.Load(); got != 0 {
		t.Fatalf("non-Turn kinds mutated activeTurns: got %d, want 0", got)
	}
}

// TestRecordSessionActivityTracksActiveTurns covers the start/complete
// counter the reaper consults to skip mid-turn sessions, plus the
// clamp-at-zero guard against an unmatched EventTurnComplete (replay,
// double-fire).
func TestRecordSessionActivityTracksActiveTurns(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-turns")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "tok",
		Liveness: newSessionLiveness(time.Unix(0, 1)),
	})

	entry, _ := app.sessionManager().get(thread.ID)
	l := entry.Liveness

	app.recordSessionActivity(thread.ID, "tok", provider.EventTurnStart, "")
	if got := l.ActiveTurns.Load(); got != 1 {
		t.Fatalf("after TurnStart activeTurns = %d, want 1", got)
	}

	app.recordSessionActivity(thread.ID, "tok", provider.EventTurnComplete, "")
	if got := l.ActiveTurns.Load(); got != 0 {
		t.Fatalf("after TurnComplete activeTurns = %d, want 0", got)
	}

	// Unmatched TurnComplete (replay envelope, double-fire) must clamp
	// at zero, not drive the counter negative.
	app.recordSessionActivity(thread.ID, "tok", provider.EventTurnComplete, "")
	if got := l.ActiveTurns.Load(); got != 0 {
		t.Fatalf("unmatched TurnComplete drove activeTurns negative: %d", got)
	}
}

// TestRecordSessionActivityDisconnectDrainsActiveTurns guards the
// stuck-counter case: a turn that erred before EventTurnComplete left
// activeTurns at 1; the subsequent EventSessionStatus("disconnected")
// must clamp it back to zero so a replacement session on the same
// thread isn't shielded from the reaper by a stale counter from the
// previous incarnation. Non-disconnect status events ("ready", "error")
// must not drain.
func TestRecordSessionActivityDisconnectDrainsActiveTurns(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-drain")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	l := newSessionLiveness(time.Now())
	l.ActiveTurns.Store(1)
	// simulate stuck counter
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "tok",
		Liveness: l,
	})

	// Non-disconnect status must NOT drain.
	app.recordSessionActivity(thread.ID, "tok", provider.EventSessionStatus, "ready")
	if got := l.ActiveTurns.Load(); got != 1 {
		t.Fatalf("non-disconnect status drained counter: %d, want 1", got)
	}

	// Disconnect status drains regardless of prior count.
	app.recordSessionActivity(thread.ID, "tok", provider.EventSessionStatus, "disconnected")
	if got := l.ActiveTurns.Load(); got != 0 {
		t.Fatalf("disconnect did not drain counter: %d", got)
	}
}

// TestRecordSessionActivityIgnoresStaleToken protects against a
// reconnect-race where an older handler fires after a replacement
// session is in place — the bump must not target the wrong session.
// Mirrors TestUnregisterSessionKeepsSessionWhenTokenIsStale.
func TestRecordSessionActivityIgnoresStaleToken(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-stale-tok")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	current := newSessionLiveness(time.Unix(0, 5_000))
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "current",
		Liveness: current,
	})

	app.recordSessionActivity(thread.ID, "stale", provider.EventTextDelta, "")
	if got := current.LastActivityUnixNano.Load(); got != 5_000 {
		t.Fatalf("stale token bumped current Liveness: %d", got)
	}
	app.recordSessionActivity(thread.ID, "stale", provider.EventTurnStart, "")
	if got := current.ActiveTurns.Load(); got != 0 {
		t.Fatalf("stale token mutated activeTurns: %d", got)
	}
}

// TestReapIdleSessionsClosesStaleSession is the load-bearing test for
// the fix. A session with last activity older than the threshold and
// zero active turns must be evicted from a.sessions when the reaper
// sweeps.
func TestReapIdleSessionsClosesStaleSession(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-stale-session")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	now := time.Now()
	past := now.Add(-idleReapThreshold - time.Minute)
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "tok",
		Liveness: newSessionLiveness(past),
		// claude + codex left nil — closeProviderSession is a no-op
		// when providerSession() returns nil, which is what we want
		// for a pure liveness/eviction test.
	})

	app.reapIdleSessions(now)

	_, present := app.sessionManager().get(thread.ID)
	if present {
		t.Fatal("expected idle session to be reaped")
	}
}

// TestReapIdleSessionsSkipsRecentActivity confirms the threshold is a
// floor: a session whose last bump is inside the window stays alive.
func TestReapIdleSessionsSkipsRecentActivity(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-recent")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	now := time.Now()
	recent := now.Add(-idleReapThreshold + time.Minute)
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "tok",
		Liveness: newSessionLiveness(recent),
	})

	app.reapIdleSessions(now)

	_, present := app.sessionManager().get(thread.ID)
	if !present {
		t.Fatal("recently-active session was incorrectly reaped")
	}
}

// TestReapIdleSessionsSkipsActiveTurn confirms a session mid-turn is
// never reaped even when wall-clock idleness would otherwise qualify
// it. Mirrors the t3-code reaper's activeTurnId guard.
func TestReapIdleSessionsSkipsActiveTurn(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-active-turn")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	now := time.Now()
	past := now.Add(-idleReapThreshold - time.Minute)
	l := newSessionLiveness(past)
	l.ActiveTurns.Store(1)
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "tok",
		Liveness: l,
	})

	app.reapIdleSessions(now)

	_, present := app.sessionManager().get(thread.ID)
	if !present {
		t.Fatal("active-turn session was incorrectly reaped")
	}
}

// TestReapIdleSessionsSkipsRunningBackgroundItems pins the second
// safety check: even with stale activity and zero active turns, a
// session whose thread has a still-`running` background tool call
// must stay alive so the reaper doesn't kill a long-running build
// (Codex) or a server the model left running (Claude).
func TestReapIdleSessionsSkipsRunningBackgroundItems(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-bg-running")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	now := time.Now()
	pastMillis := now.Add(-time.Hour).UnixMilli()
	if _, err := app.store.AppendItem(store.Item{
		ID:           "bg-1",
		ThreadID:     thread.ID,
		TurnIndex:    1,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "running",
		IsBackground: true,
		Summary:      "Bash: long-running build",
		ToolName:     "Bash",
		CreatedAt:    pastMillis,
		UpdatedAt:    pastMillis,
	}); err != nil {
		t.Fatalf("seed bg item: %v", err)
	}

	past := now.Add(-idleReapThreshold - time.Minute)
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "tok",
		Liveness: newSessionLiveness(past),
	})

	app.reapIdleSessions(now)

	_, present := app.sessionManager().get(thread.ID)
	if !present {
		t.Fatal("session with running background tool call was reaped")
	}
}

// TestReapIdleSessionsSkipsOnBackgroundItemsQueryError protects the
// "fail-safe" behavior: if the SQLite probe fails (transient lock,
// closed DB on shutdown), the reaper must not interpret the failure
// as "no background work, safe to kill." Skipping the candidate
// preserves the session until the next sweep when the probe can
// succeed.
func TestReapIdleSessionsSkipsOnBackgroundItemsQueryError(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-store-err")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	now := time.Now()
	past := now.Add(-idleReapThreshold - time.Minute)
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "tok",
		Liveness: newSessionLiveness(past),
	})

	// Close the store under the reaper to force ListRunningBackgroundToolCalls
	// to fail. The reaper logs and continues; the session must survive.
	if err := app.store.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	app.reapIdleSessions(now)

	_, present := app.sessionManager().get(thread.ID)
	if !present {
		t.Fatal("session was reaped despite background-items query error — fail-open would risk killing live work")
	}
}

// TestIdleCloseSessionRespectsRaceWithFreshTurn covers the first
// in-lock guard: if a turn started between the sweep's snapshot and
// the close call, the close is skipped. Without this guard the reaper
// could yank a session out from under a live send.
func TestIdleCloseSessionRespectsRaceWithFreshTurn(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-late-turn")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	cutoff := time.Now().Add(-idleReapThreshold).UnixNano()
	l := newSessionLiveness(time.Now().Add(-time.Hour))
	l.ActiveTurns.Store(1)
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "tok",
		Liveness: l,
	})

	if err := app.idleCloseSession(thread.ID, cutoff); err != nil {
		t.Fatalf("idleCloseSession: %v", err)
	}

	_, present := app.sessionManager().get(thread.ID)
	if !present {
		t.Fatal("idleCloseSession reaped a session with an in-flight turn")
	}
}

// TestIdleCloseSessionRespectsRaceWithFreshSend covers the TOCTOU
// guard that the activeTurns check alone cannot catch: sendToProvider
// stamps lastActivityUnixNano BEFORE writing to stdin, but
// EventTurnStart arrives back from the wire asynchronously. A user
// send in that window leaves activeTurns at 0 but bumps the activity
// floor — idleCloseSession must honor the floor or it'll close the
// subprocess mid-send. This is a regression test for the TOCTOU bug
// the post-task review surfaced.
func TestIdleCloseSessionRespectsRaceWithFreshSend(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-fresh-send")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	now := time.Now()
	cutoff := now.Add(-idleReapThreshold).UnixNano()
	// Simulate "the sweep saw this session as idle past the threshold"
	// followed by "a user send landed between sweep and close, which
	// bumped lastActivity but hasn't yet triggered EventTurnStart so
	// activeTurns is still 0."
	l := newSessionLiveness(now.Add(-time.Hour))
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "tok",
		Liveness: l,
	})
	// Pre-close bump: this is what sendToProvider / RespondToApproval /
	// etc. do before stdin write. The bump alone (without an
	// EventTurnStart yet) must prevent the close.
	l.BumpActivity(now)

	if err := app.idleCloseSession(thread.ID, cutoff); err != nil {
		t.Fatalf("idleCloseSession: %v", err)
	}

	_, present := app.sessionManager().get(thread.ID)
	if !present {
		t.Fatal("idleCloseSession reaped a session with a fresh pre-EventTurnStart send")
	}
}

// TestIdleCloseSessionRunsTeardownWhenEntryGone confirms the shared
// teardownAndCloseSession helper still runs when the session was
// already removed (e.g. unregisterSession fired between sweep and
// close). Mirrors StopSession's "missing entry still scrubs state"
// contract.
func TestIdleCloseSessionRunsTeardownWhenEntryGone(t *testing.T) {
	app := newTestAppWithStore(t)
	// No session inserted. idleCloseSession returns nil; nothing should
	// panic.
	if err := app.idleCloseSession("absent-thread", time.Now().UnixNano()); err != nil {
		t.Fatalf("idleCloseSession on absent entry: %v", err)
	}
}

// TestStartStopIdleSessionReaperRoundTrip verifies the goroutine
// lifecycle: start spins up exactly one sweeper, stop blocks until it
// exits, and calling stop a second time is a no-op. Repeats start to
// confirm the idle reaper can be restarted after a stop (defensive —
// production never restarts it, but tests should be able to).
func TestStartStopIdleSessionReaperRoundTrip(t *testing.T) {
	app := newTestAppWithStore(t)
	app.startIdleSessionReaper()
	// Idempotency: a second start while running must not fan out a
	// second goroutine. The WaitGroup invariant is "stop returns after
	// exactly the right number of Adds" so if start fan-out broke,
	// either WG.Wait would never return after one close (deadlock) or
	// it'd return early and a second goroutine would survive past
	// Stop.
	app.startIdleSessionReaper()
	app.stopIdleSessionReaper()
	// Second stop is a no-op; if it tried to close the channel again
	// it would panic.
	app.stopIdleSessionReaper()
	// Restart after stop must work.
	app.startIdleSessionReaper()
	app.stopIdleSessionReaper()
}

// TestStopIdleSessionReaperBeforeStart guards against shutdown
// invariants assuming start always ran. Pre-startup test harnesses
// (and any partial-init error path) must be able to call stop
// without panicking.
func TestStopIdleSessionReaperBeforeStart(t *testing.T) {
	app := newTestAppWithStore(t)
	app.stopIdleSessionReaper()
}

// TestReapIdleSessionsIsRaceFreeUnderChurn runs a sweep while parallel
// goroutines mutate the session map. The reaper must not panic, must
// not deadlock with the churn, and must reap every seeded entry while
// leaving the churn entries (which all have fresh activity) untouched.
// The assertion that matters is "every seeded entry is gone" — that
// validates the lock + snapshot pattern correctly observes the map
// despite contention.
//
// Primarily a -race-detector gate: a regression that dropped a.mu in
// the sweep would surface here as a data race, even when the
// assertion still happens to pass.
func TestReapIdleSessionsIsRaceFreeUnderChurn(t *testing.T) {
	app := newTestAppWithStore(t)

	const seeded = 20
	for i := 0; i < seeded; i++ {
		id := fmt.Sprintf("t-%02d", i)
		th := testThread(id)
		if err := app.store.CreateThread(th); err != nil {
			t.Fatalf("CreateThread: %v", err)
		}
		past := time.Now().Add(-idleReapThreshold - time.Minute)
		app.sessionManager().put(id, session{
			Provider: string(provider.Codex),
			Token:    "tok",
			Liveness: newSessionLiveness(past),
		})
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			churnID := fmt.Sprintf("churn-%d", slot)
			for {
				select {
				case <-stop:
					return
				default:
				}
				app.sessionManager().put(churnID, session{
					Provider: string(provider.Codex),
					Token:    "churn",
					Liveness: newSessionLiveness(time.Now()),
				})
				app.sessionManager().take(churnID)
			}
		}(i)
	}

	app.reapIdleSessions(time.Now())
	close(stop)
	wg.Wait()

	survived := 0
	for k := range app.sessionManager().runtime.Snapshot() {
		if k == "churn" {
			continue
		}
		// Any seeded id starts with "t-" — see fmt.Sprintf above.
		if len(k) >= 2 && k[:2] == "t-" {
			survived++
		}
	}
	if survived != 0 {
		t.Fatalf("expected all %d seeded sessions to be reaped, %d survived", seeded, survived)
	}
}

// --- Triage-aware reaper tests ---
//
// These tests verify that the reaper skips sessions when triage holds
// user-blocking live state, aligning the reaper's idle predicate with
// the frontend's isThreadWorking / status-priority-ladder derivation.

func newTestAppWithTriage(t *testing.T) *App {
	t.Helper()
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	return app
}

func staleSession() session {
	past := time.Now().Add(-idleReapThreshold - time.Minute)
	return session{
		Provider: string(provider.Claude),
		Token:    "tok",
		Liveness: newSessionLiveness(past),
	}
}

// TestReapIdleSessionsSkipsPendingApproval verifies that a session
// waiting for the user to respond to a permission prompt is never
// reaped, regardless of how long the user takes to answer.
func TestReapIdleSessionsSkipsPendingApproval(t *testing.T) {
	app := newTestAppWithTriage(t)
	thread := testThread("thread-pending-approval")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	app.sessionManager().put(thread.ID, staleSession())

	meta, _ := json.Marshal(provider.ApprovalRequest{
		RequestID:   "req-1",
		ThreadID:    thread.ID,
		ToolName:    "Bash",
		Description: "Run command",
	})
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventApprovalRequest,
		ThreadID:  thread.ID,
		ItemID:    "req-1",
		Meta:      meta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle approval request: %v", err)
	}

	app.reapIdleSessions(time.Now())

	_, present := app.sessionManager().get(thread.ID)
	if !present {
		t.Fatal("session with pending approval was reaped")
	}
}

// TestReapIdleSessionsSkipsPendingUserInput verifies that a session
// waiting for the user to answer an AskUserQuestion prompt is never
// reaped. This is the exact scenario from the bug report.
func TestReapIdleSessionsSkipsPendingUserInput(t *testing.T) {
	app := newTestAppWithTriage(t)
	thread := testThread("thread-pending-input")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	app.sessionManager().put(thread.ID, staleSession())

	meta, _ := json.Marshal(provider.UserInputRequest{
		RequestID: "req-2",
		ThreadID:  thread.ID,
		ToolName:  "user_input",
		Title:     "Column design",
		Questions: []provider.UserInputQuestion{{
			ID:       "q1",
			Header:   "Design",
			Question: "Which approach?",
			Options: []provider.UserInputQuestionOption{
				{Label: "Option A", Description: "First option"},
			},
		}},
	})
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserInputRequest,
		ThreadID:  thread.ID,
		ItemID:    "req-2",
		Meta:      meta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle user-input request: %v", err)
	}

	app.reapIdleSessions(time.Now())

	_, present := app.sessionManager().get(thread.ID)
	if !present {
		t.Fatal("session with pending user-input request was reaped")
	}
}

// TestReapIdleSessionsSkipsQueuedFlushItems verifies that a session
// with user messages queued behind an in-flight turn is not reaped.
func TestReapIdleSessionsSkipsQueuedFlushItems(t *testing.T) {
	app := newTestAppWithTriage(t)
	thread := testThread("thread-queued-flush")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	app.sessionManager().put(thread.ID, staleSession())

	app.triage.RegisterQueueItem(thread.ID, triage.QueuedFlushItem{
		ID:      "queue:test-1",
		Message: "follow-up message",
	})

	app.reapIdleSessions(time.Now())

	_, present := app.sessionManager().get(thread.ID)
	if !present {
		t.Fatal("session with queued flush items was reaped")
	}
}

// TestReapIdleSessionsSkipsPendingSend verifies that a session with a
// pending send awaiting wire echo is not reaped.
func TestReapIdleSessionsSkipsPendingSend(t *testing.T) {
	app := newTestAppWithTriage(t)
	thread := testThread("thread-pending-send")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	app.sessionManager().put(thread.ID, staleSession())

	app.triage.RegisterPendingSendWithExpectation(thread.ID, "user:1", 1, triage.PendingSendExpectation{})

	app.reapIdleSessions(time.Now())

	_, present := app.sessionManager().get(thread.ID)
	if !present {
		t.Fatal("session with pending send was reaped")
	}
}

// TestReapIdleSessionsReapsAfterApprovalResolves confirms that once
// all pending work clears, the session becomes reapable again. Without
// this, a resolved approval would shield a session from the reaper
// forever.
func TestReapIdleSessionsReapsAfterApprovalResolves(t *testing.T) {
	app := newTestAppWithTriage(t)
	thread := testThread("thread-resolved-approval")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	app.sessionManager().put(thread.ID, staleSession())

	reqMeta, _ := json.Marshal(provider.ApprovalRequest{
		RequestID: "req-3",
		ThreadID:  thread.ID,
		ToolName:  "Bash",
	})
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventApprovalRequest,
		ThreadID:  thread.ID,
		ItemID:    "req-3",
		Meta:      reqMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle approval request: %v", err)
	}

	resolveMeta, _ := json.Marshal(map[string]any{
		"requestId": "req-3",
		"decision":  "approved",
	})
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventApprovalResolved,
		ThreadID:  thread.ID,
		ItemID:    "req-3",
		Meta:      resolveMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle approval resolved: %v", err)
	}

	// Re-stamp stale liveness — triage.Handle doesn't touch session
	// liveness (that's recordSessionActivity's job via the provider
	// event loop), so force a stale timestamp to make the reaper
	// consider this session reapable.
	sess, _ := app.sessionManager().get(thread.ID)
	sess.Liveness.LastActivityUnixNano.Store(time.Now().Add(-idleReapThreshold - time.Minute).UnixNano())

	app.reapIdleSessions(time.Now())

	_, present := app.sessionManager().get(thread.ID)
	if present {
		t.Fatal("session with resolved approval was not reaped")
	}
}

// TestReapIdleSessionsSkipsPendingWakeup confirms a wall-clock-idle
// session is protected while the Claude harness holds a pending
// ScheduleWakeup timer (the timer is in-process state a close would
// silently kill), and becomes reapable again once the fire time plus
// grace has elapsed — a fired wakeup protects the session through
// normal turn activity instead.
func TestReapIdleSessionsSkipsPendingWakeup(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	thread := testThread("thread-pending-wakeup")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	now := time.Now()
	past := now.Add(-idleReapThreshold - time.Minute)
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "tok",
		Liveness: newSessionLiveness(past),
	})

	scheduleWakeup := func(fireAt time.Time) {
		meta, err := json.Marshal(provider.SessionWakeupMeta{ScheduledForUnixMs: fireAt.UnixMilli()})
		if err != nil {
			t.Fatalf("marshal wakeup meta: %v", err)
		}
		if err := app.triage.Handle(provider.ProviderEvent{
			Kind:      provider.EventSessionWakeup,
			ThreadID:  thread.ID,
			Meta:      meta,
			Timestamp: now,
		}); err != nil {
			t.Fatalf("handle wakeup: %v", err)
		}
	}

	// Future fire time: protected.
	scheduleWakeup(now.Add(20 * time.Minute))
	app.reapIdleSessions(now)
	_, present := app.sessionManager().get(thread.ID)
	if !present {
		t.Fatal("session with a pending future wakeup was incorrectly reaped")
	}

	// Fire time inside the grace window: still protected.
	scheduleWakeup(now.Add(-wakeupReapGrace / 2))
	app.reapIdleSessions(now)
	_, present = app.sessionManager().get(thread.ID)
	if !present {
		t.Fatal("session inside the wakeup grace window was incorrectly reaped")
	}

	// Fire time past the grace window: the wakeup either fired (and its
	// turn activity would have protected the session) or died with a
	// stuck harness — reapable again.
	scheduleWakeup(now.Add(-wakeupReapGrace - time.Minute))
	app.reapIdleSessions(now)
	_, present = app.sessionManager().get(thread.ID)
	if present {
		t.Fatal("session with an elapsed wakeup must be reapable")
	}
}
