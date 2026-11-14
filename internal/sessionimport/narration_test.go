package sessionimport

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// narration_test.go covers a thinking block the provider marked as prose
// for the user (provider.MetaNarrationKey). Live, the block streams as
// thinking and settles as assistant text at its stop; a transcript holds
// it whole. Both writers must store the same row.

const narrationImportHead = "quokka"

func narrationImportText() string {
	return narrationImportHead + " opens the note.\n\n" + strings.Repeat("The summary keeps going for a while. ", 20) + "The end."
}

// narrationLiveEvents is the block as the live Claude parser delivers it:
// thinking deltas, then a stop carrying the marker.
func narrationLiveEvents(threadID, text string) []provider.ProviderEvent {
	return []provider.ProviderEvent{
		{Kind: provider.EventTurnStart, ThreadID: threadID, Timestamp: at(0)},
		{Kind: provider.EventUserText, ThreadID: threadID, Content: "Summarise the change.",
			Meta: json.RawMessage(`{"provider_item_id":"user-uuid-1"}`), Timestamp: at(1)},
		{Kind: provider.EventThinking, ThreadID: threadID, Content: text[:200], Timestamp: at(2)},
		{Kind: provider.EventThinking, ThreadID: threadID, Content: text[200:], Timestamp: at(2)},
		{Kind: provider.EventContentBlockStop, ThreadID: threadID,
			Meta: json.RawMessage(`{"index":0,"blockType":"thinking","narration":true}`), Timestamp: at(3)},
		{Kind: provider.EventThinking, ThreadID: threadID, Content: "Plain reasoning.", Timestamp: at(4)},
		{Kind: provider.EventContentBlockStop, ThreadID: threadID,
			Meta: json.RawMessage(`{"index":1,"blockType":"thinking"}`), Timestamp: at(5)},
		{Kind: provider.EventTextDelta, ThreadID: threadID, Content: "Done.", Role: "assistant", Timestamp: at(6)},
		{Kind: provider.EventContentBlockStop, ThreadID: threadID,
			Meta: json.RawMessage(`{"index":2,"blockType":"text"}`), Timestamp: at(7)},
		{Kind: provider.EventTurnComplete, ThreadID: threadID, Timestamp: at(10),
			TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn", AssistantMessageID: "msg_1"}},
	}
}

// narrationTranscriptEvents is the same turn as the Claude transcript
// reader delivers it: whole blocks, the marker on the thinking event.
func narrationTranscriptEvents(threadID, text string) []provider.ProviderEvent {
	return []provider.ProviderEvent{
		{Kind: provider.EventTurnStart, ThreadID: threadID, Timestamp: at(0)},
		{Kind: provider.EventUserText, ThreadID: threadID, Content: "Summarise the change.",
			Meta: json.RawMessage(`{"provider_item_id":"user-uuid-1"}`), Timestamp: at(1)},
		{Kind: provider.EventThinking, ThreadID: threadID, ItemID: "msg_1#0", Content: text, ContentPresent: true,
			Meta: json.RawMessage(`{"signature":"sig-narration","narration":true}`), Timestamp: at(2)},
		{Kind: provider.EventThinking, ThreadID: threadID, ItemID: "msg_1#1", Content: "Plain reasoning.", ContentPresent: true, Timestamp: at(4)},
		{Kind: provider.EventTextDelta, ThreadID: threadID, ItemID: "msg_1#2", Content: "Done.", Role: "assistant", ContentPresent: true, Timestamp: at(6)},
		{Kind: provider.EventTurnComplete, ThreadID: threadID, Timestamp: at(10),
			TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn", AssistantMessageID: "msg_1"}},
	}
}

func TestImportWritesNarrationThinkingAsAssistantText(t *testing.T) {
	workspace := t.TempDir()
	text := narrationImportText()
	if strings.Contains(triage.ThinkingSummaryPreview(text), narrationImportHead) {
		t.Fatal("fixture must outrun the thinking preview with its head outside it")
	}

	importStore := newTestStore(t)
	thread := seedThread(t, importStore, testThreadID, "claude", workspace)
	if warnings := buildAndApply(t, importStore, thread, importEvents(narrationTranscriptEvents(testThreadID, text))); len(warnings) > 0 {
		t.Fatalf("import produced unexpected warnings: %+v", warnings)
	}
	imported := readParityRows(t, importStore, testThreadID)

	narrationID := triage.ThinkingItemID(1, "", 0)
	wantShapes := []parityShape{
		{"user:1", "user_text", "", ""},
		{narrationID, "assistant_text", "assistant_text", ""},
		{triage.ThinkingItemID(1, "", 1), "thinking", "thinking", ""},
		{triage.TextItemID(1, "", 0), "assistant_text", "assistant_text", ""},
	}
	if len(imported) != len(wantShapes) {
		t.Fatalf("imported rows: %s", formatRows(imported))
	}
	for i, want := range wantShapes {
		got := imported[i]
		payloadKind := ""
		if got.Payload != nil {
			payloadKind = got.Payload.Kind
		}
		if got.ID != want.id || got.Kind != want.kind || payloadKind != want.payloadKind {
			t.Fatalf("imported row %d = %s, want %+v", i, formatRow(got), want)
		}
	}
	narration := imported[1]
	if narration.Summary != text || narration.Status != "completed" || narration.Role != "assistant" {
		t.Fatalf("narration row = %s", formatRow(narration))
	}
	if narration.Payload.ID != triage.ThinkingPayloadID(narrationID) || narration.Payload.Data != text ||
		!reflect.DeepEqual(narration.Payload.Meta, map[string]any{}) {
		t.Fatalf("narration payload = %+v", *narration.Payload)
	}
	meta, _ := narration.Meta.(map[string]any)
	if meta[provider.MetaNarrationKey] != true || meta["provider_item_id"] != "msg_1#0" {
		t.Fatalf("narration meta = %v", narration.Meta)
	}
	if plain, _ := imported[2].Meta.(map[string]any); plain[provider.MetaNarrationKey] != nil {
		t.Fatalf("the plain thinking row carries the narration key: %v", imported[2].Meta)
	}

	// The live session of the same turn stores the same rows. Item meta is
	// compared on the narration key alone: a live streamed block has no
	// provider item id and no signature to record.
	liveStore := newTestStore(t)
	seedThread(t, liveStore, testThreadID, "claude", workspace)
	driveLiveSession(t, liveStore, narrationLiveEvents(testThreadID, text), "user-uuid-1")
	live := readParityRows(t, liveStore, testThreadID)
	if len(live) != len(imported) {
		t.Fatalf("row count: live=%d import=%d\nlive=%s\nimport=%s", len(live), len(imported), formatRows(live), formatRows(imported))
	}
	for i := range live {
		liveMeta, _ := live[i].Meta.(map[string]any)
		importMeta, _ := imported[i].Meta.(map[string]any)
		if liveMeta[provider.MetaNarrationKey] != importMeta[provider.MetaNarrationKey] {
			t.Errorf("row %d narration key: live=%v import=%v", i, liveMeta[provider.MetaNarrationKey], importMeta[provider.MetaNarrationKey])
		}
		a, b := live[i], imported[i]
		a.Meta, b.Meta = nil, nil
		if !reflect.DeepEqual(a, b) {
			t.Errorf("row %d differs:\n live  = %s\n import= %s", i, formatRow(a), formatRow(b))
		}
	}

	// The imported narration row is searchable by its whole text, as the
	// live row is.
	for name, st := range map[string]*store.Store{"import": importStore, "live": liveStore} {
		if err := st.BuildSearchIndex(context.Background()); err != nil {
			t.Fatalf("%s: build search index: %v", name, err)
		}
		hits, err := st.SearchThreads(narrationImportHead, store.ThreadSearchFilter{ThreadIDs: []string{testThreadID}})
		if err != nil {
			t.Fatalf("%s: search: %v", name, err)
		}
		if len(hits) != 1 || hits[0].ItemID != narrationID || hits[0].Kind != store.ThreadSearchKindAssistant {
			t.Errorf("%s: search hits = %+v, want the narration row", name, hits)
		}
	}
}

// A block stop that settles a narration row keeps the row's own shape.
func TestImportNarrationRowKeepsItsShapeWhenExtended(t *testing.T) {
	workspace := t.TempDir()
	st := newTestStore(t)
	thread := seedThread(t, st, testThreadID, "claude", workspace)
	marked := json.RawMessage(`{"narration":true}`)
	first, second := "First half. ", strings.Repeat("Second half keeps going. ", 30)
	if full := first + second; triage.ThinkingSummaryPreview(full) == full {
		t.Fatal("fixture must outrun the thinking preview")
	}
	events := []provider.ProviderEvent{
		{Kind: provider.EventTurnStart, ThreadID: testThreadID, Timestamp: at(0)},
		{Kind: provider.EventUserText, ThreadID: testThreadID, Content: "Go.", Meta: json.RawMessage(`{"provider_item_id":"user-uuid-1"}`), Timestamp: at(1)},
		{Kind: provider.EventThinking, ThreadID: testThreadID, ItemID: "block-1", Content: first, Meta: marked, Timestamp: at(2)},
		{Kind: provider.EventThinking, ThreadID: testThreadID, ItemID: "block-1", Content: second, Meta: marked, Timestamp: at(3)},
		{Kind: provider.EventTurnComplete, ThreadID: testThreadID, Timestamp: at(10),
			TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"}},
	}
	if warnings := buildAndApply(t, st, thread, importEvents(events)); len(warnings) > 0 {
		t.Fatalf("import produced unexpected warnings: %+v", warnings)
	}
	rows := readParityRows(t, st, testThreadID)
	if len(rows) != 2 {
		t.Fatalf("rows: %s", formatRows(rows))
	}
	row := rows[1]
	if row.Kind != "assistant_text" || row.Summary != first+second || row.Payload.Kind != "assistant_text" ||
		row.Payload.Data != row.Summary || !reflect.DeepEqual(row.Payload.Meta, map[string]any{}) {
		t.Fatalf("extended narration row = %s", formatRow(row))
	}
}
