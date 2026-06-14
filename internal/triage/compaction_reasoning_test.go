package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// scopedThinking builds an EventThinking carrying the reserved compaction scope
// — the shape the claudetui provider streams for the summarizer's reasoning.
func scopedThinking(threadID, content string) provider.ProviderEvent {
	return provider.ProviderEvent{
		Kind:            provider.EventThinking,
		ThreadID:        threadID,
		Content:         content,
		ParentToolUseID: provider.CompactionReasoningScope,
		Timestamp:       time.Now(),
	}
}

// scopedThinkingStop builds the EventContentBlockStop that settles the
// summarizer's reasoning block (also carrying the reserved scope).
func scopedThinkingStop(threadID string) provider.ProviderEvent {
	return provider.ProviderEvent{
		Kind:            provider.EventContentBlockStop,
		ThreadID:        threadID,
		Meta:            json.RawMessage(`{"blockType":"thinking"}`),
		ParentToolUseID: provider.CompactionReasoningScope,
		Timestamp:       time.Now(),
	}
}

// TestCompactionReasoningStreamsAsTopLevelRow is the headline triage behavior:
// EventThinking carrying the reserved compaction scope creates its OWN
// top-level `compaction_reasoning` row (not a nested subagent thinking block,
// not a main-loop thinking row), streams deltas under that kind, and settles to
// completed on the scoped content-block-stop with the full reasoning in its
// on-demand payload. The dispatch must route the sentinel away from
// handleThinking — proven by the absence of any plain `thinking` row.
func TestCompactionReasoningStreamsAsTopLevelRow(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(scopedThinking("t1", "Reviewing the conversation")); err != nil {
		t.Fatalf("first reasoning delta: %v", err)
	}
	if err := router.Handle(scopedThinking("t1", " so far.")); err != nil {
		t.Fatalf("second reasoning delta: %v", err)
	}

	// Exactly one compaction_reasoning row, top-level, still streaming. No plain
	// thinking row leaked (the sentinel did not fall through to handleThinking).
	reasoning := findItemsByKind(t, st, "t1", itemKindCompactionReasoning)
	if len(reasoning) != 1 {
		t.Fatalf("compaction_reasoning rows = %d, want 1 (%+v)", len(reasoning), reasoning)
	}
	if leaked := findItemsByKind(t, st, "t1", itemKindThinking); len(leaked) != 0 {
		t.Fatalf("sentinel leaked %d plain thinking row(s); dispatch must route it to the compaction handler", len(leaked))
	}
	row := reasoning[0]
	if row.ParentID != "" {
		t.Errorf("compaction_reasoning ParentID = %q, want top-level (empty)", row.ParentID)
	}
	if row.Status != statusStreaming {
		t.Errorf("compaction_reasoning status = %q, want streaming", row.Status)
	}
	if row.Role != "assistant" {
		t.Errorf("compaction_reasoning role = %q, want assistant", row.Role)
	}

	// Every streamed delta carries the compaction_reasoning kind so the frontend
	// routes them to the compact tail, never the thinking renderer.
	deltas := filterItemEventDeltas(*emissions)
	if len(deltas) == 0 {
		t.Fatal("no streaming deltas emitted for compaction reasoning")
	}
	for _, d := range deltas {
		if d.Kind != itemKindCompactionReasoning {
			t.Errorf("delta kind = %q, want compaction_reasoning", d.Kind)
		}
	}

	// The scoped content-block-stop settles the row to completed.
	if err := router.Handle(scopedThinkingStop("t1")); err != nil {
		t.Fatalf("reasoning stop: %v", err)
	}
	router.WaitForPendingSettles()

	settled, found, err := st.GetThreadItem("t1", row.ID)
	if err != nil || !found {
		t.Fatalf("get settled reasoning: found=%v err=%v", found, err)
	}
	if settled.Status != statusCompleted {
		t.Errorf("settled status = %q, want completed", settled.Status)
	}
	if settled.Kind != itemKindCompactionReasoning {
		t.Errorf("settle must preserve kind, got %q", settled.Kind)
	}

	// The full reasoning lives in the on-demand payload (raw text, like thinking).
	if settled.PayloadID == "" {
		t.Fatal("settled reasoning linked no payload")
	}
	data, err := st.GetPayloadData(settled.PayloadID)
	if err != nil {
		t.Fatalf("get payload data: %v", err)
	}
	if string(data) != "Reviewing the conversation so far." {
		t.Errorf("payload data = %q, want the full concatenated reasoning", data)
	}
	metaRow, err := st.GetPayloadMeta(settled.PayloadID)
	if err != nil {
		t.Fatalf("get payload meta: %v", err)
	}
	var pm ThinkingMeta
	if err := json.Unmarshal([]byte(metaRow.Meta), &pm); err != nil {
		t.Fatalf("decode payload meta %q: %v", metaRow.Meta, err)
	}
	if pm.Preview == "" {
		t.Errorf("payload meta preview empty; compaction reasoning shares the thinking meta shape")
	}

	// Counters balance: the settle decremented the streaming counts, so the
	// thread is idle (invariant 11 — a later mid-stream row would not defer).
	if router.hasActiveStreamingItem("t1") {
		t.Error("thread still marked streaming after reasoning settled; counts did not balance")
	}
}

