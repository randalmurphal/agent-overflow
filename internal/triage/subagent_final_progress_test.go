package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// Final-progress persistence (docs/specs/agent-visibility.md, Q5
// fold-in). Live ticks are never persisted; the FINAL numbers land on
// the launch row at whichever terminal settles it, so a reloaded thread
// still shows what the agent spent. There are three such terminals and
// each one is pinned below.

func persistedProgressFor(t *testing.T, st *store.Store, threadID, itemID string) (provider.SubagentProgressMeta, bool) {
	t.Helper()
	item, found, err := st.GetThreadItem(threadID, itemID)
	if err != nil || !found {
		t.Fatalf("launch %s lookup: found=%v err=%v", itemID, found, err)
	}
	var decoded struct {
		Progress *provider.SubagentProgressMeta `json:"subagentProgress"`
	}
	if err := json.Unmarshal([]byte(item.Meta), &decoded); err != nil {
		t.Fatalf("decode meta %q: %v", item.Meta, err)
	}
	if decoded.Progress == nil {
		return provider.SubagentProgressMeta{}, false
	}
	return *decoded.Progress, true
}

func tickProgress(t *testing.T, r *Router, threadID, itemID string, meta provider.SubagentProgressMeta) {
	t.Helper()
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentProgress, ThreadID: threadID, ItemID: itemID,
		Meta: raw, Timestamp: time.UnixMilli(1_700_000_000_000),
	}); err != nil {
		t.Fatalf("progress tick: %v", err)
	}
}

// TestInlineAgentCompletionPersistsFinalProgress pins the AWAITED
// launch's terminal: an inline Agent settles through
// persistToolCallCompletion and nowhere else, so that is the only place
// its counters can become durable.
func TestInlineAgentCompletionPersistsFinalProgress(t *testing.T) {
	r, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "Agent",
		"input":    map[string]any{"description": "review the diff"},
	})
	if err := r.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "toolu_agent",
		ItemType: "Agent", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("launch: %v", err)
	}
	// The agent's own work is what makes the row a LAUNCH structurally.
	if err := r.persistItem(store.Item{
		ID: "toolu_child_read", ThreadID: "t1", Kind: itemKindToolCall, Role: "assistant",
		Status: statusCompleted, ToolName: "Read", Summary: "Read: main.go",
		ParentID: "toolu_agent", CreatedAt: 2, UpdatedAt: 2,
	}, nil); err != nil {
		t.Fatal(err)
	}
	tickProgress(t, r, "t1", "toolu_agent", provider.SubagentProgressMeta{
		TaskID: "task-1", ToolUses: 7, TotalTokens: 4200, DurationMs: 31000,
		Activity: "Reading main.go", AgentType: "code-review",
	})

	completeMeta, _ := json.Marshal(map[string]any{"toolName": "Agent"})
	if err := r.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "toolu_agent",
		Meta: completeMeta, Content: "looks good", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	progress, ok := persistedProgressFor(t, st, "t1", "toolu_agent")
	if !ok {
		t.Fatal("inline agent completion did not persist final progress")
	}
	if progress.ToolUses != 7 || progress.TotalTokens != 4200 || progress.DurationMs != 31000 || progress.AgentType != "code-review" {
		t.Fatalf("final progress = %+v", progress)
	}
	// The live activity line is a "right now" statement; a settled row
	// must not keep claiming the agent is reading main.go.
	if progress.Activity != "" {
		t.Fatalf("final progress kept the live activity line: %q", progress.Activity)
	}
	if _, live := r.PeekSubagentProgress("t1", "toolu_agent"); live {
		t.Fatal("the live entry must be consumed at the terminal")
	}
}

// TestOrdinaryToolCompletionNeverPersistsProgress pins the narrowing:
// the inline terminal settles every ordinary tool too, and a Read must
// never come out of it wearing agent counters. The launch predicate is
// structural, so a row with nothing attributed to it is not a launch
// however the tick was addressed.
func TestOrdinaryToolCompletionNeverPersistsProgress(t *testing.T) {
	r, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "Read",
		"input":    map[string]any{"file_path": "/tmp/main.go"},
	})
	if err := r.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "toolu_read",
		ItemType: "Read", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("launch: %v", err)
	}
	tickProgress(t, r, "t1", "toolu_read", provider.SubagentProgressMeta{ToolUses: 3, TotalTokens: 99})

	completeMeta, _ := json.Marshal(map[string]any{"toolName": "Read"})
	if err := r.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "toolu_read",
		Meta: completeMeta, Content: "file body", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	if progress, ok := persistedProgressFor(t, st, "t1", "toolu_read"); ok {
		t.Fatalf("a plain tool row was stamped with agent counters: %+v", progress)
	}
	if _, live := r.PeekSubagentProgress("t1", "toolu_read"); live {
		t.Fatal("the misaddressed live entry must be dropped, not left to leak")
	}
}

