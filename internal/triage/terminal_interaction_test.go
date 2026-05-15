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

func TestTerminalInteraction_UntrackedPollDoesNotStayRunningAfterTurnComplete(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTerminalInteraction,
		ThreadID:  "t1",
		TurnID:    "turn-0",
		ItemID:    "cmd-1",
		Content:   "",
		Meta:      terminalInteractionMetaBlob(t, "pid-missing", ""),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle terminal interaction: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1", TurnID: "turn-0",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("handle turn complete: %v", err)
	}

	waits := findItemsByKind(t, st, "t1", string(provider.ItemTerminalInteraction))
	if len(waits) != 1 {
		t.Fatalf("wait rows = %d, want 1", len(waits))
	}
	if waits[0].Status != statusCompleted {
		t.Fatalf("untracked wait status = %q, want completed", waits[0].Status)
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
		launchID:     "cmd-1",
		processID:    "pid-42",
		command:      "sleep 1; echo done",
		summary:      "sleep 1; echo done",
		backgrounded: true,
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
	if matched.Status != statusRunning {
		t.Fatalf("status = %q, want running", matched.Status)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(matched.Meta), &meta); err != nil {
		t.Fatalf("unmarshal stored meta: %v", err)
	}
	if meta["command"] != "sleep 1; echo done" {
		t.Errorf("meta.command = %v, want command", meta["command"])
	}
}

func TestTerminalInteraction_CommandCompletionSettlesVisibleWaitCarrier(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 2)

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-1",
		ItemType: "commandExecution", TurnID: "turn-0",
		Meta:      buildUnifiedExecStartMeta(t, "pid-42", "false"),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle command start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "backgrounded",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle yield: %v", err)
	}

	typedPoll := provider.ProviderEvent{
		Kind:      provider.EventTerminalInteraction,
		ThreadID:  "t1",
		TurnID:    "turn-2",
		ItemID:    "cmd-1",
		Content:   "",
		Meta:      terminalInteractionMetaBlob(t, "pid-42", ""),
		Timestamp: time.Now(),
	}
	if err := router.Handle(typedPoll); err != nil {
		t.Fatalf("handle typed poll: %v", err)
	}

	waits := findItemsByKind(t, st, "t1", string(provider.ItemTerminalInteraction))
	if len(waits) != 1 {
		t.Fatalf("wait rows after typed poll = %d, want 1: %+v", len(waits), waits)
	}
	waitID := waits[0].ID
	if waits[0].Status != statusRunning {
		t.Fatalf("wait status after typed poll = %q, want running", waits[0].Status)
	}

	completeMeta := buildUnifiedExecCompleteMeta(t, "failed", "pid-42", "false", 1)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventCommandOutput, ThreadID: "t1", ItemID: "cmd-1",
		ItemType: "commandExecution", TurnID: "turn-0", Content: "",
		Meta: completeMeta, Replace: true, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle command output: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "cmd-1",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: completeMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle command completion: %v", err)
	}

	wait, found, err := st.GetThreadItem("t1", waitID)
	if err != nil || !found {
		t.Fatalf("wait row missing after command completion: found=%v err=%v", found, err)
	}
	if wait.Status != statusCompleted {
		t.Fatalf("wait status after command completion = %q, want completed", wait.Status)
	}
	completion, found, err := st.GetThreadItem("t1", nextToolCompletionID("cmd-1"))
	if err != nil || !found {
		t.Fatalf("command completion missing: found=%v err=%v", found, err)
	}
	completionMeta := decodeItemMetaMap(t, completion.Meta)
	if completionMeta["wait_carrier_id"] != waitID {
		t.Fatalf("completion wait_carrier_id = %v, want %s", completionMeta["wait_carrier_id"], waitID)
	}

	seenCompletionBeforeWaitUpdate := false
	for i, emission := range *emissions {
		if emission.eventName != "provider:item_event" {
			continue
		}
		stream, ok := emission.data.(ItemStreamEvent)
		if !ok || stream.Action != "upsert" || stream.Item == nil {
			continue
		}
		if stream.Item.ID == completion.ID {
			for _, later := range (*emissions)[i+1:] {
				laterStream, ok := later.data.(ItemStreamEvent)
				if later.eventName == "provider:item_event" && ok && laterStream.Action == "upsert" && laterStream.Item != nil && laterStream.Item.ID == waitID && laterStream.Item.Status == statusCompleted {
					seenCompletionBeforeWaitUpdate = true
					break
				}
			}
		}
	}
	if !seenCompletionBeforeWaitUpdate {
		t.Fatalf("expected command completion upsert followed by completed wait upsert; emissions=%+v", *emissions)
	}
}

