package workflowhost

import (
	"context"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/workflow/engine"
)

// The bound on `Runner.Stop`, and the guard on the interrupt it leaves behind.
//
// Both exist because a stop runs ON the engine's command-loop goroutine, the
// sole owner of every run's FSM state, and the wait it used to take there was
// `attempt.sendMu` — unbounded by construction. One send wedged on provider IO
// froze every run and every verb in the process (incident 2026-08-15 — a session
// restart blocked under the per-thread action lock mid-send).

// stopHarness is `newObserveHarness` with the stop bound shrunk to something a
// test may actually sit through, and the interrupt observable. The bound's whole
// subject is a wait nobody can wall-clock, so the duration is a runner field
// rather than a constant read at the call site.
func newStopHarness(t *testing.T) (*observeHarness, chan string) {
	t.Helper()
	h := newObserveHarness(t, string(provider.Codex))
	h.runner.StopSendWait = 20 * time.Millisecond
	interrupted := make(chan string, 4)
	h.runner.interrupt = func(_ context.Context, threadID string) error {
		interrupted <- threadID
		return nil
	}
	return h, interrupted
}

// detached models the state every late interrupt starts from: the attempt has
// been claimed out of the registry, synchronously, by whoever is stopping it.
func detachedAttempt(t *testing.T, h *observeHarness) *workflowAttempt {
	t.Helper()
	attempt, ok := h.runner.detach(h.runKey)
	if !ok {
		t.Fatal("the harness attempt was not registered")
	}
	return attempt
}

// A stop must return to the engine even while a send is wedged on provider IO.
// The interrupt is not abandoned — it lands once the wedge clears — but the
// command loop is not held for it.
func TestWorkflowStopReturnsWhileAWedgedSendHoldsTheAttempt(t *testing.T) {
	h, interrupted := newStopHarness(t)
	// From the runner's point of view this is a send that reached the wire and
	// never came back: `sendIfActive` holds `sendMu` across the whole dispatch.
	h.attempt.sendMu.Lock()

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		if partial, err := h.runner.Stop(context.Background(), h.attempt.key); err != nil || partial != nil {
			t.Errorf("Stop = (%s, %v), want the engine told nothing went wrong", partial, err)
		}
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop blocked on a wedged send; the engine's command loop is frozen for every run in the process")
	}

	// The detach is synchronous even though the interrupt is not: a send decided
	// before the stop drops at the door's admission recheck, and no provider event
	// can reach the attempt again.
	h.runner.mu.Lock()
	still := h.runner.runs[h.runKey] != nil
	h.runner.mu.Unlock()
	if still {
		t.Fatal("Stop returned without claiming the attempt out of the registry")
	}
	select {
	case threadID := <-interrupted:
		t.Fatalf("the interrupt landed underneath the in-flight send on thread %s", threadID)
	default:
	}

	h.attempt.sendMu.Unlock()
	select {
	case threadID := <-interrupted:
		if threadID != h.attempt.threadID {
			t.Fatalf("interrupted thread %q, want %q", threadID, h.attempt.threadID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the session was never interrupted after the wedge cleared")
	}
}

// The ordinary case the bound must not distort: nothing is wedged, so the stop
// completes its whole blocking half — the send-barrier wait and the interrupt —
// before returning to the engine, and nothing about it is latched as wedged.
func TestWorkflowStopWithNoWedgeInterruptsBeforeReturning(t *testing.T) {
	h, interrupted := newStopHarness(t)
	if partial, err := h.runner.Stop(context.Background(), h.attempt.key); err != nil || partial != nil {
		t.Fatalf("Stop = (%s, %v), want a clean stop", partial, err)
	}
	select {
	case threadID := <-interrupted:
		if threadID != h.attempt.threadID {
			t.Fatalf("interrupted thread %q, want %q", threadID, h.attempt.threadID)
		}
	default:
		t.Fatal("Stop returned before its interrupt fired; an unwedged stop must complete synchronously")
	}
	if wedged := h.runner.wedgedStops.Load(); wedged != 0 {
		t.Fatalf("wedgedStops = %d after a clean stop, want 0", wedged)
	}
}

// One wedged send must not cost a teardown loop the bound once per attempt: a
// teardown bringing a wave down calls Stop per unit, and the wedge cause is
// usually shared. While an expired stop's abandoned work is still stuck, later
// stops skip the wait outright — their interrupts still land, on their own
// goroutines — and the latch drains once the wedge clears.
func TestWorkflowStopSkipsTheWaitWhileAnEarlierStopIsWedged(t *testing.T) {
	h, interrupted := newStopHarness(t)
	h.attempt.sendMu.Lock()
	if _, err := h.runner.Stop(context.Background(), h.attempt.key); err != nil {
		t.Fatal(err)
	}
	if wedged := h.runner.wedgedStops.Load(); wedged != 1 {
		t.Fatalf("wedgedStops = %d after an expired stop, want 1", wedged)
	}

	// A sibling attempt wedged on the same cause, stopped while the first is
	// still stuck. The skip is what keeps this from paying the bound again: a
	// bounded wait would have expired too and latched a second wedge, so the
	// count staying at one when Stop returns is the whole assertion.
	next := engine.RunKey{ItemID: h.attempt.key.ItemID, PhaseID: h.attempt.key.PhaseID, Attempt: 2}
	sibling := &workflowAttempt{workflowCompletion: workflowCompletion{key: next}, threadID: "sibling-thread"}
	sibling.sendMu.Lock()
	h.runner.mu.Lock()
	h.runner.runs[workflowRunKey(next)] = sibling
	h.runner.mu.Unlock()
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		if _, err := h.runner.Stop(context.Background(), next); err != nil {
			t.Errorf("Stop of the sibling: %v", err)
		}
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("a stop issued under the wedge latch still waited out the bound")
	}
	if wedged := h.runner.wedgedStops.Load(); wedged != 1 {
		t.Fatalf("wedgedStops = %d after a skipped stop, want the original 1: the skip paid the bound anyway", wedged)
	}

	// Both wedges clear: the abandoned interrupts fire and the latch drains.
	h.attempt.sendMu.Unlock()
	sibling.sendMu.Unlock()
	seen := map[string]bool{}
	for range 2 {
		select {
		case threadID := <-interrupted:
			seen[threadID] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("only %v were interrupted after the wedges cleared", seen)
		}
	}
	if !seen[h.attempt.threadID] || !seen["sibling-thread"] {
		t.Fatalf("interrupted threads = %v, want both wedged attempts' threads", seen)
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.runner.wedgedStops.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("wedgedStops never drained after the wedge cleared")
		}
		time.Sleep(time.Millisecond)
	}
}

