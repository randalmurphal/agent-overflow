package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// itemStreamEventsFor filters the emission log down to the
// provider:item_event stream for one item id, in emission order.
// Upserts carry the id on Item.ID; deltas/patches carry it on ItemID.
func itemStreamEventsFor(t *testing.T, log *emissionLog, itemID string) []ItemStreamEvent {
	t.Helper()
	var out []ItemStreamEvent
	for _, e := range log.snapshot() {
		if e.eventName != "provider:item_event" {
			continue
		}
		evt, ok := e.data.(ItemStreamEvent)
		if !ok {
			t.Fatalf("provider:item_event carries %T, want ItemStreamEvent", e.data)
		}
		id := evt.ItemID
		if evt.Item != nil {
			id = evt.Item.ID
		}
		if id == itemID {
			out = append(out, evt)
		}
	}
	return out
}

// TestRecoveredTopLevelBlocksEmitStreamingWireShape pins the wire
// projection for top-level never-streamed snapshot recovery (CLI-internal
// API retry): each recovered block must emit upsert(streaming, blank
// summary) → delta(full content) → patch(completed, persisted summary),
// in that order, while SQLite holds the settled completed row throughout.
// A single completed upsert here is the wholesale-mount regression: the
// frontend only animates content that arrives as deltas, so the recovered
// reply would land as one multi-viewport jump behind the reveal gate.
func TestRecoveredTopLevelBlocksEmitStreamingWireShape(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Longer than thinkingPreviewRunes so the settle patch provably
	// carries the TRIMMED preview, not the full text.
	longThinking := strings.Repeat("reasoning tail segment. ", 30)
	if len([]rune(longThinking)) <= thinkingPreviewRunes {
		t.Fatalf("fixture must exceed thinkingPreviewRunes (%d runes)", thinkingPreviewRunes)
	}
	const recoveredText = "Recovered synthesis paragraph delivered snapshot-only."

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	for _, blk := range []struct{ itemID, kind, content string }{
		{"msg_retry#0", "thinking", longThinking},
		{"msg_retry#1", "text", recoveredText},
	} {
		if err := router.Handle(provider.ProviderEvent{
			Kind:           provider.EventContentBlockStop,
			ThreadID:       "t1",
			ItemID:         blk.itemID,
			Content:        blk.content,
			ContentPresent: true,
			Meta:           json.RawMessage(`{"blockType":"` + blk.kind + `"}`),
			Timestamp:      time.Now(),
		}); err != nil {
			t.Fatalf("recover %s: %v", blk.kind, err)
		}
	}
	router.WaitForPendingSettles()

	thinkingItems := findItemsByKind(t, st, "t1", itemKindThinking)
	textItems := findItemsByKind(t, st, "t1", itemKindAssistantText)
	if len(thinkingItems) != 1 || len(textItems) != 1 {
		t.Fatalf("expected 1 thinking + 1 text row, got %d + %d", len(thinkingItems), len(textItems))
	}

	cases := []struct {
		name        string
		itemID      string
		kind        string
		content     string
		wantSummary string
	}{
		{"thinking", thinkingItems[0].ID, itemKindThinking, longThinking, ThinkingSummaryPreview(longThinking)},
		{"text", textItems[0].ID, itemKindAssistantText, recoveredText, recoveredText},
	}
	if cases[0].wantSummary == longThinking {
		t.Fatalf("thinking preview did not trim — fixture broken")
	}
	for _, tc := range cases {
		events := itemStreamEventsFor(t, emissions, tc.itemID)
		if len(events) != 3 {
			t.Fatalf("%s: got %d item events, want 3 (upsert, delta, patch): %+v", tc.name, len(events), events)
		}
		upsert, delta, patch := events[0], events[1], events[2]
		if upsert.Action != itemStreamActionUpsert || upsert.Item == nil {
			t.Fatalf("%s: first event = %+v, want upsert with item", tc.name, upsert)
		}
		if upsert.Item.Status != statusStreaming {
			t.Errorf("%s: upsert status = %q, want %q (streaming wire projection)", tc.name, upsert.Item.Status, statusStreaming)
		}
		if upsert.Item.Summary != "" {
			t.Errorf("%s: upsert summary = %q, want blank (content rides the delta)", tc.name, upsert.Item.Summary)
		}
		if delta.Action != itemStreamActionDelta || delta.Delta != tc.content || delta.Kind != tc.kind {
			t.Errorf("%s: second event = %+v, want delta with full content", tc.name, delta)
		}
		if patch.Action != itemStreamActionPatch || patch.Patch == nil {
			t.Fatalf("%s: third event = %+v, want patch", tc.name, patch)
		}
		if patch.Patch.Status == nil || *patch.Patch.Status != statusCompleted {
			t.Errorf("%s: patch status = %v, want completed", tc.name, patch.Patch.Status)
		}
		if patch.Patch.Summary == nil || *patch.Patch.Summary != tc.wantSummary {
			t.Errorf("%s: patch summary = %v, want %q", tc.name, patch.Patch.Summary, tc.wantSummary)
		}
		if patch.Patch.UpdatedAt == nil {
			t.Errorf("%s: patch carries no updatedAt", tc.name)
		}

		// SQLite holds the settled row regardless of the wire projection:
		// crash/reload must read completed content, never a phantom
		// streaming row.
		row, found, err := st.GetThreadItem("t1", tc.itemID)
		if err != nil || !found {
			t.Fatalf("%s: get row: found=%v err=%v", tc.name, found, err)
		}
		if row.Status != statusCompleted {
			t.Errorf("%s: persisted status = %q, want completed", tc.name, row.Status)
		}
		if row.Summary != tc.wantSummary {
			t.Errorf("%s: persisted summary = %q, want %q", tc.name, row.Summary, tc.wantSummary)
		}
	}

	// The thinking payload must hold the FULL recovered reasoning (the
	// summary is only the tail preview).
	data, err := st.GetPayloadData(thinkingItems[0].PayloadID)
	if err != nil {
		t.Fatalf("thinking payload: %v", err)
	}
	if string(data) != longThinking {
		t.Errorf("thinking payload = %d bytes, want the full %d-byte content", len(data), len(longThinking))
	}
}

