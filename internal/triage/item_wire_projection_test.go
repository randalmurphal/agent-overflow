package triage

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/itemwire"
	"agent-overflow/internal/store"
)

// The push path has to project too. A window arrives over RPC and then
// keeps arriving over the event bus; if only the RPC half were bounded,
// a long-lived connection would take the full uncompressed row for every
// item that settled after attach, and the same row would have two shapes
// depending on how it reached the client.

func bigToolMeta(t *testing.T, contentBytes int) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"toolName": "Write",
		"input": map[string]any{
			"file_path": "src/lib/mod.ts",
			"content":   strings.Repeat("x", contentBytes),
		},
	})
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	return string(raw)
}

func TestItemStreamUpsert_ProjectsTheRowItPushes(t *testing.T) {
	item := store.Item{
		ID:       "item-1",
		ThreadID: "thread-1",
		Kind:     "tool_call",
		ToolName: "Write",
		Meta:     bigToolMeta(t, 8<<10),
	}
	evt := NewItemStreamUpsert(item)
	if evt.Item == nil {
		t.Fatal("upsert carried no item")
	}
	if len(evt.Item.Meta) >= len(item.Meta) {
		t.Fatalf("pushed meta is %d bytes against %d stored; nothing was projected",
			len(evt.Item.Meta), len(item.Meta))
	}
	if !strings.Contains(evt.Item.Meta, itemwire.MarkerKey) {
		t.Errorf("pushed meta lost bytes without a marker: %s", evt.Item.Meta)
	}
	// The elision takes the oversized leaf, not the row's shape.
	if !strings.Contains(evt.Item.Meta, `"file_path"`) {
		t.Errorf("pushed meta dropped a small sibling leaf: %s", evt.Item.Meta)
	}
	// The caller's row is the stored row; projecting must not touch it.
	if !strings.Contains(item.Meta, strings.Repeat("x", 8<<10)) {
		t.Error("the projection mutated the caller's stored row")
	}
}

func TestItemStreamUpsert_UnderBudgetPushesByteIdenticalRows(t *testing.T) {
	item := store.Item{
		ID:       "item-1",
		ThreadID: "thread-1",
		Kind:     "tool_call",
		ToolName: "Read",
		Meta:     bigToolMeta(t, 200),
	}
	evt := NewItemStreamUpsert(item)
	if evt.Item.Meta != item.Meta {
		t.Errorf("an under-budget row was rewritten:\n got %s\nwant %s", evt.Item.Meta, item.Meta)
	}
}

// Inline previews stay on for pushed rows: the bus encodes once for
// every subscriber, so a push frame cannot carry a per-client
// preference, and the direction that renders without a fetch is the one
// to take. Only the byte budget applies.
func TestItemStreamUpsert_KeepsInlinePreviews(t *testing.T) {
	payloadMeta, err := json.Marshal(map[string]any{
		"inlineDiff": map[string]any{
			"files": []any{
				map[string]any{
					"path":         "src/lib/mod.ts",
					"insertions":   3,
					"deletions":    1,
					"previewPatch": "@@ -1 +1 @@\n-old\n+new\n",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal payloadMeta: %v", err)
	}
	item := store.Item{
		ID:          "item-1",
		ThreadID:    "thread-1",
		Kind:        "tool_completion",
		PayloadMeta: string(payloadMeta),
	}
	evt := NewItemStreamUpsert(item)
	if evt.Item.PayloadMeta != item.PayloadMeta {
		t.Errorf("a small preview was dropped from a pushed row:\n got %s\nwant %s",
			evt.Item.PayloadMeta, item.PayloadMeta)
	}
}

// A row must not be able to arrive projected over the bus and then be
// patched back to its stored shape at settle.
func TestItemStreamPatch_ProjectsMetaLikeAnUpsert(t *testing.T) {
	meta := bigToolMeta(t, 8<<10)
	evt := newItemStreamPatch("thread-1", "item-1", "tool_call", ItemPatchFields{Meta: &meta})
	if evt.Patch == nil || evt.Patch.Meta == nil {
		t.Fatal("patch carried no meta")
	}
	if len(*evt.Patch.Meta) >= len(meta) {
		t.Fatalf("patched meta is %d bytes against %d stored; nothing was projected",
			len(*evt.Patch.Meta), len(meta))
	}
	if !strings.Contains(*evt.Patch.Meta, itemwire.MarkerKey) {
		t.Errorf("patched meta lost bytes without a marker: %s", *evt.Patch.Meta)
	}
	upsert := NewItemStreamUpsert(store.Item{Kind: "tool_call", Meta: meta})
	if *evt.Patch.Meta != upsert.Item.Meta {
		t.Errorf("patch and upsert projected the same meta differently:\npatch  %s\nupsert %s",
			*evt.Patch.Meta, upsert.Item.Meta)
	}
}

// Deltas are the streaming hot path and carry text, not JSON blobs:
// they are deliberately outside the projection, and a delta that started
// paying a per-chunk decode would be a regression rather than a fix.
func TestItemStreamDelta_IsNotProjected(t *testing.T) {
	delta := strings.Repeat("streamed text ", 4<<10)
	evt := newItemStreamDelta(ItemDeltaEvent{
		ThreadID: "thread-1",
		ItemID:   "item-1",
		Kind:     "assistant_text",
		Delta:    delta,
	})
	if evt.Delta != delta {
		t.Error("a streaming delta was rewritten on its way to the wire")
	}
}
