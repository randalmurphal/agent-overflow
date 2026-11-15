package app

// These tests document current behavior, not the intended contract: the
// Agents list and the sidebar's worker summary follow a request's lifetime,
// not the worker thread's. Each assertion pins what a person sees today, so a
// change that makes the list follow the worker flips them on purpose.

import (
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// callerAgentFrames counts the thread:agent_rows_changed frames addressed to
// threadID so far.
func (f *requestFixture) callerAgentFrames(t *testing.T, threadID string) int {
	t.Helper()
	f.bus.mu.Lock()
	defer f.bus.mu.Unlock()
	n := 0
	for _, event := range f.bus.all {
		if event.Name != "thread:agent_rows_changed" {
			continue
		}
		if change, ok := event.Data.(ThreadAgentRowChange); ok && change.ThreadID == threadID {
			n++
		}
	}
	return n
}

// listingAfterWindow reads threadID's listing as it reads once the settled
// window has passed every row that settled by now.
func (f *requestFixture) listingAfterWindow(t *testing.T, threadID string) ThreadAgentRows {
	t.Helper()
	view := newThreadAgentView(f.app)
	view.since = time.Now().Add(time.Second).UnixMilli()
	listing, _, err := f.app.threadAgentTreeDepth(t.Context(), view, threadID, threadAgentRowDepth, threadAgentPeersCached)
	if err != nil {
		t.Fatalf("listing after the window: %v", err)
	}
	return listing
}

func (f *requestFixture) workerSummary(t *testing.T, threadID string) ThreadWorkerSummary {
	t.Helper()
	got, err := f.app.GetThreadWorkerSummary(t.Context(), threadID)
	if err != nil {
		t.Fatalf("GetThreadWorkerSummary: %v", err)
	}
	return got
}

// Finding (a), current behavior: a worker that calls thread_reply and keeps
// working in the same turn is shown as settled. Its spawner's row reads
// replied with no activity line, it is not counted open, it is in Past at
// once and leaves the listing with the settled window, and the worker's
// later prompt reaches neither the row nor a change frame, while the
// worker's turn is still running.
func TestCurrentBehaviorAWorkerThatRepliesAndKeepsWorkingReadsAsSettled(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.holdCallerTurn(t)
	f.app.triage.SetPromptRouter(f.app.routeWorkerPrompt)
	f.watchAgentRows()
	f.mockClaudeHoldingTheTurn(t, "rerunning the deploy")
	ack := f.runningRequest(t, "rerun the deploy, tell me when it starts, then watch it")
	if got := f.workerSummary(t, f.caller.ID); got.Open != 1 {
		t.Fatalf("before the reply: summary %+v, want one open worker", got)
	}

	if _, err := f.adapter().Reply(t.Context(), f.targetIdentity(t, ack.ThreadID), threadtools.ReplyCall{
		Token: ack.Token, Text: "The deploy started; I am watching it.",
	}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	f.awaitRequestState(t, ack.Token, store.ThreadRequestReplied)

	live := threadToolsLiveState(f.app.triage.LiveStateSnapshotForThread(ack.ThreadID))
	if !live.ActiveTurn {
		t.Fatal("the worker's turn ended with its reply; this test needs it still running")
	}
	rows := f.agentRows(t, f.caller.ID).Rows
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want the one spawn", rows)
	}
	row := rows[0]
	t.Logf("while the worker still runs: state=%q open=%v activity=%q answer=%q", row.State, row.open(), row.Activity, row.Answer)
	if row.State != threadtools.RequestReplied || row.open() || row.Activity != "" || row.BlockedOn != "" {
		t.Fatalf("row = %+v; documents a replied, settled row with no live state", row)
	}
	if got := f.workerSummary(t, f.caller.ID); got.Open != 0 || got.NeedYou != 0 {
		t.Fatalf("summary %+v; documents the running worker not counted", got)
	}
	past, err := f.app.ListThreadPastAgentRows(f.caller.ID, store.PastAgentRowsCursor{}, 8)
	if err != nil || past.Total != 1 || len(past.Rows) != 1 || past.Rows[0].Token != ack.Token {
		t.Fatalf("past = %+v, %v; documents the running worker already in Past", past, err)
	}
	if after := f.listingAfterWindow(t, f.caller.ID); len(after.Rows) != 0 {
		t.Fatalf("after the settled window: rows %+v; documents the running worker leaving the list", after.Rows)
	}

	// The worker blocks on an approval, still in the same turn. It is routed
	// to the spawner, and nothing in the spawner's list says so.
	f.app.threadRequestsWG.Wait()
	frames := f.callerAgentFrames(t, f.caller.ID)
	if frames == 0 {
		t.Fatal("no frame reached the caller at all; the frame count below would prove nothing")
	}
	approvalOn(t, f.app, ack.ThreadID, "after-reply-approval")
	pending := f.app.triage.PendingInteractiveRequests(ack.ThreadID).Approvals
	if len(pending) != 1 || pending[0].RoutedTo == nil || pending[0].RoutedTo.ThreadID != f.caller.ID {
		t.Fatalf("the worker's approval = %+v, want it routed to its spawner", pending)
	}
	f.app.threadRequestsWG.Wait()
	if got := f.callerAgentFrames(t, f.caller.ID); got != frames {
		t.Fatalf("frames to the caller went from %d to %d; documents none sent for a settled request's worker", frames, got)
	}
	row = f.agentRows(t, f.caller.ID).Rows[0]
	if row.State != threadtools.RequestReplied || row.BlockedOn != "" {
		t.Fatalf("row = %+v; documents the blocked worker shown as replied", row)
	}
	// Handed back to the user, it is still not counted.
	if !f.app.triage.RejectPrompt(f.caller.ID, openPromptRef(t, f.app.triage, ack.ThreadID, "after-reply-approval")) {
		t.Fatal("the prompt did not go back to the user")
	}
	if got := f.workerSummary(t, f.caller.ID); got.Open != 0 || got.NeedYou != 0 {
		t.Fatalf("summary %+v; documents a worker waiting on the user not counted", got)
	}

	// The tray offers no Stop on a settled row, but the backend Stop of it
	// still cascades into the worker and interrupts the turn.
	stop, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, ack.Token)
	t.Logf("Stop of the settled row: %+v, %v", stop, err)
	if err != nil || stop.Outcome != threadAgentEnded || !stop.Interrupted {
		t.Fatalf("Stop = %+v, %v; documents ended with the worker interrupted", stop, err)
	}
}

