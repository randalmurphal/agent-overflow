package app

import (
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// A message the app queues for a paused assistant waits in its queue with no
// session and no error, and turning assistants on starts the session for
// it.
func TestAMessageForAPausedAssistantWaitsForAssistants(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	a := f.app
	a.registerSettingsBroadcast()
	target, agent := f.assistantTarget(t, "Ops.")
	setAssistants(t, a, false)

	sendID := assistantSendID("opening", agent.ID)
	if err := a.queueAssistantMessage(target.ID, sendID, target.Title, "who are you"); err != nil {
		t.Fatalf("queue a message for the paused assistant: %v", err)
	}
	a.waitAgentNoticeWork()
	if _, live := a.sessionManager().get(target.ID); live {
		t.Fatal("a session started for a paused assistant")
	}
	queued := durableQueueRows(t, a, target.ID)
	if len(queued) != 1 || queued[0].SendID != sendID {
		t.Fatalf("the paused assistant's queue = %+v, want the message waiting", queued)
	}
	items, err := a.store.ListItems(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Kind == "error" {
			t.Fatalf("the paused assistant has an error row: %s", item.Summary)
		}
	}

	sent := f.mockClaudeRecording(t, "I am Ops.")
	setAssistants(t, a, true)
	awaitSent(t, sent, func(text string) bool { return strings.Contains(text, "who are you") })
}

// A message queued in a paused assistant's composer queue, by a client
// that still shows it, waits there too, rather than coming back as a
// failed start.
func TestAQueuedMessageForAPausedAssistantWaits(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "paused-queue")
	testAssistant(t, a, thread.ID)
	setAssistants(t, a, false)
	if _, err := a.RegisterQueueItem(t.Context(), thread.ID, "later", SendMessageOptions{SendID: "person-later"}); err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}
	if err := a.drainFlushDispatch(t.Context(), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if queued := durableQueueRows(t, a, thread.ID); len(queued) != 1 || queued[0].SendID != "person-later" {
		t.Fatalf("queue = %+v, want the message waiting", queued)
	}
	items, err := a.store.ListItems(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Kind == "error" {
			t.Fatalf("an error row: %s", item.Summary)
		}
	}
}

// Toggled off and on again before the first change has been applied, the
// computer ends where the setting ends: on, nothing paused.
func TestAssistantsToggledTwiceQuicklyEndOn(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "toggle-twice")
	testAssistant(t, a, thread.ID)
	loop := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeLoop, Label: "board", Note: "n", IntervalSeconds: 3600, NextAt: time.Now().Add(time.Hour).UnixMilli()})
	// Both writes land before either convergence runs.
	a.assistantsSwitch.mu.Lock()
	switchAssistants(t, a, false)
	switchAssistants(t, a, true)
	a.assistantsSwitch.mu.Unlock()
	awaitAssistants(t, a, true)
	if a.assistantsPaused() {
		t.Fatal("assistants are paused after ending on")
	}
	live, err := a.store.ListLiveAgentWakes()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].ID != loop.ID {
		t.Fatalf("live rows = %+v, want the assistant's loop", live)
	}
}

// Off, then a restart, then on: the boot leaves a paused assistant's rows
// alone, a run a quit cut among them included, and turning assistants on
// recovers the cut run and runs it again once, while the sweep serves the
// loop that came due once, with its count.
func TestAssistantsOffAcrossARestartResumeWhenTurnedOn(t *testing.T) {
	t.Parallel()
	first, dbPath := newTestAppWithStorePath(t)
	thread := noticeTestThread(t, first, "restart-off")
	testAssistant(t, first, thread.ID)
	now := time.Now()
	loop := registerTestWake(t, first, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeLoop, Label: "board", Note: "n", IntervalSeconds: 3600, NextAt: now.Add(-150 * time.Minute).UnixMilli()})
	command, marker := markerWatch(t)
	watch := registerTestWake(t, first, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "quiet", Command: command, IntervalSeconds: 3600, TimeoutSeconds: 60, NextAt: now.Add(time.Hour).UnixMilli()})
	// The quit cut this run.
	if _, claimed, err := first.store.ClaimAgentWake(watch, now.UnixMilli(), now.Add(time.Hour).UnixMilli(), 0); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if err := first.store.Close(); err != nil {
		t.Fatal(err)
	}

	a := newTestAppAtStorePath(t, dbPath)
	t.Cleanup(func() { a.stopAgentWakeRunsForThread(thread.ID) })
	// The setting as the boot reads it: off, with nothing converged yet.
	a.settings.SetChangeObserver(nil)
	if _, err := a.settings.Update(map[string]any{"assistantsEnabled": false}); err != nil {
		t.Fatal(err)
	}
	a.registerSettingsBroadcast()
	a.initAssistantsPause(a.store)
	a.recoverAgentWakesAtBoot()
	a.fireDueAgentWakes(time.Now())
	if row := agentWake(t, a, watch.ID); row.State != store.AgentWakeRunning || row.LastOutcome != "" {
		t.Fatalf("the boot recovered a paused assistant's watch: %+v", row)
	}
	if row := agentWake(t, a, loop.ID); row.LastRunAt != 0 {
		t.Fatalf("the boot fired a paused assistant's loop: %+v", row)
	}

	setAssistants(t, a, true)
	// The rerun's own outcome replaces the cut's, so the run is what shows
	// the cut was recovered: the claim that reruns it needs the row armed.
	waitUntil(t, 10*time.Second, func() bool { return markerRuns(t, marker) == 1 })
	awaitNoAgentWakeRun(t, a, watch.ID)
	if runs, row := markerRuns(t, marker), agentWake(t, a, watch.ID); runs != 1 || row.State != store.AgentWakeArmed || row.LastRunAt <= now.UnixMilli() {
		t.Fatalf("the cut watch after assistants came back ran %d times: %+v; want it run again once", runs, row)
	}
	a.fireDueAgentWakes(time.Now())
	if row := agentWake(t, a, loop.ID); row.LastRunAt == 0 || row.Missed != 2 {
		t.Fatalf("the loop after assistants came back = %+v; want it fired once standing in for 2", row)
	}
}

// Message search follows the setting as soon as it is written, both ways,
// while the pause of what runs waits for the convergence: an open search
// that requeries on the settings event finds the assistant's messages once
// they are on again, however long recovery takes, and none once they are
// off.
func TestMessageSearchFollowsTheSettingAtOnce(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "search-at-once")
	testAssistant(t, a, thread.ID)
	insertUserItem(t, a.store, thread.ID, "search-at-once-item", 0, "quokka in the assistant")
	found := func() bool {
		t.Helper()
		hits, err := a.SearchThreadMessages("quokka", 10)
		if err != nil {
			t.Fatal(err)
		}
		return len(hits) > 0
	}
	setAssistants(t, a, false)
	if found() {
		t.Fatal("a paused assistant's message is a search hit")
	}

	// The convergence that would release the pause cannot run yet.
	a.assistantsSwitch.mu.Lock()
	switchAssistants(t, a, true)
	shown := found()
	a.assistantsSwitch.mu.Unlock()
	if !shown {
		t.Fatal("the assistant's message is no hit right after assistants were turned on")
	}
	awaitAssistants(t, a, true)

	a.assistantsSwitch.mu.Lock()
	switchAssistants(t, a, false)
	hidden := !found()
	a.assistantsSwitch.mu.Unlock()
	if !hidden {
		t.Fatal("the assistant's message is still a hit right after assistants were turned off")
	}
}
