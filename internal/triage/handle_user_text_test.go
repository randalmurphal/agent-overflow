package triage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
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

// seedLaunchRow inserts the `tool_call` row an agent launch persists as,
// so a scoped event can resolve its turn from the launch it names.
func seedLaunchRow(t *testing.T, st *store.Store, threadID string, turnIndex int, itemID string) store.Item {
	t.Helper()
	now := time.Now().UnixMilli()
	persisted, err := st.UpsertItem(store.Item{
		ID:        itemID,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		Kind:      "tool_call",
		Role:      "assistant",
		Status:    "running",
		ToolName:  "Agent",
		Summary:   "Agent",
		CreatedAt: now,
		UpdatedAt: now,
	}, nil)
	if err != nil {
		t.Fatalf("seed tool_call row %s: %v", itemID, err)
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
	if err := st.UpsertMessageAnchor(store.MessageAnchor{
		ThreadID:   "t1",
		UserItemID: "user:0",
		TurnIndex:  0,
		CreatedAt:  time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed anchor: %v", err)
	}

	// Reset emissions captured during the seed so the assertion below
	// only sees the upsert produced by handleUserText.
	emissions.reset()

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
	// The parent uuid stamps into item meta in the SAME write as the
	// item id (round-5, R5-8): the anchor's copy below is a separate
	// follow-up that can fail, and the already-cut rollback retry needs
	// a durable parent it can slice through.
	if p, _ := meta["provider_parent_uuid"].(string); p != "parent-abc" {
		t.Fatalf("meta.provider_parent_uuid = %v, want parent-abc (full meta: %s)", meta["provider_parent_uuid"], persisted.Meta)
	}
	anchor, ok, err := st.GetMessageAnchor("t1", "user:0")
	if err != nil || !ok {
		t.Fatalf("anchor missing after provider id stamp: ok=%v err=%v", ok, err)
	}
	if anchor.ProviderUserMessageID != "msg_xyz" || anchor.ProviderParentUUID != "parent-abc" {
		t.Fatalf("anchor provider ids = %q/%q, want msg_xyz/parent-abc",
			anchor.ProviderUserMessageID, anchor.ProviderParentUUID)
	}

	// No new row should be minted under user:wire:msg_xyz when the
	// pending-send branch took ownership.
	if _, foundWire, err := st.GetThreadItem("t1", "user:wire:msg_xyz"); err != nil {
		t.Fatalf("probe wire row: %v", err)
	} else if foundWire {
		t.Fatalf("pending-send branch must not also mint a wire-only row")
	}

	upserts := itemUpsertEmissionsForID(emissions.snapshot(), "t1", "user:0")
	if len(upserts) != 1 {
		t.Fatalf("expected exactly one provider:item_event upsert for user:0, got %d (emissions: %+v)", len(upserts), emissions.snapshot())
	}
	if upserts[0].Meta != persisted.Meta {
		t.Fatalf("emitted upsert meta %q != persisted meta %q", upserts[0].Meta, persisted.Meta)
	}
}

// TestHandleUserText_DirectSendMatch_DoesNotReposition locks the `:flush:`
// scoping of the echo-side reposition. A direct send (`user:<turn>`) shares
// attachProviderItemIDToUserRow with queued flush rows, but unlike a queued
// message it is already at its intended timeline slot and must NOT be bumped
// to the turn tail. The guard is strings.Contains(aoItemID, ":flush:"); if it
// were dropped, a direct-send echo would reposition the prompt BELOW any
// assistant rows that landed after it. A sibling row at the same turn is
// seeded so a wrongful bump is observable — a sole row at its turn bumps to a
// near-identical slot and would hide the regression. (Steers, `:steer:`, take
// the same non-`:flush:` branch and are covered by the same scoping rule.)
func TestHandleUserText_DirectSendMatch_DoesNotReposition(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	router.RegisterPendingSend("t1", "user:0", 0)
	seedUserTextRow(t, st, "t1", 0, "original prompt", "")

	// An assistant row lands at the same turn AFTER the prompt. A wrongful
	// bump would move user:0 to MAX+1, i.e. below this row.
	now := time.Now().UnixMilli()
	if _, err := st.UpsertItem(store.Item{
		ID: "assistant:0:0", ThreadID: "t1", TurnIndex: 0,
		Kind: "assistant_text", Role: "assistant", Status: "completed",
		Summary: "response", CreatedAt: now + 1, UpdatedAt: now + 1,
	}, nil); err != nil {
		t.Fatalf("seed assistant row: %v", err)
	}

	before, found, err := st.GetThreadItem("t1", "user:0")
	if err != nil || !found {
		t.Fatalf("user:0 before echo: found=%v err=%v", found, err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "original prompt",
		Meta:      json.RawMessage(`{"provider_item_id":"msg_xyz"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle EventUserText: %v", err)
	}

	after, found, err := st.GetThreadItem("t1", "user:0")
	if err != nil || !found {
		t.Fatalf("user:0 after echo: found=%v err=%v", found, err)
	}
	if after.ItemIndex != before.ItemIndex {
		t.Fatalf("direct-send row repositioned on echo: item_index %d -> %d (the :flush: scoping guard must keep direct sends in place)", before.ItemIndex, after.ItemIndex)
	}
	// The stamp must still happen — scoping the bump must not skip the merge.
	var meta map[string]any
	if err := json.Unmarshal([]byte(after.Meta), &meta); err != nil {
		t.Fatalf("decode meta %q: %v", after.Meta, err)
	}
	if id, _ := meta["provider_item_id"].(string); id != "msg_xyz" {
		t.Fatalf("provider_item_id after echo = %v, want msg_xyz", meta["provider_item_id"])
	}
}

// TestHandleUserText_InterruptPromotedFlush_EchoDoesNotRebump reproduces
// the interrupt-time queued-message reordering bug (2026-07-03). A message
// queued mid-turn is quietly persisted at the active turn; the user
// interrupts, which promotes the row to the turn tail
// (PromoteQuietFlushSends — the visible cut point); then the interrupted
// turn's post-interrupt tail (a thinking block Claude flushes while
// stopping) lands BELOW the promoted message. The wire echo arriving with
// the response turn must stamp provider_item_id WITHOUT re-bumping the
// row — a second BumpItemToTurnEnd would leapfrog the message over the
// tail row, persisting an order the live view never showed (thinking
// above the message after a reload).
func TestHandleUserText_InterruptPromotedFlush_EchoDoesNotRebump(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 3)

	// Quiet eager persist at dispatch time (app_flush_queue.go eager path).
	now := time.Now().UnixMilli()
	flushRow := store.Item{
		ID:        "user:3:flush:0",
		ThreadID:  "t1",
		TurnIndex: 3,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "queued follow-up",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := router.PersistItemQuiet(flushRow, nil); err != nil {
		t.Fatalf("quiet persist flush row: %v", err)
	}
	router.RegisterPendingQuietFlushSend("t1", "queue:m1", flushRow, 4, now, "")

	// User interrupt → the promote anchors the row at the turn tail.
	if promoted := promoteQuietForTest(router, "t1"); len(promoted) != 1 {
		t.Fatalf("PromoteQuietFlushSends: got %d, want 1", len(promoted))
	}

	// The interrupted turn's post-interrupt tail: a thinking block Claude
	// flushes while stopping streams in AFTER the promote and lands below
	// the message (MAX+1).
	if _, err := st.AppendItem(store.Item{
		ID:        "thinking:3:tail",
		ThreadID:  "t1",
		TurnIndex: 3,
		Kind:      "thinking",
		Role:      "assistant",
		Status:    "completed",
		Summary:   "tail reasoning flushed on stop",
		CreatedAt: now + 1,
		UpdatedAt: now + 1,
	}); err != nil {
		t.Fatalf("seed post-interrupt tail row: %v", err)
	}

	// Echo arrives when Claude starts the response turn.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "queued follow-up",
		Meta:      json.RawMessage(`{"provider_item_id":"msg_flush_echo"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle wire echo: %v", err)
	}

	queued, found, err := st.GetThreadItem("t1", flushRow.ID)
	if err != nil || !found {
		t.Fatalf("flush row after echo: found=%v err=%v", found, err)
	}
	tail, found, err := st.GetThreadItem("t1", "thinking:3:tail")
	if err != nil || !found {
		t.Fatalf("tail row after echo: found=%v err=%v", found, err)
	}
	if queued.ItemIndex >= tail.ItemIndex {
		t.Fatalf(
			"promoted flush row re-bumped on echo: user item_index %d >= tail item_index %d — the message leapfrogged the post-interrupt tail the user watched stream below it",
			queued.ItemIndex, tail.ItemIndex,
		)
	}
	// The stamp must still happen — skipping the bump must not skip the merge.
	var meta map[string]any
	if err := json.Unmarshal([]byte(queued.Meta), &meta); err != nil {
		t.Fatalf("decode meta %q: %v", queued.Meta, err)
	}
	if id, _ := meta["provider_item_id"].(string); id != "msg_flush_echo" {
		t.Fatalf("provider_item_id after echo = %v, want msg_flush_echo", meta["provider_item_id"])
	}
}

// TestHandleUserText_EagerPersistedDeferredFlush_EchoDoesNotRebump covers
// the second interrupt eager-persist path (2026-07-05 recurrence of the
// 2026-07-03 bug). A message queued while the thread's `turns` row is
// already settled — e.g. between wire rounds of a multi-round logical
// turn — dispatches on the DEFERRED path, not the quiet path. On
// interrupt, EagerPersistDeferredFlushSends persists the row at the turn
// tail; the interrupted turn's post-interrupt thinking tail then lands
// below it. The echo must stamp without re-bumping, exactly like the
// quiet-promote path — this path was missed by the first fix.
func TestHandleUserText_EagerPersistedDeferredFlush_EchoDoesNotRebump(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 3)

	// Deferred registration at dispatch time (no active turns row).
	now := time.Now().UnixMilli()
	flushRow := store.Item{
		ID:        "user:3:flush:0",
		ThreadID:  "t1",
		TurnIndex: 3,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "queued follow-up",
		CreatedAt: now,
		UpdatedAt: now,
	}
	router.RegisterPendingFlushSendWithEnqueuedAt("t1", "queue:m1", flushRow, now, "")

	// User interrupt → the deferred row is eagerly persisted at the tail.
	persisted := eagerPersistForTest(router, "t1", router.OpenTurnIndex("t1"))
	if len(persisted) != 1 {
		t.Fatalf("EagerPersistDeferredFlushSends: got %d rows, want 1", len(persisted))
	}

	// The interrupted turn's post-interrupt thinking tail lands below the
	// message (MAX+1).
	if _, err := st.AppendItem(store.Item{
		ID:        "thinking:3:tail",
		ThreadID:  "t1",
		TurnIndex: 3,
		Kind:      "thinking",
		Role:      "assistant",
		Status:    "completed",
		Summary:   "tail reasoning flushed on stop",
		CreatedAt: now + 1,
		UpdatedAt: now + 1,
	}); err != nil {
		t.Fatalf("seed post-interrupt tail row: %v", err)
	}

	// Echo arrives when the provider starts the response turn.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "queued follow-up",
		Meta:      json.RawMessage(`{"provider_item_id":"msg_flush_echo"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle wire echo: %v", err)
	}

	queued, found, err := st.GetThreadItem("t1", flushRow.ID)
	if err != nil || !found {
		t.Fatalf("flush row after echo: found=%v err=%v", found, err)
	}
	tail, found, err := st.GetThreadItem("t1", "thinking:3:tail")
	if err != nil || !found {
		t.Fatalf("tail row after echo: found=%v err=%v", found, err)
	}
	if queued.ItemIndex >= tail.ItemIndex {
		t.Fatalf(
			"eager-persisted deferred flush row re-bumped on echo: user item_index %d >= tail item_index %d — the message leapfrogged the post-interrupt tail the user watched stream below it",
			queued.ItemIndex, tail.ItemIndex,
		)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(queued.Meta), &meta); err != nil {
		t.Fatalf("decode meta %q: %v", queued.Meta, err)
	}
	if id, _ := meta["provider_item_id"].(string); id != "msg_flush_echo" {
		t.Fatalf("provider_item_id after echo = %v, want msg_flush_echo", meta["provider_item_id"])
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

// A top-level EventUserText (no parent tool) that matches no pending send
// is provider-injected context, NOT user-authored. It must persist as a
// non-user `notification` row under an `injected:wire:` id — never a
// `user_text` bubble. This is the fix for the incident where a 2.1.x
// `<agent-message>` subagent report surfaced as a top-level user message.
func TestHandleUserText_NoPending_TopLevel_PersistsInjectedNotification(t *testing.T) {
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

	// The old buggy id must NOT exist — no user bubble.
	if _, found, _ := st.GetThreadItem("t1", "user:wire:codex_item_42"); found {
		t.Fatalf("top-level injected content must not persist as a user:wire user_text row")
	}

	persisted, found, err := st.GetThreadItem("t1", "injected:wire:codex_item_42")
	if err != nil || !found {
		t.Fatalf("expected injected:wire row to exist: found=%v err=%v", found, err)
	}
	if persisted.Kind != "notification" {
		t.Fatalf("Kind = %q, want notification", persisted.Kind)
	}
	if persisted.Role != "system" {
		t.Fatalf("Role = %q, want system", persisted.Role)
	}
	if persisted.Status != "completed" {
		t.Fatalf("Status = %q, want completed", persisted.Status)
	}
	if persisted.TurnIndex != 2 {
		t.Fatalf("TurnIndex = %d, want 2 (open turn)", persisted.TurnIndex)
	}
	if persisted.ParentID != "" {
		t.Fatalf("ParentID = %q, want empty (top-level)", persisted.ParentID)
	}
	if !strings.HasPrefix(persisted.Summary, "Injected provider context:") ||
		!strings.Contains(persisted.Summary, "task notification echo body") {
		t.Fatalf("Summary = %q, want an 'Injected provider context:' label carrying the body preview", persisted.Summary)
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
	if injected, _ := meta["injected"].(bool); !injected {
		t.Fatalf("meta.injected = %v, want true", meta["injected"])
	}
	if after := readThreadUpdatedAt(t, st, "t1"); after != before {
		t.Fatalf("threads.updated_at moved across injected-context EventUserText: before=%d after=%d", before, after)
	}

	upserts := itemUpsertEmissionsForID(emissions.snapshot(), "t1", "injected:wire:codex_item_42")
	if len(upserts) != 1 {
		t.Fatalf("expected one provider:item_event upsert for the injected-context row, got %d", len(upserts))
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

func TestSubagentLaunchPromptKeepsItsOpeningPositionWhenTranscriptIdentityArrivesLate(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	launchMeta := json.RawMessage(`{"toolName":"Agent","subagent_launch":true,"input":{"prompt":"Inspect the parser"}}`)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-1",
		ItemType: "Agent", Meta: launchMeta, Timestamp: time.UnixMilli(1_700_000_000_000),
	}); err != nil {
		t.Fatalf("launch agent: %v", err)
	}
	deliverSubagentBlock(t, router, "t1", "spawn-1", "child-text#0", "text", "I found the parser")

	launch, found, err := st.GetThreadItem("t1", "spawn-1")
	if err != nil || !found {
		t.Fatalf("load launch: found=%v err=%v", found, err)
	}
	if wrote, err := router.replaySubagentEvent("t1", launch, provider.ProviderEvent{
		Kind:            provider.EventUserText,
		ItemID:          "prompt-uuid",
		Content:         "Inspect the parser",
		ParentToolUseID: "spawn-1",
		Meta:            json.RawMessage(`{"subagent_opening_prompt":true}`),
		Timestamp:       time.UnixMilli(1_700_000_000_100),
	}); err != nil || !wrote {
		t.Fatalf("reconcile transcript prompt: %v", err)
	}

	children := childrenOfLaunch(t, st, "t1", "spawn-1", 0)
	if got, want := childIDs(children), []string{"user:subagent-prompt:spawn-1", TextItemID(0, "spawn-1", 1)}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("children = %v, want opening prompt before output %v", got, want)
	}
	if got := readProviderItemIDFromMeta(json.RawMessage(children[0].Meta)); got != "prompt-uuid" {
		t.Fatalf("opening prompt provider item id = %q, want prompt-uuid", got)
	}
	if decodeItemMetaMap(t, children[0].Meta)[provider.MetaSubagentPromptProvisionalKey] != nil {
		t.Fatalf("reconciled opening prompt retained provisional meta: %s", children[0].Meta)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:            provider.EventUserText,
		ThreadID:        "t1",
		Content:         "One more constraint",
		ParentToolUseID: "spawn-1",
		Meta:            json.RawMessage(`{"provider_item_id":"followup-uuid"}`),
		Timestamp:       time.UnixMilli(1_700_000_000_200),
	}); err != nil {
		t.Fatalf("persist later user-role delivery: %v", err)
	}
	children = childrenOfLaunch(t, st, "t1", "spawn-1", 0)
	if got, want := childIDs(children), []string{
		"user:subagent-prompt:spawn-1",
		TextItemID(0, "spawn-1", 1),
		"user:wire:followup-uuid",
	}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("children after later delivery = %v, want %v", got, want)
	}
}

// A subagent prompt lands on the LAUNCH's turn, not the thread's current
// one. A backgrounded agent's prompt can arrive from the transcript
// backfill long after the launching turn closed, and the pane reads a
// scope's rows within the launch's turn — a row filed under a later turn
// simply vanishes from the card it belongs to.
func TestHandleUserText_SubagentPromptLandsOnTheLaunchTurnNotTheOpenOne(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 2)
	seedLaunchRow(t, st, "t1", 2, "spawn-1")

	// The thread has moved on by the time the prompt arrives.
	seedOpenTurn(t, router, st, "t1", 5)

	if err := router.Handle(provider.ProviderEvent{
		Kind:            provider.EventUserText,
		ThreadID:        "t1",
		Content:         "Inspect the parser",
		ParentToolUseID: "spawn-1",
		Meta:            json.RawMessage(`{"provider_item_id":"child_prompt_late"}`),
		Timestamp:       time.UnixMilli(1_700_000_000_000),
	}); err != nil {
		t.Fatalf("Handle EventUserText: %v", err)
	}

	persisted, found, err := st.GetThreadItem("t1", "user:wire:child_prompt_late")
	if err != nil || !found {
		t.Fatalf("expected subagent prompt row to exist: found=%v err=%v", found, err)
	}
	if persisted.TurnIndex != 2 {
		t.Fatalf("TurnIndex = %d, want the launch's turn 2", persisted.TurnIndex)
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

	pending, ok := router.consumeMatchingPendingSend("t1", "")
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
		if r.ID == "injected:wire:dup_1" {
			wireRows++
		}
	}
	if wireRows != 1 {
		t.Fatalf("expected exactly one injected:wire:dup_1 row across two Handle calls, got %d (rows: %+v)", wireRows, rows)
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
	for _, e := range emissions.snapshot() {
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
	emissions.reset()

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

	for _, e := range emissions.snapshot() {
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

// TestHandleUserText_PendingSendMatch_HonoredID_FoldsParentUUID pins
// the common direct-send success path: app_send.go pre-stamps the
// minted uuid on the row + anchor, Claude honours it and echoes
// that exact id back plus the parentUuid it assigned. The echo must
// fold the echo-only parent_uuid into BOTH the item meta — the same
// write as the id key, so the already-cut retry has a durable parent
// the anchor follow-up can't lose (round-5, R5-8) — and the
// anchor. Since the parent is new, the meta write is not a no-op:
// exactly one upsert re-emits with the enriched meta.
func TestHandleUserText_PendingSendMatch_HonoredID_FoldsParentUUID(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	router.RegisterPendingSend("t1", "user:0", 0)
	// Row + anchor carry the honoured send uuid; the anchor has no
	// parent_uuid yet (Claude assigns parentUuid, unknown at pre-stamp).
	seedUserTextRow(t, st, "t1", 0, "ship it", `{"provider_item_id":"p"}`)
	if err := st.UpsertMessageAnchor(store.MessageAnchor{
		ThreadID:              "t1",
		UserItemID:            "user:0",
		TurnIndex:             0,
		ProviderUserMessageID: "p",
		CreatedAt:             time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed anchor: %v", err)
	}
	emissions.reset()

	var logBuf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(prev)

	// Echo carries the SAME id (honoured) plus the parentUuid Claude assigned.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "ship it",
		Meta:      json.RawMessage(`{"provider_item_id":"p","parent_uuid":"parent-honored"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle EventUserText: %v", err)
	}

	// Neither failure-mode log should fire: the ids match (no drift) and
	// the id is non-empty (no stuck-queue warning).
	if got := logBuf.String(); strings.Contains(got, "did not honour the supplied uuid") ||
		strings.Contains(got, "queue-confirm path will not fire") {
		t.Fatalf("honoured-id path must log nothing, got: %q", got)
	}

	// parent_uuid — known only from the echo — must be folded into the
	// anchor even though the message id is unchanged.
	anchor, ok, err := st.GetMessageAnchor("t1", "user:0")
	if err != nil || !ok {
		t.Fatalf("anchor missing: ok=%v err=%v", ok, err)
	}
	if anchor.ProviderUserMessageID != "p" || anchor.ProviderParentUUID != "parent-honored" {
		t.Fatalf("anchor provider ids = %q/%q, want p/parent-honored (fold in the echoed parent_uuid)",
			anchor.ProviderUserMessageID, anchor.ProviderParentUUID)
	}

	// The parent is new to the row meta, so exactly one upsert re-emits
	// with the enriched meta (R5-8).
	upserts := itemUpsertEmissionsForID(emissions.snapshot(), "t1", "user:0")
	if len(upserts) != 1 {
		t.Fatalf("honoured-id path emitted %d upserts for user:0, want 1 (the parent-enriched meta)", len(upserts))
	}
	persisted, found, err := st.GetThreadItem("t1", "user:0")
	if err != nil || !found {
		t.Fatalf("expected user:0 to exist: found=%v err=%v", found, err)
	}
	if usermessage.ReadProviderItemID(persisted.Meta) != "p" {
		t.Fatalf("row provider_item_id mutated: %q", persisted.Meta)
	}
	if usermessage.ReadProviderParentUUID(persisted.Meta) != "parent-honored" {
		t.Fatalf("row meta missing the echoed parent (R5-8): %q", persisted.Meta)
	}
	if upserts[0].Meta != persisted.Meta {
		t.Fatalf("emitted upsert meta %q != persisted meta %q", upserts[0].Meta, persisted.Meta)
	}

	// A REPLAYED echo (both ids already stored) is a true no-op: no
	// further write, no re-emit.
	emissions.reset()
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "ship it",
		Meta:      json.RawMessage(`{"provider_item_id":"p","parent_uuid":"parent-honored"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("replayed echo: %v", err)
	}
	if replays := itemUpsertEmissionsForID(emissions.snapshot(), "t1", "user:0"); len(replays) != 0 {
		t.Fatalf("replayed echo re-emitted %d upserts, want 0", len(replays))
	}
}

// TestHandleUserText_PreStampedIDDrift_OverwritesAndLogs pins the
// send-time-uuid contract's failure mode. app_send.go stamps a minted
// uuid onto the row meta and sends it to Claude as the envelope's
// top-level uuid; Claude is expected to echo that exact id back. If the
// echo carries a DIFFERENT id (Claude did not honour the supplied uuid
// — a binary-contract drift), attachProviderItemIDToUserRow must
// overwrite the row + anchor to the echoed id (the real transcript
// uuid, the correct slice anchor) and log loudly so the regression is
// observable instead of silently falling back to the ordinal walk.
func TestHandleUserText_PreStampedIDDrift_OverwritesAndLogs(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	router.RegisterPendingSend("t1", "user:0", 0)
	// Row + anchor carry the minted send uuid (the pre-stamp).
	seedUserTextRow(t, st, "t1", 0, "fix the bug", `{"provider_item_id":"minted-uuid"}`)
	if err := st.UpsertMessageAnchor(store.MessageAnchor{
		ThreadID:              "t1",
		UserItemID:            "user:0",
		TurnIndex:             0,
		ProviderUserMessageID: "minted-uuid",
		CreatedAt:             time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed anchor: %v", err)
	}
	emissions.reset()

	var logBuf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(prev)

	// Echo carries a DIFFERENT id than the pre-stamp.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "fix the bug",
		Meta:      json.RawMessage(`{"provider_item_id":"claude-real-uuid","parent_uuid":"parent-xyz"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle EventUserText: %v", err)
	}

	if !strings.Contains(logBuf.String(), "did not honour the supplied uuid") {
		t.Fatalf("expected drift log about Claude not honouring the supplied uuid, got: %q", logBuf.String())
	}

	persisted, found, err := st.GetThreadItem("t1", "user:0")
	if err != nil || !found {
		t.Fatalf("expected user:0 to exist: found=%v err=%v", found, err)
	}
	if got := readProviderItemIDFromMeta(json.RawMessage(persisted.Meta)); got != "claude-real-uuid" {
		t.Fatalf("row provider_item_id = %q, want claude-real-uuid (overwrite to the echoed id)", got)
	}

	anchor, ok, err := st.GetMessageAnchor("t1", "user:0")
	if err != nil || !ok {
		t.Fatalf("anchor missing: ok=%v err=%v", ok, err)
	}
	if anchor.ProviderUserMessageID != "claude-real-uuid" || anchor.ProviderParentUUID != "parent-xyz" {
		t.Fatalf("anchor provider ids = %q/%q, want claude-real-uuid/parent-xyz (self-heal to the echoed id)",
			anchor.ProviderUserMessageID, anchor.ProviderParentUUID)
	}

	upserts := itemUpsertEmissionsForID(emissions.snapshot(), "t1", "user:0")
	if len(upserts) != 1 {
		t.Fatalf("expected exactly one upsert for the overwrite, got %d", len(upserts))
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
	if _, ok := router.consumeMatchingPendingSend("t1", ""); ok {
		t.Fatalf("pending FIFO should be drained even when the row was missing")
	}

	// And no spurious upsert emission should reach the frontend.
	for _, e := range emissions.snapshot() {
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

	// Wire echoes arrive in registration order; with no expected ids
	// registered, consumeMatchingPendingSend pops FIFO — head, then tail.
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
	// (its exact id is generated by TextItemID and not stable to assert).
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

// TestHandleUserText_MismatchedEchoDoesNotConsumeIdentityPending is the
// regression guard for the FIFO-mispair window: while a send awaits its echo,
// a provider-injected user envelope (e.g. an <agent-message> subagent report
// that slipped a parser denylist) arrives FIRST. Under order-based matching it
// would steal the pending entry and stamp the user's row with the injection's
// id; under identity matching it must consume nothing, land as an
// injected-context notification, and leave the entry for the real echo.
func TestHandleUserText_MismatchedEchoDoesNotConsumeIdentityPending(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	router.RegisterPendingSendExpecting("t1", "user:0", 0, "uuid-mine")
	seedUserTextRow(t, st, "t1", 0, "hello world", "")

	// Injected envelope arrives before our echo, with a different uuid.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "subagent final report body",
		Meta:      json.RawMessage(`{"provider_item_id":"uuid-injected"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle injected EventUserText: %v", err)
	}

	// The user's row must be untouched — no provider_item_id stamp.
	persisted, found, err := st.GetThreadItem("t1", "user:0")
	if err != nil || !found {
		t.Fatalf("user:0 row: found=%v err=%v", found, err)
	}
	if strings.Contains(persisted.Meta, "uuid-injected") {
		t.Fatalf("injected echo stamped the user's row: meta=%q", persisted.Meta)
	}

	// The pending entry must survive for the real echo.
	if !router.HasPendingSendForThread("t1") {
		t.Fatalf("injected echo consumed the pending send entry")
	}

	// The injection lands as a notification row, never a user bubble.
	if _, found, _ := st.GetThreadItem("t1", "user:wire:uuid-injected"); found {
		t.Fatalf("injected echo persisted as a user:wire user_text row")
	}
	if _, found, _ := st.GetThreadItem("t1", "injected:wire:uuid-injected"); !found {
		t.Fatalf("injected echo did not persist an injected:wire notification row")
	}

	// The real echo then matches by identity and stamps the row.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "hello world",
		Meta:      json.RawMessage(`{"provider_item_id":"uuid-mine"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle real EventUserText: %v", err)
	}
	persisted, found, err = st.GetThreadItem("t1", "user:0")
	if err != nil || !found {
		t.Fatalf("user:0 row after real echo: found=%v err=%v", found, err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(persisted.Meta), &meta); err != nil {
		t.Fatalf("decode meta %q: %v", persisted.Meta, err)
	}
	if id, _ := meta["provider_item_id"].(string); id != "uuid-mine" {
		t.Fatalf("meta.provider_item_id = %v, want uuid-mine", meta["provider_item_id"])
	}
	if router.HasPendingSendForThread("t1") {
		t.Fatalf("real echo did not consume the pending send entry")
	}
}

// TestHandleUserText_MismatchedEchoDoesNotPersistDeferredFlush covers the
// deferred-flush variant of the mispair window: a queued message dispatched
// mid-turn defers its row persist until the echo. An injected envelope with a
// different uuid must not trigger that persist (the queued bubble would show
// as delivered before the model saw it); only the matching echo may.
func TestHandleUserText_MismatchedEchoDoesNotPersistDeferredFlush(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	queuedItem := store.Item{
		ID:        "user:0:flush:0",
		ThreadID:  "t1",
		TurnIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "queued follow-up",
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}
	router.RegisterPendingFlushSendWithEnqueuedAt("t1", "queue:abc", queuedItem, 0, "uuid-mine")

	// Injected envelope with a different uuid arrives first.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "subagent final report body",
		Meta:      json.RawMessage(`{"provider_item_id":"uuid-injected"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle injected EventUserText: %v", err)
	}

	if _, found, _ := st.GetThreadItem("t1", queuedItem.ID); found {
		t.Fatalf("injected echo persisted the deferred flush row")
	}
	if _, found, _ := st.GetThreadItem("t1", "injected:wire:uuid-injected"); !found {
		t.Fatalf("injected echo did not persist an injected:wire notification row")
	}
	if !router.HasPendingSendForThread("t1") {
		t.Fatalf("injected echo consumed the deferred flush entry")
	}

	// The matching echo persists the deferred row with the id stamped.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "queued follow-up",
		Meta:      json.RawMessage(`{"provider_item_id":"uuid-mine"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle real EventUserText: %v", err)
	}
	persisted, found, err := st.GetThreadItem("t1", queuedItem.ID)
	if err != nil || !found {
		t.Fatalf("deferred flush row after real echo: found=%v err=%v", found, err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(persisted.Meta), &meta); err != nil {
		t.Fatalf("decode meta %q: %v", persisted.Meta, err)
	}
	if id, _ := meta["provider_item_id"].(string); id != "uuid-mine" {
		t.Fatalf("meta.provider_item_id = %v, want uuid-mine", meta["provider_item_id"])
	}
}

// TestHandleUserText_FlushConfirmedHook_EagerQuietRows pins the
// checkpoint-capture moments for flush rows (2026-07-16, revised for
// round-7 R7-1): the confirmed hook fires once per quiet flush echo —
// after the turn-tail bump and provider-id stamp — and never for
// direct sends (captured at send time in app_send.go).
// Interrupt-anchored rows capture TWICE: once at promote time (the
// baseline, in case the session dies before the echo) and again at the
// echo (the consumption-boundary refresh — the interrupt-time snapshot
// predates the interrupted tail's and sibling queued messages'
// responses, which the conversation cut keeps). Before the echo hook
// existed at all, quiet flush rows never got a checkpoint, so a
// message queued into an active Claude turn was permanently
// non-revertable.
func TestHandleUserText_FlushConfirmedHook_EagerQuietRows(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 3)

	var hookCalls []store.Item
	router.SetFlushUserTextConfirmedHook(func(threadID string, item store.Item) {
		if threadID != "t1" {
			t.Fatalf("hook threadID = %q, want t1", threadID)
		}
		hookCalls = append(hookCalls, item)
	})

	now := time.Now().UnixMilli()

	// Direct send: pre-persisted row, no queue item id. The echo stamps
	// but must NOT fire the hook.
	seedUserTextRow(t, st, "t1", 3, "direct send", "")
	router.RegisterPendingSendExpecting("t1", "user:3", 3, "uuid-direct")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1", Content: "direct send",
		Meta:      json.RawMessage(`{"provider_item_id":"uuid-direct"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("direct echo: %v", err)
	}
	if len(hookCalls) != 0 {
		t.Fatalf("hook fired for a direct send (%d calls) — direct checkpoints are captured at send time", len(hookCalls))
	}

	// Eager quiet flush row: the echo bump+stamp is its capture moment.
	quietRow := store.Item{
		ID: "user:3:flush:0", ThreadID: "t1", TurnIndex: 3,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "queued mid-turn", CreatedAt: now, UpdatedAt: now,
	}
	if err := router.PersistItemQuiet(quietRow, nil); err != nil {
		t.Fatalf("quiet persist: %v", err)
	}
	router.RegisterPendingQuietFlushSend("t1", "queue:m1", quietRow, 4, now, "uuid-quiet")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1", Content: "queued mid-turn",
		Meta:      json.RawMessage(`{"provider_item_id":"uuid-quiet"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("quiet echo: %v", err)
	}
	if len(hookCalls) != 1 {
		t.Fatalf("hook calls after quiet echo = %d, want 1", len(hookCalls))
	}
	if hookCalls[0].ID != "user:3:flush:0" {
		t.Fatalf("hook item = %s, want user:3:flush:0", hookCalls[0].ID)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(hookCalls[0].Meta), &meta); err != nil {
		t.Fatalf("decode hook item meta: %v", err)
	}
	if id, _ := meta["provider_item_id"].(string); id != "uuid-quiet" {
		t.Fatalf("hook must receive the STAMPED row (provider_item_id %v, want uuid-quiet) — the checkpoint mirrors this id at capture", meta["provider_item_id"])
	}

	// Interrupt-anchored quiet row: the promote captures the baseline;
	// the echo must re-fire the hook so the checkpoint refreshes at the
	// consumption boundary (round-7, R7-1).
	anchoredRow := store.Item{
		ID: "user:3:flush:1", ThreadID: "t1", TurnIndex: 3,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "queued then interrupted", CreatedAt: now + 1, UpdatedAt: now + 1,
	}
	if err := router.PersistItemQuiet(anchoredRow, nil); err != nil {
		t.Fatalf("quiet persist anchored: %v", err)
	}
	router.RegisterPendingQuietFlushSend("t1", "queue:m2", anchoredRow, 4, now+1, "uuid-anchored")
	if promoted := promoteQuietForTest(router, "t1"); len(promoted) != 1 {
		t.Fatalf("promote: got %d, want 1", len(promoted))
	}
	// The promote is the anchored row's baseline capture, INSIDE the
	// anchor-lock-held call — before the mutex releases, so an echo in
	// the gap can never find the checkpoint missing (round-4, CT4-1).
	if len(hookCalls) != 2 {
		t.Fatalf("hook calls after promote = %d, want 2 (promote captures the anchored row)", len(hookCalls))
	}
	if hookCalls[1].ID != "user:3:flush:1" {
		t.Fatalf("promote hook item = %s, want user:3:flush:1", hookCalls[1].ID)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1", Content: "queued then interrupted",
		Meta:      json.RawMessage(`{"provider_item_id":"uuid-anchored"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("anchored echo: %v", err)
	}
	if len(hookCalls) != 3 {
		t.Fatalf("hook calls after anchored echo = %d, want 3 (echo refreshes the anchored row's checkpoint at the consumption boundary — round-7, R7-1)", len(hookCalls))
	}
	if hookCalls[2].ID != "user:3:flush:1" {
		t.Fatalf("echo refresh hook item = %s, want user:3:flush:1", hookCalls[2].ID)
	}
	if err := json.Unmarshal([]byte(hookCalls[2].Meta), &meta); err != nil {
		t.Fatalf("decode echo refresh item meta: %v", err)
	}
	if id, _ := meta["provider_item_id"].(string); id != "uuid-anchored" {
		t.Fatalf("echo refresh must receive the STAMPED row (provider_item_id %v, want uuid-anchored) — the replacement checkpoint mirrors this id at capture", meta["provider_item_id"])
	}
}
