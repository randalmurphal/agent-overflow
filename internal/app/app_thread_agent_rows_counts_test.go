package app

// A spawn's row counts what its thread did (docs/specs/standing-agents.md,
// "Agents segment"): each tool call once, the tokens it spent itself, and
// the time its turns ran, refreshed as each moves. Its native agents are
// rows of their own with counts of their own.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// A running worker's row is framed again for each new tool call, one whose
// line reads as the last one did included, and for its usage's progress.
// Its tokens are its own: a Codex child's spend, and a nested child's under
// it, move nothing, live or recorded; its own turn's settlement moves them
// by its own spend.
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
	// A child and a child of it spend, live and then on their completion
	// records: the worker's tokens stay where they were.
	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventSubagentProgress, ItemID: "agent-launch"},
		map[string]any{"taskId": "child", "totalTokens": 500})
	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventSubagentProgress, ItemID: "nested-launch", ParentToolUseID: "agent-launch"},
		map[string]any{"taskId": "grandchild", "totalTokens": 300})
	now := time.Now().UnixMilli()
	for i, done := range []struct{ id, launch string }{{"agent-done", "agent-launch"}, {"nested-done", "nested-launch"}} {
		if err := f.app.store.InsertItem(store.Item{ID: done.id, ThreadID: worker.ID, TurnIndex: 90, ItemIndex: 1 + i, Kind: "tool_completion", Role: "assistant", Status: "completed",
			CompletionOf: done.launch, Meta: `{"subagentProgress":{"totalTokens":700}}`, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	f.app.threadRequestsWG.Wait()
	if row := f.awaitWorker(t, f.caller.ID, "counts-spawn", workerRunning); row.Tokens != 0 {
		t.Fatalf("row %+v; want none of its agents' tokens", row)
	}
	// Its own turn settles: 40 fresh input, 5 cache writes, 15 output,
	// what the provider names its own.
	own := provider.TokenUsage{InputTokens: 40, CacheReadInputTokens: 900, CacheCreationInputTokens: 5, OutputTokens: 15}
	framed("its own settlement", func() {
		workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{
			StopReason: "end_turn", UsageScope: "codex-process", Usage: &own, OwnUsage: &own,
			ModelUsage: []provider.ModelTokenUsage{{Model: "gpt-5.2-codex", TokenUsage: own}},
		}}, nil)
	})
	f.expectRested(t, "counts-spawn")
	tallies, err := f.app.store.ThreadTallies([]string{worker.ID}, time.Now())
	if err != nil || tallies[worker.ID].Tokens != 60 {
		t.Fatalf("tallies %+v, %v; want the worker's own 60", tallies, err)
	}
}

// A worker that delegates to a native subagent counts its own spend: the
// captured delegating turn (subagent_usage_inclusion_20260703.ndjson) shows
// the parent-only 23,369 on its spawn's row, while the billing ledger keeps
// the subagent-inclusive 36,709.
func TestADelegatingWorkerRowCountsItsOwnSpend(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerSession()
	data, err := os.ReadFile(filepath.Join("docs", "references", "fixtures", "claude", "subagent_usage_inclusion_20260703.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	turn := strings.Split(strings.TrimSpace(string(data)), "\n")
	installMockClaudeTurns(t, f.app, [][]string{turn})
	ack := f.spawnWorker(t, "work out 17*23 with a helper")
	var row ThreadAgentRow
	waitUntil(t, 20*time.Second, func() bool {
		row, _ = agentRowByToken(f.agentRows(t, f.caller.ID).Rows, ack.Token)
		return row.Tokens == 23369
	})
	billed, err := f.app.store.QueryUsage(store.UsageQuery{ThreadID: ack.ThreadID})
	if err != nil || len(billed) != 1 {
		t.Fatalf("ledger %+v, %v", billed, err)
	}
	if spend := billed[0].InputTokens + billed[0].CacheCreationInputTokens + billed[0].OutputTokens; spend != 36709 {
		t.Fatalf("ledger spends %d; want the subagent-inclusive 36,709", spend)
	}
}

// A worker whose turn a crash left open shows, under Past after the boot,
// the time it ran to the last activity that turn recorded, not the time
// the app was down; a later reopen and end of that turn counts it once.
func TestACrashedWorkersPastRuntimeEndsAtItsLastActivity(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	worker := f.settledSpawn(t, "crash-spawn", f.caller.ID)
	base := time.Now().Add(-2 * time.Hour).UnixMilli()
	if err := f.app.store.InsertTurn(store.Turn{TurnID: worker.ID + ":0", ThreadID: worker.ID, TurnIndex: 0, StartedAt: base}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.store.UpdateTurnCompleted(worker.ID+":0", base+60_000, "end_turn", "", "", ""); err != nil {
		t.Fatal(err)
	}
	start := base + 120_000
	if err := f.app.store.InsertTurn(store.Turn{TurnID: worker.ID + ":1", ThreadID: worker.ID, TurnIndex: 1, StartedAt: start}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.store.InsertItem(store.Item{ID: "crash-step", ThreadID: worker.ID, TurnIndex: 1, Kind: "tool_call", Role: "assistant",
		Status: "running", Summary: "Running tests", CreatedAt: start + 10_000, UpdatedAt: start + 30_000}); err != nil {
		t.Fatal(err)
	}

	after := f.restart(t)
	if _, err := after.app.triage.RecoverCrashedTurns(); err != nil {
		t.Fatal(err)
	}
	past := func() ThreadAgentRow {
		t.Helper()
		page, err := after.app.ListThreadPastAgentRows(f.caller.ID, store.PastAgentRowsCursor{}, 8)
		if err != nil {
			t.Fatal(err)
		}
		row, ok := agentRowByToken(page.Rows, "crash-spawn")
		if !ok {
			t.Fatalf("no Past row in %+v", page.Rows)
		}
		return row
	}
	if row := past(); row.TurnOpen || row.RuntimeMs != 90_000 {
		t.Fatalf("Past row after the boot %+v; want 90s: 60s and the crashed turn's 30s, not the downtime", row)
	}
	if err := after.app.store.ReopenTurn(worker.ID + ":1"); err != nil {
		t.Fatal(err)
	}
	if err := after.app.store.UpdateTurnCompleted(worker.ID+":1", start+35_000, "end_turn", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if row := past(); row.TurnOpen || row.RuntimeMs != 95_000 {
		t.Fatalf("Past row once the turn ended again %+v; want 95s, counted once", row)
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
