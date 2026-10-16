package triage

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// streamBlock drives one streamed block through the router the way the
// Claude parser delivers it: a content_block_start carrying the response id
// and block index, the deltas, and a content_block_stop.
func streamBlock(t *testing.T, router *Router, threadID, scope, apiMessageID string, index int, blockType string, deltas ...string) {
	t.Helper()
	meta, _ := json.Marshal(map[string]any{"index": index, "blockType": blockType, provider.MetaAPIMessageIDKey: apiMessageID})
	handle := func(evt provider.ProviderEvent) {
		evt.ThreadID = threadID
		evt.ParentToolUseID = scope
		evt.Timestamp = time.Now()
		if err := router.Handle(evt); err != nil {
			t.Fatalf("%s: %v", evt.Kind, err)
		}
	}
	handle(provider.ProviderEvent{Kind: provider.EventContentBlockStart, Meta: meta})
	deltaKind := provider.EventTextDelta
	if blockType == "thinking" {
		deltaKind = provider.EventThinking
	}
	for _, delta := range deltas {
		handle(provider.ProviderEvent{Kind: deltaKind, Content: delta, Role: "assistant"})
	}
	handle(provider.ProviderEvent{Kind: provider.EventContentBlockStop, Meta: meta})
}

func abandonBlocks(t *testing.T, router *Router, threadID, scope, apiMessageID string, from int) {
	t.Helper()
	meta, _ := json.Marshal(provider.BlocksAbandonedMeta{APIMessageID: apiMessageID, FromBlockIndex: from})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBlocksAbandoned, ThreadID: threadID, ParentToolUseID: scope, Meta: meta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("blocks abandoned: %v", err)
	}
	router.WaitForPendingSettles()
}

func itemsOfKind(items []store.Item, kind string) []store.Item {
	var out []store.Item
	for _, item := range items {
		if item.Kind == kind {
			out = append(out, item)
		}
	}
	return out
}

func abandonedNotifications(t *testing.T, st *store.Store, threadID string) []store.Item {
	t.Helper()
	items, err := st.ListItems(threadID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var out []store.Item
	for _, item := range itemsOfKind(items, itemKindNotification) {
		if item.ToolName == abandonedBlocksNotificationKind {
			out = append(out, item)
		}
	}
	return out
}

func metaStrings(t *testing.T, raw string, key string) []string {
	t.Helper()
	var meta map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatalf("meta %q: %v", raw, err)
	}
	var out []string
	if err := json.Unmarshal(meta[key], &out); err != nil {
		t.Fatalf("meta %s in %q: %v", key, raw, err)
	}
	return out
}

// The whole response discarded (the stall fell inside block 0): the text
// row stays as it streamed, completed and unmarked, and one notification
// row after it names the row and the response.
func TestBlocksAbandonedRecordsNotificationNamingTheRows(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	streamBlock(t, router, "t1", "", "msg_01", 0, "text", "The quick brown fox ", "stops right ")
	abandonBlocks(t, router, "t1", "", "msg_01", 0)

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	texts := itemsOfKind(items, itemKindAssistantText)
	if len(texts) != 1 {
		t.Fatalf("assistant_text rows: %+v", items)
	}
	text := texts[0]
	if text.Status != statusCompleted || text.Summary != "The quick brown fox stops right " {
		t.Fatalf("abandoned row changed: %+v", text)
	}
	var textMeta map[string]any
	if err := json.Unmarshal([]byte(text.Meta), &textMeta); err != nil {
		t.Fatalf("text meta %q: %v", text.Meta, err)
	}
	if textMeta["api_message_id"] != "msg_01" || textMeta["block_index"] != float64(0) {
		t.Fatalf("text row meta does not name its block: %q", text.Meta)
	}

	notes := abandonedNotifications(t, st, "t1")
	if len(notes) != 1 {
		t.Fatalf("abandoned notifications: %+v", notes)
	}
	note := notes[0]
	if note.ID != store.AbandonedBlocksRecordID(text.TurnIndex, "", "msg_01") {
		t.Errorf("id %q", note.ID)
	}
	if note.Status != statusCompleted || note.Role != "system" || note.Summary != abandonedBlocksSummary {
		t.Errorf("row shape: %+v", note)
	}
	if note.ItemIndex <= text.ItemIndex {
		t.Errorf("notification %d sorts before the row it describes %d", note.ItemIndex, text.ItemIndex)
	}
	if got := metaStrings(t, note.Meta, "item_ids"); !reflect.DeepEqual(got, []string{text.ID}) {
		t.Errorf("item_ids %v, want [%s]", got, text.ID)
	}
	var noteMeta map[string]any
	_ = json.Unmarshal([]byte(note.Meta), &noteMeta)
	if noteMeta["kind"] != abandonedBlocksNotificationKind || noteMeta["api_message_id"] != "msg_01" || noteMeta["from_block_index"] != float64(0) {
		t.Errorf("notification meta %q", note.Meta)
	}
}

