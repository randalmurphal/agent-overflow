package triage

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// Late provider reports on rows an idle pointer fork shows
// (docs/architecture/sqlite-store.md#ownership). A fork's history is
// fixed when it is made; a report that arrives for the source afterwards
// is the source's fact. Each case forks an idle source, delivers the
// report, and requires that the source records it and the fork keeps the
// row it showed.

// forkIdleSource forks sourceID at its tail and returns the fork's id.
func forkIdleSource(t *testing.T, st *store.Store, sourceID string) string {
	t.Helper()
	source, err := st.GetThread(sourceID)
	if err != nil {
		t.Fatalf("read fork source %s: %v", sourceID, err)
	}
	fork := store.BuildForkedThread(source)
	if err := st.CreatePointerFork(fork, sourceID, store.ForkCut{}, func(s string) string { return s }, time.Now().UnixMilli()); err != nil {
		t.Fatalf("fork %s: %v", sourceID, err)
	}
	return fork.ID
}

// requireForkKeeps fails when the fork's read of id changed.
func requireForkKeeps(t *testing.T, st *store.Store, forkID string, before store.Item) {
	t.Helper()
	after := mustItem(t, st, forkID, before.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the fork's %s changed\nbefore %+v\nafter  %+v", before.ID, before, after)
	}
}

func handleOrFail(t *testing.T, router *Router, what string, evt provider.ProviderEvent) {
	t.Helper()
	if err := router.Handle(evt); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// TestLateBackgroundCompletionEnrichmentWithAnIdleFork: a TaskOutput drain
// writes a background shell's completion sibling, the source goes idle and
// is forked, and the task's richer terminal report arrives. The report
// enriches the source's sibling in place (tool_lifecycle.go); the fork
// keeps the sibling it showed.
func TestLateBackgroundCompletionEnrichmentWithAnIdleFork(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "Bash", "is_background": true,
		"input": map[string]any{"command": "sleep 5; echo done"},
	})
	handleOrFail(t, router, "launch", provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-late",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	})
	basicMeta, _ := json.Marshal(map[string]any{
		"task_id": "tsk-late", "tool_use_id": "bg-late", "status": "completed", "source": "task_output",
	})
	handleOrFail(t, router, "drained terminal", provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "bg-late",
		Meta: basicMeta, Timestamp: time.Now(),
	})
	forkID := forkIdleSource(t, st, "t1")
	sibling := ToolCompletionID("bg-late")
	before := mustItem(t, st, forkID, sibling)

	enrichedMeta, _ := json.Marshal(map[string]any{
		"task_id": "tsk-late", "tool_use_id": "bg-late", "status": "completed",
		"exit_code": 0, "output_file": "/tmp/bg-late.txt", "source": "task_output",
	})
	handleOrFail(t, router, "the late enrichment", provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "bg-late",
		Meta: enrichedMeta, Content: "stdout body here", Timestamp: time.Now(),
	})

	enriched := mustItem(t, st, "t1", sibling)
	if enriched.PayloadID == "" {
		t.Fatalf("the source's sibling did not take the report: %+v", enriched)
	}
	data, err := st.GetPayloadData("t1", enriched.PayloadID)
	if err != nil || string(data) != "stdout body here" {
		t.Fatalf("the source's sibling output = %q, %v", data, err)
	}
	requireForkKeeps(t, st, forkID, before)
}

