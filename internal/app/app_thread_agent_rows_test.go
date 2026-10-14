package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
)

func agentRowTokens(rows []ThreadAgentRow) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Token)
	}
	return out
}

// watchAgentRows routes the fixture's emissions through the Agents rows'
// tap and records the app's own, as the production emit funnel does.
func (f *requestFixture) watchAgentRows() {
	f.bus.mu.Lock()
	f.bus.tap = f.app.tapThreadAgentRows
	f.bus.mu.Unlock()
	f.app.testEmitHook = f.bus.emit
}

func (f *requestFixture) agentRows(t *testing.T, threadID string) ThreadAgentRows {
	t.Helper()
	rows, err := f.app.ListThreadAgentRows(t.Context(), threadID)
	if err != nil {
		t.Fatalf("ListThreadAgentRows(%s): %v", threadID, err)
	}
	return rows
}

func (f *requestFixture) plainThread(t *testing.T) store.Thread {
	t.Helper()
	thread, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	return thread
}

// seedAgentRow writes a request row by hand, for a shape the tools would
// take several live threads to reach.
func (f *requestFixture) seedAgentRow(t *testing.T, token, caller, kind, target string, notify bool) {
	t.Helper()
	if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
		Token: token, CallerThreadID: caller, Kind: kind, TargetThreadID: target, Notify: notify,
		State: store.ThreadRequestAccepted,
	}); err != nil {
		t.Fatalf("insert %s: %v", token, err)
	}
}

// A spawn lists with its thread's title, provider and running state, and the
// spawned thread's own spawn nests under it. A send the thread is owed no
// answer for is not an agent; one it is owed an answer for is.
func TestThreadAgentRowsListAndNestBySpawn(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "reading the logs")
	child := f.runningRequest(t, "triage the crash")
	grand, err := f.adapter().Spawn(t.Context(), f.targetIdentity(t, child.ThreadID), threadtools.SpawnCall{
		Prompt: "bisect it", Title: "Bisect", Notify: true,
	})
	if err != nil {
		t.Fatalf("nested Spawn: %v", err)
	}
	peer := f.busyThread(t, "peer-busy")
	quiet, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{ThreadID: peer.ID, Message: "fyi"})
	if err != nil {
		t.Fatalf("quiet Send: %v", err)
	}
	owed, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{ThreadID: peer.ID, Message: "and answer this", Notify: true})
	if err != nil {
		t.Fatalf("owed Send: %v", err)
	}

	listing := f.agentRows(t, f.caller.ID)
	want := []string{child.Token, grand.Token, owed.Token}
	if got := agentRowTokens(listing.Rows); !slices.Equal(got, want) {
		t.Fatalf("rows %v; want %v (a send owed nothing, %s, is not an agent)", got, want, quiet.Token)
	}
	top, nested := listing.Rows[0], listing.Rows[1]
	if top.Depth != 0 || top.Kind != store.ThreadRequestSpawn || top.Provider != string(provider.Claude) ||
		top.State != threadtools.RequestRunning || top.ThreadID != child.ThreadID || top.OpenThreadID != child.ThreadID {
		t.Fatalf("top row = %+v", top)
	}
	if nested.Depth != 1 || nested.CallerThreadID != child.ThreadID || nested.Title != "Bisect" {
		t.Fatalf("nested row = %+v", nested)
	}
	if listing.Rows[2].Depth != 0 || listing.Rows[2].Kind != store.ThreadRequestSend {
		t.Fatalf("owed send row = %+v", listing.Rows[2])
	}
	// The child lists its own spawn on its own, unnested.
	if got := agentRowTokens(f.agentRows(t, child.ThreadID).Rows); !slices.Equal(got, []string{grand.Token}) {
		t.Fatalf("the child's own rows = %v", got)
	}
}

// The listing reads a settled answer without delivering it: the caller's
// agent is still owed its wake.
func TestThreadAgentRowsReadNoDelivery(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaude(t, "the rebuild took nine minutes")
	ctx, pending := withThreadToolsPending(t.Context())
	ack, err := f.adapter().Spawn(ctx, f.callerIdentity(), threadtools.SpawnCall{Prompt: "how long?"})
	pending.run(false)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	f.awaitRequestState(t, ack.Token, store.ThreadRequestFinished)

	rows := f.agentRows(t, f.caller.ID).Rows
	if len(rows) != 1 || rows[0].State != threadtools.RequestFinished || !strings.Contains(rows[0].Answer, "nine minutes") {
		t.Fatalf("rows = %+v; want the settled spawn with its answer", rows)
	}
	f.app.threadRequestsWG.Wait()
	if row := f.request(t, ack.Token); row.DeliveredAt != 0 || row.DeliveredHow != "" {
		t.Fatalf("listing the row delivered its answer: %q at %d", row.DeliveredHow, row.DeliveredAt)
	}
}