// Finding (b), current behavior: after a worker's spawn finished, a person
// continues the worker from its own composer. That turn creates no request:
// the spawner's row stays finished, the worker is not counted, no frame
// reaches the spawner, and the worker's approval is still routed to the
// spawner (the spawn link) while the spawner's list shows nothing of it.
func TestCurrentBehaviorAWorkerContinuedFromItsPaneIsNotTracked(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.triage.SetPromptRouter(f.app.routeWorkerPrompt)
	f.watchAgentRows()
	installMockClaudeTurns(t, f.app, [][]string{
		{mockClaudeInitLine,
			`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"The first pass is done."}]}}`,
			`{"type":"result","subtype":"success","is_error":false}`},
		// The person's follow-up turn holds open.
		{mockClaudeInitLine,
			`{"type":"assistant","message":{"id":"msg-2","role":"assistant","content":[{"type":"text","text":"Working on the follow-up."}]}}`},
	})
	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{Prompt: "do the first pass", WaitSeconds: 20})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	f.awaitRequestState(t, ack.Token, store.ThreadRequestFinished)
	worker, err := f.app.store.GetThread(ack.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("worker placement %q", worker.Placement)
	if worker.Placement != store.PlacementOwned {
		t.Fatalf("worker placement %q; this test assumes an owned worker with no sidebar row", worker.Placement)
	}
	waitUntil(t, 10*time.Second, func() bool {
		return !threadToolsLiveState(f.app.triage.LiveStateSnapshotForThread(ack.ThreadID)).ActiveTurn
	})
	f.app.threadRequestsWG.Wait()
	frames := f.callerAgentFrames(t, f.caller.ID)
	if frames == 0 {
		t.Fatal("no frame reached the caller at all; the frame count below would prove nothing")
	}

	if err := f.app.SendMessage(ack.ThreadID, "now also update the changelog", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		return threadToolsLiveState(f.app.triage.LiveStateSnapshotForThread(ack.ThreadID)).ActiveTurn
	})
	approvalOn(t, f.app, ack.ThreadID, "follow-up-approval")
	pending := f.app.triage.PendingInteractiveRequests(ack.ThreadID).Approvals
	if len(pending) != 1 || pending[0].RoutedTo == nil || pending[0].RoutedTo.ThreadID != f.caller.ID {
		t.Fatalf("the worker's approval = %+v; documents it routed to the spawner by the spawn link", pending)
	}
	f.app.threadRequestsWG.Wait()
	if got := f.callerAgentFrames(t, f.caller.ID); got != frames {
		t.Fatalf("frames to the caller went from %d to %d; documents none for the worker's untracked turn", frames, got)
	}
	rows := f.agentRows(t, f.caller.ID).Rows
	if len(rows) != 1 || rows[0].State != threadtools.RequestFinished || rows[0].open() || rows[0].BlockedOn != "" || rows[0].Activity != "" {
		t.Fatalf("rows = %+v; documents the running, blocked worker shown as finished", rows)
	}
	if got := f.workerSummary(t, f.caller.ID); got.Open != 0 || got.NeedYou != 0 {
		t.Fatalf("summary %+v; documents the running worker not counted", got)
	}
	if after := f.listingAfterWindow(t, f.caller.ID); len(after.Rows) != 0 {
		t.Fatalf("after the window: %+v; documents the worker gone from the open list", after.Rows)
	}
	all := allWorkerSummaries(t, f.app)
	if slices.ContainsFunc(all, func(s ThreadWorkerSummary) bool { return s.ThreadID == f.caller.ID }) {
		t.Fatalf("summaries %+v; documents the spawner left out of the sidebar's read", all)
	}
	// Handed to the user, the owned worker (no sidebar row) waits on them
	// and the spawner's badge does not say so.
	if !f.app.triage.RejectPrompt(f.caller.ID, openPromptRef(t, f.app.triage, ack.ThreadID, "follow-up-approval")) {
		t.Fatal("the prompt did not go back to the user")
	}
	if got := f.workerSummary(t, f.caller.ID); got.NeedYou != 0 {
		t.Fatalf("summary %+v; documents the wait on the user not counted", got)
	}
}

