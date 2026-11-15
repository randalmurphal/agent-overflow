package app

// The Agents list and the sidebar's worker summary follow a worker's
// lifetime, not its request's (docs/specs/standing-agents.md, "Owned
// workers"): a spawn's row is open while the thread it made is not at rest,
// whatever its request's state, and a worker is counted once however many
// requests went to it.

import (
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
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

// pastTokens reads the first page of threadID's Past, checking its total.
func (f *requestFixture) pastTokens(t *testing.T, threadID string) []string {
	t.Helper()
	past, err := f.app.ListThreadPastAgentRows(threadID, store.PastAgentRowsCursor{}, 8)
	if err != nil {
		t.Fatalf("ListThreadPastAgentRows: %v", err)
	}
	tokens := agentRowTokens(past.Rows)
	if past.Total != len(tokens) {
		t.Fatalf("past total %d, rows %v", past.Total, tokens)
	}
	return tokens
}

// awaitWorker waits until the row token of threadID's listing reports the
// worker state want, empty for a worker at rest, and returns the row; a
// row the listing no longer shows reads as at rest.
func (f *requestFixture) awaitWorker(t *testing.T, threadID, token, want string) ThreadAgentRow {
	t.Helper()
	var row ThreadAgentRow
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		f.app.threadRequestsWG.Wait()
		row = ThreadAgentRow{}
		for _, r := range f.agentRows(t, threadID).Rows {
			if r.Token == token {
				row = r
			}
		}
		if row.Worker == want {
			return row
		}
		if time.Now().After(deadline) {
			t.Fatalf("row %s reads worker %q, want %q: %+v; target live state %+v", token, row.Worker, want, row,
				threadToolsLiveState(f.app.triage.LiveStateSnapshotForThread(row.ThreadID)))
		}
	}
}

