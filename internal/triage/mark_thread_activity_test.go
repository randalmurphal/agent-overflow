package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// TestStreamingDeltaDoesNotBumpThreadActivity is the regression guard for
// the bug this fix addresses: streaming text/thinking events used to
// reshuffle the sidebar on every chunk because the per-row writes
// implicitly touched threads.updated_at. The new contract is that
// item-row mutations leave the activity timestamp alone — only
// MarkThreadActivity moves it.
func TestStreamingDeltaDoesNotBumpThreadActivity(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	before := readThreadUpdatedAt(t, st, "t1")

	// Multiple streaming text deltas — the path that used to fire
	// `UPDATE threads SET updated_at = ?` on every flush.
	for i := 0; i < 5; i++ {
		evt := provider.ProviderEvent{
			Kind:      provider.EventTextDelta,
			ThreadID:  "t1",
			Content:   "chunk",
			Timestamp: time.Now(),
		}
		if err := router.Handle(evt); err != nil {
			t.Fatalf("handle delta %d: %v", i, err)
		}
	}

	after := readThreadUpdatedAt(t, st, "t1")
	if after != before {
		t.Fatalf("threads.updated_at moved across streaming deltas: before=%d after=%d", before, after)
	}
}

// TestUserTextPersistBumpsThreadActivity covers the first sidebar-bump
// boundary: a user-typed message persisted through the triage
// chokepoint advances activity to the row's UpdatedAt.
func TestUserTextPersistBumpsThreadActivity(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	before := readThreadUpdatedAt(t, st, "t1")

	now := before + 5_000
	if err := router.PersistItem(store.Item{
		ID:        "user:0",
		ThreadID:  "t1",
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "hi",
		CreatedAt: now,
		UpdatedAt: now,
	}, nil); err != nil {
		t.Fatalf("persist user_text: %v", err)
	}

	after := readThreadUpdatedAt(t, st, "t1")
	if after != now {
		t.Fatalf("threads.updated_at after user_text persist = %d, want %d", after, now)
	}
}

func TestNestedTurnCompleteDoesNotBumpThreadActivity(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	before := readThreadUpdatedAt(t, st, "t1")

	if err := st.InsertTurn(store.Turn{
		TurnID:    "turn-1",
		ThreadID:  "t1",
		TurnIndex: 0,
		StartedAt: before,
	}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}

	completeAt := before + 10_000
	completeEvt := provider.ProviderEvent{
		Kind:            provider.EventTurnComplete,
		ThreadID:        "t1",
		TurnID:          "turn-1",
		ParentToolUseID: "spawn-1",
		TurnComplete:    normalTurnCompleteMeta(),
		Timestamp:       time.Unix(0, completeAt*int64(time.Millisecond)),
	}
	if err := router.Handle(completeEvt); err != nil {
		t.Fatalf("handle nested turn complete: %v", err)
	}

	after := readThreadUpdatedAt(t, st, "t1")
	if after != before {
		t.Fatalf("threads.updated_at moved across nested turn complete: before=%d after=%d", before, after)
	}
}

// TestApprovalRequestBumpsThreadActivity covers the third sidebar-bump
// boundary: an approval or structured user-input request indicates the
// agent is paused waiting on the user, so the thread surfaces to the top
// of the sidebar.
func TestApprovalRequestBumpsThreadActivity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    provider.EventKind
		itemID  string
		request any
	}{
		{
			name: "approval", kind: provider.EventApprovalRequest, itemID: "approval-1",
			request: provider.ApprovalRequest{
				RequestID: "approval-1", ThreadID: "t1", TurnID: "turn-1",
				ToolName: "Bash", Description: "Run command",
			},
		},
		{
			name: "user input", kind: provider.EventUserInputRequest, itemID: "input-1",
			request: provider.UserInputRequest{
				RequestID: "input-1", ThreadID: "t1", TurnID: "turn-1",
				ToolName: "user_input", Title: "Pick one",
				Questions: []provider.UserInputQuestion{{
					ID: "scope", Question: "Choose",
					Options: []provider.UserInputQuestionOption{{Label: "turn", Description: "this turn"}},
				}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, st, _ := newTestRouter(t)
			createTestThread(t, st, "t1")
			before := readThreadUpdatedAt(t, st, "t1")

			requestMeta, err := json.Marshal(tc.request)
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			requestAt := before + 2_000
			if err := router.Handle(provider.ProviderEvent{
				Kind: tc.kind, ThreadID: "t1", TurnID: "turn-1", ItemID: tc.itemID,
				Meta: requestMeta, Timestamp: time.Unix(0, requestAt*int64(time.Millisecond)),
			}); err != nil {
				t.Fatalf("handle request: %v", err)
			}

			after := readThreadUpdatedAt(t, st, "t1")
			if after <= before {
				t.Fatalf("threads.updated_at did not advance on %s request: before=%d after=%d", tc.name, before, after)
			}
		})
	}
}

