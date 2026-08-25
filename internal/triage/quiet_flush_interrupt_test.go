package triage

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/itemmeta"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// TestPromotedQuietFlushEcho_StampsProviderOrderBoundary pins the
// mid-loop consumption of an interrupt-promoted quiet flush row: the
// echo arrives with the round still open, the row must NOT be re-bumped
// (the interrupt already placed it), and the echo must stamp the
// provider-order boundary — the turn's max item_index at echo time — so
// revert can tell the interrupted tail (provider-order BEFORE the
// queued_command attachment) from the response that streams below the
// row afterwards (provider-order AFTER).
func TestPromotedQuietFlushEcho_StampsProviderOrderBoundary(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := st.InsertItem(store.Item{
		ID: "user:0", ThreadID: "t1", TurnIndex: 0, ItemIndex: 0,
		Kind: "user_text", Role: "user", Status: "completed",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert prompt: %v", err)
	}
	// Partial reply streams before the queue dispatch (text:0:0, idx 1).
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1",
		Content: "partial reply", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("pre text: %v", err)
	}
	if err := router.settleStreamingScope("t1", ""); err != nil {
		t.Fatalf("pre settle: %v", err)
	}

	// Eager quiet flush dispatch into the active turn.
	flushRow := store.Item{
		ID: "user:0:flush:1", ThreadID: "t1", TurnIndex: 0,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "queued while streaming", CreatedAt: now + 2, UpdatedAt: now + 2,
	}
	if err := router.PersistItemQuiet(flushRow, nil); err != nil {
		t.Fatalf("quiet persist: %v", err)
	}
	router.RegisterPendingQuietFlushSendWithExpectation("t1", "queue:q1", flushRow, 1, now+2, PendingSendExpectation{ProviderItemID: "ao-uuid-1"})

	// Interrupt: the promote bumps the row to the turn tail and marks it.
	promoted := promoteQuietForTest(router, "t1")
	if len(promoted) != 1 || promoted[0].ID != "user:0:flush:1" {
		t.Fatalf("promoted = %+v, want the flush row", promoted)
	}
	promotedIdx := promoted[0].ItemIndex

	// The interrupted round's tail persists BELOW the promoted row.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1",
		Content: "interrupted tail", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tail text: %v", err)
	}
	if err := router.settleStreamingScope("t1", ""); err != nil {
		t.Fatalf("tail settle: %v", err)
	}

	// Mid-loop echo: the CLI consumed the promoted message with the round
	// still open — no system.init, no EventTurnStart.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1",
		Content:   "queued while streaming",
		Meta:      json.RawMessage(`{"provider_item_id":"ao-uuid-1"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("echo: %v", err)
	}

	row, found, err := st.GetThreadItem("t1", "user:0:flush:1")
	if err != nil || !found {
		t.Fatalf("flush row: found=%v err=%v", found, err)
	}
	if row.ItemIndex != promotedIdx {
		t.Errorf("row item_index = %d, want %d (anchored row must not re-bump at echo)", row.ItemIndex, promotedIdx)
	}
	state, err := itemmeta.DecodePromotionState(row.Meta)
	if err != nil {
		t.Fatalf("decode promotion state: %v", err)
	}
	if !state.Promoted {
		t.Fatal("promotion marker lost across the echo stamp")
	}
	tailRow, found, err := st.GetThreadItem("t1", "text:0:1")
	if err != nil || !found {
		t.Fatalf("tail row text:0:1: found=%v err=%v", found, err)
	}
	if !state.HasEchoBoundary || state.EchoBoundary != tailRow.ItemIndex {
		t.Fatalf("echo boundary = (%v, %d), want stamped at the tail row's index %d",
			state.HasEchoBoundary, state.EchoBoundary, tailRow.ItemIndex)
	}
	if usermessage.ReadProviderItemID(row.Meta) != "ao-uuid-1" {
		t.Errorf("provider_item_id not stamped: %q", row.Meta)
	}

	// The response streams below the row in the SAME still-open turn.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1",
		Content: "response to queued", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("response text: %v", err)
	}
	if err := router.settleStreamingScope("t1", ""); err != nil {
		t.Fatalf("response settle: %v", err)
	}

	// End to end: reverting at the promoted row keeps the interrupted tail
	// (provider-order before the attachment) and cuts the response
	// (provider-order after it).
	if _, _, err := st.DeleteConversationFromItem("t1", "user:0:flush:1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var got []string
	for _, it := range items {
		got = append(got, it.ID)
	}
	want := []string{"user:0", "text:0:0", "text:0:1"} // prompt, partial reply, interrupted tail — response text:0:2 cut
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("survivors after revert = %v, want %v", got, want)
	}
}

// TestPromoteQuietFlushSends_FailedBumpUnclaimsAnchor pins the
// promote-failure fallback: when the bump-and-mark store write fails
// (row missing — the dispatch errored after registration), the
// AnchoredAtInterrupt claim must be reverted so the echo-time bump
// remains that row's positioning; entries whose write succeeded keep
// the claim so their echo stays stamp-only.
func TestPromoteQuietFlushSends_FailedBumpUnclaimsAnchor(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	now := time.Now().UnixMilli()
	okRow := store.Item{
		ID: "user:0:flush:1", ThreadID: "t1", TurnIndex: 0,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "persisted", CreatedAt: now, UpdatedAt: now,
	}
	if err := router.PersistItemQuiet(okRow, nil); err != nil {
		t.Fatalf("quiet persist: %v", err)
	}
	router.RegisterPendingQuietFlushSendWithExpectation("t1", "queue:q1", okRow, 1, now, PendingSendExpectation{ProviderItemID: "ao-uuid-1"})
	missingRow := store.Item{
		ID: "user:0:flush:2", ThreadID: "t1", TurnIndex: 0,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "never persisted", CreatedAt: now, UpdatedAt: now,
	}
	router.RegisterPendingQuietFlushSendWithExpectation("t1", "queue:q2", missingRow, 1, now, PendingSendExpectation{ProviderItemID: "ao-uuid-2"})

	promoted := promoteQuietForTest(router, "t1")
	if len(promoted) != 1 || promoted[0].ID != "user:0:flush:1" {
		t.Fatalf("promoted = %+v, want only the persisted row", promoted)
	}

	anchors := map[string]bool{}
	router.mu.Lock()
	for _, entry := range router.state("t1").pendingSends {
		anchors[entry.AOItemID] = entry.AnchoredAtInterrupt
	}
	router.mu.Unlock()
	if !anchors["user:0:flush:1"] {
		t.Error("persisted row's entry lost its anchor claim")
	}
	if anchors["user:0:flush:2"] {
		t.Error("failed bump left the anchor claimed — echo would skip its fallback bump")
	}

	// The promoted row carries the durable marker; a second interrupt must
	// not re-bump it (already anchored).
	row, _, err := st.GetThreadItem("t1", "user:0:flush:1")
	if err != nil {
		t.Fatalf("reload row: %v", err)
	}
	state, err := itemmeta.DecodePromotionState(row.Meta)
	if err != nil || !state.Promoted {
		t.Fatalf("promotion marker: state=%+v err=%v", state, err)
	}
	if again := promoteQuietForTest(router, "t1"); len(again) != 0 {
		t.Fatalf("second promote re-bumped %d rows, want 0", len(again))
	}

	// The unclaimed entry's echo self-heals: the row was consumed by the
	// provider (the echo proves it) but never made it to the store, so
	// the attach path re-persists the retained QuietItem copy and stamps
	// it — the timeline must not lose a message the provider context
	// contains.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1", Content: "never persisted",
		Meta:      json.RawMessage(`{"provider_item_id":"ao-uuid-2"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("echo for missing row: %v", err)
	}
	healed, found, err := st.GetThreadItem("t1", "user:0:flush:2")
	if err != nil || !found {
		t.Fatalf("self-healed row: found=%v err=%v", found, err)
	}
	if healed.Summary != "never persisted" {
		t.Errorf("self-healed summary = %q", healed.Summary)
	}
	healedMeta := decodeTriageMetaMap(t, healed.Meta)
	if healedMeta["provider_item_id"] != "ao-uuid-2" {
		t.Errorf("self-healed row not stamped: %q", healed.Meta)
	}
}

// TestPromoteRacesEcho_ConvergesToALegalState hammers the
// promote-vs-echo interleaving under the race detector: whichever side
// wins, the row must end up provider-id-stamped exactly once, never
// duplicated as injected context, and carrying the promotion marker IFF
// the promote's bump committed before the echo consumed the entry
// (the flush anchor lock makes any other combination unobservable).
func TestPromoteRacesEcho_ConvergesToALegalState(t *testing.T) {
	for i := 0; i < 12; i++ {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		now := time.Now().UnixMilli()
		row := store.Item{
			ID: "user:0:flush:1", ThreadID: "t1", TurnIndex: 0,
			Kind: "user_text", Role: "user", Status: "completed",
			Summary: "raced", CreatedAt: now, UpdatedAt: now,
		}
		if err := router.PersistItemQuiet(row, nil); err != nil {
			t.Fatalf("quiet persist: %v", err)
		}
		router.RegisterPendingQuietFlushSendWithExpectation("t1", "queue:q1", row, 1, now, PendingSendExpectation{ProviderItemID: "ao-race"})

		done := make(chan struct{})
		go func() {
			defer close(done)
			promoteQuietForTest(router, "t1")
		}()
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventUserText, ThreadID: "t1", Content: "raced",
			Meta:      json.RawMessage(`{"provider_item_id":"ao-race"}`),
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("echo: %v", err)
		}
		<-done

		got, found, err := st.GetThreadItem("t1", "user:0:flush:1")
		if err != nil || !found {
			t.Fatalf("row: found=%v err=%v", found, err)
		}
		meta := decodeTriageMetaMap(t, got.Meta)
		if meta["provider_item_id"] != "ao-race" {
			t.Fatalf("iteration %d: row not stamped: %q", i, got.Meta)
		}
		if _, found, _ := st.GetThreadItem("t1", "injected:wire:ao-race"); found {
			t.Fatalf("iteration %d: race produced an injected duplicate", i)
		}
		items, err := st.ListItems("t1")
		if err != nil {
			t.Fatalf("list items: %v", err)
		}
		if len(items) != 1 {
			t.Fatalf("iteration %d: %d rows, want exactly the flush row", i, len(items))
		}
	}
}

func decodeTriageMetaMap(t *testing.T, meta string) map[string]any {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal([]byte(meta), &m); err != nil {
		t.Fatalf("decode meta %q: %v", meta, err)
	}
	return m
}

// TestEagerPersistDeferredFlushSends_DeathRecoveryKeepsQuietItem pins
// CT3: a deferred flush row the interrupt eagerly persisted must stay
// recoverable when the session dies before the echo —
// DrainUnconfirmedFlushItems has to hand the app layer a QuietItem
// (message text for the draft restore, row id for the timeline delete),
// not an empty husk.
func TestEagerPersistDeferredFlushSends_DeathRecoveryKeepsQuietItem(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	router.RegisterPendingFlushSendWithExpectation("t1", "queue:q1", store.Item{
		ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "queued text to recover",
	}, time.Now().UnixMilli(), PendingSendExpectation{ProviderItemID: "ao-uuid-1"})

	persisted := eagerPersistForTest(router, "t1", router.OpenTurnIndex("t1"))
	if len(persisted) != 1 {
		t.Fatalf("eager persist = %+v, want one row", persisted)
	}
	row, found, err := st.GetThreadItem("t1", "user:1:flush:1")
	if err != nil || !found {
		t.Fatalf("persisted row: found=%v err=%v", found, err)
	}
	// The snapshot must carry the STORE-assigned position — checkpoint
	// capture orders same-turn checkpoints by it (cold-2).
	if persisted[0].ItemIndex != row.ItemIndex {
		t.Errorf("snapshot item_index = %d, want store-assigned %d", persisted[0].ItemIndex, row.ItemIndex)
	}

	drained := router.DrainUnconfirmedFlushItems("t1")
	if len(drained) != 1 {
		t.Fatalf("drained = %+v, want one entry", drained)
	}
	entry := drained[0]
	if entry.UserItemID != "user:1:flush:1" || entry.QueueItemID != "queue:q1" {
		t.Errorf("drained ids = %+v", entry)
	}
	if entry.DeferredItem != nil {
		t.Error("eager-persisted entry still reports DeferredItem — death path would treat the persisted row as never-written")
	}
	if entry.QuietItem == nil {
		t.Fatal("QuietItem lost — death path cannot restore the draft or delete the orphaned row")
	}
	if entry.Message != "queued text to recover" {
		t.Errorf("Message = %q, want the queued text", entry.Message)
	}
}