// TestBackgroundCompletionSiblingPersistsFinalProgress pins the
// BACKGROUND launch's terminal. The launch row itself stays `running`
// forever (invariant 24), so the sibling write is the only moment its
// counters can settle.
func TestBackgroundCompletionSiblingPersistsFinalProgress(t *testing.T) {
	r, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Agent",
		"is_background": true,
		"task_id":       "task-bg",
		"input":         map[string]any{"description": "background review"},
	})
	if err := r.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "toolu_bg_agent",
		ItemType: "Agent", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("launch: %v", err)
	}
	tickProgress(t, r, "t1", "toolu_bg_agent", provider.SubagentProgressMeta{
		TaskID: "task-bg", ToolUses: 12, TotalTokens: 88000, Activity: "Running tests",
	})

	terminalMeta, _ := json.Marshal(map[string]any{
		"task_id": "task-bg", "tool_use_id": "toolu_bg_agent",
		"status": "completed", "source": "task_output",
	})
	if err := r.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "toolu_bg_agent",
		Meta: terminalMeta, Content: "done", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("terminal: %v", err)
	}

	if dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone); len(dones) != 1 {
		t.Fatalf("expected the completion sibling, got %d", len(dones))
	}
	progress, ok := persistedProgressFor(t, st, "t1", "toolu_bg_agent")
	if !ok {
		t.Fatal("background terminal did not persist final progress onto the launch")
	}
	if progress.ToolUses != 12 || progress.TotalTokens != 88000 || progress.Activity != "" {
		t.Fatalf("final progress = %+v", progress)
	}
	if _, live := r.PeekSubagentProgress("t1", "toolu_bg_agent"); live {
		t.Fatal("the live entry must be consumed at the terminal")
	}
}

// TestCodexChildTerminalPersistsFinalProgress pins the third terminal:
// a Codex child going terminal is where the spawn card's token counters
// stop moving, and the tick that lands them came from the child's own
// unsuppressed `thread/tokenUsage/updated`.
func TestCodexChildTerminalPersistsFinalProgress(t *testing.T) {
	r, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, r, st, "t1", 0)
	seedCodexSpawnCard(t, r, st, "t1", "spawn-1", "child-1")

	tickProgress(t, r, "t1", "spawn-1", provider.SubagentProgressMeta{
		TaskID: "child-1", TotalTokens: 15200,
	})
	deliverMailbox(t, r, "t1", "spawn-1", mailboxDelivery(t, "/root/reviewer", "Done."))

	progress, ok := persistedProgressFor(t, st, "t1", "spawn-1")
	if !ok {
		t.Fatal("codex child terminal did not persist final progress")
	}
	if progress.TaskID != "child-1" || progress.TotalTokens != 15200 {
		t.Fatalf("final progress = %+v", progress)
	}
	if _, live := r.PeekSubagentProgress("t1", "spawn-1"); live {
		t.Fatal("the live entry must be consumed at the terminal")
	}
}

// TestBackgroundCompletionSiblingLeavesTheLaunchSettled pins the write
// ORDER at that same terminal. writeBackgroundCompletionSibling inserts
// the sibling, and inserting a completion is what stamps the launch
// `live_background_active=false` (migration v74,
// store/background_settle_triggers.go). persistFinalSubagentProgress
// then rewrites the launch's meta WHOLESALE from an in-memory copy read
// BEFORE that insert. Without the AFTER UPDATE leg of the trigger set,
// that second write restores the launch to "live" and leaves it in
// every partial live index forever — which is the shape the whole
// settlement change exists to remove.
func TestBackgroundCompletionSiblingLeavesTheLaunchSettled(t *testing.T) {
	r, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Agent",
		"is_background": true,
		"task_id":       "task-settle",
		"input":         map[string]any{"description": "background review"},
	})
	if err := r.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "toolu_settle",
		ItemType: "Agent", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if live, err := st.HasLiveBackgroundToolCall("t1"); err != nil || !live {
		t.Fatalf("a just-launched background agent must read as live: live=%v err=%v", live, err)
	}
	tickProgress(t, r, "t1", "toolu_settle", provider.SubagentProgressMeta{
		TaskID: "task-settle", ToolUses: 3, TotalTokens: 4200,
	})

	terminalMeta, _ := json.Marshal(map[string]any{
		"task_id": "task-settle", "tool_use_id": "toolu_settle",
		"status": "completed", "source": "task_output",
	})
	if err := r.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "toolu_settle",
		Meta: terminalMeta, Content: "done", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("terminal: %v", err)
	}

	launch, found, err := st.GetThreadItem("t1", "toolu_settle")
	if err != nil || !found {
		t.Fatalf("launch row: found=%v err=%v", found, err)
	}
	// Invariant 24: the launch itself never leaves `running`.
	if launch.Status != "running" {
		t.Fatalf("launch status = %q, want running", launch.Status)
	}
	var meta struct {
		LiveBackgroundActive *bool `json:"live_background_active"`
	}
	if err := json.Unmarshal([]byte(launch.Meta), &meta); err != nil {
		t.Fatalf("decode launch meta %q: %v", launch.Meta, err)
	}
	if meta.LiveBackgroundActive == nil || *meta.LiveBackgroundActive {
		t.Fatalf("the wholesale meta rewrite un-settled the launch: meta = %s", launch.Meta)
	}

	// The stamp must not have cost the progress the same write persisted.
	progress, ok := persistedProgressFor(t, st, "t1", "toolu_settle")
	if !ok {
		t.Fatal("background terminal did not persist final progress onto the launch")
	}
	if progress.ToolUses != 3 || progress.TotalTokens != 4200 {
		t.Fatalf("final progress = %+v", progress)
	}

	if live, err := st.HasLiveBackgroundToolCall("t1"); err != nil || live {
		t.Fatalf("a settled launch must not read as live: live=%v err=%v", live, err)
	}
}