// resolveApprovalOn resolves the approval requestID on threadID, as the
// provider reports an answered prompt.
func resolveApprovalOn(t *testing.T, a *App, threadID, requestID string) {
	t.Helper()
	meta, err := json.Marshal(map[string]any{"requestId": requestID, "decision": "approved"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventApprovalResolved, ThreadID: threadID, ItemID: requestID, Meta: meta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

// mockTurn is one turn of mockClaudeStoppable: its text, and whether it
// holds open until it is interrupted.
type mockTurn struct {
	text string
	held bool
}

// mockClaudeStoppable installs a provider whose n-th user message plays the
// n-th turn, and ends a held turn the way the CLI ends one it was told to
// interrupt.
func (f *requestFixture) mockClaudeStoppable(t *testing.T, turns ...mockTurn) {
	t.Helper()
	script := make([][]string, 0, len(turns))
	for i, turn := range turns {
		lines := []string{mockClaudeInitLine,
			`{"type":"assistant","message":{"id":"msg-` + strconv.Itoa(i) + `","role":"assistant","content":[{"type":"text","text":` + quoteJSON(turn.text) + `}]}}`}
		if !turn.held {
			lines = append(lines, `{"type":"result","subtype":"success","is_error":false}`)
		}
		script = append(script, lines)
	}
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), script)
	payload := binary + ".payload"
	body, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	ended := `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["[Request interrupted by user]"]}`
	patched := strings.Replace(string(body), "\"$reqid\"\n", "\"$reqid\"\n      printf '%s\\n' '"+ended+"'\n", 1)
	if patched == string(body) {
		t.Fatal("the mock script has no interrupt answer to patch")
	}
	if err := os.WriteFile(payload, []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set claude binary: %v", err)
	}
}

// settledSpawn seeds caller's spawn of a new thread, settled with a reply
// long before the settled window, and returns the thread.
func (f *requestFixture) settledSpawn(t *testing.T, token, caller string) store.Thread {
	t.Helper()
	worker := f.plainThread(t)
	seedSettledAgentRow(t, f.app, store.ThreadRequest{
		Token: token, CallerThreadID: caller, Kind: store.ThreadRequestSpawn, TargetThreadID: worker.ID,
	}, time.Now().Add(-time.Hour).UnixMilli(), "done for now")
	return worker
}

// A worker that calls thread_reply and keeps working in the same turn is
// open: its spawner's row keeps the reply and says the worker runs, is
// counted, offers Stop, stays past the settled window and out of Past, and
// shows the worker's later prompt with a frame. A Stop of it interrupts
// the worker, and once its turn ends the row settles into Past with its
// reply.
func TestAWorkerThatRepliesAndKeepsWorkingStaysOpen(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.holdCallerTurn(t)
	f.app.triage.SetPromptRouter(f.app.routeWorkerPrompt)
	f.watchAgentRows()
	f.mockClaudeStoppable(t, mockTurn{"rerunning the deploy", true})
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
	row := f.awaitWorker(t, f.caller.ID, ack.Token, workerRunning)
	if row.State != threadtools.RequestReplied || !row.open() || row.Answer != "The deploy started; I am watching it." {
		t.Fatalf("row = %+v; want the reply kept and the row open", row)
	}
	if got := f.workerSummary(t, f.caller.ID); got.Open != 1 || got.NeedYou != 0 {
		t.Fatalf("summary %+v; want the running worker counted", got)
	}
	if past := f.pastTokens(t, f.caller.ID); len(past) != 0 {
		t.Fatalf("past = %v; want the running worker kept out", past)
	}
	if after := f.listingAfterWindow(t, f.caller.ID); len(after.Rows) != 1 || after.Rows[0].Worker != workerRunning {
		t.Fatalf("after the settled window: rows %+v; want the running worker kept", after.Rows)
	}

	// The worker blocks on an approval in the same turn. It is routed to
	// the spawner, and the spawner's row says so, framed.
	f.app.threadRequestsWG.Wait()
	frames := f.callerAgentFrames(t, f.caller.ID)
	approvalOn(t, f.app, ack.ThreadID, "after-reply-approval")
	row = f.awaitWorker(t, f.caller.ID, ack.Token, workerBlocked)
	if row.BlockedOn != "approval" || row.BlockedRoutedTo == "" || row.needsYou() {
		t.Fatalf("row = %+v; want blocked on an approval routed to its spawner", row)
	}
	if got := f.callerAgentFrames(t, f.caller.ID); got <= frames {
		t.Fatalf("frames to the caller stayed at %d; want one for the blocked worker", got)
	}
	// Handed back to the user, it waits on them.
	if !f.app.triage.RejectPrompt(f.caller.ID, openPromptRef(t, f.app.triage, ack.ThreadID, "after-reply-approval")) {
		t.Fatal("the prompt did not go back to the user")
	}
	waitUntil(t, 10*time.Second, func() bool {
		return f.workerSummary(t, f.caller.ID) == ThreadWorkerSummary{ThreadID: f.caller.ID, Open: 1, NeedYou: 1}
	})
	resolveApprovalOn(t, f.app, ack.ThreadID, "after-reply-approval")
	f.awaitWorker(t, f.caller.ID, ack.Token, workerRunning)

	// Stop interrupts the worker; its turn ends and the row settles.
	stop, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, ack.Token)
	if err != nil || stop.Outcome != threadAgentEnded || !stop.Interrupted {
		t.Fatalf("Stop = %+v, %v; want ended with the worker interrupted", stop, err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		return !threadToolsLiveState(f.app.triage.LiveStateSnapshotForThread(ack.ThreadID)).ActiveTurn
	})
	f.awaitWorker(t, f.caller.ID, ack.Token, "")
	if got := f.workerSummary(t, f.caller.ID); got.Open != 0 || got.NeedYou != 0 {
		t.Fatalf("summary %+v; want nothing open once the worker rests", got)
	}
	if after := f.listingAfterWindow(t, f.caller.ID); len(after.Rows) != 0 {
		t.Fatalf("after the window: %+v; want the resting worker gone from the open list", after.Rows)
	}
	past, err := f.app.ListThreadPastAgentRows(f.caller.ID, store.PastAgentRowsCursor{}, 8)
	if err != nil || len(past.Rows) != 1 || past.Rows[0].Token != ack.Token || past.Rows[0].Answer != "The deploy started; I am watching it." {
		t.Fatalf("past = %+v, %v; want the spawn with its reply", past, err)
	}
}