// TestCodexSpawnIdentityAfterTheTurnWithAnIdleFork: Codex reports a
// spawned child's identity after the parent turn ended, and the source was
// forked in between. The identity lands on the source's spawn row
// (persistCodexSpawnIdentity); the fork keeps the spawn row it showed.
func TestCodexSpawnIdentityAfterTheTurnWithAnIdleFork(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	seedCodexSpawnCard(t, router, st, "t1", "spawn", "child")
	handleOrFail(t, router, "turn complete", provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now(),
	})
	forkID := forkIdleSource(t, st, "t1")
	before := mustItem(t, st, forkID, "spawn")

	handleOrFail(t, router, "the late identity", provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn", ItemType: "collab_agent",
		Meta:      json.RawMessage(`{"meta_update_only":true,"toolName":"collab_agent","input":{"tool":"spawn_agent","newAgentNickname":"Hypatia","newAgentRole":"default","model":"gpt-5.6-sol","reasoningEffort":"high"}}`),
		Timestamp: time.Now().Add(time.Minute),
	})

	var meta struct {
		Input struct {
			Nickname string `json:"newAgentNickname"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(mustItem(t, st, "t1", "spawn").Meta), &meta); err != nil || meta.Input.Nickname != "Hypatia" {
		t.Fatalf("the source's spawn did not take the identity: %+v %v", meta, err)
	}
	requireForkKeeps(t, st, forkID, before)
}

// forkChainToTheCap forks sourceID, then each new fork, until the store
// refuses a deeper one, and returns the deepest fork: every level of it
// is taken, so a copy for it of a row it shows has nowhere to go.
func forkChainToTheCap(t *testing.T, st *store.Store, sourceID string) string {
	t.Helper()
	deepest := sourceID
	for {
		source, err := st.GetThread(deepest)
		if err != nil {
			t.Fatalf("read fork source %s: %v", deepest, err)
		}
		fork := store.BuildForkedThread(source)
		err = st.CreatePointerFork(fork, deepest, store.ForkCut{}, func(s string) string { return s }, time.Now().UnixMilli())
		if errors.Is(err, store.ErrForkChainTooDeep) {
			return deepest
		}
		if err != nil {
			t.Fatalf("fork %s: %v", deepest, err)
		}
		deepest = fork.ID
	}
}

// requireRefusalRow fails unless threadID has the one error row of a
// report refused for the fork depth cap.
func requireRefusalRow(t *testing.T, st *store.Store, threadID string) {
	t.Helper()
	want := ShownHistoryRefusalSummary(store.ErrForkChainTooDeep)
	var rows []string
	for _, row := range findItemsByKind(t, st, threadID, ItemKindError) {
		rows = append(rows, row.Summary)
	}
	if len(rows) != 1 || rows[0] != want {
		t.Fatalf("%s error rows = %q, want [%q]", threadID, rows, want)
	}
}

// TestRefusedLateReportReachesTheThread: the late enrichment of a
// completion sibling that a chain of forks as deep as the cap shows needs
// a level the chain does not have. The store refuses it, the source keeps
// the sibling as it was, and the source's thread gets an error row that
// says the report was not recorded.
func TestRefusedLateReportReachesTheThread(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "Bash", "is_background": true,
		"input": map[string]any{"command": "sleep 5; echo done"},
	})
	handleOrFail(t, router, "launch", provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-late",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	})
	basicMeta, _ := json.Marshal(map[string]any{
		"task_id": "tsk-late", "tool_use_id": "bg-late", "status": "completed", "source": "task_output",
	})
	handleOrFail(t, router, "drained terminal", provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "bg-late",
		Meta: basicMeta, Timestamp: time.Now(),
	})
	deepest := forkChainToTheCap(t, st, "t1")
	sibling := ToolCompletionID("bg-late")
	source, shown := mustItem(t, st, "t1", sibling), mustItem(t, st, deepest, sibling)

	enrichedMeta, _ := json.Marshal(map[string]any{
		"task_id": "tsk-late", "tool_use_id": "bg-late", "status": "completed",
		"exit_code": 0, "output_file": "/tmp/bg-late.txt", "source": "task_output",
	})
	err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "bg-late",
		Meta: enrichedMeta, Content: "stdout body here", Timestamp: time.Now(),
	})
	if !store.IsShownHistoryRefusal(err) {
		t.Fatalf("the late enrichment = %v, want the refusal", err)
	}
	requireRefusalRow(t, st, "t1")
	requireForkKeeps(t, st, "t1", source)
	requireForkKeeps(t, st, deepest, shown)
}

// TestRefusedLateAnnotationReachesTheThread: a permission denial that
// arrives for a settled tool call a chain of forks as deep as the cap
// shows. Its notice row lands; the annotation of the tool call, which the
// event goes on without, is refused, and the refusal reaches the thread
// as an error row rather than a log line.
func TestRefusedLateAnnotationReachesTheThread(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	seedRunningToolCall(t, router, "t1", "toolu_01")
	completeMeta, _ := json.Marshal(map[string]any{"toolName": "Bash"})
	handleOrFail(t, router, "tool complete", provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "toolu_01", ItemType: "Bash",
		Content: "done", Meta: completeMeta, Timestamp: time.Now(),
	})
	handleOrFail(t, router, "turn complete", provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now(),
	})
	deepest := forkChainToTheCap(t, st, "t1")
	source, shown := mustItem(t, st, "t1", "toolu_01"), mustItem(t, st, deepest, "toolu_01")

	handleOrFail(t, router, "the late denial", permissionDeniedEvent("t1", "toolu_01", nil))

	if _, found, err := st.GetThreadItem("t1", "permission-denied:toolu_01"); err != nil || !found {
		t.Fatalf("notice row: found=%v err=%v", found, err)
	}
	requireRefusalRow(t, st, "t1")
	requireForkKeeps(t, st, "t1", source)
	requireForkKeeps(t, st, deepest, shown)
}
