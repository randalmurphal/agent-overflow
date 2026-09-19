package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func TestQuietFlushVisibilityTransitions(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "echo", true: "interrupt"}[interrupt], func(t *testing.T) {
			router, st, _ := newTestRouter(t)
			createTestThread(t, st, "t1")
			now := time.Now().UnixMilli()
			row := store.Item{ID: "user:0:flush:1", ThreadID: "t1", TurnIndex: 0, Kind: "user_text", Role: "user", Status: "completed", Summary: "queued", Meta: `{"sendId":"send-1","counter":9007199254740993}`, CreatedAt: now, UpdatedAt: now}
			if err := router.PersistAndRegisterPendingQuietFlushSendWithExpectation("t1", "queue-1", row, 1, now, PendingSendExpectation{ProviderItemID: "echo-1"}); err != nil {
				t.Fatal(err)
			}
			assertPending := func(want bool) {
				t.Helper()
				stored := mustGetItem(t, st, "t1", row.ID)
				var meta map[string]json.RawMessage
				if err := json.Unmarshal([]byte(stored.Meta), &meta); err != nil {
					t.Fatal(err)
				}
				// The marker carries one meaning: present while pending,
				// and REMOVED once confirmed, so a settled live row is
				// indistinguishable from the same row imported from a
				// session file.
				raw, present := meta["pendingFlush"]
				if present != want || (want && string(raw) != "true") {
					t.Fatalf("meta = %s, want pending=%v", stored.Meta, want)
				}
				if string(meta["sendId"]) != `"send-1"` || string(meta["counter"]) != "9007199254740993" {
					t.Fatalf("metadata changed: %s", stored.Meta)
				}
			}
			assertPending(true)
			if len(router.LiveStateSnapshotForThread("t1").FlushedItems) != 1 {
				t.Fatal("missing pending preview")
			}
			if interrupt {
				if len(promoteQuietForTest(router, "t1")) != 1 {
					t.Fatal("promotion failed")
				}
			} else {
				if err := router.Handle(provider.ProviderEvent{Kind: provider.EventUserText, ThreadID: "t1", TurnIndex: 1, Content: "queued", Meta: json.RawMessage(`{"provider_item_id":"echo-1"}`), Timestamp: time.Now()}); err != nil {
					t.Fatal(err)
				}
			}
			assertPending(false)
			if interrupt && len(promoteQuietForTest(router, "t1")) != 0 {
				t.Fatal("repeat interrupt promoted the row twice")
			}
			assertPending(false)
			if len(router.LiveStateSnapshotForThread("t1").FlushedItems) != 0 {
				t.Fatal("preview survived confirmation")
			}
		})
	}
}

func TestQuietFlushInvalidMetadataDoesNotPublishPendingSend(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	row := store.Item{ID: "user:0:flush:bad", ThreadID: "t1", Kind: "user_text", Meta: `{broken`}
	if err := router.PersistAndRegisterPendingQuietFlushSendWithExpectation("t1", "q-bad", row, 1, 1, PendingSendExpectation{}); err == nil {
		t.Fatal("expected metadata error")
	}
	if _, found, err := st.GetThreadItem("t1", row.ID); err != nil || found {
		t.Fatalf("failed registration left a row: found=%v err=%v", found, err)
	}
	if len(router.LiveStateSnapshotForThread("t1").FlushedItems) != 0 {
		t.Fatal("failed registration left a preview")
	}
}
