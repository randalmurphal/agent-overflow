package app

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

// Agent-to-agent work raises no unread dot (docs/specs/standing-agents.md
// ruling 30): a spawned thread whose turn answers its spawner is marked read
// through that turn, announced on a row frame; a pinned child keeps its dot,
// and an answer to a thread that did not spawn it marks nothing.

func (f *requestFixture) awaitThread(t *testing.T, id, what string, ok func(store.Thread) bool) store.Thread {
	t.Helper()
	var row store.Thread
	waitFor(t, what, func() bool {
		var err error
		row, err = f.app.store.GetThread(id)
		return err == nil && ok(row)
	})
	return row
}

// recordReadFrames keeps every thread:updated row frame. Installed before
// the fixture does anything, so no emit races the hook's write.
func recordReadFrames(f *requestFixture) func(threadID string, at int64) bool {
	var mu sync.Mutex
	var frames []triage.ThreadUpdateEvent
	f.app.testEmitHook = func(name string, data any) {
		frame, ok := data.(triage.ThreadUpdateEvent)
		if name != string(eventchan.ThreadUpdated) || !ok || frame.Action != triage.ThreadActionFull || frame.Thread == nil {
			return
		}
		mu.Lock()
		frames = append(frames, frame)
		mu.Unlock()
	}
	// Whether a row frame carried threadID marked read through at least at.
	return func(threadID string, at int64) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, frame := range frames {
			if frame.Thread.ID == threadID && frame.Thread.LastReadAt != nil && *frame.Thread.LastReadAt >= at {
				return true
			}
		}
		return false
	}
}

func TestSpawnedThreadAnsweringItsSpawnerIsMarkedRead(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	readFrame := recordReadFrames(f)
	f.mockClaude(t, "done with the first part", "done with the second part")

	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		Prompt: "do the first part", Title: "Worker", WaitSeconds: 20,
	})
	if err != nil || ack.State != store.ThreadRequestFinished {
		t.Fatalf("Spawn = %+v, %v; want a finished request", ack, err)
	}
	row := f.awaitThread(t, ack.ThreadID, "the worker marked read", func(row store.Thread) bool {
		return row.LatestTurnCompletedAt != nil && !threadUnread(row)
	})
	if *row.LastReadAt != *row.LatestTurnCompletedAt {
		t.Errorf("LastReadAt = %d, want the answering turn's completion %d", *row.LastReadAt, *row.LatestTurnCompletedAt)
	}
	waitFor(t, "the read row frame", func() bool { return readFrame(ack.ThreadID, *row.LatestTurnCompletedAt) })

	// Pinned, the next answer leaves its dot.
	if _, _, err := f.app.store.PinThread(ack.ThreadID); err != nil {
		t.Fatalf("PinThread: %v", err)
	}
	send, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{
		ThreadID: ack.ThreadID, Message: "do the second part", WaitSeconds: 20,
	})
	if err != nil || send.State != store.ThreadRequestFinished {
		t.Fatalf("Send = %+v, %v; want a finished request", send, err)
	}
	f.awaitThread(t, ack.ThreadID, "the second turn complete", func(next store.Thread) bool {
		return next.LatestTurnCompletedAt != nil && *next.LatestTurnCompletedAt > *row.LatestTurnCompletedAt
	})
	// The mark runs after the settlement; give it the time it takes.
	time.Sleep(200 * time.Millisecond)
	pinned, err := f.app.store.GetThread(ack.ThreadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if !threadUnread(pinned) {
		t.Fatalf("pinned worker marked read (LastReadAt %v, completed %v); a pin keeps its dot", *pinned.LastReadAt, *pinned.LatestTurnCompletedAt)
	}
}

func TestAnswerToAThreadThatDidNotSpawnItLeavesTheDot(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaude(t, "spawned and answered", "answered someone else")

	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		Prompt: "start", Title: "Worker", WaitSeconds: 20,
	})
	if err != nil || ack.State != store.ThreadRequestFinished {
		t.Fatalf("Spawn = %+v, %v; want a finished request", ack, err)
	}
	first := f.awaitThread(t, ack.ThreadID, "the worker marked read", func(row store.Thread) bool {
		return row.LatestTurnCompletedAt != nil && !threadUnread(row)
	})

	other, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create other thread: %v", err)
	}
	stranger := threadtools.Caller{ThreadID: other.ID, Title: other.Title, ComputerID: "local-computer", ComputerName: "This Mac"}
	send, err := f.adapter().Send(t.Context(), stranger, threadtools.SendCall{
		ThreadID: ack.ThreadID, Message: "a question from elsewhere", WaitSeconds: 20,
	})
	if err != nil || send.State != store.ThreadRequestFinished {
		t.Fatalf("Send = %+v, %v; want a finished request", send, err)
	}
	f.awaitThread(t, ack.ThreadID, "the second turn complete", func(next store.Thread) bool {
		return next.LatestTurnCompletedAt != nil && *next.LatestTurnCompletedAt > *first.LatestTurnCompletedAt
	})
	time.Sleep(200 * time.Millisecond)
	row, err := f.app.store.GetThread(ack.ThreadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if !threadUnread(row) {
		t.Fatal("an answer to a thread that did not spawn the worker marked it read")
	}
}

