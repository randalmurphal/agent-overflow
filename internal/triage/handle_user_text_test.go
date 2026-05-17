package triage

import (
	"bytes"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// seedUserTextRow inserts a `user_text` row with the same shape app_send.go
// would produce (id `user:<turnIndex>`, role=user, status=completed, the
// caller-supplied summary and meta). Returns the persisted item so tests
// can assert against the canonical row state.
func seedUserTextRow(t *testing.T, st *store.Store, threadID string, turnIndex int, summary, meta string) store.Item {
	t.Helper()
	now := time.Now().UnixMilli()
	item := store.Item{
		ID:        "user:" + strconv.Itoa(turnIndex),
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   summary,
		Meta:      meta,
		CreatedAt: now,
		UpdatedAt: now,
	}
	persisted, err := st.UpsertItem(item, nil)
	if err != nil {
		t.Fatalf("seed user_text row %s: %v", item.ID, err)
	}
	return persisted
}

func itemUpsertEmissionsForID(emissions []emitted, threadID, itemID string) []store.Item {
	hits := []store.Item{}
	for _, e := range emissions {
		if e.eventName != "provider:item_event" {
			continue
		}
		ev, ok := e.data.(ItemStreamEvent)
		if !ok {
			continue
		}
		if ev.Action != itemStreamActionUpsert {
			continue
		}
		if ev.Item == nil || ev.Item.ThreadID != threadID || ev.Item.ID != itemID {
			continue
		}
		hits = append(hits, *ev.Item)
	}
	return hits
}

func TestHandleUserText_PendingSendMatch_StampsProviderItemID(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	router.RegisterPendingSend("t1", "user:0", 0)
	seedUserTextRow(t, st, "t1", 0, "hello world", "")
	if err := st.SaveCheckpoint(store.Checkpoint{
		ID:            "checkpoint-user-0",
		ThreadID:      "t1",
		UserItemID:    "user:0",
		TurnIndex:     0,
		RefName:       "refs/agent-overflow/checkpoints/dDE/message/user-0",
		CapturedAt:    time.Now().UnixMilli(),
		WorkspacePath: t.TempDir(),
	}); err != nil {
		t.Fatalf("seed checkpoint: %v", err)
	}

	// Reset emissions captured during the seed so the assertion below
	// only sees the upsert produced by handleUserText.
	*emissions = (*emissions)[:0]

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "hello world",
		Meta:      json.RawMessage(`{"provider_item_id":"msg_xyz","parent_uuid":"parent-abc"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle EventUserText: %v", err)
	}

	persisted, found, err := st.GetThreadItem("t1", "user:0")
	if err != nil || !found {
		t.Fatalf("expected user:0 to exist after stamp: found=%v err=%v", found, err)
	}
	if persisted.Summary != "hello world" {
		t.Fatalf("Summary changed: got %q, want %q", persisted.Summary, "hello world")
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(persisted.Meta), &meta); err != nil {
		t.Fatalf("decode meta %q: %v", persisted.Meta, err)
	}
	if id, _ := meta["provider_item_id"].(string); id != "msg_xyz" {
		t.Fatalf("meta.provider_item_id = %v, want msg_xyz (full meta: %s)", meta["provider_item_id"], persisted.Meta)
	}
	checkpoint, ok, err := st.GetCheckpointByUserItemID("t1", "user:0")
	if err != nil || !ok {
		t.Fatalf("checkpoint missing after provider id stamp: ok=%v err=%v", ok, err)
	}
	if checkpoint.ProviderUserMessageID != "msg_xyz" || checkpoint.ProviderParentUUID != "parent-abc" {
		t.Fatalf("checkpoint provider ids = %q/%q, want msg_xyz/parent-abc",
			checkpoint.ProviderUserMessageID, checkpoint.ProviderParentUUID)
	}

	// No new row should be minted under user:wire:msg_xyz when the
	// pending-send branch took ownership.
	if _, foundWire, err := st.GetThreadItem("t1", "user:wire:msg_xyz"); err != nil {
		t.Fatalf("probe wire row: %v", err)
	} else if foundWire {
		t.Fatalf("pending-send branch must not also mint a wire-only row")
	}

	upserts := itemUpsertEmissionsForID(*emissions, "t1", "user:0")
	if len(upserts) != 1 {
		t.Fatalf("expected exactly one provider:item_event upsert for user:0, got %d (emissions: %+v)", len(upserts), *emissions)
	}
	if upserts[0].Meta != persisted.Meta {
		t.Fatalf("emitted upsert meta %q != persisted meta %q", upserts[0].Meta, persisted.Meta)
	}
}

func TestHandleUserText_PendingSendMatch_PreservesExistingMeta(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	existingMeta := `{"attachments":[{"id":"att_1"}],"revision_source_comment_ids":["c-9"]}`
	router.RegisterPendingSend("t1", "user:0", 0)
	seedUserTextRow(t, st, "t1", 0, "review my plan", existingMeta)

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "review my plan",
		Meta:      json.RawMessage(`{"provider_item_id":"msg_456"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle EventUserText: %v", err)
	}

	persisted, found, err := st.GetThreadItem("t1", "user:0")
	if err != nil || !found {
		t.Fatalf("expected user:0 to exist after stamp: found=%v err=%v", found, err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(persisted.Meta), &meta); err != nil {
		t.Fatalf("decode meta %q: %v", persisted.Meta, err)
	}
	if id, _ := meta["provider_item_id"].(string); id != "msg_456" {
		t.Fatalf("meta.provider_item_id = %v, want msg_456", meta["provider_item_id"])
	}
	atts, ok := meta["attachments"].([]any)
	if !ok || len(atts) != 1 {
		t.Fatalf("attachments list lost or malformed in merged meta: %s", persisted.Meta)
	}
	att0, _ := atts[0].(map[string]any)
	if id, _ := att0["id"].(string); id != "att_1" {
		t.Fatalf("attachment id = %v, want att_1 (full meta: %s)", att0["id"], persisted.Meta)
	}
	cids, ok := meta["revision_source_comment_ids"].([]any)
	if !ok || len(cids) != 1 {
		t.Fatalf("revision_source_comment_ids lost or malformed in merged meta: %s", persisted.Meta)
	}
	if got, _ := cids[0].(string); got != "c-9" {
		t.Fatalf("revision_source_comment_ids[0] = %v, want c-9", cids[0])
	}
}

func TestHandleUserText_NoPending_PersistsWireOnlyRow(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 2)
	before := readThreadUpdatedAt(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "task notification echo body",
		Meta:      json.RawMessage(`{"provider_item_id":"codex_item_42"}`),
		Timestamp: time.UnixMilli(1_700_000_000_000),
	}); err != nil {
		t.Fatalf("Handle EventUserText: %v", err)
	}

	persisted, found, err := st.GetThreadItem("t1", "user:wire:codex_item_42")
	if err != nil || !found {
		t.Fatalf("expected user:wire row to exist: found=%v err=%v", found, err)
	}
	if persisted.Kind != "user_text" {
		t.Fatalf("Kind = %q, want user_text", persisted.Kind)
	}
	if persisted.Role != "user" {
		t.Fatalf("Role = %q, want user", persisted.Role)
	}
	if persisted.Status != "completed" {
		t.Fatalf("Status = %q, want completed", persisted.Status)
	}
	if persisted.TurnIndex != 2 {
		t.Fatalf("TurnIndex = %d, want 2 (open turn)", persisted.TurnIndex)
	}
	if persisted.Summary != "task notification echo body" {
		t.Fatalf("Summary = %q, want %q", persisted.Summary, "task notification echo body")
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(persisted.Meta), &meta); err != nil {
		t.Fatalf("decode meta %q: %v", persisted.Meta, err)
	}
	if id, _ := meta["provider_item_id"].(string); id != "codex_item_42" {
		t.Fatalf("meta.provider_item_id = %v, want codex_item_42", meta["provider_item_id"])
	}
	if wireOnly, _ := meta["wire_only"].(bool); !wireOnly {
		t.Fatalf("meta.wire_only = %v, want true", meta["wire_only"])
	}
	if after := readThreadUpdatedAt(t, st, "t1"); after != before {
		t.Fatalf("threads.updated_at moved across wire-only EventUserText: before=%d after=%d", before, after)
	}

	upserts := itemUpsertEmissionsForID(*emissions, "t1", "user:wire:codex_item_42")
	if len(upserts) != 1 {
		t.Fatalf("expected one provider:item_event upsert for the wire-only row, got %d", len(upserts))
	}
}

func TestHandleUserText_NoPending_SubagentPromptPersistsUnderParent(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 2)
	before := readThreadUpdatedAt(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind:            provider.EventUserText,
		ThreadID:        "t1",
		Content:         "Inspect the parser",
		ParentToolUseID: "spawn-1",
		Meta:            json.RawMessage(`{"provider_item_id":"child_prompt_1"}`),
		Timestamp:       time.UnixMilli(1_700_000_000_000),
	}); err != nil {
		t.Fatalf("Handle EventUserText: %v", err)
	}

	persisted, found, err := st.GetThreadItem("t1", "user:wire:child_prompt_1")
	if err != nil || !found {
		t.Fatalf("expected subagent prompt row to exist: found=%v err=%v", found, err)
	}
	if persisted.ParentID != "spawn-1" {
		t.Fatalf("ParentID = %q, want spawn-1", persisted.ParentID)
	}
	if after := readThreadUpdatedAt(t, st, "t1"); after != before {
		t.Fatalf("threads.updated_at moved across subagent EventUserText: before=%d after=%d", before, after)
	}
}