// After a worker's spawn finished, a person continues it from its own
// composer. Its spawner's row opens again with a frame, the summary and
// the sidebar's bulk read count it, its approval shows on the row, and
// once its turn ends the row is back in Past.
func TestAWorkerContinuedFromItsPaneReopensForItsSpawner(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.triage.SetPromptRouter(f.app.routeWorkerPrompt)
	f.watchAgentRows()
	// The person's follow-up turn holds open.
	f.mockClaudeStoppable(t, mockTurn{"The first pass is done.", false}, mockTurn{"Working on the follow-up.", true})
	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{Prompt: "do the first pass", WaitSeconds: 20})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	f.awaitRequestState(t, ack.Token, store.ThreadRequestFinished)
	if worker, err := f.app.store.GetThread(ack.ThreadID); err != nil || worker.Placement != store.PlacementOwned {
		t.Fatalf("worker %+v, %v; this test assumes an owned worker with no sidebar row", worker, err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		return !threadToolsLiveState(f.app.triage.LiveStateSnapshotForThread(ack.ThreadID)).ActiveTurn
	})
	f.awaitWorker(t, f.caller.ID, ack.Token, "")
	frames := f.callerAgentFrames(t, f.caller.ID)

	if err := f.app.SendMessage(ack.ThreadID, "now also update the changelog", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	row := f.awaitWorker(t, f.caller.ID, ack.Token, workerRunning)
	if row.State != threadtools.RequestFinished || !row.open() {
		t.Fatalf("row = %+v; want the finished spawn open while its worker runs", row)
	}
	if got := f.callerAgentFrames(t, f.caller.ID); got <= frames {
		t.Fatalf("frames to the caller stayed at %d; want the worker's new turn framed", got)
	}
	if got := f.workerSummary(t, f.caller.ID); got.Open != 1 {
		t.Fatalf("summary %+v; want the running worker counted", got)
	}
	if all := allWorkerSummaries(t, f.app); !slices.ContainsFunc(all, func(s ThreadWorkerSummary) bool { return s.ThreadID == f.caller.ID && s.Open == 1 }) {
		t.Fatalf("summaries %+v; want the spawner in the sidebar's read", all)
	}
	if after := f.listingAfterWindow(t, f.caller.ID); len(after.Rows) != 1 {
		t.Fatalf("after the window: %+v; want the running worker kept", after.Rows)
	}
	approvalOn(t, f.app, ack.ThreadID, "follow-up-approval")
	row = f.awaitWorker(t, f.caller.ID, ack.Token, workerBlocked)
	if row.BlockedOn != "approval" || row.BlockedRoutedTo == "" {
		t.Fatalf("row = %+v; want blocked on the approval routed to the spawner", row)
	}
	if !f.app.triage.RejectPrompt(f.caller.ID, openPromptRef(t, f.app.triage, ack.ThreadID, "follow-up-approval")) {
		t.Fatal("the prompt did not go back to the user")
	}
	waitUntil(t, 10*time.Second, func() bool { return f.workerSummary(t, f.caller.ID).NeedYou == 1 })
	resolveApprovalOn(t, f.app, ack.ThreadID, "follow-up-approval")
	f.awaitWorker(t, f.caller.ID, ack.Token, workerRunning)

	// Stop All reaches the open row and ends the worker's turn: back to Past.
	if stops, err := f.app.StopThreadAgentRows(t.Context(), f.caller.ID); err != nil || len(stops) != 1 || stops[0].Token != ack.Token || !stops[0].Interrupted {
		t.Fatalf("Stop All = %+v, %v; want the worker's row stopped and the worker interrupted", stops, err)
	}
	f.awaitWorker(t, f.caller.ID, ack.Token, "")
	if got := f.workerSummary(t, f.caller.ID); got.Open != 0 || got.NeedYou != 0 {
		t.Fatalf("summary %+v; want nothing open", got)
	}
	if all := allWorkerSummaries(t, f.app); slices.ContainsFunc(all, func(s ThreadWorkerSummary) bool { return s.ThreadID == f.caller.ID }) {
		t.Fatalf("summaries %+v; want the spawner gone from the sidebar's read", all)
	}
	if past := f.pastTokens(t, f.caller.ID); !slices.Equal(past, []string{ack.Token}) {
		t.Fatalf("past = %v; want the spawn back", past)
	}
}

// Open requests to one worker, its spawn and two sends with notify, are
// three rows and one worker.
func TestRequestsToOneWorkerCountOnce(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	worker := f.plainThread(t)
	f.seedAgentRow(t, "one-spawn", f.caller.ID, store.ThreadRequestSpawn, worker.ID, true)
	f.seedAgentRow(t, "one-send-a", f.caller.ID, store.ThreadRequestSend, worker.ID, true)
	f.seedAgentRow(t, "one-send-b", f.caller.ID, store.ThreadRequestSend, worker.ID, true)
	other := f.plainThread(t)
	f.seedAgentRow(t, "other-send", f.caller.ID, store.ThreadRequestSend, other.ID, true)

	rows := f.agentRows(t, f.caller.ID).Rows
	if got, want := agentRowTokens(rows), []string{"one-spawn", "one-send-a", "one-send-b", "other-send"}; !slices.Equal(got, want) {
		t.Fatalf("rows %v; want %v, one per request", got, want)
	}
	if got := f.workerSummary(t, f.caller.ID); got.Open != 2 {
		t.Fatalf("summary %+v; want two workers, the one sent to three times counted once", got)
	}
}