// A thread_reply settles while its turn runs. The mark waits for that turn's
// end: stamped at the reply, the turn's completion would pass it and the dot
// would come back.
func TestReplyMidTurnMarksTheWorkerReadWhenTheTurnEnds(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	gate := filepath.Join(t.TempDir(), "release")
	installMockClaudeTurns(t, f.app, [][]string{{
		mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"working"}]}}`,
		testutil.MockWaitForFile(gate),
		`{"type":"result","subtype":"success","is_error":false}`,
	}})

	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		Prompt: "answer early", Title: "Worker", WaitSeconds: 0, Notify: true,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitFor(t, "the receipt bound to its turn", func() bool {
		receipt := f.receipt(t, ack.Token)
		return receipt.State == store.ThreadReceiptRunning && receipt.TurnID != ""
	})
	worker := threadtools.Caller{ThreadID: ack.ThreadID, Title: "Worker", ComputerID: "local-computer", ComputerName: "This Mac"}
	if _, err := f.adapter().Reply(t.Context(), worker, threadtools.ReplyCall{Token: ack.Token, Text: "the answer"}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	f.awaitRequestState(t, ack.Token, store.ThreadRequestReplied)
	if !f.app.hasAnsweredReads(ack.ThreadID) {
		t.Fatal("the reply settled mid-turn and holds no read mark for the turn's end")
	}

	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatalf("release the turn: %v", err)
	}
	row := f.awaitThread(t, ack.ThreadID, "the worker marked read at its turn's end", func(row store.Thread) bool {
		return row.LatestTurnCompletedAt != nil && !threadUnread(row)
	})
	if *row.LastReadAt != *row.LatestTurnCompletedAt {
		t.Errorf("LastReadAt = %d, want the turn's completion %d", *row.LastReadAt, *row.LatestTurnCompletedAt)
	}
	waitFor(t, "the hold released", func() bool { return !f.app.hasAnsweredReads(ack.ThreadID) })
}

// An assistant pins a thread it spawned with thread_update, and the pin then
// keeps the worker's dot through its next answer
// (docs/specs/standing-agents.md rulings 30 and 32: a pinned child keeps its
// dot and stays above the bound).
func TestAssistantPinsItsSpawnedThreadAndThePinKeepsTheDot(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.assistantCaller(t)
	f.mockClaude(t, "first answer", "second answer")

	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		Prompt: "start", Title: "Worker", WaitSeconds: 20,
	})
	if err != nil || ack.State != store.ThreadRequestFinished {
		t.Fatalf("Spawn = %+v, %v; want a finished request", ack, err)
	}
	first := f.awaitThread(t, ack.ThreadID, "the worker marked read", func(row store.Thread) bool {
		return row.LatestTurnCompletedAt != nil && !threadUnread(row)
	})
	if !first.SpawnedByAgent {
		t.Fatalf("worker row = %+v, want it bound to the assistant", first)
	}

	report, err := f.adapter().UpdateThreads(t.Context(), f.callerIdentity(), threadtools.UpdateCall{
		ThreadIDs: []string{ack.ThreadID},
		Pin:       stringPtr(threadtools.PinFront),
	})
	if err != nil {
		t.Fatalf("UpdateThreads: %v", err)
	}
	if row := updateResultFor(t, report, ack.ThreadID); !row.Updated || row.Error != "" {
		t.Fatalf("pin result = %+v, want updated", row)
	}
	if pinned := f.reload(t, ack.ThreadID); pinned.PinnedAt == nil {
		t.Fatal("the assistant's pin did not land")
	}

	send, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{
		ThreadID: ack.ThreadID, Message: "again", WaitSeconds: 20,
	})
	if err != nil || send.State != store.ThreadRequestFinished {
		t.Fatalf("Send = %+v, %v; want a finished request", send, err)
	}
	f.awaitThread(t, ack.ThreadID, "the second turn complete", func(row store.Thread) bool {
		return row.LatestTurnCompletedAt != nil && *row.LatestTurnCompletedAt > *first.LatestTurnCompletedAt
	})
	time.Sleep(200 * time.Millisecond)
	if !threadUnread(f.reload(t, ack.ThreadID)) {
		t.Fatal("the pinned worker was marked read by its answer; a pin keeps the dot")
	}
}

func (f *requestFixture) reload(t *testing.T, threadID string) store.Thread {
	t.Helper()
	row, err := f.app.store.GetThread(threadID)
	if err != nil {
		t.Fatalf("GetThread(%s): %v", threadID, err)
	}
	return row
}

// A worker whose turn errored still answered its spawner: the spawner has the
// error, so the worker is marked read through that turn (ruling 30).
func TestAnErroredAnswerToTheSpawnerMarksTheWorkerRead(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	installMockClaudeTurns(t, f.app, [][]string{{
		mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"tried"}]}}`,
		`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["the tool failed"]}`,
	}})
	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		Prompt: "try it", Title: "Worker", WaitSeconds: 20,
	})
	if err != nil || ack.State != store.ThreadRequestErrored {
		t.Fatalf("Spawn = %+v, %v; want an errored request", ack, err)
	}
	row := f.awaitThread(t, ack.ThreadID, "the errored worker marked read", func(row store.Thread) bool {
		return row.LatestTurnCompletedAt != nil && !threadUnread(row)
	})
	if *row.LastReadAt != *row.LatestTurnCompletedAt {
		t.Errorf("LastReadAt = %d, want the errored turn's completion %d", *row.LastReadAt, *row.LatestTurnCompletedAt)
	}
}

