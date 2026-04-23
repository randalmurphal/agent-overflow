package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// terminalInteractionMetaBlob is the Meta JSON the Codex parser emits
// for `item/commandExecution/terminalInteraction`. Mirrors
// buildTerminalInteractionMeta in internal/provider/codex/protocol.go
// so the test drives the same shape production receives.
func terminalInteractionMetaBlob(t *testing.T, processID, stdin string) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"process_id": processID,
		"stdin":      stdin,
	})
	if err != nil {
		t.Fatalf("marshal terminal_interaction meta: %v", err)
	}
	return encoded
}

// TestTerminalInteraction_EmptyStdinPersistsRow drives the primary
// Phase 6 contract: an EventTerminalInteraction with empty stdin must
// persist a `terminal_interaction` row on the current open turn with a
// "Waited for background terminal" summary. Meta carries process_id.
func TestTerminalInteraction_EmptyStdinPersistsRow(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 2)

	evt := provider.ProviderEvent{
		Kind:      provider.EventTerminalInteraction,
		ThreadID:  "t1",
		TurnID:    "turn-2",
		ItemID:    "cmd-1",
		Content:   "",
		Meta:      terminalInteractionMetaBlob(t, "pid-42", ""),
		Timestamp: time.Now(),
	}
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle empty-stdin terminal_interaction: %v", err)
	}

	items, err := st.ListTurnItems("t1", 2)
	if err != nil {
		t.Fatalf("list turn items: %v", err)
	}
	var matched *store.Item
	for i := range items {
		if items[i].Kind == string(provider.ItemTerminalInteraction) {
			matched = &items[i]
			break
		}
	}
	if matched == nil {
		t.Fatalf("expected a terminal_interaction row on turn 2, got %d items", len(items))
	}
	if matched.Summary != "Waited for background terminal" {
		t.Errorf("summary = %q, want %q", matched.Summary, "Waited for background terminal")
	}
	if matched.Status != statusCompleted {
		t.Errorf("status = %q, want %q", matched.Status, statusCompleted)
	}
	if matched.TurnIndex != 2 {
		t.Errorf("turn_index = %d, want 2 (current open turn)", matched.TurnIndex)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(matched.Meta), &meta); err != nil {
		t.Fatalf("unmarshal stored meta: %v", err)
	}
	if meta["process_id"] != "pid-42" {
		t.Errorf("meta.process_id = %v, want pid-42", meta["process_id"])
	}
}

// TestTerminalInteraction_NonEmptyStdinDropped pins Phase 6 scope: the
// non-empty-stdin (keystrokes-forwarded) variant is parsed by the Codex
// protocol but MUST NOT persist a row. The event is observable on the
// event hook; persistence is deferred to a future phase that decides
// how to render an "Interacted" cell.
func TestTerminalInteraction_NonEmptyStdinDropped(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	evt := provider.ProviderEvent{
		Kind:      provider.EventTerminalInteraction,
		ThreadID:  "t1",
		TurnID:    "turn-0",
		ItemID:    "cmd-1",
		Content:   "password\n",
		Meta:      terminalInteractionMetaBlob(t, "pid-42", "password\n"),
		Timestamp: time.Now(),
	}
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle non-empty-stdin terminal_interaction: %v", err)
	}

	items, err := st.ListTurnItems("t1", 0)
	if err != nil {
		t.Fatalf("list turn items: %v", err)
	}
	for _, it := range items {
		if it.Kind == string(provider.ItemTerminalInteraction) {
			t.Errorf("unexpected terminal_interaction row on non-empty stdin: %+v", it)
		}
	}
}

