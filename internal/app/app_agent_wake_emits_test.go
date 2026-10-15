package app

import (
	"context"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// Each write to a loop, watch or reminder outside the tray's own actions
// tells the tray, with the row as it stands after the write.

// awaitWakeChange is the first frame for id that matches, waiting up to a
// few seconds for one.
func awaitWakeChange(t *testing.T, rec *emitRecorder, threadID, id string, match func(ThreadWakeChange) bool) ThreadWakeChange {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, change := range wakeChanges(rec, threadID) {
			if change.ID == id && match(change) {
				return change
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no thread:wakes_changed frame for %s matched; frames: %+v", id, wakeChanges(rec, threadID))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A run a quit cut is recorded at boot, and the tray hears the outcome.
func TestBootRecoveryTellsTheTray(t *testing.T) {
	t.Parallel()
	a, rec := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "emits-recover")
	testAssistant(t, a, thread.ID)
	next := time.Now().Add(time.Hour).UnixMilli()
	// A quiet watch: the rerun the boot pass starts wakes nothing.
	watch := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "quiet", Command: "true", IntervalSeconds: 3600, TimeoutSeconds: 60, NextAt: next})
	t.Cleanup(func() { a.stopAgentWakeRunsForThread(thread.ID) })
	if claimed, err := a.store.ClaimAgentWake(watch.ID, time.Now().UnixMilli(), next, 0); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}

	a.recoverAgentWakesAtBoot()
	awaitWakeChange(t, rec, thread.ID, watch.ID, func(c ThreadWakeChange) bool {
		return c.Row != nil && c.Row.State == store.AgentWakeArmed && c.Row.LastOutcome == agentWakeOutcomeCutByQuit
	})
}

// A wake still queued when the app quit is recorded at boot, not put in the
// composer, and the tray hears it was not delivered.
func TestADiscardedBootWakeTellsTheTray(t *testing.T) {
	t.Parallel()
	a, rec := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "emits-discard")
	fire := time.Now().UnixMilli()
	loop := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeLoop, Label: "board", Note: "n", IntervalSeconds: 3600, NextAt: fire + 3600_000})

	if !a.discardAgentWakeAtBoot(store.FlushQueueItem{ThreadID: thread.ID, SendID: agentWakeSendID(loop.ID, fire), Message: "Loop \"board\" fired."}) {
		t.Fatal("the wake was not discarded")
	}
	awaitWakeChange(t, rec, thread.ID, loop.ID, func(c ThreadWakeChange) bool {
		return c.Row != nil && c.Row.Undelivered == agentWakeOutcomeNotDelivered
	})
}

// A long-running watch's process start is the row's `running`, and the tray
// hears it.
func TestALongRunningWatchStartTellsTheTray(t *testing.T) {
	t.Parallel()
	a, rec := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "emits-process")
	testAssistant(t, a, thread.ID)
	watch := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "tail", Command: "exec sleep 60", LongRunning: true})
	t.Cleanup(func() { a.stopAgentWakeRunsForThread(thread.ID) })

	// The process start itself, before any exit or restart has an outcome
	// to record.
	awaitWakeChange(t, rec, thread.ID, watch.ID, func(c ThreadWakeChange) bool {
		return c.Row != nil && c.Row.State == store.AgentWakeRunning && c.Row.LastOutcome == ""
	})
	if row, _, _ := a.store.GetAgentWake(watch.ID); row.LastOutcome != "" {
		t.Fatalf("the watch's process exited: %+v", row)
	}
}

