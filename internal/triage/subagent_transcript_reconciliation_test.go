package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func sidechainCompactSummaryRow(uuid, boundary, text string, seconds int) map[string]any {
	return map[string]any{
		"type": "user", "uuid": uuid, "parentUuid": boundary,
		"isSidechain": true, "isCompactSummary": true,
		"timestamp": sidechainStamp(seconds),
		"message":   map[string]any{"role": "user", "content": text},
	}
}

func markSubagentTranscriptMirrored(t *testing.T, router *Router, threadID, launchID string) {
	t.Helper()
	meta, err := json.Marshal(map[string]any{
		"meta_update_only":                 true,
		provider.MetaTranscriptMirroredKey: true,
	})
	if err != nil {
		t.Fatalf("marshal mirror marker: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: threadID, ItemID: launchID,
		Meta: meta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("mark transcript mirrored: %v", err)
	}
}

// A scoped compaction has a provider UUID, so its absence is decidable even
// when every row after it already streamed. The terminal transcript must add
// the divider and its exact committed summary without replaying a guessed
// user row from the SDK's lossy agent_progress envelope.
func TestSubagentTranscriptReconcilesMissingCompactionBetweenDeliveredRows(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startAgentLaunch(t, router, "t1", "agent-compact-gap", "", "task-compact-gap")
	deliverSubagentBlock(t, router, "t1", "agent-compact-gap", "msg_open#0", "text", "before compacting")
	deliverSubagentBlock(t, router, "t1", "agent-compact-gap", "msg_close#0", "text", "after compacting")

	const summary = "The child retained the exact work state after compaction."
	transcript := writeSubagentTranscript(t, "agent-compact-gap.jsonl",
		sidechainPromptRow("s1", "the task prompt", 1),
		sidechainTextRow("s2", "s1", "msg_open", "before compacting", 2),
		sidechainCompactRow("boundary-1", "s2", 3),
		sidechainCompactSummaryRow("summary-1", "boundary-1", summary, 4),
		sidechainTextRow("s5", "summary-1", "msg_close", "after compacting", 5),
	)
	stashAgentTerminal(t, router, "t1", "agent-compact-gap", "task-compact-gap")
	notifyAgent(t, router, "t1", "agent-compact-gap", "task-compact-gap", transcript, nil)
	router.WaitForPendingSettles()

	children := childrenOfLaunch(t, st, "t1", "agent-compact-gap", 0)
	var compactionID string
	assistantRows := 0
	for _, child := range children {
		if child.Kind == itemKindAssistantText {
			assistantRows++
		}
		if child.Kind != "compaction" {
			continue
		}
		if compactionID != "" {
			t.Fatalf("duplicate compaction rows: %v", childIDs(children))
		}
		compactionID = child.ID
		if child.PayloadID == "" {
			t.Fatal("compaction lost its committed summary payload")
		}
		data, err := st.GetPayloadData("t1", child.PayloadID)
		if err != nil {
			t.Fatalf("load compaction summary: %v", err)
		}
		if string(data) != summary {
			t.Fatalf("summary = %q, want %q", data, summary)
		}
	}
	if compactionID == "" {
		t.Fatalf("missing compaction row between delivered neighbours: %v", childIDs(children))
	}
	if assistantRows != 2 {
		t.Fatalf("terminal reconciliation duplicated delivered prose: %v", childIDs(children))
	}
}

func TestMissingCompactionDoesNotClaimTheFollowingTailWasUndelivered(t *testing.T) {
	events := []importir.Event{
		{ProviderEvent: provider.ProviderEvent{Kind: provider.EventCompactBoundary, ItemID: "boundary-1"}},
		{ProviderEvent: provider.ProviderEvent{Kind: provider.EventError, ItemID: "unmatchable-error"}},
	}
	delivered := subagentDeliveredRows{turnIndex: 0, byID: map[string]store.Item{}}
	cut := subagentBackfillCut(events, delivered)
	if cut != len(events) {
		t.Fatalf("missing compaction moved delivery cut to %d, want %d", cut, len(events))
	}
	if !replaySubagentEventAt(0, cut, events[0].ProviderEvent, delivered) {
		t.Fatal("missing exact compaction was not selected for independent reconciliation")
	}
	if replaySubagentEventAt(1, cut, events[1].ProviderEvent, delivered) {
		t.Fatal("unidentifiable row after selective compaction omission was fabricated as missing")
	}
}

// transcript_mirrored means some prefix arrived live, not that the projection
// was complete. The terminal output file remains the authority for a missing
// tool_result and must settle the exact tool_use row by its provider ID.
func TestSubagentTranscriptReconcilesMirroredToolCompletionAtTerminal(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startAgentLaunch(t, router, "t1", "agent-mirror-gap", "", "task-mirror-gap")
	markSubagentTranscriptMirrored(t, router, "t1", "agent-mirror-gap")
	toolStart, _ := json.Marshal(map[string]any{
		"toolName": "Read", "input": map[string]any{"file_path": "/repo/a.go"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "toolu_mirror_read",
		ItemType: "Read", Meta: toolStart, ParentToolUseID: "agent-mirror-gap", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("mirrored nested tool start: %v", err)
	}

	transcript := writeSubagentTranscript(t, "agent-mirror-gap.jsonl",
		sidechainPromptRow("s1", "the task prompt", 1),
		sidechainToolUseRow("s2", "s1", "msg_tool", "toolu_mirror_read", "Read", 2),
		sidechainToolResultRow("s3", "s2", "toolu_mirror_read", "package main", 3),
	)
	stashAgentTerminal(t, router, "t1", "agent-mirror-gap", "task-mirror-gap")
	notifyAgent(t, router, "t1", "agent-mirror-gap", "task-mirror-gap", transcript, nil)
	router.WaitForPendingSettles()

	tool, ok, err := st.GetThreadItem("t1", "toolu_mirror_read")
	if err != nil || !ok {
		t.Fatalf("lookup reconciled tool: ok=%v err=%v", ok, err)
	}
	if tool.Status != statusCompleted {
		t.Fatalf("mirrored tool status = %q, want exact terminal result to settle it", tool.Status)
	}
}