// TestTerminalInteraction_NoOpenTurn_Dropped covers the pathological
// path: an EventTerminalInteraction arrives without any open turn OR
// persisted turn to fall back to. Handler must log + drop rather than
// panic; no row should land.
func TestTerminalInteraction_NoOpenTurn_Dropped(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	// Deliberately skip seedOpenTurn: no open turn, no persisted turn.

	evt := provider.ProviderEvent{
		Kind:      provider.EventTerminalInteraction,
		ThreadID:  "t1",
		TurnID:    "turn-0",
		ItemID:    "cmd-1",
		Content:   "",
		Meta:      terminalInteractionMetaBlob(t, "pid-42", ""),
		Timestamp: time.Now(),
	}
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle: %v", err)
	}

	items, err := st.ListTurnItems("t1", 0)
	if err != nil {
		t.Fatalf("list turn items: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 rows when no turn is open, got %d: %+v", len(items), items)
	}
}

// TestTerminalInteraction_IdempotentReplay pins the stable-id contract:
// two events that end up at the same (processID, turn_index, seq)
// position must upsert in place, not double-insert.
//
// Sequence-counter reset context: in normal operation every genuine
// event gets a fresh seq so replays shouldn't happen at the handler
// level — but a crash-restart that drops router state and a subsequent
// re-dispatch from a buffered wire message could reproduce a row id.
// Rather than depend on that specific scenario, the test forces the
// seq collision directly: clear the counter between the two Handle
// calls so both events compute the same id, and verify the store
// collapses them to one row via UpsertItem's
// INSERT OR REPLACE semantics.
func TestTerminalInteraction_IdempotentReplay(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	evt := provider.ProviderEvent{
		Kind:      provider.EventTerminalInteraction,
		ThreadID:  "t1",
		TurnID:    "turn-0",
		ItemID:    "cmd-1",
		Content:   "",
		Meta:      terminalInteractionMetaBlob(t, "pid-42", ""),
		Timestamp: time.Now(),
	}
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle 1: %v", err)
	}

	// Force the seq counter back to 0 so the second event computes the
	// SAME id as the first. Without this, the second event would
	// naturally land at seq=1 (different id) and both rows would coexist
	// — the exact behavior TestTerminalInteraction_MultiplePollsDistinctRows
	// verifies for legitimate multi-poll cases.
	router.mu.Lock()
	router.terminalInteractionSeq = make(map[string]int)
	router.mu.Unlock()
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle 2 (replay): %v", err)
	}

	items, err := st.ListTurnItems("t1", 0)
	if err != nil {
		t.Fatalf("list turn items: %v", err)
	}
	count := 0
	for _, it := range items {
		if it.Kind == string(provider.ItemTerminalInteraction) {
			count++
		}
	}
	if count != 1 {
		t.Errorf("idempotent replay produced %d rows, want 1", count)
	}
}

// TestTerminalInteraction_MultiplePollsDistinctRows exercises the
// normal multi-poll case: Codex polls the same PTY five times in a
// turn. We should see five distinct rows — matching Codex's own TUI
// behavior (the unified_exec_wait_streak tracker collapses runs
// visually but our timeline stays flat at the event level).
func TestTerminalInteraction_MultiplePollsDistinctRows(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	for i := 0; i < 5; i++ {
		evt := provider.ProviderEvent{
			Kind:      provider.EventTerminalInteraction,
			ThreadID:  "t1",
			TurnID:    "turn-0",
			ItemID:    "cmd-1",
			Content:   "",
			Meta:      terminalInteractionMetaBlob(t, "pid-42", ""),
			Timestamp: time.Now(),
		}
		if err := router.Handle(evt); err != nil {
			t.Fatalf("handle poll %d: %v", i, err)
		}
	}

	items, err := st.ListTurnItems("t1", 0)
	if err != nil {
		t.Fatalf("list turn items: %v", err)
	}
	count := 0
	seenIDs := make(map[string]struct{})
	for _, it := range items {
		if it.Kind != string(provider.ItemTerminalInteraction) {
			continue
		}
		if _, dup := seenIDs[it.ID]; dup {
			t.Errorf("duplicate id %q", it.ID)
		}
		seenIDs[it.ID] = struct{}{}
		count++
	}
	if count != 5 {
		t.Errorf("expected 5 distinct terminal_interaction rows, got %d", count)
	}
}
