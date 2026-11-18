package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// Turning assistants off and on (docs/specs/standing-agents.md, "Turning
// assistants off").

// setAssistants turns the Assistants setting on or off and waits until this
// computer has converged on it.
func setAssistants(t *testing.T, a *App, on bool) {
	t.Helper()
	switchAssistants(t, a, on)
	awaitAssistants(t, a, on)
}

// switchAssistants writes the setting without waiting for the convergence.
func switchAssistants(t *testing.T, a *App, on bool) {
	t.Helper()
	if _, err := a.settings.Update(map[string]any{"assistantsEnabled": on}); err != nil {
		t.Fatalf("set assistantsEnabled=%v: %v", on, err)
	}
}

// awaitAssistants waits until this computer has converged on on: the
// convergence applied it and the pause matches.
func awaitAssistants(t *testing.T, a *App, on bool) {
	t.Helper()
	waitUntil(t, 20*time.Second, func() bool {
		a.assistantsSwitch.mu.Lock()
		defer a.assistantsSwitch.mu.Unlock()
		return a.assistantsSwitch.paused == !on && a.assistantsPaused() == !on
	})
}

// agentWake reads one loop or watch row.
func agentWake(t *testing.T, a *App, id string) store.AgentWake {
	t.Helper()
	row, found, err := a.store.GetAgentWake(id)
	if err != nil || !found {
		t.Fatalf("GetAgentWake(%s) = %v, %v", id, found, err)
	}
	return row
}

// agentWakeRunning reports whether the supervisor runs anything of the row.
func agentWakeRunning(a *App, id string) bool { return a.agentWakeRegistered(id) }

// Off with a running turn, a request open on the assistant, a watch script
// mid-run, a loop and a reminder come due: the turn is cut and the session
// ends, the request settles saying assistants were turned off, the script
// stops with its row armed again, and nothing fires or starts while it lasts.
// On again: the loop's missed fires replay once with their count, the
// reminder is delivered, and the watch runs again. No row was cancelled.
func TestAssistantsOffPausesTheirWorkAndOnResumesIt(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working")
	target, _ := f.assistantTarget(t, "Ops.")
	a := f.app
	// The fixture's settings service is built without the app's observer.
	a.registerSettingsBroadcast()
	now := time.Now()
	// Due since before the pause, and never served while assistants ran:
	// two whole intervals and a half have passed, so its fire stands in for
	// two missed ones.
	loop := registerTestWake(t, a, store.AgentWake{ThreadID: target.ID, Kind: store.AgentWakeLoop, Label: "board", Note: "check the board", IntervalSeconds: 3600, NextAt: now.Add(-150 * time.Minute).UnixMilli()})
	watch := registerTestWake(t, a, store.AgentWake{ThreadID: target.ID, Kind: store.AgentWakeWatch, Label: "poll", Command: "exec sleep 60", IntervalSeconds: 3600, TimeoutSeconds: 600, NextAt: now.Add(time.Hour).UnixMilli()})
	insertTestReminder(t, a, "assistants-off-remind", target.ID, "look at the release", now.Add(-time.Minute).UnixMilli())
	t.Cleanup(func() { a.stopAgentWakeRunsForThread(target.ID) })

	ack, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{ThreadID: target.ID, Message: "look"})
	if err != nil {
		t.Fatalf("send to the assistant: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for f.receipt(t, ack.Token).State != store.ThreadReceiptRunning {
		if time.Now().After(deadline) {
			items, _ := a.store.ListItems(target.ID)
			for _, item := range items {
				t.Logf("item %s %s", item.Kind, item.Summary)
			}
			_, live := a.sessionManager().get(target.ID)
			t.Fatalf("receipt = %+v live=%v queue=%+v", f.receipt(t, ack.Token), live, durableQueueRows(t, a, target.ID))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := a.fireAgentWake(watch, time.Now(), agentWakeFire{at: time.Now().UnixMilli()}); err != nil {
		t.Fatal("the watch did not start")
	}
	for deadline := time.Now().Add(10 * time.Second); len(a.agentWatchDirs(target.ID)) != 1; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the watch script is not running: %+v", agentWake(t, a, watch.ID))
		}
	}

	setAssistants(t, a, false)

	if _, live := a.sessionManager().get(target.ID); live {
		t.Fatal("the assistant's session is still live")
	}
	request := f.request(t, ack.Token)
	if words := requestAnswerWords(request, request.Answer); request.SettledAt == 0 || !strings.Contains(words, "Assistants were turned off") {
		t.Fatalf("the open request = state %s answer %q; want it settled saying assistants were turned off", request.State, words)
	}
	if row := agentWake(t, a, watch.ID); row.State != store.AgentWakeArmed || row.LastOutcome != agentWakeOutcomePaused || agentWakeRunning(a, watch.ID) {
		t.Fatalf("the watch after the pause = %+v running=%v; want it armed with the pause's outcome and nothing running", row, agentWakeRunning(a, watch.ID))
	}
	if len(a.agentWatchDirs(target.ID)) != 0 {
		t.Fatal("the watch script still runs")
	}

	a.fireDueAgentWakes(time.Now())
	a.fireDueThreadReminders(time.Now())
	if row := agentWake(t, a, loop.ID); row.State != store.AgentWakeArmed || row.LastRunAt != 0 {
		t.Fatalf("the loop fired while assistants were off: %+v", row)
	}
	if row := agentWake(t, a, watch.ID); row.State != store.AgentWakeArmed || agentWakeRunning(a, watch.ID) {
		t.Fatalf("the watch ran while assistants were off: %+v", row)
	}
	if reminder := f.request(t, "assistants-off-remind"); reminder.State != store.ThreadRequestAccepted {
		t.Fatalf("the reminder fired while assistants were off: %s", reminder.State)
	}
	if err := a.startSession(t.Context(), target.ID); !errors.Is(err, errAssistantsOff) {
		t.Fatalf("a session start while assistants are off = %v, want it refused", err)
	}
	if _, err := a.admitWork(target.ID, ""); !errors.Is(err, errAssistantsOff) {
		t.Fatalf("work the assistant starts while assistants are off = %v, want it refused", err)
	}

	// The session the next start resumes, as the real CLI leaves it.
	paused, err := a.store.GetThread(target.ID)
	if err != nil || paused.SessionRef == "" {
		t.Fatalf("the paused thread's session = %q, %v", paused.SessionRef, err)
	}
	transcripts := filepath.Join(testProviderProjectsDir(t, a), "paused")
	if err := os.MkdirAll(transcripts, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(transcripts, paused.SessionRef+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sent := f.mockClaudeRecording(t, "ok")
	setAssistants(t, a, true)

	a.fireDueAgentWakes(time.Now())
	a.fireDueThreadReminders(time.Now())
	// The provider may take the two in one message.
	awaitSent(t, sent, func(text string) bool {
		return strings.Contains(text, "check the board") && strings.Contains(text, "missed 2 since the previous wake")
	})
	awaitSent(t, sent, func(text string) bool { return strings.Contains(text, "look at the release") })
	waitUntil(t, 10*time.Second, func() bool { return agentWakeRunning(a, watch.ID) })
	for _, id := range []string{loop.ID, watch.ID} {
		if row := agentWake(t, a, id); row.State == store.AgentWakeCancelled {
			t.Fatalf("%s was cancelled: %+v", id, row)
		}
	}
}