// TestConsumedEchoReplay_DoesNotPersistInjectedRow pins CT5: once an
// AO-sent user message's echo is consumed (pending entry popped), a
// session-resume REPLAY of the same envelope finds no pending match and
// falls to the wire-only branch — which must dedup on the consumed id
// instead of persisting an injected:wire:* duplicate of the user's own
// message. Covered per consumption shape: the eager quiet flush stamp,
// the pre-stamped direct send (unchanged-meta early return), and the
// deferred flush persist.
func TestConsumedEchoReplay_DoesNotPersistInjectedRow(t *testing.T) {
	echo := func(uuid, content string) provider.ProviderEvent {
		return provider.ProviderEvent{
			Kind: provider.EventUserText, ThreadID: "t1", Content: content,
			Meta:      json.RawMessage(`{"provider_item_id":"` + uuid + `"}`),
			Timestamp: time.Now(),
		}
	}

	t.Run("eager quiet flush", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		now := time.Now().UnixMilli()
		row := store.Item{
			ID: "user:0:flush:1", ThreadID: "t1", TurnIndex: 0,
			Kind: "user_text", Role: "user", Status: "completed",
			Summary: "queued", CreatedAt: now, UpdatedAt: now,
		}
		if err := router.PersistItemQuiet(row, nil); err != nil {
			t.Fatalf("quiet persist: %v", err)
		}
		router.RegisterPendingQuietFlushSendWithExpectation("t1", "queue:q1", row, 1, now, PendingSendExpectation{ProviderItemID: "uuid-flush"})

		if err := router.Handle(echo("uuid-flush", "queued")); err != nil {
			t.Fatalf("echo: %v", err)
		}
		if err := router.Handle(echo("uuid-flush", "queued")); err != nil {
			t.Fatalf("replay: %v", err)
		}
		if _, found, _ := st.GetThreadItem("t1", "injected:wire:uuid-flush"); found {
			t.Fatal("replayed flush echo persisted an injected-context duplicate")
		}
	})

	t.Run("pre-stamped direct send", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		meta, err := usermessage.MergeProviderItemID("", "uuid-direct")
		if err != nil {
			t.Fatalf("pre-stamp meta: %v", err)
		}
		now := time.Now().UnixMilli()
		if err := st.InsertItem(store.Item{
			ID: "user:0", ThreadID: "t1", TurnIndex: 0, ItemIndex: 0,
			Kind: "user_text", Role: "user", Status: "completed",
			Summary: "direct", Meta: meta, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("insert row: %v", err)
		}
		router.RegisterPendingSendWithExpectation("t1", "user:0", 0, PendingSendExpectation{ProviderItemID: "uuid-direct"})

		if err := router.Handle(echo("uuid-direct", "direct")); err != nil {
			t.Fatalf("echo: %v", err)
		}
		if err := router.Handle(echo("uuid-direct", "direct")); err != nil {
			t.Fatalf("replay: %v", err)
		}
		if _, found, _ := st.GetThreadItem("t1", "injected:wire:uuid-direct"); found {
			t.Fatal("replayed direct-send echo persisted an injected-context duplicate")
		}
	})

	t.Run("deferred flush", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		router.RegisterPendingFlushSendWithExpectation("t1", "queue:q1", store.Item{
			ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1,
			Kind: "user_text", Role: "user", Status: "completed",
			Summary: "deferred",
		}, time.Now().UnixMilli(), PendingSendExpectation{ProviderItemID: "uuid-deferred"})

		if err := router.Handle(echo("uuid-deferred", "deferred")); err != nil {
			t.Fatalf("echo: %v", err)
		}
		router.WaitForPendingSettles()
		if _, found, err := st.GetThreadItem("t1", "user:1:flush:1"); err != nil || !found {
			t.Fatalf("deferred row not persisted: found=%v err=%v", found, err)
		}
		if err := router.Handle(echo("uuid-deferred", "deferred")); err != nil {
			t.Fatalf("replay: %v", err)
		}
		if _, found, _ := st.GetThreadItem("t1", "injected:wire:uuid-deferred"); found {
			t.Fatal("replayed deferred echo persisted an injected-context duplicate")
		}
	})
}

// TestEagerPersistDeferredFlushSends_CapturesCheckpointUnderAnchorMu
// pins CT4-1 (round 4) for the deferred path: the confirmed hook
// (checkpoint capture) fires INSIDE EagerPersistDeferredFlushSends —
// before the flush anchor lock releases — with the persisted row at its
// store-assigned position. Captured any later, an echo in the gap
// would stamp the row while UpdateCheckpointProviderIDs no-ops against
// a checkpoint that doesn't exist yet. (The promote path's twin is
// pinned in TestHandleUserText_FlushConfirmedHook_EagerQuietRows.)
func TestEagerPersistDeferredFlushSends_CapturesCheckpointUnderAnchorMu(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	var hookItems []store.Item
	router.SetFlushUserTextConfirmedHook(func(threadID string, item store.Item) {
		if threadID != "t1" {
			t.Fatalf("hook threadID = %q, want t1", threadID)
		}
		hookItems = append(hookItems, item)
	})

	router.RegisterPendingFlushSendWithExpectation("t1", "queue:q1", store.Item{
		ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "queued text",
	}, time.Now().UnixMilli(), PendingSendExpectation{ProviderItemID: "ao-uuid-1"})

	persisted := eagerPersistForTest(router, "t1", router.OpenTurnIndex("t1"))
	if len(persisted) != 1 {
		t.Fatalf("eager persist = %+v, want one row", persisted)
	}
	if len(hookItems) != 1 {
		t.Fatalf("hook calls = %d, want 1 (capture must happen inside the eager persist)", len(hookItems))
	}
	if hookItems[0].ID != "user:1:flush:1" {
		t.Errorf("hook item = %s, want user:1:flush:1", hookItems[0].ID)
	}
	row, found, err := st.GetThreadItem("t1", "user:1:flush:1")
	if err != nil || !found {
		t.Fatalf("persisted row: found=%v err=%v", found, err)
	}
	if hookItems[0].ItemIndex != row.ItemIndex {
		t.Errorf("hook item_index = %d, want store-assigned %d (previous-checkpoint ordering)", hookItems[0].ItemIndex, row.ItemIndex)
	}
}

// TestDrainRacesEagerPersist_ConvergesToALegalState pins CT4-2
// (round 4): DrainUnconfirmedFlushItems holds the flush anchor lock, so a
// session-death drain either runs strictly before the interrupt's
// eager persist claimed the entry (DeferredItem returned, no store
// row) or strictly after the persist committed (QuietItem returned,
// store row present for the death path to delete). The mid-write
// interleaving — drain restores the draft, then the persist commits
// after teardown, duplicating the message — must be unobservable.
// Run with -race.
func TestDrainRacesEagerPersist_ConvergesToALegalState(t *testing.T) {
	for i := 0; i < 12; i++ {
		router, st, _ := newTestRouter(t)
		threadID := fmt.Sprintf("t-race-%d", i)
		createTestThread(t, st, threadID)
		rowID := "user:1:flush:1"
		router.RegisterPendingFlushSendWithExpectation(threadID, "queue:q1", store.Item{
			ID: rowID, ThreadID: threadID, TurnIndex: 1,
			Kind: "user_text", Role: "user", Status: "completed",
			Summary: "raced queued text",
		}, time.Now().UnixMilli(), PendingSendExpectation{ProviderItemID: "ao-uuid-race"})

		var drained []UnconfirmedFlushItem
		done := make(chan struct{})
		go func() {
			defer close(done)
			eagerPersistForTest(router, threadID, router.OpenTurnIndex(threadID))
		}()
		drained = router.DrainUnconfirmedFlushItems(threadID)
		<-done

		if len(drained) != 1 {
			t.Fatalf("iter %d: drained = %+v, want exactly one entry", i, drained)
		}
		entry := drained[0]
		_, rowExists, err := st.GetThreadItem(threadID, rowID)
		if err != nil {
			t.Fatalf("iter %d: load row: %v", i, err)
		}
		switch {
		case entry.QuietItem != nil:
			if !rowExists {
				t.Fatalf("iter %d: drain reported QuietItem but the store row is missing — death path would delete nothing and the persist could commit after teardown", i)
			}
		case entry.DeferredItem != nil:
			if rowExists {
				t.Fatalf("iter %d: drain reported DeferredItem but the store row exists — death path would strand the persisted row in the timeline", i)
			}
		default:
			t.Fatalf("iter %d: drained entry carries neither DeferredItem nor QuietItem — message unrecoverable", i)
		}
	}
}

// TestEchoFailurePreDurability_ReinsertsPendingEntry pins CT4-3
// (round 4) and R5-3 (round 5): when echo handling fails before any
// durable write (here: the deferred item's meta is malformed, so the
// provider-id merge errors before persistItem), the popped pending
// entry is reinserted — a re-delivered echo re-matches it instead of
// persisting an injected-context duplicate. The echo's arrival proved
// the provider consumed the message, so a session-death drain must NOT
// hand it back as restorable (a draft restore would re-send content
// the provider transcript already has); it self-heals the timeline row
// from the retained copy instead.
func TestEchoFailurePreDurability_ReinsertsPendingEntry(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	router.RegisterPendingFlushSendWithExpectation("t1", "queue:q1", store.Item{
		ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "queued text to keep",
		Meta:    "{not-json",
	}, time.Now().UnixMilli(), PendingSendExpectation{ProviderItemID: "ao-uuid-1"})

	echo := provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1", Content: "queued text to keep",
		Meta:      json.RawMessage(`{"provider_item_id":"ao-uuid-1"}`),
		Timestamp: time.Now(),
	}
	if err := router.Handle(echo); err == nil {
		t.Fatal("echo with malformed deferred meta must error")
	}
	if !router.HasPendingSendForThread("t1") {
		t.Fatal("pending entry lost after pre-durability failure — death drain could no longer restore the message")
	}
	if _, found, _ := st.GetThreadItem("t1", "injected:wire:ao-uuid-1"); found {
		t.Fatal("failed echo persisted an injected-context row")
	}

	// A re-delivered echo re-matches the reinserted entry (fails the
	// same way here, but never falls through to the injected branch).
	if err := router.Handle(echo); err == nil {
		t.Fatal("replayed echo must still error against the malformed meta")
	}
	if _, found, _ := st.GetThreadItem("t1", "injected:wire:ao-uuid-1"); found {
		t.Fatal("replayed echo persisted an injected-context duplicate")
	}

	// R5-3: the echo proved the provider consumed the message, so the
	// session-death drain returns nothing restorable — a draft restore
	// would re-send content the provider transcript already has. (This
	// fixture's malformed meta also defeats the drain's self-heal
	// persist — loudly logged; production deferred rows carry
	// Marshal-built meta, so the heal itself is pinned in
	// TestEchoConsumedEntries_DrainSelfHealsInsteadOfRestoring.)
	drained := router.DrainUnconfirmedFlushItems("t1")
	if len(drained) != 0 {
		t.Fatalf("drain after consumed echo = %+v, want nothing restorable (R5-3)", drained)
	}
	if router.HasPendingSendForThread("t1") {
		t.Fatal("drain left the echo-consumed entry registered")
	}
}

// TestEchoConsumedEntries_DrainSelfHealsInsteadOfRestoring pins the
// R5-3 drain semantics for both retained-copy shapes. EchoConsumed
// entries (reinserted after a pre-durability echo failure — the
// provider transcript provably contains the message) must not be
// returned as restorable: the old behavior deleted the persisted quiet
// row and pushed the message back into the draft, re-sending content
// the provider context already has. Instead the drain leaves an
// existing row in place and persists a missing one from the retained
// copy.
func TestEchoConsumedEntries_DrainSelfHealsInsteadOfRestoring(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Quiet-row shape: the row is already in the store; only the echo's
	// stamp write was lost. Heal is a no-op — the row must survive.
	quietRow := store.Item{
		ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "eager quiet text",
	}
	if err := router.PersistItemQuiet(quietRow, nil); err != nil {
		t.Fatalf("persist quiet row: %v", err)
	}
	router.reinsertPendingSendHead("t1", pendingSend{
		AOItemID: "user:1:flush:1", QueueItemID: "queue:q1", TurnIndex: 1,
		Shape:     sendShapeFlush,
		QuietItem: &quietRow, EchoPromotedBoundary: -1,
	})

	// Deferred shape: the retained copy never reached the store (the
	// eager persist and the echo both failed transiently). The drain
	// persists it so the timeline matches the provider transcript.
	deferredRow := store.Item{
		ID: "user:2:flush:1", ThreadID: "t1", TurnIndex: 2,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "deferred text",
	}
	router.reinsertPendingSendHead("t1", pendingSend{
		AOItemID: "user:2:flush:1", QueueItemID: "queue:q2", TurnIndex: 2,
		Shape:        sendShapeFlush,
		DeferredItem: &deferredRow, EchoPromotedBoundary: -1,
	})

	drained := router.DrainUnconfirmedFlushItems("t1")
	if len(drained) != 0 {
		t.Fatalf("drain of echo-consumed entries = %+v, want nothing restorable (R5-3)", drained)
	}
	kept, found, err := st.GetThreadItem("t1", "user:1:flush:1")
	if err != nil || !found {
		t.Fatalf("quiet row after drain: found=%v err=%v", found, err)
	}
	if kept.Summary != "eager quiet text" {
		t.Fatalf("quiet row summary = %q, want unchanged content", kept.Summary)
	}
	healed, found, err := st.GetThreadItem("t1", "user:2:flush:1")
	if err != nil || !found {
		t.Fatalf("self-healed deferred row after drain: found=%v err=%v", found, err)
	}
	if healed.Summary != "deferred text" {
		t.Fatalf("self-healed row summary = %q, want the retained copy", healed.Summary)
	}
	if router.HasPendingSendForThread("t1") {
		t.Fatal("drain left echo-consumed entries registered")
	}
}