// TestApprovalResolveDoesNotBumpThreadActivity: answering an approval or
// a structured user-input request is the user-text path, not a separate
// bump. This guards against double-bumping when the user replies.
func TestApprovalResolveDoesNotBumpThreadActivity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    provider.EventKind
		itemID  string
		resolve map[string]any
	}{
		{
			name: "approval", kind: provider.EventApprovalResolved, itemID: "approval-1",
			resolve: map[string]any{"requestId": "approval-1", "decision": "approved"},
		},
		{
			name: "user input", kind: provider.EventUserInputResolved, itemID: "input-1",
			resolve: map[string]any{"requestId": "input-1", "decision": "answered", "answers": map[string]string{"scope": "turn"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, st, _ := newTestRouter(t)
			createTestThread(t, st, "t1")
			// Manually drive activity forward so we can detect a regression
			// that bumps it again on resolve.
			if err := st.MarkThreadActivity("t1", time.Now().UnixMilli()); err != nil {
				t.Fatalf("seed activity: %v", err)
			}
			before := readThreadUpdatedAt(t, st, "t1")

			resolveMeta, err := json.Marshal(tc.resolve)
			if err != nil {
				t.Fatalf("marshal resolve: %v", err)
			}
			if err := router.Handle(provider.ProviderEvent{
				Kind: tc.kind, ThreadID: "t1", ItemID: tc.itemID, Meta: resolveMeta, Timestamp: time.Now(),
			}); err != nil {
				t.Fatalf("handle %s resolved: %v", tc.name, err)
			}

			after := readThreadUpdatedAt(t, st, "t1")
			if after != before {
				t.Fatalf("%s resolve unexpectedly bumped activity: before=%d after=%d", tc.name, before, after)
			}
		})
	}
}

// ---------------------------------------------------------------------
// The wire half of the same three boundaries. threads.updated_at moving
// in SQLite is only half the fact: since provider:item_event was narrowed
// to the threads a client watches, a client that is not watching learns
// about a user_text bump ONLY from the thread:updated patch below. The
// tests above pin the column; these pin the frame, and the column too
// where one fixture serves both.

// threadActivityPatches returns the thread:updated PATCH emissions that
// carry an activity bump. The patch is deliberately field-free otherwise
// — it is applied without a cached row, so a receiver that has never
// listed the thread can still reorder its sidebar and clear its badge.
func threadActivityPatches(t *testing.T, emissions []emitted) []ThreadUpdateEvent {
	t.Helper()
	out := make([]ThreadUpdateEvent, 0)
	for _, e := range filterEmissions(emissions, "thread:updated") {
		evt, ok := e.data.(ThreadUpdateEvent)
		if !ok {
			t.Fatalf("thread:updated payload type = %T, want ThreadUpdateEvent", e.data)
		}
		if evt.Action != ThreadActionPatch || evt.UpdatedAt == nil {
			continue
		}
		out = append(out, evt)
	}
	return out
}

func TestUserTextPersistEmitsActivityPatch(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	now := readThreadUpdatedAt(t, st, "t1") + 5_000
	emissions.reset()

	if err := router.PersistItem(store.Item{
		ID:        "user:0",
		ThreadID:  "t1",
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "hi",
		CreatedAt: now,
		UpdatedAt: now,
	}, nil); err != nil {
		t.Fatalf("persist user_text: %v", err)
	}

	patches := threadActivityPatches(t, emissions.snapshot())
	if len(patches) != 1 {
		t.Fatalf("activity patches = %d, want 1 (%+v)", len(patches), emissions.snapshot())
	}
	if patches[0].ID != "t1" {
		t.Fatalf("patch id = %q, want t1", patches[0].ID)
	}
	if *patches[0].UpdatedAt != now {
		t.Fatalf("patch updatedAt = %d, want %d", *patches[0].UpdatedAt, now)
	}
	// Field-free: a receiver merging these into the row would replace the
	// row object on every message the reader sends.
	if patches[0].Thread != nil || patches[0].Title != nil || patches[0].Model != nil || patches[0].SessionRef != nil {
		t.Fatalf("activity patch carried more than the bump: %+v", patches[0])
	}
}

// The eager persist on interrupt (app_flush_queue) writes the reader's
// message QUIETLY — the deferred echo owns the timeline row — but it
// still bumps activity, so the sidebar fact still has to reach the wire.
// Tying the patch to the bump rather than to the item emission is what
// makes that automatic.
func TestQuietUserTextPersistStillEmitsActivityPatch(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	now := readThreadUpdatedAt(t, st, "t1") + 5_000
	emissions.reset()

	if err := router.PersistItemQuiet(store.Item{
		ID:        "user:0",
		ThreadID:  "t1",
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "hi",
		CreatedAt: now,
		UpdatedAt: now,
	}, nil); err != nil {
		t.Fatalf("quiet persist user_text: %v", err)
	}

	if upserts := filterItemEventUpserts(emissions.snapshot()); len(upserts) != 0 {
		t.Fatalf("quiet persist emitted %d item upserts, want 0", len(upserts))
	}
	if patches := threadActivityPatches(t, emissions.snapshot()); len(patches) != 1 {
		t.Fatalf("activity patches = %d, want 1 (%+v)", len(patches), emissions.snapshot())
	}
}