// Only the blocks at the index and above are named: the finished first
// block keeps its standing when the stall cut the second.
func TestBlocksAbandonedNamesOnlyBlocksFromTheIndex(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	streamBlock(t, router, "t1", "", "msg_01", 0, "text", "First block.")
	streamBlock(t, router, "t1", "", "msg_01", 1, "text", "SECOND BLOCK starts ", "and is cut ")
	abandonBlocks(t, router, "t1", "", "msg_01", 1)

	items, _ := st.ListItems("t1")
	texts := itemsOfKind(items, itemKindAssistantText)
	if len(texts) != 2 {
		t.Fatalf("assistant_text rows: %+v", items)
	}
	notes := abandonedNotifications(t, st, "t1")
	if len(notes) != 1 {
		t.Fatalf("abandoned notifications: %+v", notes)
	}
	if got := metaStrings(t, notes[0].Meta, "item_ids"); !reflect.DeepEqual(got, []string{texts[1].ID}) {
		t.Errorf("item_ids %v, want [%s]", got, texts[1].ID)
	}
}

// An index above every block seen means the stall fell between blocks and
// nothing visible was discarded: no row is written.
func TestBlocksAbandonedBeyondSeenBlocksWritesNothing(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	streamBlock(t, router, "t1", "", "msg_01", 0, "text", "Finished block.")
	abandonBlocks(t, router, "t1", "", "msg_01", 1)

	if notes := abandonedNotifications(t, st, "t1"); len(notes) != 0 {
		t.Fatalf("unexpected notification: %+v", notes)
	}
}

// A response the turn never streamed (an unknown id) names no rows.
func TestBlocksAbandonedUnknownResponseWritesNothing(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	streamBlock(t, router, "t1", "", "msg_01", 0, "text", "Finished block.")
	abandonBlocks(t, router, "t1", "", "msg_other", 0)

	if notes := abandonedNotifications(t, st, "t1"); len(notes) != 0 {
		t.Fatalf("unexpected notification: %+v", notes)
	}
}

// Thinking blocks are discarded the same way, and a thinking row cut with
// the text row after it are both named, in block order.
func TestBlocksAbandonedNamesThinkingRows(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	streamBlock(t, router, "t1", "", "msg_01", 0, "thinking", "Let me think about ")
	streamBlock(t, router, "t1", "", "msg_01", 1, "text", "Partial answer ")
	abandonBlocks(t, router, "t1", "", "msg_01", 0)

	items, _ := st.ListItems("t1")
	thinks := itemsOfKind(items, itemKindThinking)
	texts := itemsOfKind(items, itemKindAssistantText)
	if len(thinks) != 1 || len(texts) != 1 {
		t.Fatalf("rows: %+v", items)
	}
	if thinks[0].Status != statusCompleted {
		t.Errorf("thinking row status %q", thinks[0].Status)
	}
	notes := abandonedNotifications(t, st, "t1")
	if len(notes) != 1 {
		t.Fatalf("abandoned notifications: %+v", notes)
	}
	if got := metaStrings(t, notes[0].Meta, "item_ids"); !reflect.DeepEqual(got, []string{thinks[0].ID, texts[0].ID}) {
		t.Errorf("item_ids %v", got)
	}
}