// A fire whose wake never reached the queue because the app was shutting
// down is released for the next boot, and the tray hears it.
func TestAWakeLostToShutdownTellsTheTray(t *testing.T) {
	t.Parallel()
	a, rec := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "emits-release")
	now := time.Now()
	loop := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeLoop, Label: "board", Note: "n", IntervalSeconds: 3600, NextAt: now.Add(time.Hour).UnixMilli()})
	if claimed, err := a.store.ClaimAgentWake(loop.ID, now.UnixMilli(), now.Add(time.Hour).UnixMilli(), 0); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.appCtx = ctx
	run := &agentWakeRun{threadID: thread.ID, cancel: func() {}, done: make(chan struct{})}

	a.settleAgentWakeDelivery(run, loop, agentWakeFire{at: now.UnixMilli()}, "Loop \"board\" fired.", context.Canceled)
	change := awaitWakeChange(t, rec, thread.ID, loop.ID, func(c ThreadWakeChange) bool {
		return c.Row != nil && c.Row.State == store.AgentWakeArmed && c.Row.Undelivered == agentWakeOutcomeNotDelivered
	})
	if change.Row.NextAt != now.UnixMilli() {
		t.Fatalf("released row next fire = %d, want the fire it lost (%d)", change.Row.NextAt, now.UnixMilli())
	}
}

// A reminder the agent sets with thread_remind joins the tray at once.
func TestARemindToolCallTellsTheTray(t *testing.T) {
	t.Parallel()
	a, rec := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "emits-remind")
	due := time.Now().Add(time.Hour).UnixMilli()
	ack, err := threadToolsApp{app: a}.Remind(t.Context(), threadtools.Caller{ThreadID: thread.ID}, threadtools.RemindCall{DueAtUnixMs: due, Note: "check the deploy"})
	if err != nil {
		t.Fatalf("Remind: %v", err)
	}
	change := awaitWakeChange(t, rec, thread.ID, ack.Token, func(c ThreadWakeChange) bool { return c.Row != nil })
	if change.Row.Kind != threadWakeKindReminder || change.Row.Label != "check the deploy" || change.Row.NextAt != due {
		t.Fatalf("reminder frame row = %+v", change.Row)
	}
}

// A handoff moves an assistant's watches: the tray drops each from the old
// thread and lists it on the new one.
func TestAHandOffTellsTheTray(t *testing.T) {
	t.Parallel()
	a, rec := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "emits-handoff")
	testAssistant(t, a, thread.ID)
	watch := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "poll", Command: "true", IntervalSeconds: 3600, NextAt: time.Now().Add(time.Hour).UnixMilli()})

	successor, err := a.StartNextAssistantThread(t.Context(), thread.ID)
	if err != nil {
		t.Fatalf("StartNextAssistantThread: %v", err)
	}
	awaitWakeChange(t, rec, thread.ID, watch.ID, func(c ThreadWakeChange) bool { return c.Removed && c.Row == nil })
	awaitWakeChange(t, rec, successor.ID, watch.ID, func(c ThreadWakeChange) bool { return c.Row != nil && c.Row.Label == "poll" })
}

// A wake that was queued, and recorded delivered, but that the session
// ended before taking in, turns its row to not delivered, and the tray
// hears it: no other write follows to carry the change.
func TestAWakeTheSessionNeverTookInTellsTheTray(t *testing.T) {
	t.Parallel()
	a, rec := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "emits-undelivered")
	now := time.Now()
	loop := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeLoop, Label: "board", Note: "n", IntervalSeconds: 3600, NextAt: now.Add(time.Hour).UnixMilli()})
	if _, err := a.store.FinishAgentWake(loop.ID, agentWakeOutcomeDelivered, "", 0, store.AgentWakeCarried); err != nil {
		t.Fatal(err)
	}
	before := len(wakeChanges(rec, thread.ID))

	a.settleNotTakenIn(thread.ID, []notTakenIn{{messages: []appMessageOutcome{{
		sendID: agentWakeSendID(loop.ID, now.UnixMilli()), wakeOutcome: agentWakeOutcomeSessionEnded, body: "Loop \"board\" fired.",
	}}}})
	changes := wakeChanges(rec, thread.ID)[before:]
	if len(changes) != 1 || changes[0].ID != loop.ID || changes[0].Row == nil ||
		changes[0].Row.Undelivered != agentWakeOutcomeSessionEnded || changes[0].Row.LastOutcome != agentWakeOutcomeSessionEnded {
		t.Fatalf("frames after the session ended = %+v, want the row turned not delivered", changes)
	}
}
