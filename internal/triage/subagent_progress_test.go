package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

type subagentTestHarness struct {
	router   *Router
	store    *store.Store
	emits    *emissionLog
	threadID string
}

func newSubagentTestHarness(t *testing.T) subagentTestHarness {
	t.Helper()
	router, st, emits := newTestRouter(t)
	createTestThread(t, st, "t1")
	return subagentTestHarness{router: router, store: st, emits: emits, threadID: "t1"}
}

func (h subagentTestHarness) emitsNamed(name string) []any {
	var out []any
	for _, e := range h.emits.snapshot() {
		if e.eventName == name {
			out = append(out, e.data)
		}
	}
	return out
}

func subagentProgressEvent(t *testing.T, threadID, itemID, parentID string, meta provider.SubagentProgressMeta) provider.ProviderEvent {
	t.Helper()
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	return provider.ProviderEvent{
		Kind:            provider.EventSubagentProgress,
		ThreadID:        threadID,
		ItemID:          itemID,
		ParentToolUseID: parentID,
		Meta:            raw,
		Timestamp:       time.UnixMilli(1_700_000_000_000),
	}
}

func TestSubagentProgressMergesTicksAndEmitsLiveOnly(t *testing.T) {
	h := newSubagentTestHarness(t)
	r := h.router
	threadID := h.threadID

	first := subagentProgressEvent(t, threadID, "toolu_launch", "", provider.SubagentProgressMeta{
		TaskID: "task-1", ToolUses: 1, TotalTokens: 100, DurationMs: 500, Activity: "Reading main.go", LastToolName: "Read", AgentType: "general-purpose",
	})
	if err := r.Handle(first); err != nil {
		t.Fatal(err)
	}
	// Codex-shaped tick: tokens only. Must not zero the Claude counters.
	second := subagentProgressEvent(t, threadID, "toolu_launch", "", provider.SubagentProgressMeta{TotalTokens: 250})
	if err := r.Handle(second); err != nil {
		t.Fatal(err)
	}

	emits := h.emitsNamed("provider:subagent_progress")
	if len(emits) != 2 {
		t.Fatalf("expected 2 progress emits, got %d", len(emits))
	}
	last, ok := emits[1].(SubagentProgressEvent)
	if !ok {
		t.Fatalf("unexpected emit payload %T", emits[1])
	}
	if last.ItemID != "toolu_launch" || last.Progress.ToolUses != 1 || last.Progress.TotalTokens != 250 || last.Progress.Activity != "Reading main.go" {
		t.Fatalf("merged tick wrong: %+v", last.Progress)
	}

	if items, err := h.store.ListItems(threadID); err != nil || len(items) != 0 {
		t.Fatalf("progress ticks must not persist rows, got %d (err %v)", len(items), err)
	}

	got, ok := r.PeekSubagentProgress(threadID, "toolu_launch")
	if !ok || got.TotalTokens != 250 {
		t.Fatalf("peek: ok=%v %+v", ok, got)
	}
	taken, ok := r.TakeSubagentProgress(threadID, "toolu_launch")
	if !ok || taken.TotalTokens != 250 {
		t.Fatalf("take: ok=%v %+v", ok, taken)
	}
	if _, ok := r.PeekSubagentProgress(threadID, "toolu_launch"); ok {
		t.Fatal("take must clear the entry")
	}
}

func TestSubagentProgressSweptOnThreadCleanup(t *testing.T) {
	h := newSubagentTestHarness(t)
	r := h.router
	if err := r.Handle(subagentProgressEvent(t, h.threadID, "toolu_a", "", provider.SubagentProgressMeta{ToolUses: 3})); err != nil {
		t.Fatal(err)
	}
	r.CleanupThread(h.threadID)
	if _, ok := r.PeekSubagentProgress(h.threadID, "toolu_a"); ok {
		t.Fatal("cleanup must drop the thread's live progress")
	}
}

