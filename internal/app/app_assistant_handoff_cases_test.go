package app

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil/mockexec"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
)

// mockClaudeHoldingTheTurnRecording is mockClaudeHoldingTheTurn that logs
// every line each session is written, so a test can see a message the
// provider took off the queue.
func (f *requestFixture) mockClaudeHoldingTheTurnRecording(t *testing.T, reply string) (stdinLog string) {
	t.Helper()
	stdinLog = filepath.Join(t.TempDir(), "stdin.log")
	answer := []string{
		mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"msg-hold","role":"assistant","content":[{"type":"text","text":` + quoteJSON(reply) + `}]}}`,
	}
	var script strings.Builder
	script.WriteString("#!/bin/sh\nset -u\nwhile IFS= read -r line; do\n")
	script.WriteString("  printf '%s\\n' \"$line\" >> " + shellQuoteForTest(stdinLog) + "\n")
	script.WriteString("  case \"$line\" in\n    *'\"type\":\"user\"'*)\n")
	for _, wire := range answer {
		script.WriteString("      printf '%s\\n' " + shellQuoteForTest(wire) + "\n")
	}
	script.WriteString("      ;;\n  esac\ndone\n")
	binary := filepath.Join(t.TempDir(), "claude-holding.sh")
	mockexec.Write(t, binary, script.String())
	if _, err := f.app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set claude binary: %v", err)
	}
	return stdinLog
}

// awaitQueuedOrSent waits for a message on threadID: a row of its durable
// queue with sendID, or, once a session took it off the queue, a message the
// provider was written that contains needle, past the first sentBefore such
// messages. It returns the message.
func awaitQueuedOrSent(t *testing.T, a *App, threadID, sendID, stdinLog, needle string, sentBefore int) string {
	t.Helper()
	var found string
	waitUntil(t, 20*time.Second, func() bool {
		for _, row := range durableQueueRows(t, a, threadID) {
			if row.SendID == sendID {
				found = row.Message
				return true
			}
		}
		sent := sentContaining(t, stdinLog, needle)
		if len(sent) > sentBefore {
			found = sent[len(sent)-1]
			return true
		}
		return false
	})
	return found
}

// sentContaining is what the provider was written that contains needle.
func sentContaining(t *testing.T, stdinLog, needle string) []string {
	t.Helper()
	var out []string
	for _, envelope := range recordedUserEnvelopes(t, stdinLog) {
		if text, _ := envelopeText(t, envelope); strings.Contains(text, needle) {
			out = append(out, text)
		}
	}
	return out
}

// A request an assistant's thread had read and not
// answered settles interrupted naming the old thread, and the successor's
// opening block lists its token and sender.
func TestHandOffInterruptsAReadRequest(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	stdinLog := f.mockClaudeHoldingTheTurnRecording(t, "reading it")
	target, _ := f.assistantTarget(t, "Reviewer.")
	ack, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{ThreadID: target.ID, Message: "review this", Notify: true})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitUntil(t, 20*time.Second, func() bool {
		row, found, err := f.app.store.GetThreadRequestReceipt(ack.Token)
		return err == nil && found && row.State == store.ThreadReceiptRunning && row.TurnID != ""
	})
	successor, err := f.app.StartNextAssistantThread(t.Context(), target.ID)
	if err != nil {
		t.Fatalf("StartNextAssistantThread: %v", err)
	}
	receipt := f.receipt(t, ack.Token)
	if receipt.State != store.ThreadReceiptInterrupted || !strings.Contains(string(receipt.Answer), target.ID) {
		t.Fatalf("receipt = %s %q, want interrupted naming %s", receipt.State, receipt.Answer, target.ID)
	}
	opening := awaitQueuedOrSent(t, f.app, successor.ID, assistantSendID(assistantWakeKindHandOff, target.ID), stdinLog, "replaced your previous thread "+target.ID, 0)
	if !strings.Contains(opening, "token "+ack.Token) || !strings.Contains(opening, f.caller.ID) {
		t.Fatalf("handoff block does not list the interrupted request:\n%s", opening)
	}
}

// A worker that answers after its assistant moved wakes the
// current thread, and the wake names the old thread's id.
func TestLateWorkerAnswerReachesTheSuccessor(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	stdinLog := f.mockClaudeHoldingTheTurnRecording(t, "working")
	f.assistantCaller(t)
	ack := f.runningRequest(t, "build it")
	successor, err := f.app.StartNextAssistantThread(t.Context(), f.caller.ID)
	if err != nil {
		t.Fatalf("StartNextAssistantThread: %v", err)
	}
	if _, err := f.adapter().Reply(t.Context(), f.targetIdentity(t, ack.ThreadID), threadtools.ReplyCall{Token: ack.Token, Text: "built"}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	wake := awaitQueuedOrSent(t, f.app, successor.ID, threadWakeSendID(ack.Token), stdinLog, "[token "+ack.Token+"]", 0)
	for _, row := range durableQueueRows(t, f.app, f.caller.ID) {
		if row.SendID == threadWakeSendID(ack.Token) {
			t.Fatal("the answer was queued on the superseded thread")
		}
	}
	if !strings.Contains(wake, threadtools.WakeMadeIn(f.caller.ID, successor.ID)) {
		t.Errorf("the wake on the successor does not name the thread that made the request (%s):\n%s", f.caller.ID, wake)
	}
}

// An inbound request the old thread never took in (handed
// to a session busy with another turn, never echoed) moves to the
// successor's queue and its receipt follows; it is not interrupted.
func TestHandOffMovesAnUnreadRequest(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	stdinLog := f.mockClaudeHoldingTheTurnRecording(t, "busy")
	target, _ := f.assistantTarget(t, "Reviewer.")
	if err := f.app.SendMessage(target.ID, "work on something", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	waitUntil(t, 20*time.Second, func() bool {
		_, live := f.app.sessionManager().get(target.ID)
		return live
	})
	ack, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{ThreadID: target.ID, Message: "unread please", Notify: true})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	awaitQueuedOrSent(t, f.app, target.ID, threadRequestSendID(ack.Token), stdinLog, "unread please", 0)
	sentBefore := len(sentContaining(t, stdinLog, "unread please"))
	successor, err := f.app.StartNextAssistantThread(t.Context(), target.ID)
	if err != nil {
		t.Fatalf("StartNextAssistantThread: %v", err)
	}
	awaitQueuedOrSent(t, f.app, successor.ID, threadRequestSendID(ack.Token), stdinLog, "unread please", sentBefore)
	receipt := f.receipt(t, ack.Token)
	if receipt.TargetThreadID != successor.ID || receipt.State == store.ThreadReceiptInterrupted {
		t.Fatalf("receipt = %s on %s, want it live on %s", receipt.State, receipt.TargetThreadID, successor.ID)
	}
	if row := f.request(t, ack.Token); row.TargetThreadID != successor.ID {
		t.Fatalf("request target = %s, want %s", row.TargetThreadID, successor.ID)
	}
}

// Two handoffs of one thread at once leave one successor
// and no stray thread titled with the name.
func TestConcurrentHandOffsLeaveOneSuccessor(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	f.mockClaude(t, "ok", "ok", "ok")
	f.assistantCaller(t)
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = f.app.StartNextAssistantThread(t.Context(), f.caller.ID)
		}()
	}
	wg.Wait()
	if (results[0] == nil) == (results[1] == nil) {
		t.Fatalf("two concurrent handoffs = %v, %v; want exactly one to succeed", results[0], results[1])
	}
	threads, err := f.app.store.ListThreads()
	if err != nil {
		t.Fatal(err)
	}
	var named []string
	for _, thread := range threads {
		if thread.Title == f.caller.Title && !thread.Archived {
			named = append(named, thread.ID)
		}
	}
	if len(named) != 1 {
		t.Fatalf("unarchived threads titled with the name = %v, want one", named)
	}
	// And a second handoff in a row moves it again.
	next, err := f.app.StartNextAssistantThread(t.Context(), named[0])
	if err != nil {
		t.Fatalf("second handoff in a row: %v", err)
	}
	if current, found, err := f.app.store.AssistantCurrentThreadOf(f.caller.ID); err != nil || !found || current != next.ID {
		t.Fatalf("first thread maps to %s %v %v, want %s", current, found, err, next.ID)
	}
}

// A handoff while a turn runs ends that session and the
// successor starts.
func TestHandOffWhileATurnRuns(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	f.mockClaudeHoldingTheTurn(t, "busy")
	f.assistantCaller(t)
	if err := f.app.SendMessage(f.caller.ID, "start working", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	waitUntil(t, 20*time.Second, func() bool {
		_, live := f.app.sessionManager().get(f.caller.ID)
		return live
	})
	successor, err := f.app.StartNextAssistantThread(t.Context(), f.caller.ID)
	if err != nil {
		t.Fatalf("StartNextAssistantThread: %v", err)
	}
	if _, live := f.app.sessionManager().get(f.caller.ID); live {
		t.Fatal("the old thread's session survived the handoff")
	}
	waitUntil(t, 20*time.Second, func() bool {
		_, live := f.app.sessionManager().get(successor.ID)
		return live
	})
}

// A loop's wake the old session was handed and had not taken in when the
// handoff ended it goes on to the successor, as such a request does, and
// the row records no wake that failed to reach the assistant.
func TestHandOffMovesALoopWakeTheOldSessionHadNotTakenIn(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	stdinLog := f.mockClaudeHoldingTheTurnRecording(t, "busy")
	target, _ := f.assistantTarget(t, "Reviewer.")
	if err := f.app.SendMessage(target.ID, "work on something", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	waitUntil(t, 20*time.Second, func() bool {
		_, live := f.app.sessionManager().get(target.ID)
		return live
	})
	loop := registerTestWake(t, f.app, store.AgentWake{ThreadID: target.ID, Kind: store.AgentWakeLoop, Label: "board", Note: "check", IntervalSeconds: 3600, NextAt: time.Now().Add(time.Hour).UnixMilli()})
	if err := f.app.deliverAgentWake(t.Context(), loop, 7, "board fired"); err != nil {
		t.Fatalf("deliverAgentWake: %v", err)
	}
	waitUntil(t, 20*time.Second, func() bool { return len(sentContaining(t, stdinLog, "board fired")) == 1 })

	successor, err := f.app.StartNextAssistantThread(t.Context(), target.ID)
	if err != nil {
		t.Fatalf("StartNextAssistantThread: %v", err)
	}
	awaitQueuedOrSent(t, f.app, successor.ID, agentWakeSendID(loop.ID, 7), stdinLog, "board fired", 1)
	if after, _, err := f.app.store.GetAgentWake(loop.ID); err != nil || after.Undelivered != "" {
		t.Fatalf("loop after the handoff = %+v %v, want no undelivered wake", after, err)
	}
}

// A watch's wake still queued when a handoff's teardown drops the old
// thread's queue goes on to the successor, and the drop does not record it
// as cleared.
func TestHandOffTeardownTakesBackAQueuedWake(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	stdinLog := f.mockClaudeRecording(t, "ok")
	target, _ := f.assistantTarget(t, "Reviewer.")
	watch := registerTestWake(t, f.app, store.AgentWake{ThreadID: target.ID, Kind: store.AgentWakeWatch, Label: "inbox", Command: "true", IntervalSeconds: 3600, NextAt: time.Now().Add(time.Hour).UnixMilli()})
	successor, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	sendID := agentWakeSendID(watch.ID, 7)
	if err := f.app.store.QueueAgentWake(store.FlushQueueItem{ID: "row-1", ThreadID: target.ID, SendID: sendID, Message: "inbox fired", EnqueuedAt: 1}, agentWakeSendIDPrefix(watch.ID), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.store.HandOffAssistant(target.ID, successor.ID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}

	held, owed := f.app.dropQueueForTeardown(target.ID, false)
	f.app.giveBackToOwners(target.ID, held)
	f.app.giveBackTeardownRequests(target.ID, owed)
	awaitQueuedOrSent(t, f.app, successor.ID, sendID, stdinLog, "inbox fired", 0)
	if after, _, err := f.app.store.GetAgentWake(watch.ID); err != nil || after.Undelivered != "" {
		t.Fatalf("watch after the handoff = %+v %v, want no undelivered wake", after, err)
	}
}

// A handoff leaves remote watches where they are, so a job the old thread
// started reports to the assistant's current thread through the notice's
// own redirect (queueAgentNotice).
func TestARemoteCompletionOfAPastThreadReachesTheCurrentThread(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	a.startSessionFn = func(string) error { return nil }
	old := remoteWatchThread(t, a, string(provider.Claude))
	testAssistant(t, a, old.ID)
	w := completedRemoteWatch(t, a, old)
	successor := remoteWatchThread(t, a, string(provider.Claude))
	if _, err := a.store.HandOffAssistant(old.ID, successor.ID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}

	if err := a.deliverRemoteCompletion(w, remoteCompletionOutput{Tail: w.Receipt.Output}); err != nil {
		t.Fatalf("deliverRemoteCompletion: %v", err)
	}
	if rows := durableQueueRows(t, a, successor.ID); len(rows) != 1 || rows[0].SendID != remoteCompletionSendID(w) {
		t.Fatalf("current thread's queue = %+v, want the completion", rows)
	}
	if rows := durableQueueRows(t, a, old.ID); len(rows) != 0 {
		t.Fatalf("past thread's queue = %+v, want nothing", rows)
	}
}