func TestHandleUserText_SubagentPromptDoesNotConsumePendingSend(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 2)
	router.RegisterPendingSend("t1", "user:2", 2)
	seedUserTextRow(t, st, "t1", 2, "top-level prompt", "")

	if err := router.Handle(provider.ProviderEvent{
		Kind:            provider.EventUserText,
		ThreadID:        "t1",
		Content:         "Inspect the parser",
		ParentToolUseID: "spawn-1",
		Meta:            json.RawMessage(`{"provider_item_id":"child_prompt_1"}`),
		Timestamp:       time.UnixMilli(1_700_000_000_000),
	}); err != nil {
		t.Fatalf("Handle child EventUserText: %v", err)
	}

	child, found, err := st.GetThreadItem("t1", "user:wire:child_prompt_1")
	if err != nil || !found {
		t.Fatalf("expected subagent prompt row to exist: found=%v err=%v", found, err)
	}
	if child.ParentID != "spawn-1" {
		t.Fatalf("child ParentID = %q, want spawn-1", child.ParentID)
	}

	pending, ok := router.consumePendingSendHead("t1")
	if !ok {
		t.Fatalf("pending send was consumed by child prompt")
	}
	if pending.AOItemID != "user:2" {
		t.Fatalf("pending AOItemID = %q, want user:2", pending.AOItemID)
	}
}