// TestRecoveredSubagentBlockKeepsCompletedUpsertShape pins the subagent
// carve-out: snapshot recovery is the NORMAL delivery path for subagent
// messages (the CLI emits no partial stream events for them), they render
// inside fold cards, and a settle patch would race the frontend's
// evictSettledChildren before any reveal wrote text. Scoped recoveries
// must therefore stay a single completed upsert — no delta, no patch.
func TestRecoveredSubagentBlockKeepsCompletedUpsertShape(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	const subContent = "Subagent reply delivered as a coalesced snapshot."
	if err := router.Handle(provider.ProviderEvent{
		Kind:            provider.EventContentBlockStop,
		ThreadID:        "t1",
		ItemID:          "msg_sub#0",
		ParentToolUseID: "toolu_parent",
		Content:         subContent,
		ContentPresent:  true,
		Meta:            json.RawMessage(`{"blockType":"text"}`),
		Timestamp:       time.Now(),
	}); err != nil {
		t.Fatalf("subagent recovery: %v", err)
	}
	router.WaitForPendingSettles()

	texts := findItemsByKind(t, st, "t1", itemKindAssistantText)
	if len(texts) != 1 {
		t.Fatalf("expected 1 recovered subagent text row, got %d", len(texts))
	}
	row := texts[0]
	if row.ParentID != "toolu_parent" || row.Status != statusCompleted || row.Summary != subContent {
		t.Fatalf("recovered subagent row = %+v, want completed under toolu_parent", row)
	}

	events := itemStreamEventsFor(t, emissions, row.ID)
	if len(events) != 1 {
		t.Fatalf("got %d item events, want exactly 1 completed upsert: %+v", len(events), events)
	}
	if events[0].Action != itemStreamActionUpsert || events[0].Item == nil ||
		events[0].Item.Status != statusCompleted || events[0].Item.Summary != subContent {
		t.Fatalf("subagent recovery event = %+v, want completed upsert with content", events[0])
	}
}