// A worker whose spawn settled long ago blocks on an approval with no
// request running: the row its spawner lists shows it blocked, framed, and
// the summary counts it as waiting on the user once it is handed back.
// Nested one spawn further down, it keeps the top spawner's row listed as
// open work below and counts in the top's summary, as a parked request
// does. Answered, it rests, and the rows settle into Past.
func TestAWorkerBlockedWithNoRequestRunning(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.triage.SetPromptRouter(f.app.routeWorkerPrompt)
	f.watchAgentRows()
	child := f.settledSpawn(t, "lead-child", f.caller.ID)
	grand := f.settledSpawn(t, "child-grand", child.ID)
	if rows := f.agentRows(t, f.caller.ID).Rows; len(rows) != 0 {
		t.Fatalf("at rest the rows %v; want none", agentRowTokens(rows))
	}
	f.app.threadRequestsWG.Wait()
	leadFrames, childFrames := f.callerAgentFrames(t, f.caller.ID), f.callerAgentFrames(t, child.ID)

	approvalOn(t, f.app, grand.ID, "grand-approval")
	row := f.awaitWorker(t, f.caller.ID, "child-grand", workerBlocked)
	rows := f.agentRows(t, f.caller.ID).Rows
	if got, want := agentRowTokens(rows), []string{"lead-child", "child-grand"}; !slices.Equal(got, want) {
		t.Fatalf("rows %v; want %v", got, want)
	}
	if rows[0].open() || rows[0].Worker != "" || row.Depth != 1 || row.BlockedOn != "approval" || row.BlockedRoutedTo != child.Title {
		t.Fatalf("rows %+v; want the top row settled over its blocked nested worker", rows)
	}
	if f.callerAgentFrames(t, f.caller.ID) <= leadFrames || f.callerAgentFrames(t, child.ID) <= childFrames {
		t.Fatal("no frame reached the spawners of the blocked worker")
	}
	if got := f.workerSummary(t, f.caller.ID); got.Open != 1 || got.NeedYou != 0 {
		t.Fatalf("lead's summary %+v; want the nested worker counted, its prompt routed", got)
	}
	if !f.app.triage.RejectPrompt(child.ID, openPromptRef(t, f.app.triage, grand.ID, "grand-approval")) {
		t.Fatal("the prompt did not go back to the user")
	}
	waitUntil(t, 10*time.Second, func() bool { return f.workerSummary(t, child.ID).NeedYou == 1 })
	if got := f.workerSummary(t, f.caller.ID); got.Open != 1 || got.NeedYou != 1 {
		t.Fatalf("lead's summary %+v; want the nested worker waiting on the user", got)
	}
	if past := f.pastTokens(t, child.ID); len(past) != 0 {
		t.Fatalf("child's past %v; want its blocked worker kept out", past)
	}

	resolveApprovalOn(t, f.app, grand.ID, "grand-approval")
	waitUntil(t, 10*time.Second, func() bool {
		f.app.threadRequestsWG.Wait()
		return len(f.agentRows(t, f.caller.ID).Rows) == 0
	})
	if got := f.workerSummary(t, f.caller.ID); got.Open != 0 {
		t.Fatalf("lead's summary %+v; want none", got)
	}
	if past := f.pastTokens(t, child.ID); !slices.Equal(past, []string{"child-grand"}) {
		t.Fatalf("child's past %v; want its worker back", past)
	}
}