func TestHandleUserText_NoPending_DedupsRepeatedProviderItemID(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	evt := provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "duplicate cascade",
		Meta:      json.RawMessage(`{"provider_item_id":"dup_1"}`),
		Timestamp: time.Now(),
	}
	if err := router.Handle(evt); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	if err := router.Handle(evt); err != nil {
		t.Fatalf("second Handle: %v", err)
	}

	rows, err := st.ListItemsForTurn("t1", 0)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	wireRows := 0
	for _, r := range rows {
		if r.ID == "user:wire:dup_1" {
			wireRows++
		}
	}
	if wireRows != 1 {
		t.Fatalf("expected exactly one user:wire:dup_1 row across two Handle calls, got %d (rows: %+v)", wireRows, rows)
	}
}

func TestHandleUserText_NoPending_NoProviderItemID_NoOp(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "no id",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	rows, err := st.ListItemsForTurn("t1", 0)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	for _, r := range rows {
		if r.Kind == "user_text" {
			t.Fatalf("no rows should be persisted for missing provider_item_id, got: %+v", r)
		}
	}
	for _, e := range *emissions {
		if e.eventName == "provider:item_event" {
			ev, ok := e.data.(ItemStreamEvent)
			if !ok {
				continue
			}
			if ev.Action == itemStreamActionUpsert && ev.Item != nil && ev.Item.Kind == "user_text" {
				t.Fatalf("no upsert should be emitted for missing provider_item_id, got: %+v", ev)
			}
		}
	}
}