// Nesting stops at the depth bound, which the last row says it hides, and a
// spawn cycle lists each thread once.
func TestThreadAgentRowsBoundTheirDepthAndCycles(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	chain := []string{f.caller.ID}
	for range threadAgentRowDepth + 1 {
		chain = append(chain, f.plainThread(t).ID)
	}
	for index := 0; index+1 < len(chain); index++ {
		f.seedAgentRow(t, "chain-"+string(rune('a'+index)), chain[index], store.ThreadRequestSpawn, chain[index+1], false)
	}
	// The last thread spawns the caller back.
	f.seedAgentRow(t, "cycle", chain[len(chain)-1], store.ThreadRequestSpawn, f.caller.ID, false)

	rows := f.agentRows(t, f.caller.ID).Rows
	if len(rows) != threadAgentRowDepth {
		t.Fatalf("listed %d rows; want %d, one per level", len(rows), threadAgentRowDepth)
	}
	for depth, row := range rows {
		if row.Depth != depth {
			t.Fatalf("row %d at depth %d", depth, row.Depth)
		}
	}
	if last := rows[len(rows)-1]; !last.NestedHidden {
		t.Fatalf("the row at the bound does not say it hides nested rows: %+v", last)
	}
	// From the last thread the cycle comes back to the caller, which the
	// walk lists once and does not enter again.
	tail := f.agentRows(t, chain[len(chain)-1]).Rows
	if got := agentRowTokens(tail); len(got) != threadAgentRowDepth || got[0] != "cycle" {
		t.Fatalf("rows from the cycle's end = %v", got)
	}
}

// A person's Stop on a spawn cancels the request with the wake left armed,
// so the agent is told in words that name the user, and stops the work.
func TestThreadAgentRowStopCancelsForTheUser(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working")
	// The caller is mid-turn, so its wake waits in the queue to be read.
	if err := f.app.store.InsertTurn(store.Turn{
		TurnID: "caller-open", ThreadID: f.caller.ID, TurnIndex: 1, StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	ack := f.runningRequest(t, "rebuild the index")

	stop, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, ack.Token)
	if err != nil || stop.Outcome != threadAgentStopped || len(stop.Failures) != 0 {
		t.Fatalf("StopThreadAgentRow = %+v, %v", stop, err)
	}
	row := f.awaitRequestState(t, ack.Token, store.ThreadRequestCancelled)
	if !row.Notify {
		t.Fatal("a person's Stop disarmed the wake; the agent would never learn its child was stopped")
	}
	var wake string
	waitUntil(t, 10*time.Second, func() bool {
		for _, queued := range durableQueueRows(t, f.app, f.caller.ID) {
			if queued.SendID == threadWakeSendID(ack.Token) {
				wake = queued.Message
				return true
			}
		}
		return false
	})
	if !strings.Contains(wake, "was cancelled: The user stopped it") {
		t.Fatalf("the wake does not say the user stopped it: %q", wake)
	}
	if again, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, ack.Token); err != nil || again.Outcome != threadAgentEnded {
		t.Fatalf("a second Stop = %+v, %v; want ended", again, err)
	}
}

// The agent's own thread_cancel still disarms the wake: it reads the report.
func TestThreadCancelByTheAgentStillDisarmsTheWake(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working")
	ack := f.runningRequest(t, "rebuild the index")
	if _, err := f.adapter().Cancel(t.Context(), f.callerIdentity(), threadtools.CancelCall{Token: ack.Token}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if row := f.request(t, ack.Token); row.Notify || row.State != store.ThreadRequestCancelled {
		t.Fatalf("after the agent's cancel: notify %v, state %q", row.Notify, row.State)
	}
}

// Stop interrupts a target's running turn only when the row's caller spawned
// it: a thread it sent to belongs to someone else, and the cancel stops only
// the request.
func TestThreadAgentRowStopInterruptsOnlyASpawnedThread(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		kind        string
		interrupted bool
	}{{store.ThreadRequestSpawn, true}, {store.ThreadRequestSend, false}} {
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			interrupted := f.mockClaudeRecordingInterrupts(t, "busy with someone else's work")
			// Another thread's request keeps the target's turn running.
			busy := f.runningRequest(t, "someone else's work")
			stranger := f.plainThread(t)
			f.seedAgentRow(t, "row", stranger.ID, tc.kind, busy.ThreadID, true)
			waitUntil(t, 10*time.Second, func() bool {
				live, err := f.adapter().LiveState(t.Context(), busy.ThreadID)
				return err == nil && live.ActiveTurn
			})

			stop, err := f.app.StopThreadAgentRow(t.Context(), stranger.ID, "row")
			if err != nil || stop.Outcome != threadAgentStopped {
				t.Fatalf("StopThreadAgentRow = %+v, %v", stop, err)
			}
			sent := func() bool { _, err := os.Stat(interrupted); return err == nil }
			if tc.interrupted {
				waitUntil(t, 10*time.Second, sent)
				return
			}
			// The interrupt is answered by the provider: give one that was
			// sent the time to arrive before saying none was.
			time.Sleep(300 * time.Millisecond)
			if sent() {
				t.Fatal("a send's Stop interrupted a turn that was not its own")
			}
		})
	}
}