// Persisting any row other than a user-typed message (assistant text, a
// wire-only or parented subagent prompt, or a prompt naming a missing
// parent) leaves activity alone: the column does not move and no
// activity patch reaches the wire.
func TestNonActivityPersistDoesNotBumpThreadActivity(t *testing.T) {
	cases := []struct {
		name string
		item store.Item
	}{
		{
			name: "wire only",
			item: store.Item{
				ID:   "user:wire:child_prompt_1",
				Kind: "user_text", Role: "user", Status: "completed",
				Summary: "subagent prompt",
				Meta:    `{"provider_item_id":"child_prompt_1","wire_only":true}`,
			},
		},
		{
			name: "parented",
			item: store.Item{
				ID:   "user:wire:child_prompt_2",
				Kind: "user_text", Role: "user", Status: "completed",
				Summary: "subagent prompt", ParentID: "spawn-1",
			},
		},
		{
			name: "invalid parent",
			item: store.Item{
				ID:   "user:wire:child_prompt_3",
				Kind: "user_text", Role: "user", Status: "completed",
				Summary: "subagent prompt with missing parent", ParentID: "missing-spawn",
			},
		},
		{
			name: "assistant text",
			item: store.Item{
				ID:   "asst:0:0",
				Kind: "assistant_text", Role: "assistant", Status: "completed",
				Summary: "ok",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, st, emissions := newTestRouter(t)
			createTestThread(t, st, "t1")
			before := readThreadUpdatedAt(t, st, "t1")
			now := before + 5_000
			if err := router.PersistItem(store.Item{
				ID: "spawn-1", ThreadID: "t1", Kind: "tool_call", Role: "assistant",
				Status: "running", Summary: "Spawn subagent",
				CreatedAt: now - 1, UpdatedAt: now - 1,
			}, nil); err != nil {
				t.Fatalf("persist parent tool_call: %v", err)
			}
			emissions.reset()

			item := tc.item
			item.ThreadID = "t1"
			item.CreatedAt = now
			item.UpdatedAt = now
			if err := router.PersistItem(item, nil); err != nil {
				t.Fatalf("persist %s: %v", tc.name, err)
			}

			if after := readThreadUpdatedAt(t, st, "t1"); after != before {
				t.Fatalf("threads.updated_at moved across %s persist: before=%d after=%d", tc.name, before, after)
			}
			if patches := threadActivityPatches(t, emissions.snapshot()); len(patches) != 0 {
				t.Fatalf("%s emitted %d activity patches, want 0", tc.name, len(patches))
			}
		})
	}
}

// TestTurnCompleteBumpsThreadActivity covers the second sidebar-bump
// boundary: a settled top-level turn (no parent, or a blank one) advances
// activity. The router's settleTurnRow path is the one production caller
// of UpdateTurnCompleted. The bump reaches every client on
// provider:turn_completed, which is wildcard and which the sidebar
// already reads, so it emits no activity patch: a second carrier for the
// same fact would double-apply it.
func TestTurnCompleteBumpsThreadActivity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		parent string
	}{
		{"no parent", ""},
		{"whitespace parent", "   "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, st, emissions := newTestRouter(t)
			createTestThread(t, st, "t1")
			before := readThreadUpdatedAt(t, st, "t1")
			if err := st.InsertTurn(store.Turn{
				TurnID: "turn-1", ThreadID: "t1", TurnIndex: 0, StartedAt: before,
			}); err != nil {
				t.Fatalf("insert turn: %v", err)
			}
			if err := router.Handle(provider.ProviderEvent{
				Kind: provider.EventTurnStart, ThreadID: "t1", TurnID: "turn-1",
				Timestamp: time.Unix(0, before*int64(time.Millisecond)),
			}); err != nil {
				t.Fatalf("handle turn start: %v", err)
			}
			emissions.reset()

			completeAt := before + 10_000
			if err := router.Handle(provider.ProviderEvent{
				Kind: provider.EventTurnComplete, ThreadID: "t1", TurnID: "turn-1",
				ParentToolUseID: tc.parent,
				TurnComplete:    normalTurnCompleteMeta(),
				Timestamp:       time.Unix(0, completeAt*int64(time.Millisecond)),
			}); err != nil {
				t.Fatalf("handle turn complete: %v", err)
			}

			if after := readThreadUpdatedAt(t, st, "t1"); after != completeAt {
				t.Fatalf("threads.updated_at after turn complete = %d, want %d", after, completeAt)
			}
			if patches := threadActivityPatches(t, emissions.snapshot()); len(patches) != 0 {
				t.Fatalf("turn completion emitted %d activity patches, want 0", len(patches))
			}
		})
	}
}

func readThreadUpdatedAt(t *testing.T, st *store.Store, threadID string) int64 {
	t.Helper()
	thr, err := st.GetThread(threadID)
	if err != nil {
		t.Fatalf("get thread %s: %v", threadID, err)
	}
	return thr.UpdatedAt
}
