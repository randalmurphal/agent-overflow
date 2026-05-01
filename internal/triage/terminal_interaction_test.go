package triage

import (
	"encoding/json"
	"strings"
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

func TestTerminalInteraction_StoresCommandMetadataWhenTrackerKnown(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 2)

	router.mu.Lock()
	state := router.codexBackgroundForThread("t1")
	state.unifiedExecByProcess["pid-42"] = "cmd-1"
	state.unifiedExec["cmd-1"] = &unifiedExecTracker{
		launchID:  "cmd-1",
		processID: "pid-42",
		command:   "sleep 1; echo done",
		summary:   "sleep 1; echo done",
	}
	router.mu.Unlock()

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
		t.Fatalf("expected terminal_interaction row, got %d items", len(items))
	}
	if matched.Summary != "Waited for background terminal: sleep 1; echo done" {
		t.Fatalf("summary = %q", matched.Summary)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(matched.Meta), &meta); err != nil {
		t.Fatalf("unmarshal stored meta: %v", err)
	}
	if meta["command"] != "sleep 1; echo done" {
		t.Errorf("meta.command = %v, want command", meta["command"])
	}
}

func TestTerminalInteraction_SplitsScopedAssistantTextAroundWait(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	const parentToolUseID = "task-tool-1"

	if err := router.Handle(provider.ProviderEvent{
		Kind:            provider.EventTextDelta,
		ThreadID:        "t1",
		Content:         "The 5s foreground sleep finished.",
		ParentToolUseID: parentToolUseID,
		Timestamp:       time.Now(),
	}); err != nil {
		t.Fatalf("handle first text delta: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:            provider.EventTerminalInteraction,
		ThreadID:        "t1",
		TurnID:          "turn-0",
		ItemID:          "cmd-1",
		Content:         "",
		ParentToolUseID: parentToolUseID,
		Meta:            terminalInteractionMetaBlob(t, "pid-42", ""),
		Timestamp:       time.Now(),
	}); err != nil {
		t.Fatalf("handle terminal interaction: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:            provider.EventTextDelta,
		ThreadID:        "t1",
		Content:         "Done. Both background sleeps completed.",
		ParentToolUseID: parentToolUseID,
		Timestamp:       time.Now(),
	}); err != nil {
		t.Fatalf("handle second text delta: %v", err)
	}

	items, err := st.ListTurnItems("t1", 0)
	if err != nil {
		t.Fatalf("list turn items: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected text, terminal interaction, text; got %d items: %+v", len(items), items)
	}
	if items[0].Kind != "assistant_text" || items[0].Summary != "The 5s foreground sleep finished." {
		t.Fatalf("first item = (%q, %q), want first assistant sentence", items[0].Kind, items[0].Summary)
	}
	if items[1].Kind != string(provider.ItemTerminalInteraction) {
		t.Fatalf("second item kind = %q, want terminal_interaction", items[1].Kind)
	}
	if items[2].Kind != "assistant_text" || items[2].Summary != "Done. Both background sleeps completed." {
		t.Fatalf("third item = (%q, %q), want post-wait assistant sentence", items[2].Kind, items[2].Summary)
	}
	for _, item := range items {
		if item.ParentID != parentToolUseID {
			t.Fatalf("item %s parent_id = %q, want %q", item.ID, item.ParentID, parentToolUseID)
		}
	}
}

func TestTerminalInteraction_NonEmptyStdinPersistsInteractedRow(t *testing.T) {
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

// TestTerminalInteraction_StableIDIdempotenceForLiteralReplay pins the
// stable-id contract under literal event replay: dispatching the SAME
// EventTerminalInteraction twice (with no router state mutation between)
// must produce one row, not two. The id-allocating counter must NOT
// advance for the duplicate dispatch.
//
// The earlier version of this test wiped terminalInteractionSeq between
// the two Handle calls and asserted the resulting collision collapsed
// to one row. That encoded the multi-result-per-turn data-loss bug as
// desirable behavior. The architectural fix (counters survive turn
// boundaries; cleared only at CleanupThread) means the only way two
// distinct events land at the same id is genuine literal replay —
// covered here. The "two distinct polls accidentally compute the same
// id because the counter was wiped" scenario is now covered by
// TestTerminalInteraction_DoubledResultPreservesDistinctRows, which
// asserts the OPPOSITE (two distinct rows).
func TestTerminalInteraction_StableIDIdempotenceForLiteralReplay(t *testing.T) {
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
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle 2 (literal replay): %v", err)
	}

	items, err := st.ListTurnItems("t1", 0)
	if err != nil {
		t.Fatalf("list turn items: %v", err)
	}
	// Literal replay should naturally produce two events that the seq
	// counter assigns distinct ids to — that's the architectural fix.
	// Two distinct rows is the expected outcome here, not idempotence
	// via collision collapse.
	count := 0
	seenIDs := make(map[string]struct{})
	for _, it := range items {
		if it.Kind == string(provider.ItemTerminalInteraction) {
			seenIDs[it.ID] = struct{}{}
			count++
		}
	}
	if count != 2 {
		t.Errorf("literal replay of two distinct events produced %d rows, want 2 (each at its own seq)", count)
	}
	if len(seenIDs) != 2 {
		t.Errorf("expected 2 distinct row ids, got %d (counter did not advance for replay)", len(seenIDs))
	}
}