// TestSecondEagerDeferredEcho_SettlesSiblingAsEndTurn pins CT4-7
// (round 4): one interrupt eager-persists TWO deferred queued rows;
// the CLI consumes both mid-loop, echoing them back to back with no
// init between. The first echo settles the genuinely interrupted turn
// as "interrupted"; the second echo settles its SIBLING's turn — which
// ended naturally when the CLI drained the next queued message — as
// "end_turn", not "interrupted".
func TestSecondEagerDeferredEcho_SettlesSiblingAsEndTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	now := time.Now().UnixMilli()
	router.RegisterPendingFlushSendWithExpectation("t1", "queue:q1", store.Item{
		ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1,
		Kind: "user_text", Role: "user", Status: "completed", Summary: "first queued",
	}, now, PendingSendExpectation{ProviderItemID: "ao-uuid-1"})

	router.RegisterPendingFlushSendWithExpectation("t1", "queue:q2", store.Item{
		ID: "user:2:flush:1", ThreadID: "t1", TurnIndex: 2,
		Kind: "user_text", Role: "user", Status: "completed", Summary: "second queued",
	}, now, PendingSendExpectation{ProviderItemID: "ao-uuid-2"})

	if persisted := eagerPersistForTest(router, "t1", router.OpenTurnIndex("t1")); len(persisted) != 2 {
		t.Fatalf("eager persist = %+v, want two rows", persisted)
	}

	echo := func(uuid, content string) provider.ProviderEvent {
		return provider.ProviderEvent{
			Kind: provider.EventUserText, ThreadID: "t1", Content: content,
			Meta:      json.RawMessage(`{"provider_item_id":"` + uuid + `"}`),
			Timestamp: time.Now(),
		}
	}
	if err := router.Handle(echo("ao-uuid-1", "first queued")); err != nil {
		t.Fatalf("first echo: %v", err)
	}
	if err := router.Handle(echo("ao-uuid-2", "second queued")); err != nil {
		t.Fatalf("second echo: %v", err)
	}
	router.WaitForPendingSettles()

	turn0, found, err := st.GetTurn("t1:0")
	if err != nil || !found {
		t.Fatalf("turns row t1:0: found=%v err=%v", found, err)
	}
	if turn0.StopReason != "interrupted" {
		t.Errorf("turn 0 stop_reason = %q, want interrupted (the turn the user actually cut)", turn0.StopReason)
	}
	turn1, found, err := st.GetTurn("t1:1")
	if err != nil || !found {
		t.Fatalf("turns row t1:1: found=%v err=%v", found, err)
	}
	if turn1.StopReason != "end_turn" {
		t.Errorf("turn 1 stop_reason = %q, want end_turn (drained naturally by the next queued message)", turn1.StopReason)
	}
}

// TestOpenTurnIndex_SamplesLiveTurn pins the R5-4 sampler: -1 with no
// turn in flight, the open index otherwise.
func TestOpenTurnIndex_SamplesLiveTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	if got := router.OpenTurnIndex("t1"); got != -1 {
		t.Fatalf("OpenTurnIndex with no open turn = %d, want -1", got)
	}
	seedOpenTurn(t, router, st, "t1", 3)
	if got := router.OpenTurnIndex("t1"); got != 3 {
		t.Fatalf("OpenTurnIndex = %d, want 3", got)
	}
}

// TestEagerPersistDeferredFlushSends_UsesCallerSampledInterruptedTurn
// pins R5-4 (round 5): the interrupted-turn index is the caller's
// PRE-ACK OpenTurnIndex sample, used verbatim — never re-sampled from
// openTurns at persist time. InterruptTurn awaits the provider's
// interrupt ack between its sample and this call, and the read loop
// keeps processing wire events during that wait: a re-sample here can
// miss (turn settled in the gap) or mis-name (a mid-loop drain moved
// the open turn) the turn the user provably cut.
func TestEagerPersistDeferredFlushSends_UsesCallerSampledInterruptedTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// No turn is open at persist time — the cut turn settled during the
	// ack wait. The caller's earlier sample must still be stamped.
	router.RegisterPendingFlushSendWithExpectation("t1", "queue:q1", store.Item{
		ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1,
		Kind: "user_text", Role: "user", Status: "completed", Summary: "queued",
	}, time.Now().UnixMilli(), PendingSendExpectation{ProviderItemID: "ao-uuid-1"})

	if persisted := eagerPersistForTest(router, "t1", 0); len(persisted) != 1 {
		t.Fatalf("eager persist = %+v, want one row", persisted)
	}

	got := -2
	router.mu.Lock()
	for _, entry := range router.state("t1").pendingSends {
		if entry.AOItemID == "user:1:flush:1" {
			got = entry.InterruptedTurnIndex
		}
	}
	router.mu.Unlock()
	if got != 0 {
		t.Fatalf("InterruptedTurnIndex = %d, want the caller's pre-ack sample 0", got)
	}
}

// TestEchoConsumedDrain_StampsStashedEchoIdentity pins R6-1 (round 6):
// a reinserted EchoConsumed entry carries the failed echo's wire
// identity, and the session-death self-heal stamps it — onto an
// existing row (only the echo's write was lost) and into a re-persisted
// retained copy alike. Without the stamp the entry is dropped with the
// row permanently anchor-less: a later revert would ordinal-walk or
// full-clone while the provider transcript still contains the message.
func TestEchoConsumedDrain_StampsStashedEchoIdentity(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Existing-row shape: the row is in the store; the echo's stamp
	// write failed. The heal must stamp, not skip.
	quietRow := store.Item{
		ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "eager quiet text",
	}
	if err := router.PersistItemQuiet(quietRow, nil); err != nil {
		t.Fatalf("persist quiet row: %v", err)
	}
	router.reinsertPendingSendHead("t1", pendingSend{
		AOItemID: "user:1:flush:1", QueueItemID: "queue:q1", TurnIndex: 1,
		Shape:                sendShapeFlush,
		QuietItem:            &quietRow,
		EchoProviderItemID:   "uuid-echo-1",
		EchoParentUUID:       "uuid-parent-1",
		EchoPromotedBoundary: -1,
	})

	// Missing-row shape: the retained copy persists WITH the ids (and,
	// for a promoted row, the echo-time boundary) already merged.
	deferredRow := store.Item{
		ID: "user:2:flush:1", ThreadID: "t1", TurnIndex: 2,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "deferred text",
	}
	router.reinsertPendingSendHead("t1", pendingSend{
		AOItemID: "user:2:flush:1", QueueItemID: "queue:q2", TurnIndex: 2,
		Shape:                sendShapeFlush,
		DeferredItem:         &deferredRow,
		EchoProviderItemID:   "uuid-echo-2",
		EchoParentUUID:       "uuid-parent-2",
		EchoPromotedBoundary: 4,
	})

	if drained := router.DrainUnconfirmedFlushItems("t1"); len(drained) != 0 {
		t.Fatalf("drain of echo-consumed entries = %+v, want nothing restorable", drained)
	}

	stamped, found, err := st.GetThreadItem("t1", "user:1:flush:1")
	if err != nil || !found {
		t.Fatalf("quiet row after drain: found=%v err=%v", found, err)
	}
	if got := usermessage.ReadProviderItemID(stamped.Meta); got != "uuid-echo-1" {
		t.Errorf("existing row provider_item_id = %q, want the stashed echo id", got)
	}
	if got := usermessage.ReadProviderParentUUID(stamped.Meta); got != "uuid-parent-1" {
		t.Errorf("existing row provider_parent_uuid = %q, want the stashed parent", got)
	}

	healed, found, err := st.GetThreadItem("t1", "user:2:flush:1")
	if err != nil || !found {
		t.Fatalf("healed row after drain: found=%v err=%v", found, err)
	}
	if got := usermessage.ReadProviderItemID(healed.Meta); got != "uuid-echo-2" {
		t.Errorf("healed row provider_item_id = %q, want the stashed echo id", got)
	}
	if got := usermessage.ReadProviderParentUUID(healed.Meta); got != "uuid-parent-2" {
		t.Errorf("healed row provider_parent_uuid = %q, want the stashed parent", got)
	}
	state, err := itemmeta.DecodePromotionState(healed.Meta)
	if err != nil {
		t.Fatalf("decode healed promotion state: %v", err)
	}
	if !state.Promoted || !state.HasEchoBoundary || state.EchoBoundary != 4 {
		t.Errorf("healed promotion state = %+v, want promoted with the stashed echo-time boundary 4", state)
	}
}

