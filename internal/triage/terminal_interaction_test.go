package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// terminalInteractionMetaBlob is the Meta JSON the Codex parser emits
// for `item/commandExecution/terminalInteraction`. Mirrors
// buildTerminalInteractionMeta in internal/provider/codex/protocol.go
// so the test drives the same shape production receives.
func terminalInteractionMetaBlob(t *testing.T, processID, stdin string) json.RawMessage {
	t.Helper()
	payload := map[string]any{
		"process_id": processID,
		"stdin":      stdin,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal terminal_interaction meta: %v", err)
	}
	return encoded
}

func seedTerminalInteractionBackgroundExec(t *testing.T, router *Router, threadID, processID, launchID, command string) {
	t.Helper()
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: threadID, ItemID: launchID,
		ItemType: "commandExecution", TurnID: "turn-0",
		Meta:      buildUnifiedExecStartMeta(t, processID, command),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("seed unified exec start: %v", err)
	}
	markCodexExecRunning(t, router, threadID, "turn-0", launchID, processID, command)
}

func TestTerminalInteraction_NonEmptyStdinPersistsInteractedRow(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	seedTerminalInteractionBackgroundExec(t, router, "t1", "pid-42", "cmd-1", "sleep 10")

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
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	if items[0].Kind != string(provider.ItemTerminalInteraction) {
		t.Fatalf("kind = %q, want terminal_interaction", items[0].Kind)
	}
	if !strings.Contains(items[0].Summary, "Interacted with background terminal") {
		t.Fatalf("summary = %q, want interacted row", items[0].Summary)
	}
	if strings.Contains(items[0].Meta, "password") {
		t.Fatalf("meta persisted stdin bytes: %s", items[0].Meta)
	}
	meta := decodeItemMetaMap(t, items[0].Meta)
	if meta["has_stdin"] != true {
		t.Fatalf("meta has_stdin = %v, want true", meta["has_stdin"])
	}
}

func TestTerminalInteraction_UntrackedNonEmptyStdinIsDropped(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTerminalInteraction,
		ThreadID:  "t1",
		TurnID:    "turn-0",
		ItemID:    "cmd-1",
		Content:   "q",
		Meta:      terminalInteractionMetaBlob(t, "pid-missing", "q"),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle non-empty stdin: %v", err)
	}

	items, err := st.ListTurnItems("t1", 0)
	if err != nil {
		t.Fatalf("list turn items: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("untracked non-empty stdin persisted rows: %+v", items)
	}
}

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

func TestTerminalInteraction_DoubledResultPreservesInteractedSeq(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	seedTerminalInteractionBackgroundExec(t, router, "t1", "pid-42", "cmd-1", "sleep 10")

	firstInteraction := provider.ProviderEvent{
		Kind:      provider.EventTerminalInteraction,
		ThreadID:  "t1",
		TurnID:    "turn-0",
		ItemID:    "cmd-1",
		Content:   "q",
		Meta:      terminalInteractionMetaBlob(t, "pid-42", "q"),
		Timestamp: time.Now(),
	}
	if err := router.Handle(firstInteraction); err != nil {
		t.Fatalf("first interaction: %v", err)
	}

	// Drive a turn-complete: clearOpenTurn fires under the hood. Under
	// the architectural fix the counter survives. Without it, the next
	// interaction would land at seq=0 and overwrite the first row.
	if err := router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("first turn-complete: %v", err)
	}

	// Re-open the turn so handleTerminalInteraction's openTurnIndex
	// guard (which drops events when no turn is open) doesn't reject
	// the post-close interaction. In the real wire pattern Codex would emit a
	// second `turn/started` for the same turnId — we just call
	// setOpenTurn directly here since the turns row already exists from
	// the first seedOpenTurn (the production handleTurnStart is
	// idempotent against an existing row but the seedOpenTurn helper
	// uses InsertTurn directly which is not).
	router.setOpenTurn("t1", 0)

	secondInteraction := provider.ProviderEvent{
		Kind:      provider.EventTerminalInteraction,
		ThreadID:  "t1",
		TurnID:    "turn-0",
		ItemID:    "cmd-2",
		Content:   "q",
		Meta:      terminalInteractionMetaBlob(t, "pid-42", "q"),
		Timestamp: time.Now(),
	}
	if err := router.Handle(secondInteraction); err != nil {
		t.Fatalf("second interaction: %v", err)
	}

	items, err := st.ListTurnItems("t1", 0)
	if err != nil {
		t.Fatalf("list turn items: %v", err)
	}
	seenIDs := make(map[string]struct{})
	for _, it := range items {
		if it.Kind != string(provider.ItemTerminalInteraction) {
			continue
		}
		seenIDs[it.ID] = struct{}{}
	}
	if len(seenIDs) != 2 {
		t.Errorf("expected 2 distinct interacted rows across the doubled-turn-complete, got %d (counter wipe regression?)", len(seenIDs))
	}
	if _, ok := seenIDs["interacted:pid-42:0:0"]; !ok {
		t.Errorf("missing interacted:pid-42:0:0 row; second interaction likely overwrote the first via colliding seq=0 id")
	}
	if _, ok := seenIDs["interacted:pid-42:0:1"]; !ok {
		t.Errorf("missing interacted:pid-42:0:1 row; counter did not advance after clearOpenTurn")
	}
}

func TestTerminalInteraction_EmptyProcessIDPollIsDropped(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	for i := 0; i < 2; i++ {
		evt := provider.ProviderEvent{
			Kind:      provider.EventTerminalInteraction,
			ThreadID:  "t1",
			TurnID:    "turn-0",
			ItemID:    "cmd-1",
			Content:   "",
			Meta:      terminalInteractionMetaBlob(t, "", ""),
			Timestamp: time.Now(),
		}
		if err := router.Handle(evt); err != nil {
			t.Fatalf("handle empty-process poll %d: %v", i, err)
		}
	}

	items, err := st.ListTurnItems("t1", 0)
	if err != nil {
		t.Fatalf("list turn items: %v", err)
	}
	for _, it := range items {
		if it.Kind == string(provider.ItemTerminalInteraction) {
			t.Fatalf("empty-process poll persisted terminal interaction: %+v", it)
		}
	}
}