// TestCompactionReasoningSortsBeforeDivider proves the placement contract: the
// live reasoning row, created while the summarizer streams, sorts ABOVE the
// `compaction` divider that the PostCompact boundary persists afterward — both
// share the compaction-window turn, and the reasoning's earlier creation gives
// it the lower item_index.
func TestCompactionReasoningSortsBeforeDivider(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Reasoning streams + settles first.
	if err := router.Handle(scopedThinking("t1", "Deciding what still matters.")); err != nil {
		t.Fatalf("reasoning delta: %v", err)
	}
	if err := router.Handle(scopedThinkingStop("t1")); err != nil {
		t.Fatalf("reasoning stop: %v", err)
	}
	router.WaitForPendingSettles()

	// Then the PostCompact boundary persists the divider with the committed summary.
	meta, _ := json.Marshal(map[string]string{"trigger": "auto", "summary": "Committed summary."})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventCompactBoundary,
		ThreadID:  "t1",
		ItemID:    "compact-a",
		Meta:      json.RawMessage(meta),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("compact boundary: %v", err)
	}

	reasoning := findItemsByKind(t, st, "t1", itemKindCompactionReasoning)
	divider := findItemsByKind(t, st, "t1", "compaction")
	if len(reasoning) != 1 || len(divider) != 1 {
		t.Fatalf("rows: reasoning=%d divider=%d, want 1 each", len(reasoning), len(divider))
	}
	if reasoning[0].TurnIndex != divider[0].TurnIndex {
		t.Errorf("reasoning turn %d != divider turn %d; they must share the compaction-window turn", reasoning[0].TurnIndex, divider[0].TurnIndex)
	}
	if reasoning[0].ItemIndex >= divider[0].ItemIndex {
		t.Errorf("reasoning item_index %d must sort before divider item_index %d", reasoning[0].ItemIndex, divider[0].ItemIndex)
	}
}

// TestCompactionReasoningEmptyContentNoOps proves an empty reasoning delta
// creates nothing — the summarizer occasionally opens a thinking block with no
// content, and an empty row would render a phantom compact tail.
func TestCompactionReasoningEmptyContentNoOps(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(scopedThinking("t1", "")); err != nil {
		t.Fatalf("empty reasoning delta: %v", err)
	}
	if rows := findItemsByKind(t, st, "t1", itemKindCompactionReasoning); len(rows) != 0 {
		t.Fatalf("empty reasoning created %d row(s), want none", len(rows))
	}
}

// TestCompactionReasoningAbortedAttemptReusesRow pins the row-reuse contract
// that keeps an aborted-then-retried compaction from leaving a dangling
// streaming row. A user who submits mid-compaction aborts summarizer #1 (its
// reasoning already streamed, but its thinking block never stops) and triggers a
// fresh PreCompact + summarizer #2. The reserved-scope thinking deltas carry NO
// provider item id, so activeStreamKey collapses to the scope counter — #2's
// deltas append to #1's still-open row, and #2's content-block-stop settles that
// single row. If a future change ever stamped a per-attempt item id, #1's row
// would dangle in 'streaming' forever (a phantom perpetual compact tail); this
// test fails loudly in that case.
func TestCompactionReasoningAbortedAttemptReusesRow(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Summarizer #1 streams partial reasoning, then is aborted — no stop frame.
	if err := router.Handle(scopedThinking("t1", "Attempt one partial")); err != nil {
		t.Fatalf("attempt-one reasoning: %v", err)
	}
	// Summarizer #2 (re-armed) streams under the same reserved scope, then stops.
	if err := router.Handle(scopedThinking("t1", " then attempt two")); err != nil {
		t.Fatalf("attempt-two reasoning: %v", err)
	}
	if err := router.Handle(scopedThinkingStop("t1")); err != nil {
		t.Fatalf("attempt-two stop: %v", err)
	}
	router.WaitForPendingSettles()

	// Exactly one row — the retry reused the aborted attempt's open row rather
	// than starting a second. No plain thinking row leaked either.
	rows := findItemsByKind(t, st, "t1", itemKindCompactionReasoning)
	if len(rows) != 1 {
		t.Fatalf("compaction_reasoning rows = %d, want 1 (the retry must reuse the open row)", len(rows))
	}
	if leaked := findItemsByKind(t, st, "t1", itemKindThinking); len(leaked) != 0 {
		t.Fatalf("sentinel leaked %d plain thinking row(s)", len(leaked))
	}

	// That single row settled to completed — no dangling streaming row.
	got, found, err := st.GetThreadItem("t1", rows[0].ID)
	if err != nil || !found {
		t.Fatalf("get settled row: found=%v err=%v", found, err)
	}
	if got.Status != statusCompleted {
		t.Errorf("settled status = %q, want completed (an un-reused attempt would dangle as streaming)", got.Status)
	}

	// The payload holds both attempts' reasoning concatenated in arrival order:
	// the aborted partial already streamed live and cannot be un-sent, so the
	// retry appends to it (documented minor limitation, not a correctness bug).
	data, err := st.GetPayloadData(got.PayloadID)
	if err != nil {
		t.Fatalf("get payload data: %v", err)
	}
	if string(data) != "Attempt one partial then attempt two" {
		t.Errorf("payload data = %q, want both attempts concatenated", data)
	}

	// Counts balanced — thread idle after settle (invariant 11). A dangling
	// streaming row would keep this true-y.
	if router.hasActiveStreamingItem("t1") {
		t.Error("thread still streaming after the retry settled; counts did not balance")
	}
}