// TestEagerPersistFailure_RestoresDeferredState pins R6-6 (round 6):
// when the interrupt's eager persist fails, the deferred→quiet
// transition is undone in full — not just the anchor claim. An entry
// left with QuietItem set but no store row made a second interrupt
// quiet-promote a nonexistent row (failing forever), and made a
// session-death drain treat an unconsumed message as unrestorable.
func TestEagerPersistFailure_RestoresDeferredState(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Malformed meta defeats the persist (json_valid CHECK on
	// items.meta) — the transition must roll back.
	router.RegisterPendingFlushSendWithExpectation("t1", "queue:q1", store.Item{
		ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "queued text", Meta: "{not-json",
	}, time.Now().UnixMilli(), PendingSendExpectation{ProviderItemID: "ao-uuid-1"})

	if persisted := eagerPersistForTest(router, "t1", 0); len(persisted) != 0 {
		t.Fatalf("eager persist of unpersistable row = %+v, want none", persisted)
	}

	router.mu.Lock()
	var entry pendingSend
	for _, e := range router.state("t1").pendingSends {
		if e.AOItemID == "user:1:flush:1" {
			entry = e
		}
	}
	router.mu.Unlock()
	if entry.DeferredItem == nil {
		t.Fatal("DeferredItem not restored — a second interrupt would quiet-promote a nonexistent row")
	}
	if entry.QuietItem != nil {
		t.Error("QuietItem left set after the rollback")
	}
	if entry.WasDeferred {
		t.Error("WasDeferred left set — the echo would take the attach path against a missing row")
	}
	if entry.AnchoredAtInterrupt {
		t.Error("anchor claim left set after the failed persist")
	}
	if entry.InterruptedTurnIndex != 0 {
		t.Errorf("InterruptedTurnIndex = %d, want the interrupt's sample 0 kept (the turn was still cut)", entry.InterruptedTurnIndex)
	}

	// A second interrupt retries the eager persist (still failing here)
	// rather than quiet-promoting; the entry stays deferred throughout.
	if persisted := eagerPersistForTest(router, "t1", 0); len(persisted) != 0 {
		t.Fatalf("second eager persist = %+v, want none", persisted)
	}
	if promoted := promoteQuietForTest(router, "t1"); len(promoted) != 0 {
		t.Fatalf("promote picked up the restored deferred entry: %+v", promoted)
	}

	// No echo ever consumed the message, so the session-death drain must
	// return it as RESTORABLE — the transition rollback is what keeps
	// the draft-restore path reachable.
	drained := router.DrainUnconfirmedFlushItems("t1")
	if len(drained) != 1 || drained[0].DeferredItem == nil {
		t.Fatalf("drain after failed eager persist = %+v, want one restorable deferred entry", drained)
	}
}

// eagerPersistForTest mirrors the production interrupt sequence: the
// pre-ack Mark publishes the stamp and returns the token that fences
// the eager pass (round-9, R9-5).
func eagerPersistForTest(router *Router, threadID string, interruptedTurn int) []EagerPersistedFlush {
	tok := router.MarkFlushSendsInterrupted(threadID, interruptedTurn)
	return router.EagerPersistDeferredFlushSends(threadID, interruptedTurn, tok)
}

// promoteQuietForTest exercises PromoteQuietFlushSends' promote
// mechanics in isolation: a bare current-epoch token passes the
// session-replacement fence without the Mark side effects (FIFO stamps,
// interrupt-mark entry) the production interrupt path adds around it —
// those are covered by the interrupt-sequence tests.
func promoteQuietForTest(router *Router, threadID string) []store.Item {
	router.mu.Lock()
	tok := FlushStampToken{threadEpoch: router.identity(threadID).epoch}
	router.mu.Unlock()
	return router.PromoteQuietFlushSends(threadID, tok)
}

// markUserInterruptForTest mirrors the production interrupt sequence:
// pre-ack open-turn sample + Mark, then the post-ack bookkeeping with
// that sample and token.
func markUserInterruptForTest(router *Router, threadID string) (string, error) {
	open := router.OpenTurnIndex(threadID)
	tok := router.MarkFlushSendsInterrupted(threadID, open)
	return router.MarkUserInterrupt(threadID, open, tok)
}

// TestMarkFlushSendsInterrupted_PreAckStampSettlesInterrupted
// pins R6-4 (round 6): the interrupt paths publish the pre-ack
// interrupted-turn sample onto unconsumed pending flush entries BEFORE
// awaiting the provider ack. An echo the CLI's queue drain delivers
// during that wait — before EagerPersistDeferredFlushSends ever runs —
// must still settle the cut turn "interrupted"; the settlement claim
// blocks any later correction, so a post-ack stamp is too late.
func TestMarkFlushSendsInterrupted_PreAckStampSettlesInterrupted(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	router.RegisterPendingFlushSendWithExpectation("t1", "queue:q1", store.Item{
		ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1,
		Kind: "user_text", Role: "user", Status: "completed", Summary: "queued",
	}, time.Now().UnixMilli(), PendingSendExpectation{ProviderItemID: "ao-uuid-1"})

	// Pre-ack publish, then the echo lands during the ack wait — no
	// eager persist has run.
	router.MarkFlushSendsInterrupted("t1", 0)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1", Content: "queued",
		Meta:      json.RawMessage(`{"provider_item_id":"ao-uuid-1"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("echo: %v", err)
	}
	router.WaitForPendingSettles()

	turn0, found, err := st.GetTurn("t1:0")
	if err != nil || !found {
		t.Fatalf("turns row t1:0: found=%v err=%v", found, err)
	}
	if turn0.StopReason != "interrupted" {
		t.Errorf("turn 0 stop_reason = %q, want interrupted (echo consumed during the ack wait)", turn0.StopReason)
	}

	// A failed interrupt request restores the previous stamp (-1 for a
	// never-claimed entry): the turn was not provably cut, so a later
	// natural drain settles end_turn (round-7 R7-5: restore, not wipe).
	router.RegisterPendingFlushSendWithExpectation("t1", "queue:q2", store.Item{
		ID: "user:2:flush:1", ThreadID: "t1", TurnIndex: 2,
		Kind: "user_text", Role: "user", Status: "completed", Summary: "second",
	}, time.Now().UnixMilli(), PendingSendExpectation{ProviderItemID: "ao-uuid-2"})

	tok := router.MarkFlushSendsInterrupted("t1", 1)
	router.RestoreFlushSendsInterrupted("t1", tok)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1", Content: "second",
		Meta:      json.RawMessage(`{"provider_item_id":"ao-uuid-2"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("second echo: %v", err)
	}
	router.WaitForPendingSettles()
	turn1, found, err := st.GetTurn("t1:1")
	if err != nil || !found {
		t.Fatalf("turns row t1:1: found=%v err=%v", found, err)
	}
	if turn1.StopReason != "end_turn" {
		t.Errorf("turn 1 stop_reason = %q, want end_turn (cleared stamp after a failed interrupt)", turn1.StopReason)
	}
}

// TestInterruptedQueuedEchoPredecessor_FlipsStreamsToStopped pins
// R10-4 (round 10): when a queued echo settles its predecessor
// "interrupted", the predecessor's in-flight streaming rows are partial
// output cut by a user interrupt and must flip to errored + " — stopped"
// BEFORE the streaming settle — the settlement claim routes the later
// truncated wire result away from the normal interrupt cleanup, so a
// completed-status settle here would permanently display partial text
// as a finished answer.
func TestInterruptedQueuedEchoPredecessor_FlipsStreamsToStopped(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	// Partial streaming output on the turn the interrupt cuts.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Content: "partial answer",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}

	router.openQueuedEchoTurn("t1", 1, time.Now().UnixMilli(), 0)
	router.WaitForPendingSettles()

	items, err := st.ListTurnItems("t1", 0)
	if err != nil {
		t.Fatalf("list turn 0 items: %v", err)
	}
	var stream store.Item
	for _, item := range items {
		if item.Kind == "assistant_text" {
			stream = item
		}
	}
	if stream.ID == "" {
		t.Fatalf("no assistant_text row in turn 0, items = %+v", items)
	}
	if stream.Status != "errored" {
		t.Errorf("interrupted stream status = %q, want errored", stream.Status)
	}
	if !strings.HasSuffix(stream.Summary, " — stopped") {
		t.Errorf("interrupted stream summary = %q, want the — stopped suffix", stream.Summary)
	}

	// Control: an end_turn predecessor settle completes its streams.
	createTestThread(t, st, "t2")
	seedOpenTurn(t, router, st, "t2", 0)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t2", Content: "finished answer",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("t2 text delta: %v", err)
	}
	router.openQueuedEchoTurn("t2", 1, time.Now().UnixMilli(), -1)
	router.WaitForPendingSettles()
	items, err = st.ListTurnItems("t2", 0)
	if err != nil {
		t.Fatalf("list t2 turn 0 items: %v", err)
	}
	stream = store.Item{}
	for _, item := range items {
		if item.Kind == "assistant_text" {
			stream = item
		}
	}
	if stream.ID == "" {
		t.Fatalf("no assistant_text row in t2 turn 0, items = %+v", items)
	}
	if stream.Status != "completed" {
		t.Errorf("naturally drained stream status = %q, want completed", stream.Status)
	}
}

// TestInFlightEchoPoppedBeforeMark_SettlesInterrupted pins R10-6
// (round 10): the pre-ack stamp (R6-4) covers entries resident in the
// FIFO, but the CLI's queue drain can pop an entry a moment BEFORE
// MarkFlushSendsInterrupted runs — the mark then stamps nothing while
// the in-flight echo carries its retained -1 and would settle the cut
// turn "end_turn", with the settlement claim blocking the interrupt
// bookkeeping from ever correcting it. The thread-level marked-turn
// record closes that window.
func TestInFlightEchoPoppedBeforeMark_SettlesInterrupted(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	router.RegisterPendingFlushSendWithExpectation("t1", "queue:q1", store.Item{
		ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1,
		Kind: "user_text", Role: "user", Status: "completed", Summary: "queued",
	}, time.Now().UnixMilli(), PendingSendExpectation{ProviderItemID: "ao-uuid-1"})

	// Pop before the mark: the interrupt's pre-ack publish finds an
	// empty FIFO.
	popped, ok := router.consumeMatchingPendingSend("t1", "ao-uuid-1")
	if !ok {
		t.Fatal("consume did not match the registered entry")
	}
	if popped.InterruptedTurnIndex != -1 {
		t.Fatalf("popped stamp = %d, want the retained -1", popped.InterruptedTurnIndex)
	}
	router.MarkFlushSendsInterrupted("t1", 0)

	// The in-flight echo finishes and opens its turn with the stale
	// popped stamp; the record must settle the cut predecessor
	// "interrupted".
	router.openQueuedEchoTurn("t1", 1, time.Now().UnixMilli(), popped.InterruptedTurnIndex)
	router.WaitForPendingSettles()
	turn0, found, err := st.GetTurn("t1:0")
	if err != nil || !found {
		t.Fatalf("turns row t1:0: found=%v err=%v", found, err)
	}
	if turn0.StopReason != "interrupted" {
		t.Errorf("turn 0 stop_reason = %q, want interrupted (thread-level record covers the popped entry)", turn0.StopReason)
	}
}

// TestInterruptMarkedTurnRecord_RestoreAndLinger pins the R10-6 record
// transitions: a failed interrupt's restore steps the record back (a
// no-stamp mark has no stamp-epoch to fence with, so the seq fence
// carries it — including a duplicate restore), and a record lingering
// after a SUCCESSFUL interrupt matches only the turn that really was
// cut, never a later sibling's.
func TestInterruptMarkedTurnRecord_RestoreAndLinger(t *testing.T) {
	router, st, _ := newTestRouter(t)

	// Failed interrupt over an empty FIFO: restore unwinds the record;
	// the in-flight echo's predecessor settles end_turn. A duplicate
	// restore must not corrupt the unwound state.
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	tok := router.MarkFlushSendsInterrupted("t1", 0)
	router.RestoreFlushSendsInterrupted("t1", tok)
	router.RestoreFlushSendsInterrupted("t1", tok)
	router.openQueuedEchoTurn("t1", 1, time.Now().UnixMilli(), -1)
	router.WaitForPendingSettles()
	turn0, found, err := st.GetTurn("t1:0")
	if err != nil || !found {
		t.Fatalf("turns row t1:0: found=%v err=%v", found, err)
	}
	if turn0.StopReason != "end_turn" {
		t.Errorf("turn 0 stop_reason = %q, want end_turn (failed interrupt's record unwound)", turn0.StopReason)
	}

	// Successful interrupt: the record settles its own turn interrupted,
	// then lingers without mislabeling the next sibling predecessor.
	createTestThread(t, st, "t2")
	seedOpenTurn(t, router, st, "t2", 0)
	router.MarkFlushSendsInterrupted("t2", 0)
	router.openQueuedEchoTurn("t2", 1, time.Now().UnixMilli(), -1)
	router.WaitForPendingSettles()
	cut, found, err := st.GetTurn("t2:0")
	if err != nil || !found {
		t.Fatalf("turns row t2:0: found=%v err=%v", found, err)
	}
	if cut.StopReason != "interrupted" {
		t.Errorf("turn 0 stop_reason = %q, want interrupted", cut.StopReason)
	}
	router.openQueuedEchoTurn("t2", 2, time.Now().UnixMilli(), -1)
	router.WaitForPendingSettles()
	sibling, found, err := st.GetTurn("t2:1")
	if err != nil || !found {
		t.Fatalf("turns row t2:1: found=%v err=%v", found, err)
	}
	if sibling.StopReason != "end_turn" {
		t.Errorf("turn 1 stop_reason = %q, want end_turn (lingering record matches only its own turn)", sibling.StopReason)
	}

	// Two overlapping FAILED no-stamp interrupts restored out of order:
	// the earlier restore removes only its own seq-keyed entry, and the
	// later restore must not resurrect the earlier (already-withdrawn)
	// mark (round-11, R11-3 — the R10-6 prev-pair unwind did exactly
	// that).
	createTestThread(t, st, "t3")
	seedOpenTurn(t, router, st, "t3", 0)
	tokA := router.MarkFlushSendsInterrupted("t3", 0)
	tokB := router.MarkFlushSendsInterrupted("t3", 5)
	router.RestoreFlushSendsInterrupted("t3", tokA)
	router.RestoreFlushSendsInterrupted("t3", tokB)
	router.openQueuedEchoTurn("t3", 1, time.Now().UnixMilli(), -1)
	router.WaitForPendingSettles()
	outOfOrder, found, err := st.GetTurn("t3:0")
	if err != nil || !found {
		t.Fatalf("turns row t3:0: found=%v err=%v", found, err)
	}
	if outOfOrder.StopReason != "end_turn" {
		t.Errorf("turn 0 stop_reason = %q, want end_turn (both interrupts failed; no mark may survive)", outOfOrder.StopReason)
	}

	// Session replacement: revert paths REUSE turn indexes, so marks
	// must not survive MarkThreadActive — a dead session's successful
	// interrupt would otherwise settle the replacement's reused-index
	// predecessor "interrupted" with no new interrupt (round-11,
	// CT11-2).
	createTestThread(t, st, "t4")
	seedOpenTurn(t, router, st, "t4", 0)
	router.MarkFlushSendsInterrupted("t4", 0)
	router.MarkThreadActive("t4")
	router.openQueuedEchoTurn("t4", 1, time.Now().UnixMilli(), -1)
	router.WaitForPendingSettles()
	reused, found, err := st.GetTurn("t4:0")
	if err != nil || !found {
		t.Fatalf("turns row t4:0: found=%v err=%v", found, err)
	}
	if reused.StopReason != "end_turn" {
		t.Errorf("turn 0 stop_reason = %q, want end_turn (marks cleared at reactivation)", reused.StopReason)
	}
}

// TestMarkUserInterrupt_TargetsSampledTurnNotEchoOpenedTurn pins
// R11-4 (round 11): a queued echo consumed during the interrupt-ack
// wait opens the queued message's own turn, so a post-ack "current
// turn" read would land the "Stopped by user" record on the NEW turn
// — which drains naturally — instead of the turn the user provably
// cut. The post-ack bookkeeping must target the pre-ack sampled turn.
func TestMarkUserInterrupt_TargetsSampledTurnNotEchoOpenedTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	interruptedTurn := router.OpenTurnIndex("t1")
	tok := router.MarkFlushSendsInterrupted("t1", interruptedTurn)
	// The CLI's mid-loop queue drain echoes the queued message during
	// the ack wait; its echo opens the queued message's turn.
	router.openQueuedEchoTurn("t1", 1, time.Now().UnixMilli(), interruptedTurn)
	router.WaitForPendingSettles()

	errID, err := router.MarkUserInterrupt("t1", interruptedTurn, tok)
	if err != nil {
		t.Fatalf("MarkUserInterrupt: %v", err)
	}
	if errID == "" {
		t.Fatal("MarkUserInterrupt returned no error id")
	}
	stopped, found, err := st.GetThreadItem("t1", errID)
	if err != nil || !found {
		t.Fatalf("stopped row %s: found=%v err=%v", errID, found, err)
	}
	if stopped.TurnIndex != interruptedTurn {
		t.Errorf("Stopped by user landed on turn %d, want the sampled turn %d (not the echo-opened turn)",
			stopped.TurnIndex, interruptedTurn)
	}

	// Session replacement between ack and bookkeeping (round-11,
	// CT11-1): the stale token must make the whole call a no-op.
	tok = router.MarkFlushSendsInterrupted("t1", 1)
	router.MarkThreadActive("t1")
	if errID, err := router.MarkUserInterrupt("t1", 1, tok); err != nil || errID != "" {
		t.Errorf("stale MarkUserInterrupt = (%q, %v), want a silent no-op", errID, err)
	}
}