// Finding (c), current behavior: open requests to one worker, its spawn and
// two sends with notify, are three rows and count as three workers.
func TestCurrentBehaviorRequestsToOneWorkerCountAsSeveralWorkers(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	worker := f.plainThread(t)
	f.seedAgentRow(t, "one-spawn", f.caller.ID, store.ThreadRequestSpawn, worker.ID, true)
	f.seedAgentRow(t, "one-send-a", f.caller.ID, store.ThreadRequestSend, worker.ID, true)
	f.seedAgentRow(t, "one-send-b", f.caller.ID, store.ThreadRequestSend, worker.ID, true)

	rows := f.agentRows(t, f.caller.ID).Rows
	targets := map[string]int{}
	for _, row := range rows {
		targets[row.ThreadID]++
	}
	t.Logf("rows %v, targets %v", agentRowTokens(rows), targets)
	if len(rows) != 3 || len(targets) != 1 {
		t.Fatalf("rows %v over %d threads; documents three rows for one worker", agentRowTokens(rows), len(targets))
	}
	got := f.workerSummary(t, f.caller.ID)
	t.Logf("summary %+v", got)
	if got.Open != 3 {
		t.Fatalf("summary %+v; documents one worker counted three times", got)
	}
	if !strings.HasPrefix(rows[0].Token, "one-spawn") || rows[0].Kind != store.ThreadRequestSpawn {
		t.Fatalf("first row %+v", rows[0])
	}
}