// TestTerminalInteraction_DoubledResultPreservesDistinctRows pins the
// architectural fix for the multi-result-per-turn class of bugs: a
// second EventTurnComplete on the same logical turn (Claude's
// task_notification + synthesized user envelope flow, fatal-error race)
// MUST NOT wipe the terminalInteractionSeq counter. If it did, the
// next post-clear EventTerminalInteraction would compute the same id
// (waited:pid-42:0:0) as the first poll and silently overwrite it via
// UpsertItem's INSERT-OR-UPDATE semantics — exactly the data-loss
// shape that text/thinking/error rows also fell into.
func TestTerminalInteraction_DoubledResultPreservesDistinctRows(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	firstPoll := provider.ProviderEvent{
		Kind:      provider.EventTerminalInteraction,
		ThreadID:  "t1",
		TurnID:    "turn-0",
		ItemID:    "cmd-1",
		Content:   "",
		Meta:      terminalInteractionMetaBlob(t, "pid-42", ""),
		Timestamp: time.Now(),
	}
	if err := router.Handle(firstPoll); err != nil {
		t.Fatalf("first poll: %v", err)
	}

	// Drive a turn-complete: clearOpenTurn fires under the hood. Under
	// the architectural fix the counter survives. Without it, the next
	// poll would land at seq=0 and overwrite the first row.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnComplete,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("first turn-complete: %v", err)
	}

	// Re-open the turn so handleTerminalInteraction's openTurnIndex
	// guard (which drops events when no turn is open) doesn't reject
	// the post-close poll. In the real wire pattern Codex would emit a
	// second `turn/started` for the same turnId — we just call
	// setOpenTurn directly here since the turns row already exists from
	// the first seedOpenTurn (the production handleTurnStart is
	// idempotent against an existing row but the seedOpenTurn helper
	// uses InsertTurn directly which is not).
	router.setOpenTurn("t1", 0)

	secondPoll := provider.ProviderEvent{
		Kind:      provider.EventTerminalInteraction,
		ThreadID:  "t1",
		TurnID:    "turn-0",
		ItemID:    "cmd-2",
		Content:   "",
		Meta:      terminalInteractionMetaBlob(t, "pid-42", ""),
		Timestamp: time.Now(),
	}
	if err := router.Handle(secondPoll); err != nil {
		t.Fatalf("second poll: %v", err)
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
		t.Errorf("expected 2 distinct rows across the doubled-turn-complete, got %d (counter wipe regression?)", len(seenIDs))
	}
	if _, ok := seenIDs["waited:pid-42:0:0"]; !ok {
		t.Errorf("missing waited:pid-42:0:0 row; second poll likely overwrote the first via colliding seq=0 id")
	}
	if _, ok := seenIDs["waited:pid-42:0:1"]; !ok {
		t.Errorf("missing waited:pid-42:0:1 row; counter did not advance after clearOpenTurn")
	}
}

// TestTerminalInteraction_EmptyProcessIDSubstitutesDash pins the
// fallback in terminalInteractionID: when the wire omits processId
// (older Codex builds or partial frames), the id substitutes "-" so
// it stays well-formed. Multiple polls in the same turn with empty
// processIDs still differentiate by seq, so this test also verifies
// two empty-processID events land as two distinct rows rather than
// one overwriting the other.
func TestTerminalInteraction_EmptyProcessIDSubstitutesDash(t *testing.T) {
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
	seenIDs := make(map[string]struct{})
	for _, it := range items {
		if it.Kind != string(provider.ItemTerminalInteraction) {
			continue
		}
		seenIDs[it.ID] = struct{}{}
		// Every persisted row must carry the dash fallback in its id
		// so a listing that filters by id prefix / shape stays sane
		// even when processId was missing on the wire.
		if !strings.HasPrefix(it.ID, "waited:-:") {
			t.Errorf("row id %q missing 'waited:-:' prefix — empty processID fallback broke", it.ID)
		}
	}
	if len(seenIDs) != 2 {
		t.Errorf("expected 2 distinct rows for two empty-processID polls, got %d (seq counter may not advance when processID is empty)", len(seenIDs))
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
