package triage

import (
	"testing"

	"agent-overflow/internal/store"
)

// A fork flush can meet a row that settled, moved to shared history, or was
// removed before its buffered delta arrived. These transitions must complete
// successfully without appending stale bytes or recreating deleted history.
func TestHistoryTransitionsDoNotFailLateStreamFlush(t *testing.T) {
	for _, kind := range []string{itemKindAssistantText, itemKindThinking} {
		for _, state := range []string{"settled", "prepared", "deleted"} {
			t.Run(kind+"/"+state, func(t *testing.T) {
				router, st, _ := newTestRouter(t)
				createTestThread(t, st, "source")
				item := store.Item{ID: "item", ThreadID: "source", Kind: kind, Role: "assistant", Status: "completed", Summary: "authoritative text", PayloadID: "payload", Meta: "{}", CreatedAt: 1, UpdatedAt: 1}
				if err := st.InsertItemWithPayload(item, store.Payload{ID: "payload", Kind: "text", Data: []byte(item.Summary), Meta: "{}", CreatedAt: 1}); err != nil {
					t.Fatal(err)
				}
				if state != "settled" {
					if n, err := st.PrepareThreadHistory(t.Context(), item.ThreadID); err != nil || n != 1 {
						t.Fatalf("prepare: n=%d err=%v", n, err)
					}
				}
				if state == "deleted" {
					if err := st.DeleteThreadItem(item.ThreadID, item.ID); err != nil {
						t.Fatal(err)
					}
				}
				// Seed a pending window directly to make the late arrival ordering
				// deterministic, without racing a wall-clock flush timer.
				buffer := &streamPersistBuffer{threadID: item.ThreadID, itemID: item.ID, kind: kind, payloadID: item.PayloadID, updatedAt: 2}
				buffer.content.WriteString(" stale duplicate")
				router.mu.Lock()
				router.state(item.ThreadID).streamPersistBuffers = map[string]*streamPersistBuffer{item.ID: buffer}
				router.mu.Unlock()
				for attempt := 0; attempt < 2; attempt++ {
					if err := router.FlushThread(item.ThreadID); err != nil {
						t.Fatalf("late flush must not abort the caller: %v", err)
					}
				}
				got, found, err := st.GetThreadItem(item.ThreadID, item.ID)
				if err != nil || found != (state != "deleted") {
					t.Fatalf("row lifetime changed: found=%v err=%v", found, err)
				}
				if found {
					data, err := st.GetPayloadData(item.ThreadID, item.PayloadID)
					if err != nil || got.Summary != item.Summary || string(data) != item.Summary {
						t.Fatalf("late flush changed settled content: summary=%q data=%q err=%v", got.Summary, data, err)
					}
				}
			})
		}
	}
}