// TestHandleUserText_PendingSendMatch_EmptyProviderItemID_LogsAndNoOp
// pins the defensive log branch in attachProviderItemIDToUserRow:
// when the wire echo arrives with no provider_item_id (parser gap for
// a new wire shape — historically the queued_command replay shape),
// the FIFO entry is consumed but meta-merge no-ops on empty id. The
// row stays unchanged, no upsert emits, and the frontend's
// queue-confirm overlay would stay stuck. The branch logs loud so the
// gap is observable instead of presenting as a silent stuck UI.
func TestHandleUserText_PendingSendMatch_EmptyProviderItemID_LogsAndNoOp(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	router.RegisterPendingSend("t1", "user:0", 0)
	seedUserTextRow(t, st, "t1", 0, "queued message", `{"attachments":[]}`)
	*emissions = (*emissions)[:0]

	var logBuf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(prev)

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "queued message",
		Meta:      nil,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if !strings.Contains(logBuf.String(), "queue-confirm path will not fire") {
		t.Fatalf("expected log line about queue-confirm path, got: %q", logBuf.String())
	}

	if router.HasPendingSendForThread("t1") {
		t.Fatalf("FIFO should be drained — pending entry must have been popped")
	}

	persisted, found, err := st.GetThreadItem("t1", "user:0")
	if err != nil || !found {
		t.Fatalf("seed row should still exist: found=%v err=%v", found, err)
	}
	if persisted.Meta != `{"attachments":[]}` {
		t.Fatalf("row Meta should be unchanged on empty-id branch, got %q", persisted.Meta)
	}

	for _, e := range *emissions {
		if e.eventName != "provider:item_event" {
			continue
		}
		ev, ok := e.data.(ItemStreamEvent)
		if !ok {
			continue
		}
		if ev.Action == itemStreamActionUpsert && ev.Item != nil && ev.Item.Kind == "user_text" {
			t.Fatalf("no user_text upsert should be emitted on empty-id branch, got: %+v", ev)
		}
	}
}