// TestMarkUserInterrupt_NegativeSampleIsNoOp pins R12-4 (round 12):
// a -1 pre-ack OpenTurnIndex sample means no turn was open — the turn
// completed in the click-to-sample race and the interrupt ack'd an
// idle CLI. The post-ack bookkeeping must be a no-op: any fallback
// resolution would relabel completed history (LastTurnIndex) or a
// queued echo's freshly opened turn as "Stopped by user".
func TestMarkUserInterrupt_NegativeSampleIsNoOp(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	// A completed prior turn exists — exactly what a fallback would
	// wrongly target — but nothing is open.
	if err := st.InsertTurn(store.Turn{
		TurnID: "t1:0", ThreadID: "t1", TurnIndex: 0,
		StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("insert turn row: %v", err)
	}
	if open := router.OpenTurnIndex("t1"); open != -1 {
		t.Fatalf("OpenTurnIndex = %d, want -1 (no open turn)", open)
	}

	tok := router.MarkFlushSendsInterrupted("t1", -1)
	errID, err := router.MarkUserInterrupt("t1", -1, tok)
	if err != nil {
		t.Fatalf("MarkUserInterrupt: %v", err)
	}
	if errID != "" {
		t.Fatalf("MarkUserInterrupt returned error id %q, want empty (no-op)", errID)
	}
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	for _, item := range items {
		if item.Kind == "error" {
			t.Errorf("negative sample persisted error row %s on turn %d, want none", item.ID, item.TurnIndex)
		}
	}
}

// TestPostAckFlushTransition_FencedAgainstSessionReplacement pins
// R11-6 (round 11): session replacement recycles deterministic flush
// IDs — death recovery re-registers same-ID entries. A direct
// interrupt returning post-ack AFTER MarkThreadActive must not claim,
// bump, checkpoint, or persist the replacement's entries; before this
// round only the stamp write was fenced (R9-5), not the promote /
// eager-persist transitions themselves.
func TestPostAckFlushTransition_FencedAgainstSessionReplacement(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	now := time.Now().UnixMilli()

	// Quiet shape: the replacement's re-registered entry has its row
	// persisted. The stale promote must not claim or mark it.
	quietRow := store.Item{
		ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "queued", CreatedAt: now, UpdatedAt: now,
	}
	if err := router.PersistItemQuiet(quietRow, nil); err != nil {
		t.Fatalf("persist quiet row: %v", err)
	}
	router.RegisterPendingQuietFlushSendWithExpectation("t1", "queue:q1", quietRow, 1, now, PendingSendExpectation{ProviderItemID: "ao-uuid-1"})
	tok := router.MarkFlushSendsInterrupted("t1", 0)
	router.MarkThreadActive("t1")
	if promoted := router.PromoteQuietFlushSends("t1", tok); len(promoted) != 0 {
		t.Fatalf("stale promote = %+v, want none", promoted)
	}
	router.mu.Lock()
	for _, entry := range router.state("t1").pendingSends {
		if entry.AnchoredAtInterrupt {
			t.Error("stale promote claimed the replacement's entry")
		}
	}
	router.mu.Unlock()
	row, found, err := st.GetThreadItem("t1", "user:1:flush:1")
	if err != nil || !found {
		t.Fatalf("quiet row: found=%v err=%v", found, err)
	}
	if state, err := itemmeta.DecodePromotionState(row.Meta); err != nil {
		t.Fatalf("decode promotion state: %v", err)
	} else if state.Promoted {
		t.Error("stale promote stamped the promotion marker on the replacement's row")
	}

	// Deferred shape: the stale eager persist must not transition the
	// replacement's entry or write its row.
	router.RegisterPendingFlushSendWithExpectation("t1", "queue:q2", store.Item{
		ID: "user:2:flush:1", ThreadID: "t1", TurnIndex: 2,
		Kind: "user_text", Role: "user", Status: "completed", Summary: "deferred",
	}, now, PendingSendExpectation{ProviderItemID: "ao-uuid-2"})

	tok = router.MarkFlushSendsInterrupted("t1", 1)
	router.MarkThreadActive("t1")
	if persisted := router.EagerPersistDeferredFlushSends("t1", 1, tok); len(persisted) != 0 {
		t.Fatalf("stale eager persist = %+v, want none", persisted)
	}
	router.mu.Lock()
	var deferredEntry *pendingSend
	for i := range router.state("t1").pendingSends {
		if router.state("t1").pendingSends[i].AOItemID == "user:2:flush:1" {
			deferredEntry = &router.state("t1").pendingSends[i]
		}
	}
	if deferredEntry == nil {
		router.mu.Unlock()
		t.Fatal("deferred entry vanished from the FIFO")
	}
	transitioned := deferredEntry.DeferredItem == nil || deferredEntry.QuietItem != nil ||
		deferredEntry.WasDeferred || deferredEntry.AnchoredAtInterrupt
	router.mu.Unlock()
	if transitioned {
		t.Error("stale eager persist transitioned the replacement's deferred entry")
	}
	if _, found, err := st.GetThreadItem("t1", "user:2:flush:1"); err != nil {
		t.Fatalf("deferred row lookup: %v", err)
	} else if found {
		t.Error("stale eager persist wrote the replacement's row")
	}
}

// TestRestoreFlushSendsInterrupted_EpochGuard pins R8-2 + R9-2:
// interrupt paths are not serialized against each other (a stop press
// can race the revert path's interrupt), so a failed interrupt's
// restore must not clobber a newer interrupt's live stamp, while
// overlapping interrupts that ALL fail must unwind fully in either
// failure order.
func TestRestoreFlushSendsInterrupted_EpochGuard(t *testing.T) {
	router, st, _ := newTestRouter(t)
	stamp := func(threadID string) int {
		t.Helper()
		router.mu.Lock()
		defer router.mu.Unlock()
		pending := router.state(threadID).pendingSends
		if len(pending) != 1 {
			t.Fatalf("pending entries on %s = %d, want 1", threadID, len(pending))
		}
		return pending[0].InterruptedTurnIndex
	}
	seed := func(threadID string) {
		t.Helper()
		createTestThread(t, st, threadID)
		router.RegisterPendingFlushSendWithExpectation(threadID, "queue:"+threadID, store.Item{
			ID: "user:1:flush:1", ThreadID: threadID, TurnIndex: 1,
			Kind: "user_text", Role: "user", Status: "completed", Summary: "queued",
		}, time.Now().UnixMilli(), PendingSendExpectation{ProviderItemID: "ao-uuid-" + threadID})

	}

	// Newer interrupt succeeded: the older failure's restore parks and
	// the newer stamp stays live.
	seed("t1")
	tokA := router.MarkFlushSendsInterrupted("t1", 0)
	router.MarkFlushSendsInterrupted("t1", 1)
	router.RestoreFlushSendsInterrupted("t1", tokA)
	if got := stamp("t1"); got != 1 {
		t.Fatalf("stamp after stale restore = %d, want the newer interrupt's 1", got)
	}

	// Both fail, LIFO order: B restores (back to A's 0), then A.
	seed("t2")
	tokA = router.MarkFlushSendsInterrupted("t2", 0)
	tokB := router.MarkFlushSendsInterrupted("t2", 1)
	router.RestoreFlushSendsInterrupted("t2", tokB)
	if got := stamp("t2"); got != 0 {
		t.Fatalf("stamp after newest restore = %d, want the prior interrupt's 0", got)
	}
	router.RestoreFlushSendsInterrupted("t2", tokA)
	if got := stamp("t2"); got != -1 {
		t.Fatalf("stamp after LIFO unwind = %d, want -1", got)
	}

	// Both fail, out of order: A's restore parks under B's live epoch;
	// B's restore then chains the parked unwind down to -1 (R9-2).
	seed("t3")
	tokA = router.MarkFlushSendsInterrupted("t3", 0)
	tokB = router.MarkFlushSendsInterrupted("t3", 1)
	router.RestoreFlushSendsInterrupted("t3", tokA)
	if got := stamp("t3"); got != 1 {
		t.Fatalf("stamp after parked restore = %d, want the newer interrupt's 1", got)
	}
	router.RestoreFlushSendsInterrupted("t3", tokB)
	if got := stamp("t3"); got != -1 {
		t.Fatalf("stamp after chained unwind = %d, want -1", got)
	}

	// Session replacement (MarkThreadActive bumps the reactivation
	// epoch): a stale failed interrupt's restore must not touch the
	// replacement's same-ID entries (R9-6).
	seed("t4")
	tokA = router.MarkFlushSendsInterrupted("t4", 2)
	router.MarkThreadActive("t4")
	router.RestoreFlushSendsInterrupted("t4", tokA)
	if got := stamp("t4"); got != 2 {
		t.Fatalf("stamp after cross-session restore = %d, want 2 untouched", got)
	}

	// Duplicate restore of an already-applied token: applied restores
	// step the epoch back down, so a later Mark REUSES the epoch number.
	// The duplicate must be rejected, not parked — a parked duplicate
	// would chain-apply over the epoch-sharing fresh mark's live stamp
	// when a still-newer interrupt fails (R10-3).
	seed("t5")
	tokA = router.MarkFlushSendsInterrupted("t5", 0)
	router.RestoreFlushSendsInterrupted("t5", tokA)
	router.RestoreFlushSendsInterrupted("t5", tokA)
	router.MarkFlushSendsInterrupted("t5", 1) // reuses A's epoch
	tokC := router.MarkFlushSendsInterrupted("t5", 2)
	router.RestoreFlushSendsInterrupted("t5", tokC)
	if got := stamp("t5"); got != 1 {
		t.Fatalf("stamp after duplicate-restore replay = %d, want the live interrupt's 1", got)
	}
}

// TestEagerPersistStamp_PreservesNewerInterruptStamp pins R9-5
// (round 9): interrupt A's post-ack eager pass must not overwrite the
// stamp a concurrent interrupt B published during A's ack wait — B cut
// a LATER turn, and its echo must settle that turn "interrupted".
func TestEagerPersistStamp_PreservesNewerInterruptStamp(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	router.RegisterPendingFlushSendWithExpectation("t1", "queue:q1", store.Item{
		ID: "user:2:flush:1", ThreadID: "t1", TurnIndex: 2,
		Kind: "user_text", Role: "user", Status: "completed", Summary: "queued",
	}, time.Now().UnixMilli(), PendingSendExpectation{ProviderItemID: "ao-uuid-1"})

	tokA := router.MarkFlushSendsInterrupted("t1", 0)
	router.MarkFlushSendsInterrupted("t1", 1)
	if persisted := router.EagerPersistDeferredFlushSends("t1", 0, tokA); len(persisted) != 1 {
		t.Fatalf("eager persist = %d rows, want 1", len(persisted))
	}

	router.mu.Lock()
	got := router.state("t1").pendingSends[0].InterruptedTurnIndex
	wasDeferred := router.state("t1").pendingSends[0].WasDeferred
	router.mu.Unlock()
	if got != 1 {
		t.Fatalf("stamp after stale eager pass = %d, want the newer interrupt's 1", got)
	}
	if !wasDeferred {
		t.Fatal("eager pass must still claim the deferred entry — only the stamp is fenced")
	}
}

// TestDeferredEchoPersistFailure_StillOpensQueuedTurn pins R6-3
// (round 6): the echo proves the provider advanced to the queued
// message's turn regardless of AO's write, so a failed deferred persist
// must still open that logical turn — otherwise the response streaming
// in right behind the echo attributes to the predecessor while the
// replay retry / death self-heal later put the user row in its own
// turn, permanently separating prompt from response.
func TestDeferredEchoPersistFailure_StillOpensQueuedTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	router.RegisterPendingFlushSendWithExpectation("t1", "queue:q1", store.Item{
		ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "queued", Meta: "{not-json",
	}, time.Now().UnixMilli(), PendingSendExpectation{ProviderItemID: "ao-uuid-1"})

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1", Content: "queued",
		Meta:      json.RawMessage(`{"provider_item_id":"ao-uuid-1"}`),
		Timestamp: time.Now(),
	}); err == nil {
		t.Fatal("echo with malformed deferred meta must error")
	}
	router.WaitForPendingSettles()

	if got, ok := router.openTurnIndex("t1"); !ok || got != 1 {
		t.Fatalf("open turn after failed deferred persist = (%d, %v), want the queued message's turn 1", got, ok)
	}
	if _, found, err := st.GetTurn("t1:1"); err != nil || !found {
		t.Fatalf("turns row t1:1 after failed persist: found=%v err=%v", found, err)
	}
}

