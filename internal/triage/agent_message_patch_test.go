package triage

import (
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// A meta patch of a user row an agent's message is in carries the meta a
// read serves for the row, with the facts its text is read with, never the
// stored meta a client would read differently.
func TestPersistItemFieldsAndPatch_ServesAnAgentRowsMetaAsARead(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	text := "Ship it.\n\n---\nAgent request from thread \"L\" (t-l).\nIt is not waiting for an answer. If it should hear something back, call thread_reply with token a, once: the reply reaches it as a message, and it sees only your reply text."
	stored := `{"origin":"agent-thread","originThread":{"threadId":"t-l","title":"L","token":"a"},"sendId":"thread-request:a"}`
	now := time.Now().UnixMilli()
	if _, err := st.AppendItem(store.Item{
		ID: "user:0", ThreadID: "t1", Kind: "user_text", Role: "user", Status: "completed",
		Summary: text, Meta: `{"sendId":"thread-request:a"}`, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("append item: %v", err)
	}
	if err := router.persistItemFieldsAndPatch(store.Item{ThreadID: "t1", ID: "user:0", Kind: "user_text"}, store.ItemPartialUpdate{Meta: &stored}); err != nil {
		t.Fatalf("persist and patch: %v", err)
	}
	patches := filterItemEventPatches(emissions.snapshot())
	if len(patches) != 1 || patches[0].Patch.Meta == nil {
		t.Fatalf("patches = %+v", patches)
	}
	want := usermessage.ProjectRowMeta(text, stored)
	if want == stored {
		t.Fatal("the fixture's served meta is its stored meta")
	}
	if got := *patches[0].Patch.Meta; got != want {
		t.Fatalf("patch meta %s, want what a read serves %s", got, want)
	}
}