// TestHandleUserText_PendingSendMatch_DuplicateProviderItemID_NoLog
// pins the legitimate-duplicate sibling: when the same wire echo
// arrives twice (session-resume replay landing on an already-stamped
// row), the merge no-ops because the id already matches. No log
// fires — the empty-id log is reserved for the parser-gap signal.
func TestHandleUserText_PendingSendMatch_DuplicateProviderItemID_NoLog(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	router.RegisterPendingSend("t1", "user:0", 0)
	seedUserTextRow(t, st, "t1", 0, "hello", `{"provider_item_id":"msg_existing"}`)

	var logBuf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(prev)

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "hello",
		Meta:      json.RawMessage(`{"provider_item_id":"msg_existing"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if strings.Contains(logBuf.String(), "queue-confirm path will not fire") {
		t.Fatalf("duplicate-id branch must not emit the empty-id warning, got: %q", logBuf.String())
	}
}

func TestHandleUserText_PendingMatchWithMissingTargetRow_LogsAndReturns(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Register pending but DO NOT persist the matching row — simulates
	// a send that errored after RegisterPendingSend but before the
	// optimistic store write.
	router.RegisterPendingSend("t1", "user:5", 5)

	var logBuf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(prev)

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "stranded",
		Meta:      json.RawMessage(`{"provider_item_id":"msg_stranded"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if !strings.Contains(logBuf.String(), "row absent") {
		t.Fatalf("expected log line about missing row, got: %q", logBuf.String())
	}

	// No row should have been minted under user:5 OR user:wire:msg_stranded
	// (the pending branch took ownership and exited; the wire-only fallback
	// only runs when no pending entry matched).
	if _, found, _ := st.GetThreadItem("t1", "user:5"); found {
		t.Fatalf("user:5 row should not exist when handler exits early")
	}
	if _, found, _ := st.GetThreadItem("t1", "user:wire:msg_stranded"); found {
		t.Fatalf("wire-only row must not be minted when the pending branch already claimed the wire envelope")
	}

	// Pending FIFO should be drained — consuming the head should fail.
	if _, ok := router.consumePendingSendHead("t1"); ok {
		t.Fatalf("pending FIFO should be drained even when the row was missing")
	}

	// And no spurious upsert emission should reach the frontend.
	for _, e := range *emissions {
		if e.eventName == "provider:item_event" {
			ev, ok := e.data.(ItemStreamEvent)
			if !ok {
				continue
			}
			if ev.Action == itemStreamActionUpsert && ev.Item != nil && ev.Item.Kind == "user_text" {
				t.Fatalf("no user_text upsert should be emitted on missing-row path, got: %+v", ev)
			}
		}
	}
}

func TestReadProviderItemIDFromMeta(t *testing.T) {
	tests := []struct {
		name string
		meta json.RawMessage
		want string
	}{
		{name: "empty", meta: nil, want: ""},
		{name: "absent key", meta: json.RawMessage(`{"other":"x"}`), want: ""},
		{name: "valid", meta: json.RawMessage(`{"provider_item_id":"msg_x"}`), want: "msg_x"},
		{name: "non-string", meta: json.RawMessage(`{"provider_item_id":42}`), want: ""},
		{name: "malformed json", meta: json.RawMessage(`{not json`), want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := readProviderItemIDFromMeta(tc.meta); got != tc.want {
				t.Fatalf("readProviderItemIDFromMeta(%q) = %q, want %q", tc.meta, got, tc.want)
			}
		})
	}
}

// Merge-meta behavior is covered by
// internal/usermessage/usermessage_test.go::TestMergeProviderItemID*.
// The triage path is now a thin call site over
// usermessage.MergeProviderItemID; duplicate coverage here would drift.

// TestHandleUserText_DeferredFlush_LandsAfterContentThatArrivedFirst pins
// the queued-message ordering: when the user queues a message while the
// agent is busy, the persisted user_text row must land at a position
// AFTER every row that was persisted before the wire echo arrived. The
// frontend sorts strictly by (turn_index, item_index) with no timestamp
// tiebreaker (frontend/src/lib/stores/threadItems.ts), so on-disk
// ordering is the visible ordering.
//
// The previous design captured item_index at dispatch time and inserted
// at that captured slot when the wire echo arrived. Between dispatch and
// echo the model can stream rows (handleTextDelta and friends persist
// via UpsertItem -> nextItemIndexTx, MAX+1), so those rows took the
// captured slot. Insert-at-index then shifted them down to make room
// for the queued message — placing the queued message ABOVE rows that
// arrived first. persistDeferredUserText now recomputes MAX+1 at echo
// time, so the queued message lands at the actual tail.
//
// This test reproduces the screenshot scenario: a tool completes, the
// model streams assistant content while the queued send is in flight,
// and the wire echo arrives last. The queued row must be the LAST item
// in the turn.
func TestHandleUserText_DeferredFlush_LandsAfterContentThatArrivedFirst(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	// Pre-existing row at item_index=0 — the "pnpm test" bash row that
	// just completed in the screenshot scenario. AppendItem mirrors the
	// MAX+1 path triage takes for ordinary completion writes.
	if _, err := st.AppendItem(store.Item{
		ID:        "tool:pnpm-test",
		ThreadID:  "t1",
		TurnIndex: 0,
		Kind:      "tool_call",
		Role:      "assistant",
		Status:    "completed",
		Summary:   "Bash: pnpm test",
		ToolName:  "Bash",
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed pre-existing tool_call: %v", err)
	}

	// Register the deferred pending send exactly as dispatchFlushItem does
	// before writing to the provider's stdin / JSON-RPC seam.
	queuedItem := store.Item{
		ID:        "user:0:flush:0",
		ThreadID:  "t1",
		TurnIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "Also, the think icon, i want that to be a lucide icon for a brain",
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}
	router.RegisterPendingFlushSend("t1", "queue:abc", queuedItem)

	// Model emits assistant text BETWEEN the dispatch and the wire echo.
	// First text delta goes through persistItem -> UpsertItem ->
	// nextItemIndexTx, so it lands at MAX+1 = 1 (one past the seeded tool
	// row).
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "All 3029 tests pass. Now the full check:",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle intervening text delta: %v", err)
	}

	// Wire echo of the queued message finally arrives.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "Also, the think icon, i want that to be a lucide icon for a brain",
		Meta:      json.RawMessage(`{"provider_item_id":"msg_queued_echo"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle wire echo: %v", err)
	}

	// Build an index -> id map for a readable failure message.
	rows, err := st.ListItemsForTurn("t1", 0)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	idsByIndex := make(map[int]string, len(rows))
	for _, r := range rows {
		idsByIndex[r.ItemIndex] = r.ID
	}

	queued, found, err := st.GetThreadItem("t1", queuedItem.ID)
	if err != nil || !found {
		t.Fatalf("queued row missing after wire echo: found=%v err=%v", found, err)
	}

	// The queued message must be the LAST row in the turn — its
	// item_index must be greater than every row persisted before the
	// echo arrived. Anything else means we placed the queued message
	// above content that the agent had already produced.
	for _, r := range rows {
		if r.ID == queued.ID {
			continue
		}
		if r.ItemIndex >= queued.ItemIndex {
			t.Fatalf(
				"queued user_text item_index %d should be greater than "+
					"every row that arrived before its wire echo, but %s "+
					"has item_index %d (final ordering: %v)",
				queued.ItemIndex, r.ID, r.ItemIndex, idsByIndex,
			)
		}
	}
}

// TestHandleUserText_DeferredFlush_TurnBoundaryHonorsDispatchTurn pins the
// turn-placement contract for queued sends: a queued message dispatched
// at turn N must persist into turn N even if a new turn N+1 opened on
// the wire side between dispatch and the wire echo. The
// `pendingSend.TurnIndex` capture (mirrored on `DeferredItem.TurnIndex`)
// is what anchors the row to the dispatch-decided turn; if a future
// refactor stripped the capture and recomputed turn-from-current-open,
// the queued message would silently jump into the new turn.
func TestHandleUserText_DeferredFlush_TurnBoundaryHonorsDispatchTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	// Pre-existing row at turn 0, item_index 0 — establishes the
	// dispatch-time turn.
	if _, err := st.AppendItem(store.Item{
		ID:        "tool:before-flush",
		ThreadID:  "t1",
		TurnIndex: 0,
		Kind:      "tool_call",
		Role:      "assistant",
		Status:    "completed",
		Summary:   "pre-dispatch tool",
		ToolName:  "Bash",
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed pre-dispatch tool: %v", err)
	}

	queuedItem := store.Item{
		ID:        "user:0:flush:0",
		ThreadID:  "t1",
		TurnIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "queued at turn 0",
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}
	router.RegisterPendingFlushSend("t1", "queue:abc", queuedItem)

	// Turn 1 opens before the wire echo arrives — simulates the rare
	// case where the current turn completed and a new one started in
	// the gap between dispatch and echo.
	seedOpenTurn(t, router, st, "t1", 1)
	if _, err := st.AppendItem(store.Item{
		ID:        "tool:turn-one",
		ThreadID:  "t1",
		TurnIndex: 1,
		Kind:      "tool_call",
		Role:      "assistant",
		Status:    "completed",
		Summary:   "next turn tool",
		ToolName:  "Bash",
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed turn-1 tool: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "queued at turn 0",
		Meta:      json.RawMessage(`{"provider_item_id":"msg_echo_late"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle wire echo: %v", err)
	}

	queued, found, err := st.GetThreadItem("t1", queuedItem.ID)
	if err != nil || !found {
		t.Fatalf("queued row missing: found=%v err=%v", found, err)
	}
	if queued.TurnIndex != 0 {
		t.Fatalf("queued user_text TurnIndex = %d, want 0 (the dispatch-decided turn)", queued.TurnIndex)
	}
	if queued.ItemIndex != 1 {
		t.Fatalf("queued user_text ItemIndex = %d, want 1 (MAX+1 of turn 0)", queued.ItemIndex)
	}

	// Turn 1 must be untouched.
	turn1, err := st.ListItemsForTurn("t1", 1)
	if err != nil {
		t.Fatalf("list turn 1: %v", err)
	}
	if len(turn1) != 1 || turn1[0].ID != "tool:turn-one" || turn1[0].ItemIndex != 0 {
		t.Fatalf("turn 1 should be unchanged, got %+v", turn1)
	}
}

// TestHandleUserText_DeferredFlush_BatchOrderingFIFO pins the
// multi-queued-message contract: two queued sends registered in order,
// with content streamed between dispatch and the first wire echo, must
// each land at MAX+1 at their own echo time, preserving FIFO order. A
// regression that broke FIFO (or reintroduced index capture) would
// either reorder the two queued messages or place either of them
// before the intervening streaming row.
func TestHandleUserText_DeferredFlush_BatchOrderingFIFO(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	first := store.Item{
		ID:        "user:0:flush:1",
		ThreadID:  "t1",
		TurnIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "first queued",
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}
	second := first
	second.ID = "user:0:flush:2"
	second.Summary = "second queued"

	router.RegisterPendingFlushSend("t1", "queue:1", first)
	router.RegisterPendingFlushSend("t1", "queue:2", second)

	// Model streams a text row between dispatch and the first echo.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "assistant text emitted before either echo lands",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle text delta: %v", err)
	}

	// Wire echoes arrive in registration order. The FIFO at
	// consumePendingSendHead pops first the head, then the tail.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "first queued",
		Meta:      json.RawMessage(`{"provider_item_id":"msg_1"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle first echo: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "second queued",
		Meta:      json.RawMessage(`{"provider_item_id":"msg_2"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle second echo: %v", err)
	}

	firstRow, found, err := st.GetThreadItem("t1", first.ID)
	if err != nil || !found {
		t.Fatalf("first queued row missing: found=%v err=%v", found, err)
	}
	secondRow, found, err := st.GetThreadItem("t1", second.ID)
	if err != nil || !found {
		t.Fatalf("second queued row missing: found=%v err=%v", found, err)
	}

	rows, err := st.ListItemsForTurn("t1", 0)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}

	// Find the intervening text row's index for a relative assertion
	// (its exact id is generated by textItemID and not stable to assert).
	var textIndex int = -1
	for _, r := range rows {
		if r.Kind == "assistant_text" {
			textIndex = r.ItemIndex
			break
		}
	}
	if textIndex < 0 {
		t.Fatalf("expected an assistant_text row in turn 0, got %+v", rows)
	}

	if firstRow.ItemIndex <= textIndex {
		t.Fatalf("first queued ItemIndex %d must be greater than intervening text index %d", firstRow.ItemIndex, textIndex)
	}
	if secondRow.ItemIndex <= firstRow.ItemIndex {
		t.Fatalf("second queued ItemIndex %d must be greater than first queued %d (FIFO order)", secondRow.ItemIndex, firstRow.ItemIndex)
	}
}
