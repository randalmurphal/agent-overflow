package triage

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func TestCodexSpawnAndCompletedExecutionsAreImmutable(t *testing.T) {
	r, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, r, st, "t1", 0)
	seedCodexSpawnCard(t, r, st, "t1", "spawn", "child")
	spawn, found, err := st.GetThreadItem("t1", "spawn")
	if err != nil || !found {
		t.Fatalf("spawn: %v %v", found, err)
	}
	handle := func(evt provider.ProviderEvent) {
		t.Helper()
		evt.ThreadID = "t1"
		evt.Timestamp = time.Now()
		if err := r.Handle(evt); err != nil {
			t.Fatal(err)
		}
		current, _, err := st.GetThreadItem("t1", "spawn")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(spawn, current) {
			t.Fatalf("%s mutated spawn\nbefore: %+v\nafter: %+v", evt.Kind, spawn, current)
		}
	}
	status := func(turn, value string) {
		t.Helper()
		meta, err := json.Marshal(map[string]any{"agent_path": "child", "status": value})
		if err != nil {
			t.Fatal(err)
		}
		handle(provider.ProviderEvent{Kind: provider.EventSubagentStatus, ItemID: "spawn", TurnID: turn, Meta: meta})
	}
	status("A", "running")
	if len(r.ListLiveCodexAgentTasks("t1")) != 1 {
		t.Fatal("first execution missing from background")
	}
	handle(provider.ProviderEvent{Kind: provider.EventSubagentProgress, ItemID: "spawn", Meta: json.RawMessage(`{"totalTokens":10}`)})
	status("A", "completed")
	// The row is written when the child's answer lands (codex_answer_completion.go).
	handle(provider.ProviderEvent{Kind: provider.EventSubagentNotification, ItemID: "spawn", Meta: rootAnswer(t, "final-1", "First answer.")})
	first, ok, err := st.GetThreadItem("t1", "complete:spawn:turn:A")
	if err != nil || !ok {
		t.Fatalf("first completion missing: %v", err)
	}
	status("B", "running")
	// The child's identity is the one late write a settled spawn accepts;
	// the runtime state riding on the same update stays off the row.
	if err := r.Handle(provider.ProviderEvent{Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn", ItemType: "collab_agent", Timestamp: time.Now(), Meta: json.RawMessage(`{"meta_update_only":true,"toolName":"collab_agent","live_background_active":true,"input":{"tool":"spawn_agent","newAgentNickname":"Later name","model":"gpt-5.6-sol"}}`)}); err != nil {
		t.Fatal(err)
	}
	identified, _, err := st.GetThreadItem("t1", "spawn")
	if err != nil {
		t.Fatal(err)
	}
	var identityMeta struct {
		Active *bool `json:"live_background_active"`
		Input  struct {
			Nickname string `json:"newAgentNickname"`
			Model    string `json:"model"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(identified.Meta), &identityMeta); err != nil {
		t.Fatal(err)
	}
	if identityMeta.Input.Nickname != "Later name" || identityMeta.Input.Model != "gpt-5.6-sol" {
		t.Fatalf("identity did not land on spawn: %s", identified.Meta)
	}
	if identityMeta.Active != nil && *identityMeta.Active {
		t.Fatalf("runtime state landed on spawn: %s", identified.Meta)
	}
	expected := spawn
	expected.Meta = identified.Meta
	if !reflect.DeepEqual(expected, identified) {
		t.Fatalf("identity write changed more than meta\nbefore: %+v\nafter: %+v", spawn, identified)
	}
	spawn = identified
	status("A", "completed")
	if len(r.ListLiveCodexAgentTasks("t1")) != 1 {
		t.Fatal("stale completion stopped current execution")
	}
	status("B", "interrupted")
	second, found, err := st.GetThreadItem("t1", "complete:spawn:turn:B")
	if err != nil || !found || second.Status != statusKilled {
		t.Fatalf("interrupted outcome: %+v %v", second, err)
	}
	status("B", "interrupted")
	if len(r.ListLiveCodexAgentTasks("t1")) != 0 {
		t.Fatal("completed execution remains in background")
	}
	if rows := completionRowsFor(t, st, "t1", "spawn"); len(rows) != 2 {
		t.Fatalf("completions=%+v", rows)
	}
	after, _, err := st.GetThreadItem("t1", first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, after) {
		t.Fatalf("later execution mutated first completion: %+v -> %+v", first, after)
	}
	r.CleanupThread("t1")
	afterSpawn, _, err := st.GetThreadItem("t1", "spawn")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(spawn, afterSpawn) {
		t.Fatal("cleanup mutated spawn")
	}
}

func TestCodexCleanupCompletesCurrentExecutionWithoutRewritingSpawn(t *testing.T) {
	r, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, r, st, "t1", 0)
	seedCodexSpawnCard(t, r, st, "t1", "spawn", "child")
	before, _, err := st.GetThreadItem("t1", "spawn")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Handle(provider.ProviderEvent{Kind: provider.EventSubagentStatus, ThreadID: "t1", ItemID: "spawn", TurnID: "A", Meta: json.RawMessage(`{"agent_path":"child","status":"running"}`), Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	r.ClearLiveCodexBackgroundTasks("t1")
	if len(r.ListLiveCodexAgentTasks("t1")) != 1 {
		t.Fatal("cleaning terminals stopped the agent")
	}
	r.CleanupThread("t1")
	after, _, err := st.GetThreadItem("t1", "spawn")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("cleanup rewrote spawn")
	}
	completion, found, err := st.GetThreadItem("t1", "complete:spawn:turn:A")
	if err != nil || !found || completion.Status != statusKilled {
		t.Fatalf("cleanup completion=%+v found=%v err=%v", completion, found, err)
	}
	if len(r.CodexAgentRuntimeSnapshot("t1")) != 0 {
		t.Fatal("cleanup recreated runtime")
	}
}

func TestCodexRecoveryPreservesExecutionBoundariesAndCreatesMissingCompletion(t *testing.T) {
	r, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, r, st, "t1", 0)
	seedCodexSpawnCard(t, r, st, "t1", "spawn", "child")
	signal := func(turn, status string, recovered bool) {
		t.Helper()
		meta, err := json.Marshal(map[string]any{"agent_path": "child", "status": status, "recovered": recovered})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Handle(provider.ProviderEvent{Kind: provider.EventSubagentStatus, ThreadID: "t1", ItemID: "spawn", TurnID: turn, Meta: meta, Timestamp: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	signal("A", "running", false)
	if err := st.InsertItem(store.Item{ID: "child-a", ThreadID: "t1", TurnIndex: 0, ItemIndex: 100, Kind: "assistant_text", Role: "assistant", Status: "completed", ParentID: "spawn", Summary: "first answer", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	signal("A", "completed", false)
	first, _, err := st.GetThreadItem("t1", "complete:spawn:turn:A")
	if err != nil {
		t.Fatal(err)
	}
	r.CleanupThread("t1")
	r.MarkThreadActive("t1")
	signal("A", "completed", true)
	after, _, err := st.GetThreadItem("t1", first.ID)
	if err != nil || !reflect.DeepEqual(first, after) {
		t.Fatalf("recovery changed earlier completion: %v", err)
	}
	signal("B", "running", true)
	current := decodeCodexItemMeta(json.RawMessage(r.CodexAgentRuntimeSnapshot("t1")[0].Meta))
	if current.Runtime.ChildStartIndex != 100 {
		t.Fatalf("lost boundary: %+v", current.Runtime)
	}
	if err := st.InsertItem(store.Item{ID: "child-b", ThreadID: "t1", TurnIndex: 0, ItemIndex: 200, Kind: "assistant_text", Role: "assistant", Status: "completed", ParentID: "spawn", Summary: "second answer", CreatedAt: 2}); err != nil {
		t.Fatal(err)
	}
	signal("B", "completed", true)
	second, found, err := st.GetThreadItem("t1", "complete:spawn:turn:B")
	if err != nil || !found {
		t.Fatalf("missing recovered completion: %v", err)
	}
	var meta struct {
		Count int `json:"subagentDescendantCount"`
		Start int `json:"codex_execution_child_start_index"`
	}
	if err := json.Unmarshal([]byte(second.Meta), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Count != 1 || meta.Start != 100 {
		t.Fatalf("recovered completion includes other run: %s", second.Meta)
	}
}

func TestCodexIdleIsNotExecutionCompletion(t *testing.T) {
	r, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, r, st, "t1", 0)
	seedCodexSpawnCard(t, r, st, "t1", "spawn", "child")
	signal := func(turn, status string) {
		t.Helper()
		meta, err := json.Marshal(map[string]string{"agent_path": "child", "status": status})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Handle(provider.ProviderEvent{Kind: provider.EventSubagentStatus, ThreadID: "t1", ItemID: "spawn", TurnID: turn, Meta: meta, Timestamp: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	signal("", "idle")
	if rows := completionRowsFor(t, st, "t1", "spawn"); len(rows) != 0 {
		t.Fatalf("initial idle fabricated completion: %+v", rows)
	}
	signal("A", "running")
	signal("A", "idle")
	if len(r.ListLiveCodexAgentTasks("t1")) != 0 {
		t.Fatal("idle still spinning")
	}
	if rows := completionRowsFor(t, st, "t1", "spawn"); len(rows) != 0 {
		t.Fatalf("idle invented outcome: %+v", rows)
	}
	signal("A", "interrupted")
	rows := completionRowsFor(t, st, "t1", "spawn")
	if len(rows) != 1 || rows[0].Status != statusKilled {
		t.Fatalf("terminal outcome: %+v", rows)
	}
	signal("A", "idle")
	current := decodeCodexItemMeta(json.RawMessage(r.CodexAgentRuntimeSnapshot("t1")[0].Meta))
	if current.Runtime.Status != "interrupted" {
		t.Fatalf("idle erased terminal outcome: %+v", current.Runtime)
	}
	if after := completionRowsFor(t, st, "t1", "spawn"); !reflect.DeepEqual(rows, after) {
		t.Fatal("late idle changed completion")
	}
}

func TestCodexDeferredCompletionKeepsFirstOutcomeDuringParentStop(t *testing.T) {
	r, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, r, st, "t1", 0)
	seedCodexSpawnCard(t, r, st, "t1", "spawn", "child")
	if err := r.Handle(provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: "t1", Content: "parent working", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	first := store.Item{ID: "complete:spawn:turn:A", ThreadID: "t1", Kind: itemKindBackgroundDone, ToolName: "collab_agent", Role: "assistant", Status: statusCompleted, CompletionOf: "spawn", Summary: "original completion", Meta: `{}`, CreatedAt: 1, UpdatedAt: 1}
	if err := r.maybeDeferOrPersist("t1", first, nil); err != nil {
		t.Fatal(err)
	}
	later := first
	later.Summary, later.Status, later.UpdatedAt = "later delivery", statusErrored, 2
	if err := r.maybeDeferOrPersist("t1", later, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.drainInterruptQueueLocked("t1", true); err != nil {
		t.Fatal(err)
	}
	saved, found, err := st.GetThreadItem("t1", first.ID)
	if err != nil || !found {
		t.Fatalf("completion: %v %v", found, err)
	}
	if saved.Status != first.Status || saved.Summary != first.Summary || saved.UpdatedAt != first.UpdatedAt {
		t.Fatalf("deferred completion changed: %+v", saved)
	}
}
