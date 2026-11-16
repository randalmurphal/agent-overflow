package app

// A spawn's row counts what its thread did (docs/specs/standing-agents.md,
// "Agents segment"): each tool call once, the tokens of its turns and its
// own Codex agents, and the time its turns ran, refreshed as each moves.

import (
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// A running worker's row is framed again for each new tool call, one whose
// line reads as the last one did included, for its usage's progress and
// for its Codex agent's; the agent's spend counts in its tokens until its
// completion records it, and once.
func TestAWorkerRowRefreshesAsItsCountsMove(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	worker := f.settledSpawn(t, "counts-spawn", f.caller.ID)
	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: worker.ID + ":t1"}, nil)
	f.awaitWorker(t, f.caller.ID, "counts-spawn", workerRunning)
	framed := func(what string, change func()) {
		t.Helper()
		f.app.threadRequestsWG.Wait()
		before := f.callerAgentFrames(t, f.caller.ID)
		change()
		waitUntil(t, 10*time.Second, func() bool { return f.callerAgentFrames(t, f.caller.ID) > before })
		_ = what
	}
	framed("a tool call", func() { workerToolCall(t, f.app, worker.ID, "first", "make check") })
	framed("the same call again", func() { workerToolCall(t, f.app, worker.ID, "second", "make check") })
	row := f.awaitWorker(t, f.caller.ID, "counts-spawn", workerRunning)
	if row.ToolCalls != 2 {
		t.Fatalf("row %+v; want both calls counted", row)
	}
	framed("usage progress", func() {
		f.app.tapThreadAgentRows(eventchan.ProviderUsage, provider.UsageEvent{Action: "progress", ThreadID: worker.ID, UsedTokens: 10})
	})
	framed("an agent's progress", func() {
		workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventSubagentProgress, ItemID: "agent-launch"},
			map[string]any{"taskId": "child", "totalTokens": 500, "outsideParentUsage": true})
	})
	if row := f.awaitWorker(t, f.caller.ID, "counts-spawn", workerRunning); row.Tokens != 500 {
		t.Fatalf("row %+v; want the running agent's 500 tokens", row)
	}
	// Its completion records 700: counted once, the live entry past it adding
	// nothing.
	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventSubagentProgress, ItemID: "agent-launch"},
		map[string]any{"taskId": "child", "totalTokens": 700, "outsideParentUsage": true})
	now := time.Now().UnixMilli()
	if err := f.app.store.InsertItem(store.Item{ID: "agent-done", ThreadID: worker.ID, TurnIndex: 90, ItemIndex: 1, Kind: "tool_completion", Role: "assistant", Status: "completed",
		CompletionOf: "agent-launch", Meta: `{"subagentProgress":{"totalTokens":700,"outsideParentUsage":true}}`, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if row := f.awaitWorker(t, f.caller.ID, "counts-spawn", workerRunning); row.Tokens != 700 {
		t.Fatalf("row %+v; want the agent's 700 tokens once", row)
	}
}

// A worker's runtime is the time its turns ran: the open turn's so far,
// then each ended turn's, never the idle time between them, and never less
// than it read before.
func TestAWorkerRowsRuntimeIsItsTurnsTime(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	worker := f.settledSpawn(t, "runtime-spawn", f.caller.ID)
	started := time.Now()
	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: worker.ID + ":t1"}, nil)
	time.Sleep(300 * time.Millisecond)
	running := f.awaitWorker(t, f.caller.ID, "runtime-spawn", workerRunning)
	if !running.TurnOpen || running.RuntimeMs < 300 || running.RuntimeMs > time.Since(started).Milliseconds()+50 {
		t.Fatalf("running row %+v; want its open turn's time so far", running)
	}
	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"}}, nil)
	f.expectRested(t, "runtime-spawn")
	index, err := f.app.store.LastTurnIndex(worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, ok, err := f.app.store.GetTurnByThreadIndex(worker.ID, index)
	if err != nil || !ok || first.CompletedAt == nil {
		t.Fatalf("first turn %+v, %v, %v", first, ok, err)
	}
	ran := *first.CompletedAt - first.StartedAt
	past := func() ThreadAgentRow {
		t.Helper()
		page, err := f.app.ListThreadPastAgentRows(f.caller.ID, store.PastAgentRowsCursor{}, 8)
		if err != nil {
			t.Fatal(err)
		}
		row, _ := agentRowByToken(page.Rows, "runtime-spawn")
		return row
	}
	if row := past(); row.TurnOpen || row.RuntimeMs != ran || ran < running.RuntimeMs {
		t.Fatalf("rested row %+v; want exactly its turn's %dms, at least the %dms read while it ran", row, ran, running.RuntimeMs)
	}
	// Idle time is not runtime.
	time.Sleep(300 * time.Millisecond)
	if row := past(); row.RuntimeMs != ran {
		t.Fatalf("row after idling %+v; want still %dms", row, ran)
	}
	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: worker.ID + ":t2"}, nil)
	again := f.awaitWorker(t, f.caller.ID, "runtime-spawn", workerRunning)
	if !again.TurnOpen || again.RuntimeMs < ran || again.RuntimeMs > ran+time.Since(started).Milliseconds() {
		t.Fatalf("row in its second turn %+v; want %dms and the new turn's time so far", again, ran)
	}
}