// The late interrupt's guard. By the time a wedged send clears, the thread may
// belong to somebody else — a continuation re-enters the phase on the parked
// attempt's thread, and a human takeover claims that same thread — and firing
// then would cut a live turn the dead attempt never owned.
func TestWorkflowLateInterruptSkipsAThreadThatHasBeenReclaimed(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		reclaim func(h *observeHarness)
	}{{
		name: "a newer attempt runs on it",
		reclaim: func(h *observeHarness) {
			// What an Answer or resume continuation installs: a new attempt of the
			// same phase, on the thread the parked one left behind.
			next := engine.RunKey{ItemID: h.attempt.key.ItemID, PhaseID: h.attempt.key.PhaseID, Attempt: 2}
			h.runner.mu.Lock()
			h.runner.runs[workflowRunKey(next)] = &workflowAttempt{
				workflowCompletion: workflowCompletion{key: next}, threadID: h.attempt.threadID,
			}
			h.runner.mu.Unlock()
		},
	}, {
		name: "a human is steering it",
		reclaim: func(h *observeHarness) {
			h.runner.mu.Lock()
			h.runner.takeovers[h.attempt.threadID] = workflowTakeover{itemID: h.attempt.key.ItemID}
			h.runner.mu.Unlock()
		},
	}} {
		t.Run(testCase.name, func(t *testing.T) {
			h, interrupted := newStopHarness(t)
			attempt := detachedAttempt(t, h)
			testCase.reclaim(h)

			// Run it inline: this is the goroutine `Stop` left behind, at the moment
			// its send barrier cleared.
			h.runner.interruptDetachedAttempt(h.runKey, attempt)

			select {
			case threadID := <-interrupted:
				t.Fatalf("the late interrupt cut a live turn on reclaimed thread %s", threadID)
			default:
			}
		})
	}
}