func TestPersistSubagentFinalProgressFoldsLiveAndFinal(t *testing.T) {
	h := newSubagentTestHarness(t)
	r := h.router
	launch := store.Item{
		ID: "toolu_launch", ThreadID: h.threadID, Kind: itemKindToolCall, Role: "assistant",
		Status: statusRunning, ToolName: "Agent", Summary: "Agent: review", Meta: `{"input":{"description":"review"}}`,
		CreatedAt: 1, UpdatedAt: 1,
	}
	if err := r.persistItem(launch, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.Handle(subagentProgressEvent(t, h.threadID, launch.ID, "", provider.SubagentProgressMeta{ToolUses: 4, TotalTokens: 900, Activity: "Running tests"})); err != nil {
		t.Fatal(err)
	}
	if err := r.persistSubagentFinalProgress(launch, provider.SubagentProgressMeta{TotalTokens: 1200, DurationMs: 8000}); err != nil {
		t.Fatal(err)
	}
	persisted, ok, err := h.store.GetThreadItem(h.threadID, launch.ID)
	if err != nil || !ok {
		t.Fatalf("launch lookup: ok=%v err=%v", ok, err)
	}
	var meta struct {
		Input            map[string]any                `json:"input"`
		SubagentProgress provider.SubagentProgressMeta `json:"subagentProgress"`
	}
	if err := json.Unmarshal([]byte(persisted.Meta), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Input["description"] != "review" {
		t.Fatalf("existing meta must survive: %s", persisted.Meta)
	}
	p := meta.SubagentProgress
	if p.ToolUses != 4 || p.TotalTokens != 1200 || p.DurationMs != 8000 || p.Activity != "" {
		t.Fatalf("final progress wrong: %+v", p)
	}
	if _, live := r.PeekSubagentProgress(h.threadID, launch.ID); live {
		t.Fatal("final persistence must consume the live entry")
	}

	// A second terminal path (the task_notification's authoritative usage
	// after the task_updated terminal) merges over what is persisted.
	launch.Meta = persisted.Meta
	if err := r.persistSubagentFinalProgress(launch, provider.SubagentProgressMeta{ToolUses: 5, TotalTokens: 1300}); err != nil {
		t.Fatal(err)
	}
	persisted, _, _ = h.store.GetThreadItem(h.threadID, launch.ID)
	if err := json.Unmarshal([]byte(persisted.Meta), &meta); err != nil {
		t.Fatal(err)
	}
	p = meta.SubagentProgress
	if p.ToolUses != 5 || p.TotalTokens != 1300 || p.DurationMs != 8000 {
		t.Fatalf("second terminal must merge over persisted numbers: %+v", p)
	}
}

func TestSubagentBackgroundedStampsLaunchOnce(t *testing.T) {
	h := newSubagentTestHarness(t)
	r := h.router
	launch := store.Item{
		ID: "toolu_launch", ThreadID: h.threadID, Kind: itemKindToolCall, Role: "assistant",
		Status: statusRunning, ToolName: "Agent", Summary: "Agent: review", Meta: `{"input":{"description":"review"}}`,
		CreatedAt: 1, UpdatedAt: 1,
	}
	if err := r.persistItem(launch, nil); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(provider.SubagentBackgroundedMeta{TaskID: "task-1"})
	evt := provider.ProviderEvent{
		Kind: provider.EventSubagentBackgrounded, ThreadID: h.threadID, ItemID: launch.ID, Meta: raw,
		Timestamp: time.UnixMilli(5_000),
	}
	if err := r.Handle(evt); err != nil {
		t.Fatal(err)
	}
	persisted, _, err := h.store.GetThreadItem(h.threadID, launch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !persisted.IsBackground {
		t.Fatal("launch must flip to background")
	}
	var meta map[string]any
	_ = json.Unmarshal([]byte(persisted.Meta), &meta)
	if at, _ := meta[subagentBackgroundedAtMetaKey].(float64); int64(at) != 5_000 {
		t.Fatalf("backgroundedAt stamp wrong: %v", meta[subagentBackgroundedAtMetaKey])
	}
	// A second patch (the CLI re-emits task_updated) must not move the stamp.
	evt.Timestamp = time.UnixMilli(9_000)
	if err := r.Handle(evt); err != nil {
		t.Fatal(err)
	}
	persisted, _, _ = h.store.GetThreadItem(h.threadID, launch.ID)
	_ = json.Unmarshal([]byte(persisted.Meta), &meta)
	if at, _ := meta[subagentBackgroundedAtMetaKey].(float64); int64(at) != 5_000 {
		t.Fatalf("stamp must be first-wins, got %v", meta[subagentBackgroundedAtMetaKey])
	}
}

func TestBackgroundTasksChangedForwardsTheSet(t *testing.T) {
	h := newSubagentTestHarness(t)
	raw, _ := json.Marshal(provider.BackgroundTasksChangedMeta{Tasks: []provider.BackgroundTaskRef{{TaskID: "t1", ToolUseID: "toolu_1", TaskType: "local_agent", Description: "review"}}})
	if err := h.router.Handle(provider.ProviderEvent{Kind: provider.EventBackgroundTasksChanged, ThreadID: h.threadID, Meta: raw}); err != nil {
		t.Fatal(err)
	}
	emits := h.emitsNamed("provider:background_tasks_changed")
	if len(emits) != 1 {
		t.Fatalf("expected one emit, got %d", len(emits))
	}
	payload := emits[0].(BackgroundTasksChangedEvent)
	if payload.ThreadID != h.threadID || len(payload.Tasks) != 1 || payload.Tasks[0].ToolUseID != "toolu_1" {
		t.Fatalf("payload wrong: %+v", payload)
	}
	// Empty set is a real answer.
	raw, _ = json.Marshal(provider.BackgroundTasksChangedMeta{})
	if err := h.router.Handle(provider.ProviderEvent{Kind: provider.EventBackgroundTasksChanged, ThreadID: h.threadID, Meta: raw}); err != nil {
		t.Fatal(err)
	}
	payload = h.emitsNamed("provider:background_tasks_changed")[1].(BackgroundTasksChangedEvent)
	if payload.Tasks == nil || len(payload.Tasks) != 0 {
		t.Fatalf("empty set must be an empty slice: %+v", payload.Tasks)
	}
}

// Claude's task_progress figure is LATEST input + all output, so a
// subagent that compacts its own context legitimately reports a SMALLER
// number afterwards (Codex's is cumulative and cannot). A max merge
// would pin the card to the pre-compaction peak for the rest of the run
// while Claude's own UI moved on.
func TestSubagentProgressTokensFollowAClaudeCompactionDownward(t *testing.T) {
	h := newSubagentTestHarness(t)
	r := h.router

	for _, tokens := range []int64{9_000, 42_000} {
		if err := r.Handle(subagentProgressEvent(t, h.threadID, "toolu_launch", "", provider.SubagentProgressMeta{
			ToolUses: 3, TotalTokens: tokens,
		})); err != nil {
			t.Fatal(err)
		}
	}
	// The agent compacted: its context shrank, its output kept growing.
	if err := r.Handle(subagentProgressEvent(t, h.threadID, "toolu_launch", "", provider.SubagentProgressMeta{
		TotalTokens: 11_500,
	})); err != nil {
		t.Fatal(err)
	}

	got, ok := r.PeekSubagentProgress(h.threadID, "toolu_launch")
	if !ok || got.TotalTokens != 11_500 {
		t.Fatalf("tokens = %d (ok=%v), want the post-compaction 11500, not the 42000 peak", got.TotalTokens, ok)
	}
	// The genuinely monotonic counters still take the max, and a
	// tokens-only tick must not zero them.
	if got.ToolUses != 3 {
		t.Fatalf("toolUses = %d, want 3 — a tokens-only tick must not clear it", got.ToolUses)
	}
}

// Zero still means "this provider did not report it": a Codex-shaped tick
// carrying only tokens must not erase a Claude count, and a tick carrying
// no tokens must not erase the token figure.
func TestSubagentProgressUnreportedTokensKeepTheLastValue(t *testing.T) {
	h := newSubagentTestHarness(t)
	r := h.router

	if err := r.Handle(subagentProgressEvent(t, h.threadID, "toolu_launch", "", provider.SubagentProgressMeta{
		TotalTokens: 7_700,
	})); err != nil {
		t.Fatal(err)
	}
	if err := r.Handle(subagentProgressEvent(t, h.threadID, "toolu_launch", "", provider.SubagentProgressMeta{
		Activity: "Running tests",
	})); err != nil {
		t.Fatal(err)
	}

	got, ok := r.PeekSubagentProgress(h.threadID, "toolu_launch")
	if !ok || got.TotalTokens != 7_700 || got.Activity != "Running tests" {
		t.Fatalf("progress = %+v, want the tokens held across an activity-only tick", got)
	}
}