// Which settled receipts earn the mark: an answer the target's own turn gave
// (a reply, its final message, its error or its interruption). A cancel, an
// expiry and a receipt still open mark nothing. The mark costs a writer
// transaction only when it moves: an already-read worker takes none.
func TestTheReadMarkFollowsTheAnswerAndCostsNothingWhenRead(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaude(t, "done")
	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		Prompt: "do it", Title: "Worker", WaitSeconds: 20,
	})
	if err != nil || ack.State != store.ThreadRequestFinished {
		t.Fatalf("Spawn = %+v, %v; want a finished request", ack, err)
	}
	f.awaitThread(t, ack.ThreadID, "the worker marked read", func(row store.Thread) bool {
		return row.LatestTurnCompletedAt != nil && !threadUnread(row)
	})
	settled := f.receipt(t, ack.Token)
	if settled.TurnID == "" {
		t.Fatalf("settled receipt %+v carries no turn", settled)
	}

	// Already read: no writer transaction.
	before := storeCostOf(f.app.store)
	f.app.markAnsweredRead(settled)
	if cost := storeCostOf(f.app.store).since(before); cost.writes != 0 || cost.commits != 0 {
		t.Fatalf("marking an already-read worker cost %+v, want no write and no commit", cost)
	}

	for _, tc := range []struct {
		state string
		marks bool
	}{
		{store.ThreadReceiptReplied, true},
		{store.ThreadReceiptFinished, true},
		{store.ThreadReceiptErrored, true},
		{store.ThreadReceiptInterrupted, true},
		{store.ThreadReceiptCancelled, false},
		{store.ThreadReceiptExpired, false},
		{store.ThreadReceiptRunning, false},
		{store.ThreadReceiptAccepted, false},
	} {
		if err := f.app.MarkThreadUnread(ack.ThreadID); err != nil {
			t.Fatalf("MarkThreadUnread: %v", err)
		}
		receipt := settled
		receipt.State = tc.state
		before := storeCostOf(f.app.store)
		f.app.markAnsweredRead(receipt)
		cost := storeCostOf(f.app.store).since(before)
		if marked := !threadUnread(f.reload(t, ack.ThreadID)); marked != tc.marks {
			t.Errorf("a %s receipt marked the worker read = %v, want %v", tc.state, marked, tc.marks)
		}
		if tc.marks && cost.commits != 1 {
			t.Errorf("a %s mark cost %+v, want one transaction", tc.state, cost)
		}
		if !tc.marks && (cost.writes != 0 || cost.commits != 0) {
			t.Errorf("a %s receipt cost %+v, want no write", tc.state, cost)
		}
	}
}

// A held mark waits for its turn's end; a session that ends first leaves
// the turn without a completion, so the hold goes with it.
func TestASessionEndDropsTheHeldReadMarksOfItsUnfinishedTurns(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	thread, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	f.app.holdAnsweredRead(thread.ID, 3)
	f.app.finishAnsweredReads(thread.ID, false)
	if !f.app.hasAnsweredReads(thread.ID) {
		t.Fatal("a turn end that did not complete the held turn dropped its hold")
	}
	f.app.finishAnsweredReads(thread.ID, true)
	if f.app.hasAnsweredReads(thread.ID) {
		t.Fatal("the session ended and the hold for its unfinished turn is still there")
	}
}

// Placement is organization only (ruling 36): a standalone or grouped
// spawn's answer to its spawner marks it read as a nested one's does.
func TestAnAnswerMarksAPlacedSpawnRead(t *testing.T) {
	t.Parallel()
	for _, call := range []threadtools.SpawnCall{
		{Prompt: "work", Title: "Standalone worker", Placement: threadtools.PlacementStandalone, WaitSeconds: 20},
		{Prompt: "work", Title: "Grouped worker", Group: "Sweep", WaitSeconds: 20},
	} {
		t.Run(call.Title, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			f.mockClaude(t, "done")
			ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), call)
			if err != nil || ack.State != store.ThreadRequestFinished {
				t.Fatalf("Spawn = %+v, %v; want a finished request", ack, err)
			}
			row := f.awaitThread(t, ack.ThreadID, "the worker marked read", func(row store.Thread) bool {
				return row.LatestTurnCompletedAt != nil && !threadUnread(row)
			})
			if row.Placement == store.PlacementNested {
				t.Errorf("the worker is nested; the spawn asked for %+v", call)
			}
		})
	}
}