// A repeated signal for the same response upserts the one row.
func TestBlocksAbandonedRepeatedSignalKeepsOneRow(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	streamBlock(t, router, "t1", "", "msg_01", 0, "text", "Cut ")
	abandonBlocks(t, router, "t1", "", "msg_01", 0)
	abandonBlocks(t, router, "t1", "", "msg_01", 0)

	if notes := abandonedNotifications(t, st, "t1"); len(notes) != 1 {
		t.Fatalf("abandoned notifications: %+v", notes)
	}
}

// The row bindings are the turn's: once the turn completes, a late signal
// naming its response finds nothing.
func TestBlocksAbandonedAfterTurnEndWritesNothing(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	streamBlock(t, router, "t1", "", "msg_01", 0, "text", "Finished.")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	abandonBlocks(t, router, "t1", "", "msg_01", 0)

	if notes := abandonedNotifications(t, st, "t1"); len(notes) != 0 {
		t.Fatalf("unexpected notification after turn end: %+v", notes)
	}
}

// A block whose start carried no response id (an older CLI, Codex) is not
// bound, and a start without an id clears a parked identity so it cannot
// bind to the wrong row.
func TestBlocksAbandonedIgnoresBlocksWithoutResponseID(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	handle := func(evt provider.ProviderEvent) {
		evt.ThreadID = "t1"
		evt.Timestamp = time.Now()
		if err := router.Handle(evt); err != nil {
			t.Fatalf("%s: %v", evt.Kind, err)
		}
	}
	// A start that names the response, then a start without one before any
	// delta: the second start forgets the first.
	handle(provider.ProviderEvent{Kind: provider.EventContentBlockStart, Meta: json.RawMessage(`{"index":0,"blockType":"text","apiMessageId":"msg_01"}`)})
	handle(provider.ProviderEvent{Kind: provider.EventContentBlockStart, Meta: json.RawMessage(`{"index":0,"blockType":"text"}`)})
	handle(provider.ProviderEvent{Kind: provider.EventTextDelta, Content: "unbound ", Role: "assistant"})
	handle(provider.ProviderEvent{Kind: provider.EventContentBlockStop, Meta: json.RawMessage(`{"blockType":"text"}`)})
	abandonBlocks(t, router, "t1", "", "msg_01", 0)

	items, _ := st.ListItems("t1")
	texts := itemsOfKind(items, itemKindAssistantText)
	if len(texts) != 1 {
		t.Fatalf("rows: %+v", items)
	}
	var meta map[string]any
	if texts[0].Meta != "" {
		_ = json.Unmarshal([]byte(texts[0].Meta), &meta)
	}
	if _, stamped := meta["api_message_id"]; stamped {
		t.Errorf("unbound row stamped with a response: %q", texts[0].Meta)
	}
	if notes := abandonedNotifications(t, st, "t1"); len(notes) != 0 {
		t.Fatalf("unexpected notification: %+v", notes)
	}
}