// TestDuplicateContentPresentStopReassertIsNoOp pins the idempotent
// re-assert: a provider re-sending a content-present stop for a block
// that already settled with identical content must produce ZERO wire
// emissions and leave the row untouched. Re-emitting the completed
// upsert would dispose a frontend smoother mid-drain (terminal upserts
// dispose without snap), turning the rest of a still-revealing row into
// a wholesale jump.
func TestDuplicateContentPresentStopReassertIsNoOp(t *testing.T) {
	longThinking := strings.Repeat("looping over the same reasoning. ", 20)

	cases := []struct {
		name      string
		openKind  provider.EventKind
		itemKind  string
		blockType string
		content   string
	}{
		{"text", provider.EventTextDelta, itemKindAssistantText, "text", "Hello world."},
		{"thinking", provider.EventThinking, itemKindThinking, "thinking", longThinking},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, st, emissions := newTestRouter(t)
			createTestThread(t, st, "t1")

			if err := router.Handle(provider.ProviderEvent{
				Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0, Timestamp: time.Now(),
			}); err != nil {
				t.Fatalf("turn start: %v", err)
			}
			// Stream a provider-keyed block, then settle it with a
			// content-present stop (the Codex shape; also Claude recovery
			// re-delivery).
			if err := router.Handle(provider.ProviderEvent{
				Kind:      tc.openKind,
				ThreadID:  "t1",
				ItemID:    "msg_dup",
				Content:   tc.content,
				Timestamp: time.Now(),
			}); err != nil {
				t.Fatalf("open stream: %v", err)
			}
			stop := provider.ProviderEvent{
				Kind:           provider.EventContentBlockStop,
				ThreadID:       "t1",
				ItemID:         "msg_dup",
				Content:        tc.content,
				ContentPresent: true,
				Meta:           json.RawMessage(`{"blockType":"` + tc.blockType + `"}`),
				Timestamp:      time.Now(),
			}
			if err := router.Handle(stop); err != nil {
				t.Fatalf("first stop: %v", err)
			}
			router.WaitForPendingSettles()

			items := findItemsByKind(t, st, "t1", tc.itemKind)
			if len(items) != 1 || items[0].Status != statusCompleted {
				t.Fatalf("setup: expected 1 completed %s row, got %+v", tc.itemKind, items)
			}
			settled := items[0]

			emissions.reset()
			// Distinguishable UnixMilli if the no-op ever regressed into a
			// rewrite.
			time.Sleep(2 * time.Millisecond)
			if err := router.Handle(stop); err != nil {
				t.Fatalf("duplicate stop: %v", err)
			}
			router.WaitForPendingSettles()

			for _, e := range emissions.snapshot() {
				if e.eventName == "provider:item_event" {
					t.Fatalf("duplicate stop emitted %+v, want zero item events", e.data)
				}
			}
			after, found, err := st.GetThreadItem("t1", settled.ID)
			if err != nil || !found {
				t.Fatalf("re-read row: found=%v err=%v", found, err)
			}
			if after.Status != settled.Status || after.Summary != settled.Summary || after.UpdatedAt != settled.UpdatedAt {
				t.Errorf("duplicate stop mutated the row: before=%+v after=%+v", settled, after)
			}
			// The duplicate must not have minted a second row either.
			if again := findItemsByKind(t, st, "t1", tc.itemKind); len(again) != 1 {
				t.Errorf("duplicate stop minted extra rows: %+v", again)
			}
		})
	}
}