func TestTerminalInteraction_TypedPollMarksUnifiedExecBackgroundedBeforeCompletion(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 2)

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-1",
		ItemType: "commandExecution", TurnID: "turn-0",
		Meta:      buildUnifiedExecStartMeta(t, "pid-42", "false"),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle command start: %v", err)
	}

	typedPoll := provider.ProviderEvent{
		Kind:      provider.EventTerminalInteraction,
		ThreadID:  "t1",
		TurnID:    "turn-2",
		ItemID:    "cmd-1",
		Content:   "",
		Meta:      terminalInteractionMetaBlob(t, "pid-42", ""),
		Timestamp: time.Now(),
	}
	if err := router.Handle(typedPoll); err != nil {
		t.Fatalf("handle typed poll: %v", err)
	}

	waits := findItemsByKind(t, st, "t1", string(provider.ItemTerminalInteraction))
	if len(waits) != 1 {
		t.Fatalf("wait rows after typed poll = %d, want 1: %+v", len(waits), waits)
	}
	waitID := waits[0].ID
	if waits[0].Status != statusRunning {
		t.Fatalf("wait status after typed poll = %q, want running", waits[0].Status)
	}

	completeMeta := buildUnifiedExecCompleteMeta(t, "failed", "pid-42", "false", 1)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventCommandOutput, ThreadID: "t1", ItemID: "cmd-1",
		ItemType: "commandExecution", TurnID: "turn-0", Content: "",
		Meta: completeMeta, Replace: true, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle command output: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "cmd-1",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: completeMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle command completion: %v", err)
	}

	if _, found, err := st.GetThreadItem("t1", "cmd-1"); err != nil || found {
		t.Fatalf("quick foreground command row should not persist: found=%v err=%v", found, err)
	}
	wait, found, err := st.GetThreadItem("t1", waitID)
	if err != nil || !found {
		t.Fatalf("wait row missing after command completion: found=%v err=%v", found, err)
	}
	if wait.Status != statusCompleted {
		t.Fatalf("wait status after command completion = %q, want completed", wait.Status)
	}
	completion, found, err := st.GetThreadItem("t1", nextToolCompletionID("cmd-1"))
	if err != nil || !found {
		t.Fatalf("background command completion missing: found=%v err=%v", found, err)
	}
	completionMeta := decodeItemMetaMap(t, completion.Meta)
	if completionMeta["wait_carrier_id"] != waitID {
		t.Fatalf("completion wait_carrier_id = %v, want %s", completionMeta["wait_carrier_id"], waitID)
	}
}

func TestTerminalInteraction_TypedPollAfterCompletedBackgroundCommandAttachesOutput(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 2)

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-1",
		ItemType: "commandExecution", TurnID: "turn-0",
		Meta:      buildUnifiedExecStartMeta(t, "pid-42", "false"),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle command start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "backgrounded",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle yield: %v", err)
	}

	completeMeta := buildUnifiedExecCompleteMeta(t, "failed", "pid-42", "false", 1)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventCommandOutput, ThreadID: "t1", ItemID: "cmd-1",
		ItemType: "commandExecution", TurnID: "turn-0", Content: "",
		Meta: completeMeta, Replace: true, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle command output: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "cmd-1",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: completeMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle command completion: %v", err)
	}
	if completions := findItemsByKind(t, st, "t1", itemKindBackgroundDone); len(completions) != 0 {
		t.Fatalf("background completion before explicit poll = %d, want 0: %+v", len(completions), completions)
	}

	typedPoll := provider.ProviderEvent{
		Kind:      provider.EventTerminalInteraction,
		ThreadID:  "t1",
		TurnID:    "turn-2",
		ItemID:    "cmd-1",
		Content:   "",
		Meta:      terminalInteractionMetaBlob(t, "pid-42", ""),
		Timestamp: time.Now(),
	}
	if err := router.Handle(typedPoll); err != nil {
		t.Fatalf("handle typed poll: %v", err)
	}

	waits := findItemsByKind(t, st, "t1", string(provider.ItemTerminalInteraction))
	if len(waits) != 1 {
		t.Fatalf("wait rows after typed poll = %d, want 1: %+v", len(waits), waits)
	}
	if waits[0].Status != statusCompleted {
		t.Fatalf("wait status after typed poll = %q, want completed", waits[0].Status)
	}
	if !strings.Contains(waits[0].Summary, "Bash: false") {
		t.Fatalf("wait summary after typed poll = %q, want to preserve command summary", waits[0].Summary)
	}
	completions := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(completions) != 1 {
		t.Fatalf("background completions after typed poll = %d, want 1: %+v", len(completions), completions)
	}
	completionMeta := decodeItemMetaMap(t, completions[0].Meta)
	if completionMeta["wait_carrier_id"] != waits[0].ID {
		t.Fatalf("completion wait_carrier_id = %v, want %s", completionMeta["wait_carrier_id"], waits[0].ID)
	}
}

