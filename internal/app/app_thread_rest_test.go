package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

// stopRowLabels are the stop rows in a thread's timeline.
func stopRowLabels(t *testing.T, a *App, threadID string) []string {
	t.Helper()
	items, err := a.store.ListItems(threadID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var labels []string
	for _, item := range items {
		if item.Kind == triage.ItemKindError {
			labels = append(labels, item.Summary)
		}
	}
	return labels
}

// seedRunningBackgroundTask writes a background launch the thread's open
// turn is running, as the provider's Backgrounded tool call leaves it.
func seedRunningBackgroundTask(t *testing.T, a *App, threadID, id string) {
	t.Helper()
	turn := -1
	waitUntil(t, 10*time.Second, func() bool {
		turn = a.triage.OpenTurnIndex(threadID)
		return turn >= 0
	})
	now := time.Now().UnixMilli()
	if _, err := a.store.AppendItem(store.Item{
		ID: id, ThreadID: threadID, TurnIndex: turn, Kind: "tool_call", Role: "assistant",
		Status: "running", IsBackground: true, Summary: "Bash: a dev server", ToolName: "Bash",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed the background task: %v", err)
	}
}

// A person's Stop on a spawn's row brings the spawned thread to rest, in
// every placement: its session ends with its background work, its cut turn
// says the user stopped it, its loops are cancelled saying so and its
// reminders dropped. It stays where it was, and a later message starts it
// again and is answered.
func TestAStopBringsASpawnedThreadToRest(t *testing.T) {
	t.Parallel()
	for _, placement := range []string{store.PlacementOwned, store.PlacementStandalone} {
		t.Run(placement, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			f.mockClaudeHoldingTheTurn(t, "working")
			ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
				Prompt: "work on it", Notify: true, Placement: placement,
			})
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			f.awaitReceiptRunning(t, ack.Token)
			target := ack.ThreadID
			later := time.Now().Add(time.Hour).UnixMilli()
			loop := registerTestWake(t, f.app, store.AgentWake{ThreadID: target, Kind: store.AgentWakeLoop, Label: "board", Note: "n", IntervalSeconds: 3600, NextAt: later})
			reminder, err := f.adapter().Remind(t.Context(), f.targetIdentity(t, target), threadtools.RemindCall{DueAtUnixMs: later, Note: "check the build"})
			if err != nil {
				t.Fatalf("Remind: %v", err)
			}
			seedRunningBackgroundTask(t, f.app, target, "bg-rest-1")
			before, err := f.app.store.GetThread(target)
			if err != nil {
				t.Fatalf("GetThread: %v", err)
			}

			stop, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, ack.Token)
			if err != nil || stop.Outcome != threadAgentStopped || len(stop.Failures) != 0 {
				t.Fatalf("StopThreadAgentRow = %+v, %v", stop, err)
			}
			if _, live := f.app.sessionManager().get(target); live {
				t.Fatal("the stopped thread still has a session")
			}
			if labels := stopRowLabels(t, f.app, target); !slices.Contains(labels, triage.StoppedByUser) {
				t.Fatalf("the cut turn's rows = %q, want the user's stop", labels)
			}
			if running, err := f.app.runningBackgroundTaskCount(target); err != nil || running != 0 {
				t.Fatalf("background work after the stop = %d, %v; want none running", running, err)
			}
			row, found, err := f.app.store.GetAgentWake(loop.ID)
			if err != nil || !found || row.State != store.AgentWakeCancelled || row.LastOutcome != "cancelled: the user stopped the thread" {
				t.Fatalf("the loop = %+v found %v, %v; want it cancelled by the user's stop", row, found, err)
			}
			if _, found, err := f.app.store.GetThreadRequest(reminder.Token); err != nil || found {
				t.Fatalf("the reminder is still there: found %v, %v", found, err)
			}
			if spawn := f.request(t, ack.Token); spawn.State != store.ThreadRequestCancelled || !spawn.Notify {
				t.Fatalf("the pressed spawn = %s notify %v, want cancelled with its wake armed", spawn.State, spawn.Notify)
			}
			after, err := f.app.store.GetThread(target)
			if err != nil {
				t.Fatalf("GetThread: %v", err)
			}
			if after.Archived || after.Placement != before.Placement || after.GroupID != before.GroupID || after.SessionRef != before.SessionRef {
				t.Fatalf("the stopped thread moved: before %+v, after %+v", before, after)
			}

			// Nothing brings it back on its own.
			time.Sleep(300 * time.Millisecond)
			if _, live := f.app.sessionManager().get(target); live {
				t.Fatal("the stopped thread started again with nobody sending to it")
			}
			// A deliberate message does, and is answered.
			installMockClaudeReplies(t, f.app, "back on it")
			sent, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{ThreadID: target, Message: "one more thing", Notify: true})
			if err != nil {
				t.Fatalf("Send after the stop: %v", err)
			}
			answered := f.awaitRequestState(t, sent.Token, store.ThreadRequestFinished, store.ThreadRequestReplied)
			if !strings.Contains(string(answered.Answer), "back on it") {
				t.Fatalf("the send after the stop answered %q", answered.Answer)
			}
		})
	}
}