// The other half of the same guard: a thread nobody reclaimed still gets its
// session interrupted, which is what stops a wedged attempt's process leaking.
func TestWorkflowLateInterruptStillFiresOnAThreadNobodyReclaimed(t *testing.T) {
	h, interrupted := newStopHarness(t)
	attempt := detachedAttempt(t, h)

	h.runner.interruptDetachedAttempt(h.runKey, attempt)

	select {
	case threadID := <-interrupted:
		if threadID != attempt.threadID {
			t.Fatalf("interrupted thread %q, want %q", threadID, attempt.threadID)
		}
	default:
		t.Fatal("the late interrupt skipped a thread that no live attempt or takeover holds")
	}
}

// StopForTakeover takes the same wait on the same command-loop goroutine, so it
// takes the same bound. Its answer to the bound expiring is different: a human
// cannot steer a thread whose previous turn is still mid-dispatch, so the verb is
// refused and the attempt goes back exactly as it was.
func TestWorkflowStopForTakeoverRefusesAWedgedSendAndRestoresTheAttempt(t *testing.T) {
	h, interrupted := newStopHarness(t)
	// A live attempt normally rests with a watchdog armed; the refusal has to
	// put that back too, or the restored run stalls silently forever.
	deadline := h.now.Add(10 * time.Minute)
	h.runner.mu.Lock()
	h.attempt.timerMode = workflowTimerWatchdog
	h.attempt.timerDeadline = deadline
	h.attempt.timer = h.runner.newTimer(10*time.Minute, func() {})
	h.runner.mu.Unlock()
	h.attempt.sendMu.Lock()

	answered := make(chan error, 1)
	go func() {
		_, err := h.runner.StopForTakeover(context.Background(), h.attempt.key)
		answered <- err
	}()
	select {
	case err := <-answered:
		if err == nil {
			t.Fatal("StopForTakeover handed the human a thread whose send is still mid-dispatch")
		}
		if !strings.Contains(err.Error(), "has not reached the wire") {
			t.Fatalf("refusal does not name the wedge: %v", err)
		}
		if !strings.Contains(err.Error(), h.attempt.threadID) {
			t.Fatalf("refusal does not name the wedged thread: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StopForTakeover blocked on a wedged send; the engine's command loop is frozen for every run in the process")
	}
	select {
	case threadID := <-interrupted:
		t.Fatalf("a refused takeover interrupted thread %s anyway", threadID)
	default:
	}

	h.runner.mu.Lock()
	restored := h.runner.runs[h.runKey] == h.attempt
	schema := len(h.runner.schemas[h.attempt.threadID]) > 0
	itemID := h.runner.workItems[h.attempt.threadID]
	h.runner.mu.Unlock()
	if !restored {
		t.Fatal("the refused takeover left the attempt unregistered; nothing would ever settle its run")
	}
	if !schema {
		t.Fatal("the refused takeover left the thread without its envelope schema")
	}
	if itemID != h.attempt.key.ItemID {
		t.Fatalf("restored work-item attribution = %q, want %q", itemID, h.attempt.key.ItemID)
	}

	// The reliability surface came back with it: the watchdog is rearmed on its
	// original deadline rather than dropped with the detached state.
	h.runner.mu.Lock()
	mode, restoredDeadline := h.attempt.timerMode, h.attempt.timerDeadline
	timer, _ := h.attempt.timer.(*fakeWorkflowTimer)
	h.runner.mu.Unlock()
	if mode != workflowTimerWatchdog || timer == nil || !timer.active {
		t.Fatalf("restored timer = (mode %d, %+v), want the watchdog rearmed", mode, timer)
	}
	if !restoredDeadline.Equal(deadline) {
		t.Fatalf("restored watchdog deadline = %v, want the original %v", restoredDeadline, deadline)
	}
	// And provider events reach the turn machine again: the restore made a fresh
	// observer subscription, not a dangling reference to the torn-down one.
	h.host.dispatchTurnObservers(h.attempt.threadID, provider.ProviderEvent{Kind: provider.EventTurnStart})
	if state := h.state(); !state.turnStarted {
		t.Fatal("a provider event after the refused takeover never reached the attempt's turn machine")
	}

	h.attempt.sendMu.Unlock()
}
