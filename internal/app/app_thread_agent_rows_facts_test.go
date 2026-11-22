package app

// A spawn's row counts what its thread did, as a background subagent's row
// counts its run (docs/specs/standing-agents.md, "Agents segment"): its
// top-level tool calls and the tokens it spent itself, and, while it is not
// at rest, its latest tool call as the activity line.

import (
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// workerToolCall is a top-level Bash call of threadID's running turn.
func workerToolCall(t *testing.T, a *App, threadID, id, command string) {
	t.Helper()
	workerEvent(t, a, threadID, provider.ProviderEvent{Kind: provider.EventToolStart, ItemID: id, ItemType: "Bash"},
		map[string]any{"toolName": "Bash", "input": map[string]any{"command": command}})
}

// lastToolLine is the activity line threadID's latest top-level tool call
// reads as.
func lastToolLine(t *testing.T, a *App, threadID string) string {
	t.Helper()
	row, found, err := a.store.LastTopLevelItem(threadID, "tool_call")
	if err != nil || !found {
		t.Fatalf("last tool call of %s: found %v, %v", threadID, found, err)
	}
	return clipLine(row.Summary)
}

// While the worker runs, its row carries its tool calls, the tokens its
// settled turns and its running one spent, and its latest tool call as its
// line; a message
// it writes after the call leaves the line on the call. Once it rests, Past
// keeps the counts and no line. A thread that did nothing carries no counts.
func TestAWorkerRowCountsWhatItsThreadDid(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	idle := f.settledSpawn(t, "idle-spawn", f.caller.ID)
	worker := f.settledSpawn(t, "facts-spawn", f.caller.ID)
	// A settled turn's usage, its own spend as its settlement reports it.
	settled := store.UsageLedgerRow{CreatedAt: 1, ThreadID: worker.ID, TurnID: "t0", Provider: "claude", Model: "m",
		InputTokens: 1000, OutputTokens: 200, CacheReadInputTokens: 15000, CacheCreationInputTokens: 2000}
	if err := f.app.store.AppendUsageAndReconcile("t0", []store.UsageLedgerRow{settled}, store.OwnSpend{ThreadID: worker.ID, Tokens: 3200}); err != nil {
		t.Fatalf("append usage: %v", err)
	}

	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: worker.ID + ":t1"}, nil)
	// The row follows the worker once it is recorded running.
	f.awaitWorker(t, f.caller.ID, "facts-spawn", workerRunning)
	workerToolCall(t, f.app, worker.ID, "grep-call", "grep -rn deploy scripts")
	workerToolCall(t, f.app, worker.ID, "make-call", "make deploy-check")
	if _, err := f.app.store.PutUsageProgress(worker.ID, "t1", []store.UsageLedgerRow{{CreatedAt: 2, ThreadID: worker.ID, TurnID: "t1",
		Provider: "claude", Model: "m", InputTokens: 30, OutputTokens: 10}}); err != nil {
		t.Fatalf("put usage progress: %v", err)
	}
	// Its message, settled after the calls, as the item frame announces it.
	f.app.tapThreadAgentRows(eventchan.ProviderItemEvent, triage.ItemStreamEvent{Action: "upsert", ThreadID: worker.ID, Item: &store.Item{
		ID: "said", ThreadID: worker.ID, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "The deploy check passes.",
	}})
	line := lastToolLine(t, f.app, worker.ID)
	if line == "" {
		t.Fatal("the tool call has no summary; this test needs one")
	}
	var row ThreadAgentRow
	waitUntil(t, 10*time.Second, func() bool {
		row = f.awaitWorker(t, f.caller.ID, "facts-spawn", workerRunning)
		return row.Activity != ""
	})
	// The message came after the call: had it reached the line, it would
	// be there by the time the row reads it.
	f.app.threadRequestsWG.Wait()
	row = f.awaitWorker(t, f.caller.ID, "facts-spawn", workerRunning)
	if row.ToolCalls != 2 || row.Tokens != 3200+40 || row.Activity != line {
		t.Fatalf("running row %+v; want 2 tool calls, 3240 tokens (the cache reads no spend) and the line %q", row, line)
	}

	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"}}, nil)
	f.expectRested(t, "facts-spawn")
	past, err := f.app.ListThreadPastAgentRows(t.Context(), f.caller.ID, store.PastAgentRowsCursor{}, 8)
	if err != nil {
		t.Fatal(err)
	}
	rested, _ := agentRowByToken(past.Rows, "facts-spawn")
	if rested.ToolCalls != 2 || rested.Tokens != 3240 || rested.Activity != "" {
		t.Fatalf("past row %+v; want the final counts and no line", rested)
	}
	if untouched, _ := agentRowByToken(past.Rows, "idle-spawn"); untouched.ThreadID != idle.ID || untouched.ToolCalls != 0 || untouched.Tokens != 0 {
		t.Fatalf("idle past row %+v; want no counts", untouched)
	}
}

// A paired computer reports its worker's counts and latest tool call with
// its rows, for a thread its spawn made that keeps working after it
// replied; the spawner's row shows them. A computer that reports none, as
// an older one, leaves the row without them.
func TestARemoteWorkerRowCountsWhatItsThreadDid(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	pair.destBus.mu.Lock()
	pair.destBus.tap = pair.dest.tapThreadAgentRows
	pair.destBus.mu.Unlock()
	agentPair, child := remoteRunningWorker(t, pair)
	// Its spawn settled with the reply: no request runs on it there.
	if _, running := pair.dest.runningReceiptTokens(child); running {
		t.Fatal("a request still runs on the worker; this test needs it settled")
	}
	workerToolCall(t, pair.dest, child, "tail-call", "tail -n 50 deploy.log")
	line := lastToolLine(t, pair.dest, child)
	row := agentPair.askAgainUntilRow(t, func(row ThreadAgentRow, listed bool) bool {
		return listed && row.Activity == line
	})
	if row.ToolCalls != 1 || row.Worker != workerRunning {
		t.Fatalf("row %+v; want the worker running with its one tool call and its line", row)
	}
	// Its open turn, named as that computer recorded its start.
	if turn, active, err := pair.dest.store.GetActiveTurn(child); err != nil || !active || !row.TurnOpen || row.TurnStartedAt != turn.StartedAt {
		t.Fatalf("row %+v; want its open turn %+v (%v, %v) named by its start", row, turn, active, err)
	}

	parent := ThreadAgentRow{Token: agentPair.spawn.Token, Worker: workerRunning}
	pair.source.nestPeerAgentRows(&parent, threadAgentRemoteSlot{spawn: agentPair.spawn, shown: agentPair.spawn},
		threadAgentPeerAnswer{listing: ThreadAgentRows{Worker: workerRunning}}, threadAgentRowDepth)
	if parent.ToolCalls != 0 || parent.Tokens != 0 || parent.Activity != "" {
		t.Fatalf("row from an answer without counts %+v; want none", parent)
	}
}