// awaitReceiptRunning waits for a request's turn to be running.
func (f *requestFixture) awaitReceiptRunning(t *testing.T, token string) {
	t.Helper()
	waitUntil(t, 10*time.Second, func() bool {
		row, found, err := f.app.store.GetThreadRequestReceipt(token)
		return err == nil && found && row.State == store.ThreadReceiptRunning
	})
}

// Every open request on the stopped thread settles as stopped by the user,
// whoever made it, and none is queued again: no sender waits, and none
// starts the thread again.
func TestAStopSettlesEveryRequestOnTheThread(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working")
	child := f.runningRequest(t, "lead the fix")
	stranger := f.plainThread(t)
	theirs, err := f.adapter().Send(t.Context(), f.targetIdentity(t, stranger.ID), threadtools.SendCall{ThreadID: child.ThreadID, Message: "and mine", Notify: true})
	if err != nil {
		t.Fatalf("the stranger's send: %v", err)
	}
	mine, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{ThreadID: child.ThreadID, Message: "and this", Notify: true})
	if err != nil {
		t.Fatalf("the caller's send: %v", err)
	}

	if _, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, child.Token); err != nil {
		t.Fatalf("StopThreadAgentRow: %v", err)
	}
	for _, token := range []string{theirs.Token, mine.Token} {
		row := f.awaitRequestState(t, token, store.ThreadRequestCancelled)
		if string(row.Answer) != threadRequestUserStop || !row.Notify {
			t.Fatalf("request %s = %q notify %v, want stopped by the user with its sender told", token, row.Answer, row.Notify)
		}
	}
	receipts, err := f.app.store.ListThreadRequestReceiptsForThread(child.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	for _, receipt := range receipts {
		if slices.Contains(store.ThreadReceiptOpenStates(), receipt.State) {
			t.Fatalf("receipt %s is still %s", receipt.Token, receipt.State)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if durable, memory := queuedSendIDs(t, f.app, child.ThreadID); len(durable)+len(memory) != 0 {
		t.Fatalf("the stopped thread's queue = %q / %q, want it empty", durable, memory)
	}
	if _, live := f.app.sessionManager().get(child.ThreadID); live {
		t.Fatal("a settled request started the stopped thread again")
	}
}

// A thread with no session is brought to rest too: the messages the app
// queued for it are taken back, its loops cancelled and its reminders
// dropped, and nothing starts it. The Stop of its finished spawn, whose run
// ended, leaves the message someone else queued there; the Stop of its
// spawner's later send, whose run it opens, rests the thread. A second Stop
// finds it at rest.
func TestAStopRestsAThreadWithNoSession(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaude(t, "done")
	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{Prompt: "a small job", Notify: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	f.awaitRequestState(t, ack.Token, store.ThreadRequestFinished, store.ThreadRequestReplied)
	target := ack.ThreadID
	if err := f.app.StopSession(target); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	var mu sync.Mutex
	starts := 0
	f.app.startSessionFn = func(threadID string) error {
		if threadID == target {
			mu.Lock()
			starts++
			mu.Unlock()
		}
		return errors.New("no provider in this test")
	}
	later := time.Now().Add(time.Hour).UnixMilli()
	loop := registerTestWake(t, f.app, store.AgentWake{ThreadID: target, Kind: store.AgentWakeLoop, Label: "board", Note: "n", IntervalSeconds: 3600, NextAt: later})
	reminder, err := f.adapter().Remind(t.Context(), f.targetIdentity(t, target), threadtools.RemindCall{DueAtUnixMs: later, Note: "check"})
	if err != nil {
		t.Fatalf("Remind: %v", err)
	}
	if _, err := f.app.SendExternalThreadMessage(context.Background(), target, "a script says hello"); err != nil {
		t.Fatalf("SendExternalThreadMessage: %v", err)
	}
	f.app.waitAgentNoticeWork()
	if durable, _ := queuedSendIDs(t, f.app, target); len(durable) == 0 {
		t.Fatal("the outside send is not queued; the test proves nothing")
	}
	f.awaitRunEnded(t, ack.Token)
	ended, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, ack.Token)
	if err != nil || ended.Outcome != threadAgentEnded || len(ended.Failures) != 0 {
		t.Fatalf("the spawn's Stop = %+v, %v; want its ended run reported ended", ended, err)
	}
	if durable, _ := queuedSendIDs(t, f.app, target); len(durable) == 0 {
		t.Fatal("the spawn's Stop took back a message its run did not send")
	}
	send, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{ThreadID: target, Message: "one more thing", Notify: true})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if row := f.request(t, send.Token); !row.RunOpen() {
		t.Fatalf("the spawner's send %+v opens no run", row)
	}
	f.app.waitAgentNoticeWork()
	mu.Lock()
	startsBefore := starts
	mu.Unlock()

	stop, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, send.Token)
	if err != nil || stop.Outcome != threadAgentStopped || len(stop.Failures) != 0 {
		t.Fatalf("StopThreadAgentRow = %+v, %v; want the send's thread with queued messages stopped", stop, err)
	}
	f.app.waitAgentNoticeWork()
	if durable, memory := queuedSendIDs(t, f.app, target); len(durable)+len(memory) != 0 {
		t.Fatalf("the stopped thread's queue = %q / %q, want it empty", durable, memory)
	}
	if row, _, err := f.app.store.GetAgentWake(loop.ID); err != nil || row.State != store.AgentWakeCancelled {
		t.Fatalf("the loop = %+v, %v; want it cancelled", row, err)
	}
	if _, found, err := f.app.store.GetThreadRequest(reminder.Token); err != nil || found {
		t.Fatalf("the reminder is still there: found %v, %v", found, err)
	}
	mu.Lock()
	if starts != startsBefore {
		t.Errorf("the stop started the thread %d times", starts-startsBefore)
	}
	mu.Unlock()

	again, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, send.Token)
	if err != nil || again.Outcome != threadAgentEnded || len(again.Failures) != 0 {
		t.Fatalf("a second Stop = %+v, %v", again, err)
	}
	if got := workerStoppedMessages(t, f.app, f.caller.ID, ack.Token); got != 0 {
		t.Fatalf("the spawner was told %d times of its spawn's worker, want never: its run had ended", got)
	}
}

