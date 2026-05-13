package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// TestContentBlockStopSettlesStreamingItem pins the explicit-settlement
// edge: a streaming text item opens via EventTextDelta, then
// EventContentBlockStop arrives (no turn-complete yet). The streaming
// item's status must flip to completed in the store. Without this
// behavior, the frontend would render a "streaming" badge indefinitely
// for a closed block, blocking tool cards that arrive later from going
// inline.
func TestContentBlockStopSettlesStreamingItem(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Open streaming text with a first delta.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "hello",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 || items[0].Kind != "assistant_text" || items[0].Status != "streaming" {
		t.Fatalf("setup: expected 1 streaming assistant_text, got %+v", items)
	}
	openingItem := items[0]

	// Fire EventContentBlockStop with an explicit blockType=text so the
	// router settles through the text path (no ambiguity with active
	// thinking blocks).
	meta := json.RawMessage(`{"blockType":"text"}`)
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventContentBlockStop,
		ThreadID:  "t1",
		Meta:      meta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}
	// content-block-stop now spawns the settle on a goroutine so the
	// provider read-loop isn't blocked by SQLite (see stream_state.go).
	// Wait for it before asserting persisted state.
	router.WaitForPendingSettles()

	settled, found, err := st.GetThreadItem("t1", openingItem.ID)
	if err != nil {
		t.Fatalf("get item after stop: %v", err)
	}
	if !found {
		t.Fatalf("streaming item disappeared after stop")
	}
	if settled.Status != statusCompleted {
		t.Errorf("status after stop = %q, want %q", settled.Status, statusCompleted)
	}
	if settled.Summary != "hello" {
		t.Errorf("summary lost on settle: got %q", settled.Summary)
	}

	// Repeat EventContentBlockStop must be a no-op — once a block is
	// settled, a late provider re-send shouldn't re-flip the row.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventContentBlockStop,
		ThreadID:  "t1",
		Meta:      meta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("second content block stop: %v", err)
	}
	router.WaitForPendingSettles()
	after, _, _ := st.GetThreadItem("t1", openingItem.ID)
	if after.Status != statusCompleted {
		t.Errorf("status flipped on repeat stop: got %q", after.Status)
	}
}

// TestThinkingSignatureRoundTripsThroughMeta pins the signature capture
// path: when EventContentBlockStop for a thinking block carries a
// signature, the persisted item's Meta JSON must contain it so the
// frontend can replay the thinking block with the same signature on
// reload. The Claude Agent SDK uses this signature to re-bind tool
// calls back to the right reasoning block.
func TestThinkingSignatureRoundTripsThroughMeta(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Open a thinking item via EventThinking (the thinking counterpart of
	// text delta).
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventThinking,
		ThreadID:  "t1",
		Content:   "reasoning step",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("thinking: %v", err)
	}

	// Find the thinking item so we can look it up after the stop.
	thinkingItems := findItemsByKind(t, st, "t1", "thinking")
	if len(thinkingItems) != 1 {
		t.Fatalf("expected 1 thinking item, got %d", len(thinkingItems))
	}
	itemID := thinkingItems[0].ID

	// EventContentBlockStop with blockType=thinking and a signature. The
	// stop handler should persist the signature onto item.Meta.
	stopMeta := json.RawMessage(`{"blockType":"thinking","signature":"sig-abc-123"}`)
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventContentBlockStop,
		ThreadID:  "t1",
		Meta:      stopMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}
	router.WaitForPendingSettles()

	settled, found, err := st.GetThreadItem("t1", itemID)
	if err != nil || !found {
		t.Fatalf("get thinking item: found=%v err=%v", found, err)
	}
	if settled.Status != statusCompleted {
		t.Errorf("thinking status = %q, want completed", settled.Status)
	}
	if settled.Meta == "" || settled.Meta == "{}" {
		t.Fatalf("meta is empty/defaulted: %q — signature was dropped", settled.Meta)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(settled.Meta), &meta); err != nil {
		t.Fatalf("parse item.Meta: %v (raw=%q)", err, settled.Meta)
	}
	got, _ := meta["signature"].(string)
	if got != "sig-abc-123" {
		t.Errorf("meta.signature = %q, want %q (meta=%v)", got, "sig-abc-123", meta)
	}
}