// mockClaudeRecordingInterrupts is mockClaudeHoldingTheTurn whose provider
// also creates the returned file when it is sent an interrupt.
func (f *requestFixture) mockClaudeRecordingInterrupts(t *testing.T, text string) string {
	t.Helper()
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), [][]string{{
		mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"msg-hold","role":"assistant","content":[{"type":"text","text":` + quoteJSON(text) + `}]}}`,
	}})
	// The binary is a link to a shared wrapper that runs the payload beside
	// it (mockexec); only the payload is this test's to change.
	payload := binary + ".payload"
	script, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "interrupted")
	patched := strings.Replace(string(script), "\"$reqid\"\n", "\"$reqid\"\n      touch '"+marker+"'\n", 1)
	if patched == string(script) {
		t.Fatal("the mock script has no interrupt answer to patch")
	}
	if err := os.WriteFile(payload, []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set claude binary: %v", err)
	}
	return marker
}

// Stop All stops every open row, nested ones included; a row a thread is
// not waiting on is refused.
func TestThreadAgentRowsStopAllCascades(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working")
	child := f.runningRequest(t, "lead the fix")
	grand, err := f.adapter().Spawn(t.Context(), f.targetIdentity(t, child.ThreadID), threadtools.SpawnCall{Prompt: "write the test", Notify: true})
	if err != nil {
		t.Fatalf("nested Spawn: %v", err)
	}
	stranger := f.plainThread(t)
	if _, err := f.app.StopThreadAgentRow(t.Context(), stranger.ID, child.Token); err == nil {
		t.Fatal("a thread stopped a request it is not waiting on")
	}

	stops, err := f.app.StopThreadAgentRows(t.Context(), f.caller.ID)
	if err != nil {
		t.Fatalf("StopThreadAgentRows: %v", err)
	}
	// One result per top-level row; the child's Stop reached its own spawn.
	if len(stops) != 1 || stops[0].Token != child.Token || stops[0].Outcome != threadAgentStopped ||
		stops[0].Nested != 1 || len(stops[0].Failures) != 0 {
		t.Fatalf("stops = %+v; want the child stopped with its one nested agent", stops)
	}
	f.awaitRequestState(t, child.Token, store.ThreadRequestCancelled)
	f.awaitRequestState(t, grand.Token, store.ThreadRequestCancelled)
}

// Every request transition reaches the caller's pane as a frame with the row
// as it stands, and so does the target becoming blocked on a person.
func TestThreadAgentRowsChangesAreEmitted(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.mockClaudeHoldingTheTurn(t, "working")
	ack := f.runningRequest(t, "rerun the deploy")
	frame := func(want func(ThreadAgentRowChange) bool) ThreadAgentRowChange {
		t.Helper()
		var found ThreadAgentRowChange
		waitUntil(t, 10*time.Second, func() bool {
			f.bus.mu.Lock()
			defer f.bus.mu.Unlock()
			for _, event := range f.bus.all {
				if event.Name != "thread:agent_rows_changed" {
					continue
				}
				change, ok := event.Data.(ThreadAgentRowChange)
				if ok && want(change) {
					found = change
					return true
				}
			}
			return false
		})
		return found
	}
	running := frame(func(c ThreadAgentRowChange) bool {
		return c.ID == ack.Token && c.Row != nil && c.Row.State == threadtools.RequestRunning
	})
	if running.ThreadID != f.caller.ID {
		t.Fatalf("frame addressed to %q; want the caller", running.ThreadID)
	}

	approval, err := json.Marshal(provider.ApprovalRequest{RequestID: "approval-1", ToolName: "Bash", Description: "rm -rf build", Kind: "command"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventApprovalRequest, ThreadID: ack.ThreadID, ItemID: "approval-1", Meta: approval, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("approval request: %v", err)
	}
	blocked := frame(func(c ThreadAgentRowChange) bool {
		return c.ID == ack.Token && c.Row != nil && c.Row.State == threadtools.RequestBlocked
	})
	if blocked.Row.BlockedOn != "approval" || !strings.Contains(blocked.Row.BlockedDetail, "Bash") {
		t.Fatalf("blocked row = %+v; want what it is blocked on", blocked.Row)
	}

	if _, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, ack.Token); err != nil {
		t.Fatalf("StopThreadAgentRow: %v", err)
	}
	frame(func(c ThreadAgentRowChange) bool {
		return c.ID == ack.Token && c.Row != nil && c.Row.State == threadtools.RequestCancelled
	})
}

// The thread's own Stop ends its turn and leaves the threads it is waiting
// on running: a Stop does not quiet later wakes.
func TestThreadStopLeavesItsAgentRows(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working")
	child := f.runningRequest(t, "lead the fix")
	if err := f.app.interruptTurnCtx(t.Context(), f.caller.ID); err != nil {
		t.Fatalf("interrupt the caller: %v", err)
	}
	if row := f.request(t, child.Token); row.State != store.ThreadRequestRunning || !row.Notify {
		t.Fatalf("after the caller's Stop the child's request is %q, notify %v", row.State, row.Notify)
	}
	if rows := f.agentRows(t, f.caller.ID).Rows; len(rows) != 1 || rows[0].State != threadtools.RequestRunning {
		t.Fatalf("rows after the caller's Stop = %+v", rows)
	}
}