// A worker holding a queued message and no turn is open, shown as queued.
// After a restart nothing is live until a turn starts or a queue fills, so
// its row is settled again with the message still in the durable queue.
func TestAWorkerHoldingAQueuedMessageIsOpenUntilARestart(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.configureTriageQueueCallbacks()
	f.watchAgentRows()
	worker := f.settledSpawn(t, "queued-spawn", f.caller.ID)
	// The worker's session never starts, so the message stays queued.
	f.app.startSessionFn = func(threadID string) error {
		if threadID == worker.ID {
			return nil
		}
		return f.app.startSessionNow(threadID)
	}
	if _, err := f.app.RegisterQueueItem(t.Context(), worker.ID, "pick this up next", SendMessageOptions{SendID: "queued-send"}); err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}
	row := f.awaitWorker(t, f.caller.ID, "queued-spawn", workerQueued)
	if live := threadToolsLiveState(f.app.triage.LiveStateSnapshotForThread(worker.ID)); live.ActiveTurn || !live.PendingSends {
		t.Fatalf("worker live state %+v; this test needs a queued message and no turn", live)
	}
	if !row.open() || row.State != threadtools.RequestReplied {
		t.Fatalf("row = %+v; want the settled spawn open while its message waits", row)
	}
	if got := f.workerSummary(t, f.caller.ID); got.Open != 1 {
		t.Fatalf("summary %+v; want the queued worker counted", got)
	}

	after := f.restart(t)
	after.watchAgentRows()
	if rows := after.agentRows(t, f.caller.ID).Rows; len(rows) != 0 {
		t.Fatalf("after the restart rows %+v; want none until the worker moves", rows)
	}
	if got := after.workerSummary(t, f.caller.ID); got.Open != 0 {
		t.Fatalf("after the restart summary %+v; want none", got)
	}
	if queued, err := after.app.store.ListFlushQueueItems(worker.ID); err != nil || len(queued) != 1 {
		t.Fatalf("durable queue %+v, %v; want the message kept for the boot restore", queued, err)
	}
}

// A live thread is a row only through the spawn that made it: one the
// caller only sent to, and one nothing spawned, add no row, no summary and
// no frame, and the never spawned one is looked up once while it is live.
func TestALiveThreadOnlySentToIsNoRow(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	peer := f.plainThread(t)
	seedSettledAgentRow(t, f.app, store.ThreadRequest{
		Token: "quiet-send", CallerThreadID: f.caller.ID, Kind: store.ThreadRequestSend, TargetThreadID: peer.ID, Notify: true,
	}, time.Now().Add(-time.Hour).UnixMilli(), "ok")
	alone := f.plainThread(t)
	f.app.threadRequestsWG.Wait()
	frames := f.callerAgentFrames(t, f.caller.ID)
	approvalOn(t, f.app, peer.ID, "peer-approval")
	approvalOn(t, f.app, alone.ID, "alone-approval")
	approvalOn(t, f.app, alone.ID, "alone-approval-2")
	waitUntil(t, 10*time.Second, func() bool {
		f.app.threadRequestsWG.Wait()
		w := &f.app.liveWorkers
		w.mu.Lock()
		defer w.mu.Unlock()
		_, a := w.threads[alone.ID]
		_, p := w.threads[peer.ID]
		return a && p
	})
	if live := f.app.liveWorkerThreads(); len(live) != 0 {
		t.Fatalf("live workers %v; want none, neither was spawned", live)
	}
	if rows := f.agentRows(t, f.caller.ID).Rows; len(rows) != 0 {
		t.Fatalf("rows %+v; want none", rows)
	}
	if got := f.workerSummary(t, f.caller.ID); got.Open != 0 {
		t.Fatalf("summary %+v; want none", got)
	}
	if got := f.callerAgentFrames(t, f.caller.ID); got != frames {
		t.Fatalf("frames to the caller went from %d to %d; want none", frames, got)
	}
	resolveApprovalOn(t, f.app, alone.ID, "alone-approval")
	resolveApprovalOn(t, f.app, alone.ID, "alone-approval-2")
	waitUntil(t, 10*time.Second, func() bool {
		f.app.threadRequestsWG.Wait()
		w := &f.app.liveWorkers
		w.mu.Lock()
		defer w.mu.Unlock()
		_, a := w.threads[alone.ID]
		return !a
	})
}