func TestTerminalInteraction_StaleCarrierWithoutLiveTrackerClearsAtBoundary(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 2)

	typedPoll := provider.ProviderEvent{
		Kind:      provider.EventTerminalInteraction,
		ThreadID:  "t1",
		TurnID:    "turn-2",
		ItemID:    "cmd-1",
		Content:   "",
		Meta:      terminalInteractionMetaBlob(t, "pid-42", ""),
		Timestamp: time.Now(),
	}
	if err := router.Handle(typedPoll); err != nil {
		t.Fatalf("handle typed poll: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		TurnID:    "turn-2",
		Content:   "next boundary",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle boundary text: %v", err)
	}
	if err := router.Handle(typedPoll); err != nil {
		t.Fatalf("handle second typed poll: %v", err)
	}

	waits := findItemsByKind(t, st, "t1", string(provider.ItemTerminalInteraction))
	if len(waits) != 2 {
		t.Fatalf("wait rows after stale-carrier boundary = %d, want 2: %+v", len(waits), waits)
	}
	seen := make(map[string]struct{}, len(waits))
	for _, wait := range waits {
		seen[wait.ID] = struct{}{}
	}
	if _, ok := seen["waited:pid-42:2:0"]; !ok {
		t.Fatalf("missing original wait carrier: %+v", waits)
	}
	if _, ok := seen["waited:pid-42:2:1"]; !ok {
		t.Fatalf("missing fresh wait carrier after boundary: %+v", waits)
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
// same-process poll coalescing contract: dispatching the SAME empty-stdin
// EventTerminalInteraction twice must update the same visible wait carrier,
// not create a second row. The canonical typed notification is the only UI
// lifecycle source for background-terminal waits.
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
	count := 0
	seenIDs := make(map[string]struct{})
	for _, it := range items {
		if it.Kind == string(provider.ItemTerminalInteraction) {
			seenIDs[it.ID] = struct{}{}
			count++
		}
	}
	if count != 1 {
		t.Errorf("literal replay of the same poll produced %d rows, want 1 carrier", count)
	}
	if len(seenIDs) != 1 {
		t.Errorf("expected 1 carrier id, got %d", len(seenIDs))
	}
}

// TestTerminalInteraction_DoubledResultPreservesInteractedSeq pins the
// architectural fix for the multi-result-per-turn class of bugs: a
// second EventTurnComplete on the same logical turn (Claude's
// task_notification + synthesized user envelope flow, fatal-error race)
// MUST NOT wipe the terminalInteractionSeq counter. Empty polls now
// intentionally coalesce by process, so this drives forwarded-stdin
// interactions where each event must still allocate its own row.
func TestTerminalInteraction_DoubledResultPreservesInteractedSeq(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

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

// TestTerminalInteraction_MultiplePollsCoalesceByProcess exercises the
// normal multi-poll case: Codex can surface repeated typed signals for the
// same PTY wait. We keep one visible carrier per process until another
// timeline boundary settles it, matching Codex's visible TUI behavior.
func TestTerminalInteraction_MultiplePollsCoalesceByProcess(t *testing.T) {
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
	if count != 1 {
		t.Errorf("expected 1 coalesced terminal_interaction row, got %d", count)
	}
}