// The Stop reaches down the spawn links: a thread the stopped one spawned is
// brought to rest with its spawn's wake disarmed, and a thread it only sent
// to has that request cancelled and keeps its session.
func TestAStopRestsWhatTheThreadSpawnedAndOnlyCancelsWhatItSent(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working")
	child := f.runningRequest(t, "lead the fix")
	grand, err := f.adapter().Spawn(t.Context(), f.targetIdentity(t, child.ThreadID), threadtools.SpawnCall{Prompt: "write the test", Notify: true})
	if err != nil {
		t.Fatalf("nested Spawn: %v", err)
	}
	f.awaitReceiptRunning(t, grand.Token)
	other := f.plainThread(t)
	if err := f.app.StartSession(other.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	sent, err := f.adapter().Send(t.Context(), f.targetIdentity(t, child.ThreadID), threadtools.SendCall{ThreadID: other.ID, Message: "look at this", Notify: true})
	if err != nil {
		t.Fatalf("the child's send: %v", err)
	}
	f.awaitReceiptRunning(t, sent.Token)

	stop, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, child.Token)
	if err != nil || stop.Outcome != threadAgentStopped || stop.Nested != 2 || len(stop.Failures) != 0 {
		t.Fatalf("StopThreadAgentRow = %+v, %v; want the spawn and the send below it stopped", stop, err)
	}
	for _, id := range []string{child.ThreadID, grand.ThreadID} {
		if _, live := f.app.sessionManager().get(id); live {
			t.Fatalf("%s still has a session", id)
		}
	}
	for _, token := range []string{grand.Token, sent.Token} {
		if row := f.awaitRequestState(t, token, store.ThreadRequestCancelled); row.Notify {
			t.Fatalf("request %s kept its wake armed: the stopped child would be started for it", token)
		}
	}
	if _, live := f.app.sessionManager().get(other.ID); !live {
		t.Fatal("the thread the child only sent to lost its session")
	}
	_ = f.app.StopSession(other.ID)
}