// TestPromotedEchoBoundary_CoversRowsDeferredBehindMidSettleStream pins
// R6-2 (round 6): a row deferred behind a mid-settle stream
// (invariant 11) has no item_index when the promoted echo arrives — it
// arrived on the serial read loop BEFORE the echo, so it precedes the
// queued_command attachment in provider order, but a naive MAX sample
// would place it above the boundary and revert would cut it while the
// session slice retains it. The echo drains the queue first, re-bumps
// the promoted row past the drained rows (they were never displayed,
// so no watched order changes), and stamps the boundary to cover them.
func TestPromotedEchoBoundary_CoversRowsDeferredBehindMidSettleStream(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}
	now := time.Now().UnixMilli()

	// Eager quiet flush dispatch, then the interrupt promotes it.
	flushRow := store.Item{
		ID: "user:0:flush:1", ThreadID: "t1", TurnIndex: 0,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "queued while streaming", CreatedAt: now, UpdatedAt: now,
	}
	if err := router.PersistItemQuiet(flushRow, nil); err != nil {
		t.Fatalf("quiet persist: %v", err)
	}
	router.RegisterPendingQuietFlushSendWithExpectation("t1", "queue:q1", flushRow, 1, now, PendingSendExpectation{ProviderItemID: "ao-uuid-1"})
	if promoted := promoteQuietForTest(router, "t1"); len(promoted) != 1 {
		t.Fatalf("promote = %+v, want the flush row", promoted)
	}

	// A tool completion dispatched before the echo sits in the interrupt
	// queue (its same-scope stream is still mid-settle) — no item_index
	// yet.
	deferredCompletion := store.Item{
		ID: "tool:0:1", ThreadID: "t1", TurnIndex: 0,
		Kind: "tool_call", Role: "assistant", Status: "completed",
		Summary: "pre-echo completion", CreatedAt: now + 1, UpdatedAt: now + 1,
	}
	router.mu.Lock()
	router.state("t1").interruptQueue = append(router.state("t1").interruptQueue, queuedPersistence{item: deferredCompletion})
	router.mu.Unlock()

	// Mid-loop echo of the promoted message.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1",
		Content:   "queued while streaming",
		Meta:      json.RawMessage(`{"provider_item_id":"ao-uuid-1"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("echo: %v", err)
	}

	completion, found, err := st.GetThreadItem("t1", "tool:0:1")
	if err != nil || !found {
		t.Fatalf("deferred completion after echo: found=%v err=%v", found, err)
	}
	row, found, err := st.GetThreadItem("t1", "user:0:flush:1")
	if err != nil || !found {
		t.Fatalf("flush row after echo: found=%v err=%v", found, err)
	}
	if row.ItemIndex <= completion.ItemIndex {
		t.Errorf("promoted row index %d not re-bumped past the drained completion %d — display order diverges from provider order", row.ItemIndex, completion.ItemIndex)
	}
	state, err := itemmeta.DecodePromotionState(row.Meta)
	if err != nil {
		t.Fatalf("decode promotion state: %v", err)
	}
	if !state.HasEchoBoundary || state.EchoBoundary < completion.ItemIndex {
		t.Fatalf("echo boundary = (%v, %d), must cover the drained completion at %d", state.HasEchoBoundary, state.EchoBoundary, completion.ItemIndex)
	}

	// End to end: the response persists after the echo; revert at the
	// promoted row keeps the pre-echo completion and cuts the response.
	responseRow := store.Item{
		ID: "text:0:9", ThreadID: "t1", TurnIndex: 0,
		Kind: "assistant_text", Role: "assistant", Status: "completed",
		Summary: "response to queued", CreatedAt: now + 3, UpdatedAt: now + 3,
	}
	if err := router.persistItem(responseRow, nil); err != nil {
		t.Fatalf("persist response: %v", err)
	}
	if _, _, err := st.DeleteConversationFromItem("t1", "user:0:flush:1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, found, _ := st.GetThreadItem("t1", "tool:0:1"); !found {
		t.Error("revert cut the pre-echo completion — provider-order-before content lost while the session slice retains it")
	}
	if _, found, _ := st.GetThreadItem("t1", "text:0:9"); found {
		t.Error("revert kept the response row — provider-order-after content should be cut")
	}
}

// TestEchoBoundaryAnchor_RecordedOnFailureNotReplacedOnRetry pins
// R10-2 (round 10): when a flush echo's write fails pre-durability, the
// confirmed-hook anchor is recorded RIGHT THEN against the existing
// row — the echo is the consumption boundary. The retry's success-path
// hook must then be skipped so the boundary-time record stands.
// Deferred shapes whose row never persisted stay honestly anchor-less
// (the anchor row needs the item FK).
func TestEchoBoundaryAnchor_RecordedOnFailureNotReplacedOnRetry(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	var hookRows []store.Item
	router.SetFlushUserTextConfirmedHook(func(threadID string, item store.Item) {
		hookRows = append(hookRows, item)
	})
	now := time.Now().UnixMilli()

	quietRow := store.Item{
		ID: "user:0:flush:1", ThreadID: "t1", TurnIndex: 0,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "queued text", CreatedAt: now, UpdatedAt: now,
	}
	if err := router.PersistItemQuiet(quietRow, nil); err != nil {
		t.Fatalf("persist quiet row: %v", err)
	}

	// Failure-path capture: hook runs once against the existing row and
	// the entry is marked; a second call (echo replayed and failing
	// again) must not re-capture.
	entry := pendingSend{
		AOItemID: "user:0:flush:1", QueueItemID: "queue:q1", TurnIndex: 0,
		Shape:     sendShapeFlush,
		QuietItem: &quietRow, ExpectedProviderItemID: "ao-uuid-1",
		InterruptedTurnIndex: -1, EchoPromotedBoundary: -1,
	}
	router.recordEchoBoundaryAnchor("t1", &entry)
	if len(hookRows) != 1 || hookRows[0].ID != "user:0:flush:1" {
		t.Fatalf("hook rows after failure record = %+v, want the quiet row once", hookRows)
	}
	if !entry.AnchorRecordedAtEcho {
		t.Fatal("AnchorRecordedAtEcho not set after record")
	}
	router.recordEchoBoundaryAnchor("t1", &entry)
	if len(hookRows) != 1 {
		t.Fatalf("hook rows after repeated record = %d, want still 1", len(hookRows))
	}

	// Deferred shape: row not in the store — no capture, no flag.
	deferredRow := store.Item{
		ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "deferred text",
	}
	deferredEntry := pendingSend{
		AOItemID: "user:1:flush:1", QueueItemID: "queue:q2", TurnIndex: 1,
		Shape:        sendShapeFlush,
		DeferredItem: &deferredRow, InterruptedTurnIndex: -1, EchoPromotedBoundary: -1,
	}
	router.recordEchoBoundaryAnchor("t1", &deferredEntry)
	if len(hookRows) != 1 || deferredEntry.AnchorRecordedAtEcho {
		t.Fatalf("deferred record = (rows %d, flag %v), want no record for a missing row", len(hookRows), deferredEntry.AnchorRecordedAtEcho)
	}

	// Retry: the reinserted entry carries the flag; the successful echo
	// must skip the success-path hook so the boundary-time record stands.
	router.reinsertPendingSendHead("t1", entry)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1", Content: "queued text",
		Meta:      json.RawMessage(`{"provider_item_id":"ao-uuid-1"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("retry echo: %v", err)
	}
	if len(hookRows) != 1 {
		t.Fatalf("hook rows after retry echo = %d, want still 1 — the retry must not replace the boundary ref", len(hookRows))
	}
	if _, found, err := st.GetThreadItem("t1", "user:0:flush:1"); err != nil || !found {
		t.Fatalf("flush row after retry echo: found=%v err=%v", found, err)
	}

	// Control: an unflagged entry's successful echo still captures.
	quietRow2 := store.Item{
		ID: "user:0:flush:2", ThreadID: "t1", TurnIndex: 0,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "second queued", CreatedAt: now, UpdatedAt: now,
	}
	if err := router.PersistItemQuiet(quietRow2, nil); err != nil {
		t.Fatalf("persist second quiet row: %v", err)
	}
	router.RegisterPendingQuietFlushSendWithExpectation("t1", "queue:q3", quietRow2, 0, now, PendingSendExpectation{ProviderItemID: "ao-uuid-2"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1", Content: "second queued",
		Meta:      json.RawMessage(`{"provider_item_id":"ao-uuid-2"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("control echo: %v", err)
	}
	if len(hookRows) != 2 || hookRows[1].ID != "user:0:flush:2" {
		t.Fatalf("hook rows after control echo = %+v, want the second row appended", hookRows)
	}
}

// TestUnanchoredEagerFlushEcho_DrainsDeferredRowsBelowBump pins the
// success half of R10-1 (round 10): the unanchored eager echo drains
// rows deferred behind a mid-settle stream BEFORE bumping its own row
// to the turn tail. A post-bump drain would sort content the model
// emitted before consuming the queued message ABOVE its attachment —
// display order inverting provider order.
func TestUnanchoredEagerFlushEcho_DrainsDeferredRowsBelowBump(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}
	now := time.Now().UnixMilli()

	// Eager quiet flush dispatch into the active turn — no interrupt,
	// so the entry stays unanchored.
	flushRow := store.Item{
		ID: "user:0:flush:1", ThreadID: "t1", TurnIndex: 0,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "queued while streaming", CreatedAt: now, UpdatedAt: now,
	}
	if err := router.PersistItemQuiet(flushRow, nil); err != nil {
		t.Fatalf("quiet persist: %v", err)
	}
	router.RegisterPendingQuietFlushSendWithExpectation("t1", "queue:q1", flushRow, 0, now, PendingSendExpectation{ProviderItemID: "ao-uuid-1"})

	// A tool completion dispatched before the echo sits in the interrupt
	// queue (its same-scope stream is still mid-settle) — no item_index
	// yet.
	deferredCompletion := store.Item{
		ID: "tool:0:1", ThreadID: "t1", TurnIndex: 0,
		Kind: "tool_call", Role: "assistant", Status: "completed",
		Summary: "pre-echo completion", CreatedAt: now + 1, UpdatedAt: now + 1,
	}
	router.mu.Lock()
	router.state("t1").interruptQueue = append(router.state("t1").interruptQueue, queuedPersistence{item: deferredCompletion})
	router.mu.Unlock()

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1",
		Content:   "queued while streaming",
		Meta:      json.RawMessage(`{"provider_item_id":"ao-uuid-1"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("echo: %v", err)
	}

	completion, found, err := st.GetThreadItem("t1", "tool:0:1")
	if err != nil || !found {
		t.Fatalf("deferred completion after echo: found=%v err=%v — the echo must drain the queue before its bump", found, err)
	}
	row, found, err := st.GetThreadItem("t1", "user:0:flush:1")
	if err != nil || !found {
		t.Fatalf("flush row after echo: found=%v err=%v", found, err)
	}
	if row.ItemIndex <= completion.ItemIndex {
		t.Errorf("bumped flush row index %d not above the drained completion %d — display order diverges from provider order", row.ItemIndex, completion.ItemIndex)
	}
	// The successful bump put the row at the tail: no promotion marker
	// belongs on it (the normal index cut is already provider order).
	state, err := itemmeta.DecodePromotionState(row.Meta)
	if err != nil {
		t.Fatalf("decode promotion state: %v", err)
	}
	if state.Promoted || state.HasEchoBoundary {
		t.Errorf("promotion state after successful bump = %+v, want unmarked", state)
	}
}

// TestSelfHealUnanchoredBumpFailure_MarksBoundary pins the failure half
// of R10-1 (round 10): when the unanchored eager echo's tail bump fails
// and the session dies, the healed row stays at its dispatch-time index
// — display position is unrecoverable — but the echo-time boundary
// stashed on the entry lets the self-heal mark it promoted-with-
// boundary, so a revert at the message keeps the provider-order prefix
// (output emitted before consumption) and cuts only the response.
func TestSelfHealUnanchoredBumpFailure_MarksBoundary(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	now := time.Now().UnixMilli()

	// Dispatch-time state: the quiet row persisted at index 0, then the
	// model emitted output that persisted after it, pre-echo.
	quietRow := store.Item{
		ID: "user:0:flush:1", ThreadID: "t1", TurnIndex: 0,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "queued text", CreatedAt: now, UpdatedAt: now,
	}
	if err := router.PersistItemQuiet(quietRow, nil); err != nil {
		t.Fatalf("persist quiet row: %v", err)
	}
	preEcho := store.Item{
		ID: "tool:0:1", ThreadID: "t1", TurnIndex: 0,
		Kind: "tool_call", Role: "assistant", Status: "completed",
		Summary: "pre-echo output", CreatedAt: now + 1, UpdatedAt: now + 1,
	}
	if err := router.persistItem(preEcho, nil); err != nil {
		t.Fatalf("persist pre-echo output: %v", err)
	}
	preEchoRow, _, err := st.GetThreadItem("t1", "tool:0:1")
	if err != nil {
		t.Fatalf("read pre-echo output: %v", err)
	}

	// Echo arrived, sampled the boundary, and the tail bump failed: the
	// entry is reinserted echo-consumed with the boundary stashed. The
	// response then persists before the session dies.
	router.reinsertPendingSendHead("t1", pendingSend{
		AOItemID: "user:0:flush:1", QueueItemID: "queue:q1", TurnIndex: 0,
		Shape:                sendShapeFlush,
		QuietItem:            &quietRow,
		EchoProviderItemID:   "uuid-echo-1",
		EchoParentUUID:       "uuid-parent-1",
		EchoPromotedBoundary: preEchoRow.ItemIndex,
	})
	response := store.Item{
		ID: "text:0:9", ThreadID: "t1", TurnIndex: 0,
		Kind: "assistant_text", Role: "assistant", Status: "completed",
		Summary: "response to queued", CreatedAt: now + 3, UpdatedAt: now + 3,
	}
	if err := router.persistItem(response, nil); err != nil {
		t.Fatalf("persist response: %v", err)
	}

	if drained := router.DrainUnconfirmedFlushItems("t1"); len(drained) != 0 {
		t.Fatalf("drain of echo-consumed entry = %+v, want nothing restorable", drained)
	}

	healed, found, err := st.GetThreadItem("t1", "user:0:flush:1")
	if err != nil || !found {
		t.Fatalf("healed row after drain: found=%v err=%v", found, err)
	}
	state, err := itemmeta.DecodePromotionState(healed.Meta)
	if err != nil {
		t.Fatalf("decode healed promotion state: %v", err)
	}
	if !state.Promoted || !state.HasEchoBoundary || state.EchoBoundary != preEchoRow.ItemIndex {
		t.Fatalf("healed promotion state = %+v, want promoted with boundary %d", state, preEchoRow.ItemIndex)
	}

	// End to end: revert at the healed message keeps the pre-echo output
	// (provider-order BEFORE the attachment) and cuts the response.
	if _, _, err := st.DeleteConversationFromItem("t1", "user:0:flush:1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, found, _ := st.GetThreadItem("t1", "tool:0:1"); !found {
		t.Error("revert cut the pre-echo output — provider-order-before content lost while the session slice retains it")
	}
	if _, found, _ := st.GetThreadItem("t1", "text:0:9"); found {
		t.Error("revert kept the response row — provider-order-after content should be cut")
	}
}

// TestRebumpOverDrained_PreservesAnchoredSiblingFIFO pins R7-2
// (round 7): when a promoted echo drains deferred rows and re-bumps its
// own row past them, later-FIFO anchored quiet siblings in the same
// turn must re-bump too — in FIFO order. Left in place, a sibling sits
// below both the drained content and the echoed row: display order
// inverts provider order, and the user-row cut predicate (item_index >=
// anchor) KEEPS the sibling on a revert at the echoed message while the
// session slice removes it.
func TestRebumpOverDrained_PreservesAnchoredSiblingFIFO(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}
	now := time.Now().UnixMilli()

	// Two quiet flush dispatches, both promoted by one interrupt (FIFO:
	// q1 then q2).
	q1 := store.Item{
		ID: "user:0:flush:1", ThreadID: "t1", TurnIndex: 0,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "first queued", CreatedAt: now, UpdatedAt: now,
	}
	q2 := store.Item{
		ID: "user:0:flush:2", ThreadID: "t1", TurnIndex: 0,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "second queued", CreatedAt: now + 1, UpdatedAt: now + 1,
	}
	for _, row := range []store.Item{q1, q2} {
		if err := router.PersistItemQuiet(row, nil); err != nil {
			t.Fatalf("quiet persist %s: %v", row.ID, err)
		}
	}
	router.RegisterPendingQuietFlushSendWithExpectation("t1", "queue:q1", q1, 1, now, PendingSendExpectation{ProviderItemID: "ao-uuid-1"})
	router.RegisterPendingQuietFlushSendWithExpectation("t1", "queue:q2", q2, 1, now+1, PendingSendExpectation{ProviderItemID: "ao-uuid-2"})
	if promoted := promoteQuietForTest(router, "t1"); len(promoted) != 2 {
		t.Fatalf("promote = %d rows, want 2", len(promoted))
	}

	// A deferred completion waits in the interrupt queue when q1's echo
	// arrives.
	deferredCompletion := store.Item{
		ID: "tool:0:1", ThreadID: "t1", TurnIndex: 0,
		Kind: "tool_call", Role: "assistant", Status: "completed",
		Summary: "pre-echo completion", CreatedAt: now + 2, UpdatedAt: now + 2,
	}
	router.mu.Lock()
	router.state("t1").interruptQueue = append(router.state("t1").interruptQueue, queuedPersistence{item: deferredCompletion})
	router.mu.Unlock()

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1", Content: "first queued",
		Meta:      json.RawMessage(`{"provider_item_id":"ao-uuid-1"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("q1 echo: %v", err)
	}

	completion := mustGetItem(t, st, "t1", "tool:0:1")
	row1 := mustGetItem(t, st, "t1", "user:0:flush:1")
	row2 := mustGetItem(t, st, "t1", "user:0:flush:2")
	if !(completion.ItemIndex < row1.ItemIndex && row1.ItemIndex < row2.ItemIndex) {
		t.Fatalf("order after q1 echo = completion %d, q1 %d, q2 %d — want completion < q1 < q2 (provider order)",
			completion.ItemIndex, row1.ItemIndex, row2.ItemIndex)
	}
	state, err := itemmeta.DecodePromotionState(row1.Meta)
	if err != nil {
		t.Fatalf("decode q1 promotion state: %v", err)
	}
	if !state.HasEchoBoundary || state.EchoBoundary < completion.ItemIndex || state.EchoBoundary >= row2.ItemIndex {
		t.Fatalf("q1 boundary = (%v, %d): must cover the completion at %d and exclude the re-bumped sibling at %d",
			state.HasEchoBoundary, state.EchoBoundary, completion.ItemIndex, row2.ItemIndex)
	}

	// Revert at q1: the drained completion (interrupted tail) survives,
	// the sibling q2 — consumed after q1 in the provider transcript — is
	// cut, matching the session slice.
	if _, _, err := st.DeleteConversationFromItem("t1", "user:0:flush:1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, found, _ := st.GetThreadItem("t1", "tool:0:1"); !found {
		t.Error("revert cut the drained completion — provider-order-before content lost")
	}
	if _, found, _ := st.GetThreadItem("t1", "user:0:flush:2"); found {
		t.Error("revert kept the later-FIFO sibling — the session slice at q1 removes it")
	}
}

// TestFailedSiblingRebump_RepairedBySiblingEcho pins R11-5 (round 11):
// when a promoted echo's sibling re-bump fails (transient store
// error), the sibling's row is left below the drained content and the
// echoed row — and nothing revisited the position, because the
// sibling's own echo skips the bump as already-anchored. The failure
// must record a repair obligation on the entry so that echo forces
// the turn-tail bump, restoring provider order and the revert cut.
func TestFailedSiblingRebump_RepairedBySiblingEcho(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}
	now := time.Now().UnixMilli()

	q1 := store.Item{
		ID: "user:0:flush:1", ThreadID: "t1", TurnIndex: 0,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "first queued", CreatedAt: now, UpdatedAt: now,
	}
	q2 := store.Item{
		ID: "user:0:flush:2", ThreadID: "t1", TurnIndex: 0,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "second queued", CreatedAt: now + 1, UpdatedAt: now + 1,
	}
	for _, row := range []store.Item{q1, q2} {
		if err := router.PersistItemQuiet(row, nil); err != nil {
			t.Fatalf("quiet persist %s: %v", row.ID, err)
		}
	}
	router.RegisterPendingQuietFlushSendWithExpectation("t1", "queue:q1", q1, 1, now, PendingSendExpectation{ProviderItemID: "ao-uuid-1"})
	router.RegisterPendingQuietFlushSendWithExpectation("t1", "queue:q2", q2, 1, now+1, PendingSendExpectation{ProviderItemID: "ao-uuid-2"})
	if promoted := promoteQuietForTest(router, "t1"); len(promoted) != 2 {
		t.Fatalf("promote = %d rows, want 2", len(promoted))
	}

	// Make q2's re-bump fail through the production path: the row is
	// unreachable while q1's echo runs (deleted here, re-inserted below
	// — the in-store stand-in for a transient store error).
	q2Promoted := mustGetItem(t, st, "t1", "user:0:flush:2")
	if err := st.DeleteThreadItem("t1", "user:0:flush:2"); err != nil {
		t.Fatalf("delete q2 row: %v", err)
	}
	router.mu.Lock()
	router.state("t1").interruptQueue = append(router.state("t1").interruptQueue, queuedPersistence{item: store.Item{
		ID: "tool:0:1", ThreadID: "t1", TurnIndex: 0,
		Kind: "tool_call", Role: "assistant", Status: "completed",
		Summary: "pre-echo completion", CreatedAt: now + 2, UpdatedAt: now + 2,
	}})
	router.mu.Unlock()
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1", Content: "first queued",
		Meta:      json.RawMessage(`{"provider_item_id":"ao-uuid-1"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("q1 echo: %v", err)
	}

	router.mu.Lock()
	flagged := false
	for _, entry := range router.state("t1").pendingSends {
		if entry.AOItemID == "user:0:flush:2" && entry.NeedsTailRebump {
			flagged = true
		}
	}
	router.mu.Unlock()
	if !flagged {
		t.Fatal("failed sibling re-bump did not record the repair obligation")
	}

	// The transient failure clears: the row is back below both the
	// drained completion and q1's re-bumped row — the inversion the
	// failed re-bump left behind. (The delete/re-insert stand-in freed
	// q2's promote-time index to the drained completion, so the
	// re-insert uses q2's pre-promote slot; only the relative order
	// matters here.)
	q2Promoted.ItemIndex = 1
	if err := st.InsertItem(q2Promoted); err != nil {
		t.Fatalf("re-insert q2 row: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1", Content: "second queued",
		Meta:      json.RawMessage(`{"provider_item_id":"ao-uuid-2"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("q2 echo: %v", err)
	}

	completion := mustGetItem(t, st, "t1", "tool:0:1")
	row1 := mustGetItem(t, st, "t1", "user:0:flush:1")
	row2 := mustGetItem(t, st, "t1", "user:0:flush:2")
	if !(completion.ItemIndex < row1.ItemIndex && row1.ItemIndex < row2.ItemIndex) {
		t.Fatalf("order after repair echo = completion %d, q1 %d, q2 %d — want completion < q1 < q2 (provider order)",
			completion.ItemIndex, row1.ItemIndex, row2.ItemIndex)
	}

	// The repaired layout restores the revert semantics the failed
	// re-bump broke: a revert at q1 cuts q2 (consumed later in the
	// provider transcript) and keeps the drained completion.
	if _, _, err := st.DeleteConversationFromItem("t1", "user:0:flush:1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, found, _ := st.GetThreadItem("t1", "tool:0:1"); !found {
		t.Error("revert cut the drained completion — provider-order-before content lost")
	}
	if _, found, _ := st.GetThreadItem("t1", "user:0:flush:2"); found {
		t.Error("revert kept the repaired sibling — the session slice at q1 removes it")
	}
}

// TestPromotedEchoBoundary_WaitsForInFlightDrain pins R7-3 (round 7):
// an async settle's drain hands the queue off (map cleared) before its
// rows commit. The promoted echo must wait for that in-flight drain —
// sampling MAX in the gap would put the handed-off row above the
// boundary, where a revert cuts it as "response" although it precedes
// the queued message in the provider transcript.
func TestPromotedEchoBoundary_WaitsForInFlightDrain(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}
	now := time.Now().UnixMilli()

	flushRow := store.Item{
		ID: "user:0:flush:1", ThreadID: "t1", TurnIndex: 0,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "queued while streaming", CreatedAt: now, UpdatedAt: now,
	}
	if err := router.PersistItemQuiet(flushRow, nil); err != nil {
		t.Fatalf("quiet persist: %v", err)
	}
	router.RegisterPendingQuietFlushSendWithExpectation("t1", "queue:q1", flushRow, 1, now, PendingSendExpectation{ProviderItemID: "ao-uuid-1"})
	if promoted := promoteQuietForTest(router, "t1"); len(promoted) != 1 {
		t.Fatalf("promote = %d rows, want 1", len(promoted))
	}

	deferredCompletion := store.Item{
		ID: "tool:0:1", ThreadID: "t1", TurnIndex: 0,
		Kind: "tool_call", Role: "assistant", Status: "completed",
		Summary: "pre-echo completion", CreatedAt: now + 1, UpdatedAt: now + 1,
	}
	router.mu.Lock()
	router.state("t1").interruptQueue = append(router.state("t1").interruptQueue, queuedPersistence{item: deferredCompletion})
	router.mu.Unlock()

	// Simulate a settle-goroutine drain mid-flight: the queue is popped
	// under the drain lock but its row has not committed yet.
	drainLock := router.drainLock("t1")
	drainLock.Lock()
	router.mu.Lock()
	handedOff := router.state("t1").interruptQueue
	router.state("t1").interruptQueue = nil
	router.mu.Unlock()

	echoDone := make(chan error, 1)
	go func() {
		echoDone <- router.Handle(provider.ProviderEvent{
			Kind: provider.EventUserText, ThreadID: "t1",
			Content:   "queued while streaming",
			Meta:      json.RawMessage(`{"provider_item_id":"ao-uuid-1"}`),
			Timestamp: time.Now(),
		})
	}()
	// Give the echo a chance to reach the boundary section; with the
	// drain lock held it must block there rather than sample MAX.
	time.Sleep(50 * time.Millisecond)

	// The in-flight drain commits its row, then releases the lock.
	for _, queued := range handedOff {
		if err := router.persistItem(queued.item, queued.payload); err != nil {
			drainLock.Unlock()
			t.Fatalf("persist handed-off row: %v", err)
		}
	}
	drainLock.Unlock()
	if err := <-echoDone; err != nil {
		t.Fatalf("echo: %v", err)
	}

	completion := mustGetItem(t, st, "t1", "tool:0:1")
	row := mustGetItem(t, st, "t1", "user:0:flush:1")
	state, err := itemmeta.DecodePromotionState(row.Meta)
	if err != nil {
		t.Fatalf("decode promotion state: %v", err)
	}
	if !state.HasEchoBoundary || state.EchoBoundary < completion.ItemIndex {
		t.Fatalf("echo boundary = (%v, %d): sampled before the in-flight drain committed its row at %d — revert would cut interrupted-tail content the session slice keeps",
			state.HasEchoBoundary, state.EchoBoundary, completion.ItemIndex)
	}
}

// TestSelfHealDeferredEcho_PersistsPromptAboveItsResponse pins R7-4
// (round 7): a deferred prompt whose turn was EMPTY at its first echo
// (EchoTurnWasEmpty stashed) owns that turn — when the first persist
// failed, the turn opened anyway (R6-3) and response rows took 0..n by
// the time the session-death self-heal re-persists the retained copy.
// A MAX+1 append would sort the prompt AFTER its own response —
// turn-head placement keeps it first, matching the provider transcript.
func TestSelfHealDeferredEcho_PersistsPromptAboveItsResponse(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// The response streamed in behind the failed first echo, taking the
	// turn's low indexes.
	response := store.Item{
		ID: "text:1:0", ThreadID: "t1", TurnIndex: 1,
		Kind: "assistant_text", Role: "assistant", Status: "completed",
		Summary: "response to queued",
	}
	if err := router.persistItem(response, nil); err != nil {
		t.Fatalf("persist response: %v", err)
	}

	deferredRow := store.Item{
		ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "queued",
	}
	router.reinsertPendingSendHead("t1", pendingSend{
		AOItemID: "user:1:flush:1", QueueItemID: "queue:q1", TurnIndex: 1,
		Shape:                sendShapeFlush,
		DeferredItem:         &deferredRow,
		EchoConsumed:         true,
		EchoProviderItemID:   "uuid-echo-1",
		EchoTurnWasEmpty:     true,
		InterruptedTurnIndex: -1,
		EchoPromotedBoundary: -1,
	})

	if drained := router.DrainUnconfirmedFlushItems("t1"); len(drained) != 0 {
		t.Fatalf("drain of echo-consumed entry = %+v, want nothing restorable", drained)
	}
	prompt := mustGetItem(t, st, "t1", "user:1:flush:1")
	persistedResponse := mustGetItem(t, st, "t1", "text:1:0")
	if prompt.ItemIndex >= persistedResponse.ItemIndex {
		t.Fatalf("healed prompt index %d not above its response %d — revert at the prompt would keep response rows the session slice removes",
			prompt.ItemIndex, persistedResponse.ItemIndex)
	}
}

// TestDeferredEchoRetry_PersistsPromptAboveItsResponse is the replay
// twin of the self-heal test above (round-7, R7-4): a re-delivered
// echo retrying a reinserted EchoConsumed entry must honor the stashed
// EchoTurnWasEmpty and persist the prompt at the turn head, above the
// response rows that filled the turn after the failed first attempt.
func TestDeferredEchoRetry_PersistsPromptAboveItsResponse(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	response := store.Item{
		ID: "text:1:0", ThreadID: "t1", TurnIndex: 1,
		Kind: "assistant_text", Role: "assistant", Status: "completed",
		Summary: "response to queued",
	}
	if err := router.persistItem(response, nil); err != nil {
		t.Fatalf("persist response: %v", err)
	}

	deferredRow := store.Item{
		ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "queued",
	}
	router.reinsertPendingSendHead("t1", pendingSend{
		AOItemID: "user:1:flush:1", QueueItemID: "queue:q1", TurnIndex: 1,
		Shape:                sendShapeFlush,
		DeferredItem:         &deferredRow,
		EchoConsumed:         true,
		EchoProviderItemID:   "uuid-echo-1",
		EchoTurnWasEmpty:     true,
		InterruptedTurnIndex: -1,
		EchoPromotedBoundary: -1,
	})

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1", Content: "queued",
		Meta:      json.RawMessage(`{"provider_item_id":"uuid-echo-1"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("retry echo: %v", err)
	}
	prompt := mustGetItem(t, st, "t1", "user:1:flush:1")
	persistedResponse := mustGetItem(t, st, "t1", "text:1:0")
	if prompt.ItemIndex >= persistedResponse.ItemIndex {
		t.Fatalf("retried prompt index %d not above its response %d — the stashed first-echo occupancy must drive placement",
			prompt.ItemIndex, persistedResponse.ItemIndex)
	}
	if got := usermessage.ReadProviderItemID(prompt.Meta); got != "uuid-echo-1" {
		t.Errorf("retried prompt provider_item_id = %q, want the echoed id", got)
	}
}

// TestSecondInterrupt_RefreshesDeferredOriginStamp pins R7-5 (round 7):
// a deferred-origin entry awaiting its echo (WasDeferred, eager-
// persisted by an EARLIER interrupt) must have its InterruptedTurnIndex
// refreshed by a SECOND interrupt. With the stale first-interrupt stamp
// the second echo settles the provably-cut sibling turn "end_turn", and
// the settlement claim blocks the truncated result from correcting it.
func TestSecondInterrupt_RefreshesDeferredOriginStamp(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	for i, id := range []string{"user:1:flush:1", "user:2:flush:1"} {
		router.RegisterPendingFlushSendWithExpectation("t1", fmt.Sprintf("queue:q%d", i+1), store.Item{
			ID: id, ThreadID: "t1", TurnIndex: i + 1,
			Kind: "user_text", Role: "user", Status: "completed",
			Summary: fmt.Sprintf("queued %d", i+1),
		}, time.Now().UnixMilli(), PendingSendExpectation{ProviderItemID: fmt.Sprintf("ao-uuid-%d", i+1)})

	}

	// First interrupt cuts turn 0 and eager-persists both entries.
	router.MarkFlushSendsInterrupted("t1", 0)
	if persisted := eagerPersistForTest(router, "t1", 0); len(persisted) != 2 {
		t.Fatalf("eager persist = %d rows, want 2", len(persisted))
	}
	// First echo opens turn 1 and settles turn 0 "interrupted".
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1", Content: "queued 1",
		Meta:      json.RawMessage(`{"provider_item_id":"ao-uuid-1"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("first echo: %v", err)
	}

	// Second interrupt cuts turn 1 (the first prompt's response turn)
	// while the second entry still awaits its echo.
	router.MarkFlushSendsInterrupted("t1", 1)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1", Content: "queued 2",
		Meta:      json.RawMessage(`{"provider_item_id":"ao-uuid-2"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("second echo: %v", err)
	}
	router.WaitForPendingSettles()

	turn0, found, err := st.GetTurn("t1:0")
	if err != nil || !found {
		t.Fatalf("turns row t1:0: found=%v err=%v", found, err)
	}
	if turn0.StopReason != "interrupted" {
		t.Errorf("turn 0 stop_reason = %q, want interrupted", turn0.StopReason)
	}
	turn1, found, err := st.GetTurn("t1:1")
	if err != nil || !found {
		t.Fatalf("turns row t1:1: found=%v err=%v", found, err)
	}
	if turn1.StopReason != "interrupted" {
		t.Errorf("turn 1 stop_reason = %q, want interrupted (second interrupt provably cut it; a stale first-interrupt stamp settles it end_turn)", turn1.StopReason)
	}
}

// TestSelfHealFoundRow_EmitsUpsert pins R7-7 (round 7): a non-interrupt
// quiet flush row is persisted quietly (never emitted) and only becomes
// visible through its echo's bump — when that write fails and the
// session dies, the self-heal's stamp must emit the upsert, or the
// user's message stays invisible until a thread reload.
func TestSelfHealFoundRow_EmitsUpsert(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	quietRow := store.Item{
		ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "quiet text",
	}
	if err := router.PersistItemQuiet(quietRow, nil); err != nil {
		t.Fatalf("persist quiet row: %v", err)
	}
	router.reinsertPendingSendHead("t1", pendingSend{
		AOItemID: "user:1:flush:1", QueueItemID: "queue:q1", TurnIndex: 1,
		Shape:                sendShapeFlush,
		QuietItem:            &quietRow,
		EchoConsumed:         true,
		EchoProviderItemID:   "uuid-echo-1",
		InterruptedTurnIndex: -1,
		EchoPromotedBoundary: -1,
	})

	emissions.reset()
	if drained := router.DrainUnconfirmedFlushItems("t1"); len(drained) != 0 {
		t.Fatalf("drain = %+v, want nothing restorable", drained)
	}
	upserts := filterItemEventUpserts(emissions.snapshot())
	var healed *store.Item
	for i := range upserts {
		if upserts[i].ID == "user:1:flush:1" {
			healed = &upserts[i]
			break
		}
	}
	if healed == nil {
		t.Fatal("self-heal stamped the row without emitting its upsert — the message stays invisible until a thread reload")
	}
	if got := usermessage.ReadProviderItemID(healed.Meta); got != "uuid-echo-1" {
		t.Errorf("emitted row provider_item_id = %q, want the stamped echo id", got)
	}
}

// mustGetItem loads an item that must exist.
func mustGetItem(t *testing.T, st *store.Store, threadID, itemID string) store.Item {
	t.Helper()
	item, found, err := st.GetThreadItem(threadID, itemID)
	if err != nil || !found {
		t.Fatalf("item %s/%s: found=%v err=%v", threadID, itemID, found, err)
	}
	return item
}

// TestDeferredEchoSampleFailure_RecordsTurnOpenFallback pins R14-2
// (round 14, D14-1): when the FIRST echo's occupancy sample fails, the
// entry must still record EchoTurnWasEmpty — from the router's
// turn-open state — before the reinsert marks it EchoConsumed. Without
// the fallback the retry / self-heal reads the zero value and appends
// an empty-turn prompt below its own response. A turn nobody opened is
// the prompt's own: the fallback must record it as empty.
func TestDeferredEchoSampleFailure_RecordsTurnOpenFallback(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	deferredRow := store.Item{
		ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "queued",
	}
	router.RegisterPendingFlushSendWithExpectation("t1", "queue:q1", deferredRow, time.Now().UnixMilli(), PendingSendExpectation{ProviderItemID: "ao-uuid-1"})

	// Closing the store fails BOTH the occupancy sample and the
	// subsequent persist — the entry reinserts EchoConsumed, and the
	// recorded occupancy is all a retry will ever have.
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1", Content: "queued",
		Meta:      json.RawMessage(`{"provider_item_id":"ao-uuid-1"}`),
		Timestamp: time.Now(),
	}); err == nil {
		t.Fatal("echo on a closed store should fail the deferred persist")
	}

	router.mu.Lock()
	pending := append([]pendingSend(nil), router.state("t1").pendingSends...)
	router.mu.Unlock()
	if len(pending) != 1 {
		t.Fatalf("pending entries = %d, want the reinserted echo-consumed entry", len(pending))
	}
	entry := pending[0]
	if !entry.EchoConsumed {
		t.Fatal("entry not reinserted EchoConsumed after failed echo handling")
	}
	if !entry.EchoTurnWasEmpty {
		t.Fatal("sample-failure fallback did not record the un-opened turn as empty — a retry would append the prompt below its own response")
	}
}

// TestDeferredEchoSampleFailure_OpenTurnFallsBackToSteerShape is the
// occupied twin of the fallback test above (round 14, D14-1): when the
// prompt's own turn is ALREADY open at the failed first echo, the
// fallback records steer-shape occupancy (EchoTurnWasEmpty=false) so
// the retry keeps the append that places pre-dispatch content first.
func TestDeferredEchoSampleFailure_OpenTurnFallsBackToSteerShape(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 1)

	deferredRow := store.Item{
		ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "queued",
	}
	router.RegisterPendingFlushSendWithExpectation("t1", "queue:q1", deferredRow, time.Now().UnixMilli(), PendingSendExpectation{ProviderItemID: "ao-uuid-1"})

	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1", Content: "queued",
		Meta:      json.RawMessage(`{"provider_item_id":"ao-uuid-1"}`),
		Timestamp: time.Now(),
	}); err == nil {
		t.Fatal("echo on a closed store should fail the deferred persist")
	}

	router.mu.Lock()
	pending := append([]pendingSend(nil), router.state("t1").pendingSends...)
	router.mu.Unlock()
	if len(pending) != 1 {
		t.Fatalf("pending entries = %d, want the reinserted echo-consumed entry", len(pending))
	}
	if pending[0].EchoTurnWasEmpty {
		t.Fatal("fallback recorded an already-open turn as empty — a retry would head-place the prompt above pre-dispatch content")
	}
}
