package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// The pause is admission at the wake owner: a fire, a Wake now and a
// process start of a paused assistant's row are refused where the row is
// claimed, whatever selected it.

// markerWatch is a watch command that appends a line to a file each time it
// runs and prints nothing, so a run is counted without a delivery.
func markerWatch(t *testing.T) (command, marker string) {
	t.Helper()
	marker = filepath.Join(t.TempDir(), "runs")
	return "echo run >> '" + marker + "'", marker
}

// markerRuns counts the runs markerWatch recorded.
func markerRuns(t *testing.T, marker string) int {
	t.Helper()
	data, err := os.ReadFile(marker)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	runs := 0
	for _, b := range data {
		if b == '\n' {
			runs++
		}
	}
	return runs
}

// awaitNoAgentWakeRun waits until nothing of the row runs.
func awaitNoAgentWakeRun(t *testing.T, a *App, id string) {
	t.Helper()
	waitUntil(t, 10*time.Second, func() bool { return !agentWakeRunning(a, id) })
	if agentWakeRunning(a, id) {
		t.Fatalf("%s still runs", id)
	}
}

// Wake now on a paused assistant's loop, watch and reminder is refused with
// the off message and changes nothing: no claim, no script, no settlement.
func TestWakeNowOnAPausedAssistantIsRefused(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "wake-now-paused")
	testAssistant(t, a, thread.ID)
	command, marker := markerWatch(t)
	next := time.Now().Add(time.Hour).UnixMilli()
	loop := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeLoop, Label: "board", Note: "n", IntervalSeconds: 3600, NextAt: next})
	watch := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "poll", Command: command, IntervalSeconds: 3600, TimeoutSeconds: 60, NextAt: next})
	insertTestReminder(t, a, "wake-now-paused-remind", thread.ID, "look", next)
	t.Cleanup(func() { a.stopAgentWakeRunsForThread(thread.ID) })
	setAssistants(t, a, false)

	for _, row := range []store.AgentWake{loop, watch} {
		before := agentWake(t, a, row.ID)
		if err := a.WakeThreadWakeNow(t.Context(), thread.ID, row.Kind, row.ID); !errors.Is(err, errAssistantsOff) {
			t.Fatalf("Wake now on the paused %s = %v, want the off message", row.Kind, err)
		}
		awaitNoAgentWakeRun(t, a, row.ID)
		if after := agentWake(t, a, row.ID); after != before {
			t.Fatalf("the refused %s changed:\n before %+v\n after  %+v", row.Kind, before, after)
		}
	}
	if runs := markerRuns(t, marker); runs != 0 {
		t.Fatalf("the paused watch's script ran %d times", runs)
	}
	if err := a.WakeThreadWakeNow(t.Context(), thread.ID, threadWakeKindReminder, "wake-now-paused-remind"); !errors.Is(err, errAssistantsOff) {
		t.Fatalf("Wake now on the paused reminder = %v, want the off message", err)
	}
	if reminder, found, err := a.store.GetThreadRequest("wake-now-paused-remind"); err != nil || !found || reminder.State != store.ThreadRequestAccepted || reminder.SettledAt != 0 {
		t.Fatalf("the refused reminder = %+v, %v, %v; want it still waiting", reminder, found, err)
	}
	if queued := durableQueueRows(t, a, thread.ID); len(queued) != 0 {
		t.Fatalf("the refused wakes queued %+v", queued)
	}
}

// A due selection the sweep made while assistants ran, served after the
// pause completed, runs nothing: the watch's script does not execute, the
// loop is not claimed and the reminder does not settle.
func TestADueSelectionHeldAcrossThePauseDoesNotRun(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "held-selection")
	testAssistant(t, a, thread.ID)
	command, marker := markerWatch(t)
	due := time.Now().Add(-time.Minute).UnixMilli()
	registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeLoop, Label: "board", Note: "n", IntervalSeconds: 3600, NextAt: due})
	registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "poll", Command: command, IntervalSeconds: 3600, TimeoutSeconds: 60, NextAt: due})
	insertTestReminder(t, a, "held-selection-remind", thread.ID, "look", due)
	t.Cleanup(func() { a.stopAgentWakeRunsForThread(thread.ID) })

	now := time.Now()
	wakes, err := a.store.DueAgentWakes(now.UnixMilli(), agentWakeDueBatch)
	if err != nil || len(wakes) != 2 {
		t.Fatalf("due loops and watches = %+v, %v; want both", wakes, err)
	}
	reminders, err := a.store.DueThreadReminders(now.UnixMilli(), threadReminderBatch)
	if err != nil || len(reminders) != 1 {
		t.Fatalf("due reminders = %+v, %v; want one", reminders, err)
	}
	setAssistants(t, a, false)

	// The sweep's pass, served from the selection it held.
	for _, row := range wakes {
		if err := a.fireAgentWake(row, now, agentWakeFire{at: now.UnixMilli()}); !errors.Is(err, errAssistantsOff) {
			t.Fatalf("the held %s fired with %v, want it refused as assistants off", row.Kind, err)
		}
	}
	for _, row := range reminders {
		if err := a.fireThreadReminder(row, now); !errors.Is(err, errAssistantsOff) {
			t.Fatalf("the held reminder fired with %v, want it refused as assistants off", err)
		}
	}
	for _, row := range wakes {
		awaitNoAgentWakeRun(t, a, row.ID)
		if after := agentWake(t, a, row.ID); after.State != store.AgentWakeArmed || after.LastRunAt != 0 {
			t.Fatalf("the held %s ran after the pause: %+v", row.Kind, after)
		}
	}
	if runs := markerRuns(t, marker); runs != 0 {
		t.Fatalf("the held watch's script ran %d times after the pause", runs)
	}
	if reminder, _, _ := a.store.GetThreadRequest("held-selection-remind"); reminder.State != store.ThreadRequestAccepted {
		t.Fatalf("the held reminder settled after the pause: %s", reminder.State)
	}
}

// A long-running watch's process does not start for a paused assistant,
// whichever path asks for it.
func TestAPausedAssistantsWatchProcessDoesNotStart(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "paused-process")
	testAssistant(t, a, thread.ID)
	command, marker := markerWatch(t)
	setAssistants(t, a, false)
	// Registered as the agent's call left it before the pause.
	watch, err := a.store.RegisterAgentWake(store.AgentWake{
		ID: newAgentWakeID(), ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "tail", Command: command + "; exec sleep 60",
		LongRunning: true,
	})
	if err != nil {
		t.Fatalf("insert the watch: %v", err)
	}
	t.Cleanup(func() { a.stopAgentWakeRunsForThread(thread.ID) })

	a.startAgentWatchProcess(watch)
	awaitNoAgentWakeRun(t, a, watch.ID)
	if row := agentWake(t, a, watch.ID); row.State != store.AgentWakeArmed {
		t.Fatalf("the paused watch = %+v; want it armed", row)
	}
	if runs := markerRuns(t, marker); runs != 0 {
		t.Fatalf("the paused watch's process ran %d times", runs)
	}
}
