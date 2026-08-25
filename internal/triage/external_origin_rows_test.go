package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
)

// The user echo of a turn another Codex process queued onto this thread
// (`codex queue --thread`) reaches the same no-pending-send branch as
// injected context — AO never sent it — but the session stamped its
// origin from the turn/started attribution, so it is a user row with a
// named provenance, never "Injected provider context".
func TestHandleUserText_ExternalQueueEcho_PersistsAUserRowWithOrigin(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 3)

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "please also run the linter",
		Meta:      json.RawMessage(`{"provider_item_id":"q-item-1","origin":"external-queue"}`),
		Timestamp: time.UnixMilli(1_700_000_000_000),
	}); err != nil {
		t.Fatalf("Handle EventUserText: %v", err)
	}

	if _, found, _ := st.GetThreadItem("t1", "injected:wire:q-item-1"); found {
		t.Fatal("an externally queued user echo persisted as injected provider context")
	}
	persisted, found, err := st.GetThreadItem("t1", "user:queue:q-item-1")
	if err != nil || !found {
		t.Fatalf("expected user:queue row: found=%v err=%v", found, err)
	}
	if persisted.Kind != string(provider.ItemUserText) || persisted.Role != "user" {
		t.Fatalf("kind/role = %q/%q, want user_text/user", persisted.Kind, persisted.Role)
	}
	if persisted.TurnIndex != 3 || persisted.Summary != "please also run the linter" {
		t.Fatalf("row = turn %d %q, want turn 3 with the queued text", persisted.TurnIndex, persisted.Summary)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(persisted.Meta), &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if got, _ := meta["origin"].(string); got != externalQueueOrigin {
		t.Fatalf("meta.origin = %v, want %q", meta["origin"], externalQueueOrigin)
	}
	if flagged, _ := meta["wire_only"].(bool); !flagged {
		t.Fatalf("meta.wire_only = %v, want true", meta["wire_only"])
	}
	if _, present := meta["cross_session_message"]; present {
		t.Fatalf("a queue echo must not carry the peer-session flag: %v", meta)
	}
}

// An origin triage does not recognise is NOT an attribution: the row
// stays on the injected-context safety net rather than becoming a user
// bubble on the strength of an unknown string.
func TestHandleUserText_UnknownOrigin_StaysInjectedContext(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 1)

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "hello",
		Meta:      json.RawMessage(`{"provider_item_id":"x-1","origin":"something-new"}`),
		Timestamp: time.UnixMilli(1_700_000_000_000),
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, found, _ := st.GetThreadItem("t1", "injected:wire:x-1"); !found {
		t.Fatal("unknown origin did not fall through to the injected-context row")
	}
}

func TestExternalQueueOriginMatchesTheProviderConstant(t *testing.T) {
	if externalQueueOrigin != codex.ExternalTurnOriginQueue {
		t.Fatalf("triage origin %q != codex.ExternalTurnOriginQueue %q", externalQueueOrigin, codex.ExternalTurnOriginQueue)
	}
}

// Codex stamps `delivery: "async"` on the stop envelope of an agentMessage
// the model sent mid-turn (send_user_message_async). The persisted row
// must carry it — the frontend's "Interim" chip reads item meta, not the
// wire — on both delivery paths: a block that streamed first, and one
// that arrived complete with no prior deltas.
func TestContentBlockStopCarriesDeliveryOntoTheRow(t *testing.T) {
	t.Run("streamed block", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventTextDelta, ThreadID: "t1", ItemID: "msg-1", Content: "heads up", Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("delta: %v", err)
		}
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventContentBlockStop, ThreadID: "t1", ItemID: "msg-1",
			Content: "heads up", ContentPresent: true,
			Meta: json.RawMessage(`{"blockType":"text","delivery":"async"}`), Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("stop: %v", err)
		}
		router.WaitForPendingSettles()
		assertSingleTextRowDelivery(t, st, "async")
	})
	t.Run("recovered block", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventContentBlockStop, ThreadID: "t1", ItemID: "msg-2",
			Content: "heads up", ContentPresent: true,
			Meta: json.RawMessage(`{"blockType":"text","delivery":"async"}`), Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("stop: %v", err)
		}
		router.WaitForPendingSettles()
		assertSingleTextRowDelivery(t, st, "async")
	})
	t.Run("ordinary block carries no delivery key", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventContentBlockStop, ThreadID: "t1", ItemID: "msg-3",
			Content: "final", ContentPresent: true,
			Meta: json.RawMessage(`{"blockType":"text"}`), Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("stop: %v", err)
		}
		router.WaitForPendingSettles()
		assertSingleTextRowDelivery(t, st, "")
	})
}

func assertSingleTextRowDelivery(t *testing.T, st *store.Store, want string) {
	t.Helper()
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 || items[0].Kind != itemKindAssistantText {
		t.Fatalf("expected one assistant_text row, got %+v", items)
	}
	if items[0].Status != statusCompleted {
		t.Fatalf("status = %q, want completed", items[0].Status)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(items[0].Meta), &meta); err != nil {
		t.Fatalf("decode meta %q: %v", items[0].Meta, err)
	}
	got, _ := meta["delivery"].(string)
	if got != want {
		t.Fatalf("meta.delivery = %q, want %q (meta %s)", got, want, items[0].Meta)
	}
	if want == "" {
		if _, present := meta["delivery"]; present {
			t.Fatalf("delivery key present on an ordinary block: %s", items[0].Meta)
		}
	}
}