// A worker leaves rest and comes back to it, again: each move is framed
// and read. Its session dying mid-turn rests it.
func TestAWorkerMovesBetweenRestAndWork(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	worker := f.settledSpawn(t, "moving-spawn", f.caller.ID)
	for i := range 2 {
		approvalOn(t, f.app, worker.ID, "move-approval")
		f.awaitWorker(t, f.caller.ID, "moving-spawn", workerBlocked)
		resolveApprovalOn(t, f.app, worker.ID, "move-approval")
		f.awaitWorker(t, f.caller.ID, "moving-spawn", "")
		if got := f.workerSummary(t, f.caller.ID); got.Open != 0 {
			t.Fatalf("round %d: summary %+v at rest", i, got)
		}
	}
	// A turn its session ends without finishing.
	if err := f.app.triage.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: worker.ID, Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	f.awaitWorker(t, f.caller.ID, "moving-spawn", workerRunning)
	if err := f.app.triage.Handle(provider.ProviderEvent{Kind: provider.EventSessionStatus, ThreadID: worker.ID, Content: "error", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.triage.Handle(provider.ProviderEvent{Kind: provider.EventSessionStatus, ThreadID: worker.ID, Content: "disconnected", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	f.awaitWorker(t, f.caller.ID, "moving-spawn", "")
	if rows := f.agentRows(t, f.caller.ID).Rows; len(rows) != 0 {
		t.Fatalf("after the session died rows %+v; want none", rows)
	}
}

// Archiving a live worker, or its spawner, rests it; deleting the worker
// forgets it; deleting its spawner takes the link, so a worker still busy
// is no row anywhere. A worker the spawner gave a sidebar row is still its
// worker: its spawn's row is open while it works.
func TestLiveWorkersAcrossArchiveDeleteAndPlacement(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.mockClaudeStoppable(t, mockTurn{"working", true})
	block := func(worker store.Thread, id string) {
		t.Helper()
		approvalOn(t, f.app, worker.ID, id)
	}
	work := func(worker store.Thread, token string) {
		t.Helper()
		if err := f.app.SendMessage(worker.ID, "keep going", nil); err != nil {
			t.Fatalf("SendMessage: %v", err)
		}
		f.awaitWorker(t, f.caller.ID, token, workerRunning)
	}

	archived := f.settledSpawn(t, "archived-spawn", f.caller.ID)
	work(archived, "archived-spawn")
	if _, err := f.app.ArchiveThread(archived.ID); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}
	f.awaitWorker(t, f.caller.ID, "archived-spawn", "")

	deleted := f.settledSpawn(t, "deleted-spawn", f.caller.ID)
	block(deleted, "deleted-approval")
	f.awaitWorker(t, f.caller.ID, "deleted-spawn", workerBlocked)
	if err := f.app.DeleteThread(deleted.ID); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}
	f.awaitWorker(t, f.caller.ID, "deleted-spawn", "")
	waitUntil(t, 10*time.Second, func() bool { return !slices.Contains(f.app.liveWorkerThreads(), deleted.ID) })

	standalone := f.settledSpawn(t, "standalone-spawn", f.caller.ID)
	if _, err := f.app.SetThreadPlacement([]string{standalone.ID}, store.PlacementStandalone); err != nil {
		t.Fatalf("SetThreadPlacement: %v", err)
	}
	block(standalone, "standalone-approval")
	if row := f.awaitWorker(t, f.caller.ID, "standalone-spawn", workerBlocked); row.Placement != store.PlacementStandalone {
		t.Fatalf("row = %+v; want the standalone worker's row open", row)
	}

	// A second spawner whose worker is busy is deleted: the worker keeps
	// working with no spawn above it.
	other := f.plainThread(t)
	orphan := f.settledSpawn(t, "orphan-spawn", other.ID)
	block(orphan, "orphan-approval")
	f.awaitWorker(t, other.ID, "orphan-spawn", workerBlocked)
	if err := f.app.DeleteThread(other.ID); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}
	waitUntil(t, 10*time.Second, func() bool { return !slices.Contains(f.app.liveWorkerThreads(), orphan.ID) })
	if !threadToolsLiveState(f.app.triage.LiveStateSnapshotForThread(orphan.ID)).Blocked() {
		t.Fatal("the orphaned worker's prompt is gone; this test needs it still waiting")
	}
	if got := f.workerSummary(t, f.caller.ID); got.Open != 1 {
		t.Fatalf("summary %+v; want only the standalone worker", got)
	}

	// Archiving the spawner archives its owned worker, which rests.
	owned := f.settledSpawn(t, "owned-spawn", f.caller.ID)
	if _, err := f.app.SetThreadPlacement([]string{owned.ID}, store.PlacementOwned); err != nil {
		t.Fatalf("own the worker: %v", err)
	}
	work(owned, "owned-spawn")
	if _, err := f.app.ArchiveThread(f.caller.ID); err != nil {
		t.Fatalf("archive the spawner: %v", err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		f.app.threadRequestsWG.Wait()
		return !slices.Contains(f.app.liveWorkerThreads(), owned.ID)
	})
}
