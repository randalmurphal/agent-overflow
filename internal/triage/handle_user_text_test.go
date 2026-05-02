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

	// Reset emissions captured during the seed so the assertion below
	// only sees the upsert produced by handleUserText.
	*emissions = (*emissions)[:0]

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "hello world",
		Meta:      json.RawMessage(`{"provider_item_id":"msg_xyz"}`),
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

	upserts := itemUpsertEmissionsForID(*emissions, "t1", "user:wire:codex_item_42")
	if len(upserts) != 1 {
		t.Fatalf("expected one provider:item_event upsert for the wire-only row, got %d", len(upserts))
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

func TestMergeProviderItemIDIntoMeta(t *testing.T) {
	t.Run("empty existing", func(t *testing.T) {
		got, err := mergeProviderItemIDIntoMeta("", "msg_a")
		if err != nil {
			t.Fatalf("merge: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(got), &m); err != nil {
			t.Fatalf("decode %q: %v", got, err)
		}
		if id, _ := m["provider_item_id"].(string); id != "msg_a" {
			t.Fatalf("provider_item_id = %v, want msg_a", m["provider_item_id"])
		}
	})

	t.Run("preserves existing keys", func(t *testing.T) {
		got, err := mergeProviderItemIDIntoMeta(`{"foo":"bar","attachments":[1,2]}`, "msg_b")
		if err != nil {
			t.Fatalf("merge: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(got), &m); err != nil {
			t.Fatalf("decode %q: %v", got, err)
		}
		if m["foo"] != "bar" {
			t.Fatalf("lost foo: %v", m)
		}
		if id, _ := m["provider_item_id"].(string); id != "msg_b" {
			t.Fatalf("provider_item_id = %v", m["provider_item_id"])
		}
	})

	t.Run("empty providerItemID returns existing", func(t *testing.T) {
		got, err := mergeProviderItemIDIntoMeta(`{"foo":"bar"}`, "")
		if err != nil {
			t.Fatalf("merge: %v", err)
		}
		if got != `{"foo":"bar"}` {
			t.Fatalf("got %q, want existing unchanged", got)
		}
	})

	t.Run("idempotent on same id", func(t *testing.T) {
		input := `{"foo":"bar","provider_item_id":"msg_c"}`
		got, err := mergeProviderItemIDIntoMeta(input, "msg_c")
		if err != nil {
			t.Fatalf("merge: %v", err)
		}
		if got != input {
			t.Fatalf("idempotent merge returned %q, want %q", got, input)
		}
	})

	t.Run("malformed existing returns error", func(t *testing.T) {
		if _, err := mergeProviderItemIDIntoMeta(`not json`, "msg_d"); err == nil {
			t.Fatalf("expected error for malformed meta")
		}
	})
}