// The provider's queue holds rows from more than one producer, drained FIFO
// by the app-server. A foreign row that dispatches BEFORE AO's must not pop
// AO's pending send: doing so stamps the foreign message onto the user's own
// optimistic row and leaves the user's real echo with nothing to match, so it
// lands as "Injected provider context". Two wrong rows from one mispop.
func TestHandleUserText_ForeignQueueEchoAheadOfOurs_LeavesThePendingSendAlone(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 3)

	now := time.UnixMilli(1_700_000_000_000).UnixMilli()
	ours := store.Item{
		ID:        "user:3:flush:1",
		ThreadID:  "t1",
		TurnIndex: 3,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "ours",
		CreatedAt: now,
		UpdatedAt: now,
	}
	// Codex registers with NO expected provider item id — item ids are
	// provider-assigned — which is exactly what leaves the entry defenceless
	// against a FIFO pop.
	router.RegisterPendingFlushSendWithExpectation("t1", "queue:m1", ours, now, PendingSendExpectation{ProviderItemID: ""})

	// The foreign row dispatches first. `codex queue` mints a v7 uuid for its
	// clientId, and the session stamps the turn's origin.
	if err := router.Handle(provider.ProviderEvent{
		Kind:     provider.EventUserText,
		ThreadID: "t1",
		Content:  "theirs",
		Meta: json.RawMessage(`{"provider_item_id":"q-foreign","origin":"external-queue",` +
			`"client_id":"01994f2b-0000-7000-8000-000000000001"}`),
		Timestamp: time.UnixMilli(1_700_000_000_001),
	}); err != nil {
		t.Fatalf("Handle foreign echo: %v", err)
	}

	foreign, found, err := st.GetThreadItem("t1", "user:queue:q-foreign")
	if err != nil || !found {
		t.Fatalf("foreign echo did not persist as its own queue row: found=%v err=%v", found, err)
	}
	if foreign.Summary != "theirs" {
		t.Fatalf("foreign row text = %q", foreign.Summary)
	}
	if _, found, _ := st.GetThreadItem("t1", ours.ID); found {
		t.Fatal("the foreign echo persisted AO's deferred row")
	}
	if !router.HasPendingSendForThread("t1") {
		t.Fatal("the foreign echo consumed AO's pending send")
	}

	// AO's own echo arrives second, carrying the client id AO handed to
	// `thread/queue/add`, and claims its own row.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "ours",
		Meta:      json.RawMessage(`{"provider_item_id":"q-ours","client_id":"user:3:flush:1"}`),
		Timestamp: time.UnixMilli(1_700_000_000_002),
	}); err != nil {
		t.Fatalf("Handle our echo: %v", err)
	}
	persisted, found, err := st.GetThreadItem("t1", ours.ID)
	if err != nil || !found {
		t.Fatalf("AO's deferred row never persisted: found=%v err=%v", found, err)
	}
	if persisted.Summary != "ours" {
		t.Fatalf("AO's row text = %q, want its own message", persisted.Summary)
	}
	if _, found, _ := st.GetThreadItem("t1", "injected:wire:q-ours"); found {
		t.Fatal("AO's own echo was persisted as injected provider context")
	}
	if router.HasPendingSendForThread("t1") {
		t.Fatal("AO's echo did not consume its pending send")
	}
}

// The client-id path must not disturb the providers that have no client id at
// all: an echo without one still pops the FIFO head.
func TestConsumeMatchingPendingSend_NoClientID_KeepsFIFO(t *testing.T) {
	router, _, _ := newTestRouter(t)
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{})
	router.RegisterPendingSendWithExpectation("t1", "user:2", 2, PendingSendExpectation{})

	head, ok := router.consumeMatchingPendingSendForEcho("t1", "codex-item-1", "")
	if !ok || head.AOItemID != "user:1" {
		t.Fatalf("head = %+v ok=%v, want user:1", head, ok)
	}
	next, ok := router.consumeMatchingPendingSendForEcho("t1", "codex-item-2", "")
	if !ok || next.AOItemID != "user:2" {
		t.Fatalf("next = %+v ok=%v, want user:2", next, ok)
	}
}

// A client id AO does not hold is not AO's message, even with entries waiting.
// Falling back to FIFO here would be the mispop this path exists to prevent.
func TestConsumeMatchingPendingSend_UnknownClientID_MatchesNothing(t *testing.T) {
	router, _, _ := newTestRouter(t)
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{})

	if got, ok := router.consumeMatchingPendingSendForEcho("t1", "codex-item-1", "01994f2b-dead"); ok {
		t.Fatalf("an unknown client id popped %+v", got)
	}
	if !router.HasPendingSendForThread("t1") {
		t.Fatal("the entry was consumed anyway")
	}
}