// A signal scoped to a subagent names that scope's rows and lands in the
// scope. The CLI emits none today (its close events hardcode a null
// parent); the binding is scope-keyed so a scoped signal would work.
func TestBlocksAbandonedScopedToSubagent(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "task_tool_abc", ItemType: "Agent", Timestamp: time.Now(),
		Meta: json.RawMessage(`{"toolName":"Agent","input":{"prompt":"go"},"subagent_launch":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	streamBlock(t, router, "t1", "", "msg_main", 0, "text", "Main text.")
	streamBlock(t, router, "t1", "task_tool_abc", "msg_sub", 0, "text", "Sub text cut ")
	abandonBlocks(t, router, "t1", "task_tool_abc", "msg_sub", 0)

	items, _ := st.ListItems("t1")
	var subText store.Item
	for _, item := range itemsOfKind(items, itemKindAssistantText) {
		if item.ParentID == "task_tool_abc" {
			subText = item
		}
	}
	if subText.ID == "" {
		t.Fatalf("no scoped text row: %+v", items)
	}
	notes := abandonedNotifications(t, st, "t1")
	if len(notes) != 1 {
		t.Fatalf("abandoned notifications: %+v", notes)
	}
	if notes[0].ParentID != "task_tool_abc" {
		t.Errorf("notification scope %q", notes[0].ParentID)
	}
	if notes[0].ID != store.AbandonedBlocksRecordID(subText.TurnIndex, "task_tool_abc", "msg_sub") {
		t.Errorf("id %q", notes[0].ID)
	}
	if got := metaStrings(t, notes[0].Meta, "item_ids"); !reflect.DeepEqual(got, []string{subText.ID}) {
		t.Errorf("item_ids %v, want [%s]", got, subText.ID)
	}
}

// A malformed signal is an error the caller sees, not a silent drop.
func TestBlocksAbandonedWithoutResponseIDIsAnError(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBlocksAbandoned, ThreadID: "t1", Meta: json.RawMessage(`{"fromBlockIndex":0}`), Timestamp: time.Now(),
	})
	if err == nil {
		t.Fatal("malformed blocks_abandoned accepted")
	}
	if want := fmt.Sprintf("blocks abandoned: meta %q names no response", `{"fromBlockIndex":0}`); err.Error() != want {
		t.Errorf("error %q", err)
	}
}

// The rows the record names were pushed before it existed. The record's
// write stamps them, and the wire refresh that follows pushes each as a
// page now reads it, carrying the record's id, so a live window holds the
// mark on the row itself once the record scrolls or trims out of it.
func TestBlocksAbandonedRefreshPushesTheRowsMarked(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	streamBlock(t, router, "t1", "", "msg_01", 0, "text", "Kept block. ")
	streamBlock(t, router, "t1", "", "msg_01", 1, "text", "Cut block ")
	// Both rows settle, are pushed settled and have had their refresh
	// before the record exists: the order a real stall produces, where the
	// CLI closes the block and waits seconds before it gives up on the
	// response, past the refresh's quiet period.
	router.WaitForPendingSettles()
	router.DrainWireItemRefresh()
	abandonBlocks(t, router, "t1", "", "msg_01", 1)
	notes := abandonedNotifications(t, st, "t1")
	if len(notes) != 1 {
		t.Fatalf("abandoned notifications: %+v", notes)
	}
	texts := itemsOfKind(mustListItems(t, st, "t1"), itemKindAssistantText)
	if len(texts) != 2 {
		t.Fatalf("assistant_text rows: %+v", texts)
	}

	emissions.reset()
	router.DrainWireItemRefresh()
	pushed := map[string]string{}
	for _, e := range emissions.snapshot() {
		if evt, ok := e.data.(ItemStreamEvent); ok && evt.Action == itemStreamActionUpsert && evt.Item != nil && evt.Item.Kind == itemKindAssistantText {
			pushed[evt.Item.ID] = evt.Item.DiscardedBy
		}
	}
	if got, ok := pushed[texts[1].ID]; !ok || got != notes[0].ID {
		t.Errorf("cut block pushed as discardedBy=%q (pushed=%v), want %q", got, ok, notes[0].ID)
	}
	if got, ok := pushed[texts[0].ID]; ok && got != "" {
		t.Errorf("kept block pushed marked: %q", got)
	}
}

func mustListItems(t *testing.T, st *store.Store, threadID string) []store.Item {
	t.Helper()
	items, err := st.ListItems(threadID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return items
}
