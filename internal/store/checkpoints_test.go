package store

import (
	"fmt"
	"testing"

	"agent-overflow/internal/diffsummary"
)

func TestMessageCheckpointRoundTrip(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")

	want := Checkpoint{
		ID:                    "chk-1",
		ThreadID:              "t1",
		UserItemID:            "t1-user:1",
		TurnIndex:             1,
		ProviderUserMessageID: "provider-user-1",
		ProviderParentUUID:    "parent-1",
		RefName:               "refs/agent-overflow/threads/t1/message/one",
		Status:                "ready",
		Files:                 []diffsummary.File{{Path: "src/foo.go", Kind: "modified", Additions: 2, Deletions: 1}},
		CapturedAt:            123,
		WorkspacePath:         "/workspace",
	}
	if err := s.SaveCheckpoint(want); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	got, ok, err := s.GetCheckpointByUserItemID("t1", "t1-user:1")
	if err != nil {
		t.Fatalf("get checkpoint: %v", err)
	}
	if !ok {
		t.Fatal("checkpoint not found")
	}
	if got.UserItemID != want.UserItemID || got.TurnIndex != want.TurnIndex ||
		got.ProviderUserMessageID != want.ProviderUserMessageID || got.ProviderParentUUID != want.ProviderParentUUID {
		t.Fatalf("checkpoint metadata mismatch: got %+v want %+v", got, want)
	}
	if len(got.Files) != 1 || got.Files[0].Path != "src/foo.go" {
		t.Fatalf("files round-trip mismatch: %+v", got.Files)
	}
}

// TestGetPreviousCheckpointSameTurnTiebreak — same-turn checkpoints
// (a queued flush message sharing its turn with the prompt that was
// running) order by the user item's timeline position, immune to
// captured_at wall-clock ties: the flush checkpoint's "previous" is
// the original prompt's same-turn checkpoint, and the original
// prompt's "previous" skips past it to the prior turn.
func TestGetPreviousCheckpointSameTurnTiebreak(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")
	if err := s.InsertItem(Item{
		ID:        "t1-flush:2",
		ThreadID:  "t1",
		TurnIndex: 2,
		ItemIndex: 1,
		Kind:      "user_text",
		Role:      "user",
		CreatedAt: 4,
	}); err != nil {
		t.Fatalf("insert flush item: %v", err)
	}
	for _, cp := range []struct {
		id, userItemID string
		turnIndex      int
		capturedAt     int64
	}{
		// Identical captured_at across all three: position, not the
		// wall clock, must decide the ordering.
		{"chk-prior", "t1-user:1", 1, 100},
		{"chk-prompt", "t1-user:2", 2, 100},
		{"chk-flush", "t1-flush:2", 2, 100},
	} {
		if err := s.SaveCheckpoint(Checkpoint{
			ID:            cp.id,
			ThreadID:      "t1",
			UserItemID:    cp.userItemID,
			TurnIndex:     cp.turnIndex,
			RefName:       "refs/agent-overflow/threads/t1/message/" + cp.id,
			CapturedAt:    cp.capturedAt,
			WorkspacePath: "/workspace",
		}); err != nil {
			t.Fatalf("save %s: %v", cp.id, err)
		}
	}

	prev, ok, err := s.GetPreviousCheckpoint("t1", 2, 1)
	if err != nil || !ok {
		t.Fatalf("previous of flush: ok=%v err=%v", ok, err)
	}
	if prev.ID != "chk-prompt" {
		t.Errorf("previous of flush = %s, want chk-prompt (same-turn, earlier position)", prev.ID)
	}

	prev, ok, err = s.GetPreviousCheckpoint("t1", 2, 0)
	if err != nil || !ok {
		t.Fatalf("previous of prompt: ok=%v err=%v", ok, err)
	}
	if prev.ID != "chk-prior" {
		t.Errorf("previous of prompt = %s, want chk-prior (prior turn)", prev.ID)
	}

	if _, ok, err := s.GetPreviousCheckpoint("t1", 1, 0); err != nil || ok {
		t.Errorf("previous of first checkpoint should be absent (ok=%v err=%v)", ok, err)
	}
}

func TestUpdateCheckpointProviderIDs(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")
	if err := s.SaveCheckpoint(Checkpoint{
		ID:            "chk-1",
		ThreadID:      "t1",
		UserItemID:    "t1-user:0",
		TurnIndex:     0,
		RefName:       "refs/agent-overflow/threads/t1/message/one",
		CapturedAt:    1,
		WorkspacePath: "/workspace",
	}); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	if err := s.UpdateCheckpointProviderIDs("t1", "t1-user:0", "provider-id", "parent-id"); err != nil {
		t.Fatalf("update provider ids: %v", err)
	}
	got, ok, err := s.GetCheckpointByUserItemID("t1", "t1-user:0")
	if err != nil {
		t.Fatalf("get checkpoint: %v", err)
	}
	if !ok {
		t.Fatal("checkpoint not found")
	}
	if got.ProviderUserMessageID != "provider-id" || got.ProviderParentUUID != "parent-id" {
		t.Fatalf("provider IDs = %q/%q", got.ProviderUserMessageID, got.ProviderParentUUID)
	}
}

// TestDeleteConversationFromTurnScopesAndReturnsCheckpointRefs pins
// the checkpoint half of the combined truncation (round-5, R5-5):
// checkpoints from the cut turn onward are deleted in the same tx as
// the conversation rows, their refs are returned in turn order, and
// other threads' checkpoints are untouched.
func TestDeleteConversationFromTurnScopesAndReturnsCheckpointRefs(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")
	mustCreateThreadForCheckpoint(t, s, "t2")
	for _, row := range []Checkpoint{
		{ID: "t1-0", ThreadID: "t1", UserItemID: "t1-user:0", TurnIndex: 0, RefName: "refs/t1/0", WorkspacePath: "/w1", CapturedAt: 0},
		{ID: "t1-1", ThreadID: "t1", UserItemID: "t1-user:1", TurnIndex: 1, RefName: "refs/t1/1", WorkspacePath: "/w1", CapturedAt: 1},
		{ID: "t1-2", ThreadID: "t1", UserItemID: "t1-user:2", TurnIndex: 2, RefName: "refs/t1/2", WorkspacePath: "/w1", CapturedAt: 2},
		{ID: "t2-2", ThreadID: "t2", UserItemID: "t2-user:2", TurnIndex: 2, RefName: "refs/t2/2", WorkspacePath: "/w2", CapturedAt: 2},
	} {
		if err := s.SaveCheckpoint(row); err != nil {
			t.Fatalf("save %s: %v", row.ID, err)
		}
	}

	refs, deleted, err := s.DeleteConversationFromTurn("t1", 1)
	if err != nil {
		t.Fatalf("delete conversation: %v", err)
	}
	if len(refs) != 2 || refs[0].RefName != "refs/t1/1" || refs[1].RefName != "refs/t1/2" {
		t.Fatalf("refs = %+v", refs)
	}
	if deleted != 2 {
		t.Fatalf("deleted items = %d, want the two cut user rows", deleted)
	}
	list, err := s.ListCheckpoints("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].UserItemID != "t1-user:0" {
		t.Fatalf("remaining checkpoints = %+v", list)
	}
	other, err := s.ListCheckpoints("t2")
	if err != nil {
		t.Fatalf("list other: %v", err)
	}
	if len(other) != 1 {
		t.Fatalf("other thread checkpoint was deleted: %+v", other)
	}
}

// TestDeleteConversationFromTurnCollectsDriftedCheckpointRefs pins
// R6-7 (round 6): a checkpoint whose own turn_index sits BELOW the cut
// but whose item is deleted still dies via the items FK cascade — its
// ref must be in the returned list or the git ref leaks.
func TestDeleteConversationFromTurnCollectsDriftedCheckpointRefs(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")
	if err := s.SaveCheckpoint(Checkpoint{
		ID: "t1-drift", ThreadID: "t1", UserItemID: "t1-user:2",
		TurnIndex: 0, RefName: "refs/t1/drift", WorkspacePath: "/w1", CapturedAt: 0,
	}); err != nil {
		t.Fatalf("save drifted checkpoint: %v", err)
	}

	refs, _, err := s.DeleteConversationFromTurn("t1", 2)
	if err != nil {
		t.Fatalf("delete conversation: %v", err)
	}
	if len(refs) != 1 || refs[0].RefName != "refs/t1/drift" {
		t.Fatalf("refs = %+v, want the drifted checkpoint's ref (its row cascade-deletes with the item)", refs)
	}
	list, err := s.ListCheckpoints("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("remaining checkpoints = %+v, want none (cascade via t1-user:2)", list)
	}
}

// TestUpdateThreadAndRemapProviderIDs pins R6-5 (round 6): the thread
// row, item meta rewrites, and checkpoint provider-id rewrites commit
// in ONE transaction — a failing rewrite rolls back the thread update
// too, so SessionRef can never move without its uuid remap.
func TestUpdateThreadAndRemapProviderIDs(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")
	if err := s.SaveCheckpoint(Checkpoint{
		ID: "t1-0", ThreadID: "t1", UserItemID: "t1-user:0", TurnIndex: 0,
		RefName: "refs/t1/0", WorkspacePath: "/w1", CapturedAt: 0,
		ProviderUserMessageID: "old-uuid", ProviderParentUUID: "old-parent",
	}); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}
	thread, err := s.GetThread("t1")
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}

	thread.SessionRef = "new-session-ref"
	if err := s.UpdateThreadAndRemapProviderIDs(thread,
		[]ItemMetaUpdate{{ItemID: "t1-user:0", Meta: `{"provider_item_id":"new-uuid"}`}},
		[]CheckpointProviderIDsUpdate{{UserItemID: "t1-user:0", ProviderUserMessageID: "new-uuid", ProviderParentUUID: ""}},
	); err != nil {
		t.Fatalf("UpdateThreadAndRemapProviderIDs: %v", err)
	}
	updated, err := s.GetThread("t1")
	if err != nil {
		t.Fatalf("GetThread updated: %v", err)
	}
	if updated.SessionRef != "new-session-ref" {
		t.Fatalf("SessionRef = %q, want the new ref", updated.SessionRef)
	}
	item, found, err := s.GetThreadItem("t1", "t1-user:0")
	if err != nil || !found {
		t.Fatalf("item: found=%v err=%v", found, err)
	}
	if item.Meta != `{"provider_item_id":"new-uuid"}` {
		t.Fatalf("item meta = %q, want the remapped blob", item.Meta)
	}
	cps, err := s.ListCheckpoints("t1")
	if err != nil || len(cps) != 1 {
		t.Fatalf("checkpoints: %+v err=%v", cps, err)
	}
	if cps[0].ProviderUserMessageID != "new-uuid" {
		t.Fatalf("checkpoint provider_user_message_id = %q, want remapped", cps[0].ProviderUserMessageID)
	}
	if cps[0].ProviderParentUUID != "old-parent" {
		t.Fatalf("checkpoint provider_parent_uuid = %q, want empty-preserves to keep the stored value", cps[0].ProviderParentUUID)
	}

	// A failing item rewrite (unknown id) rolls the WHOLE commit back.
	thread.SessionRef = "half-committed-ref"
	err = s.UpdateThreadAndRemapProviderIDs(thread,
		[]ItemMetaUpdate{{ItemID: "no-such-item", Meta: `{}`}}, nil)
	if err == nil {
		t.Fatal("remap against a missing item must error")
	}
	after, err := s.GetThread("t1")
	if err != nil {
		t.Fatalf("GetThread after rollback: %v", err)
	}
	if after.SessionRef != "new-session-ref" {
		t.Fatalf("SessionRef = %q after failed remap, want the previous ref (tx rolled back)", after.SessionRef)
	}
}

func TestDeleteCheckpointsForThreadReturningRefs(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")
	mustCreateThreadForCheckpoint(t, s, "t2")
	for _, row := range []Checkpoint{
		{ID: "t1-0", ThreadID: "t1", UserItemID: "t1-user:0", TurnIndex: 0, RefName: "refs/t1/0", WorkspacePath: "/w1", CapturedAt: 0},
		{ID: "t1-1", ThreadID: "t1", UserItemID: "t1-user:1", TurnIndex: 1, RefName: "refs/t1/1", WorkspacePath: "/w1", CapturedAt: 1},
		{ID: "t2-0", ThreadID: "t2", UserItemID: "t2-user:0", TurnIndex: 0, RefName: "refs/t2/0", WorkspacePath: "/w2", CapturedAt: 2},
	} {
		if err := s.SaveCheckpoint(row); err != nil {
			t.Fatalf("save %s: %v", row.ID, err)
		}
	}

	refs, err := s.DeleteCheckpointsForThreadReturningRefs("t1")
	if err != nil {
		t.Fatalf("DeleteCheckpointsForThreadReturningRefs: %v", err)
	}
	if len(refs) != 2 || refs[0].RefName != "refs/t1/0" || refs[1].RefName != "refs/t1/1" {
		t.Fatalf("refs = %+v, want t1 refs in turn order", refs)
	}
	list, err := s.ListCheckpoints("t1")
	if err != nil {
		t.Fatalf("list t1: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("remaining t1 checkpoints = %+v, want none", list)
	}
	other, err := s.ListCheckpoints("t2")
	if err != nil {
		t.Fatalf("list t2: %v", err)
	}
	if len(other) != 1 {
		t.Fatalf("other thread checkpoint was deleted: %+v", other)
	}
}

func TestUpdateThreadAndDeleteCheckpoints(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")
	if err := s.SaveCheckpoint(Checkpoint{
		ID: "t1-0", ThreadID: "t1", UserItemID: "t1-user:0", TurnIndex: 0,
		RefName: "refs/t1/0", WorkspacePath: "/old", CapturedAt: 0,
	}); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	thread, err := s.GetThread("t1")
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	thread.WorkspacePath = "/new"
	thread.WorktreePath = "/new"
	thread.Branch = "feature/new"
	refs, err := s.UpdateThreadAndDeleteCheckpoints(thread)
	if err != nil {
		t.Fatalf("UpdateThreadAndDeleteCheckpoints: %v", err)
	}
	if len(refs) != 1 || refs[0].RefName != "refs/t1/0" || refs[0].WorkspacePath != "/old" {
		t.Fatalf("refs = %+v, want old checkpoint ref", refs)
	}
	updated, err := s.GetThread("t1")
	if err != nil {
		t.Fatalf("GetThread updated: %v", err)
	}
	if updated.WorkspacePath != "/new" || updated.WorktreePath != "/new" || updated.Branch != "feature/new" {
		t.Fatalf("updated thread workspace = %q/%q/%q", updated.WorkspacePath, updated.WorktreePath, updated.Branch)
	}
	list, err := s.ListCheckpoints("t1")
	if err != nil {
		t.Fatalf("list checkpoints: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("remaining checkpoints = %+v, want none", list)
	}
}

func TestTrackedFilesRoundTrip(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")
	if err := s.UpsertTrackedFiles("t1", 1, []string{"b.go", "a.go", "a.go", ""}); err != nil {
		t.Fatalf("upsert tracked: %v", err)
	}
	if err := s.UpsertTrackedFiles("t1", 2, []string{"c.go", "a.go"}); err != nil {
		t.Fatalf("upsert tracked second turn: %v", err)
	}
	got, err := s.ListTrackedFiles("t1")
	if err != nil {
		t.Fatalf("list tracked: %v", err)
	}
	if len(got) != 3 || got[0] != "a.go" || got[1] != "b.go" || got[2] != "c.go" {
		t.Fatalf("tracked files = %v", got)
	}
	fromTurn, err := s.ListTrackedFilesFromTurn("t1", 2)
	if err != nil {
		t.Fatalf("list tracked from turn: %v", err)
	}
	if len(fromTurn) != 2 || fromTurn[0] != "a.go" || fromTurn[1] != "c.go" {
		t.Fatalf("tracked files from turn = %v", fromTurn)
	}
}

func TestTrackedFilesPreserveWhitespace(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")
	if err := s.UpsertTrackedFiles("t1", 1, []string{"dir/file with trailing space.txt "}); err != nil {
		t.Fatalf("upsert tracked: %v", err)
	}
	got, err := s.ListTrackedFiles("t1")
	if err != nil {
		t.Fatalf("list tracked: %v", err)
	}
	if len(got) != 1 || got[0] != "dir/file with trailing space.txt " {
		t.Fatalf("tracked files = %q, want trailing-space path preserved", got)
	}
}

func TestTrackedFilesRejectUnsafePaths(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")
	for _, p := range []string{"../outside.txt", "/tmp/outside.txt", ":/magic", ".git/config"} {
		if err := s.UpsertTrackedFiles("t1", 1, []string{p}); err == nil {
			t.Fatalf("UpsertTrackedFiles(%q) succeeded, want error", p)
		}
	}
}

// TestListCheckpointsOrdersByItemTimelinePosition pins R9-3 (round 9):
// an echo-time replace gives an earlier sibling's checkpoint a LATER
// captured_at than a later sibling's interrupt baseline, and list
// order is consumed as message order (the UI list, the session-diff
// baseline pick) — so it must follow the linked item's
// (turn_index, item_index), not capture time.
func TestListCheckpointsOrdersByItemTimelinePosition(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")
	if err := s.InsertItem(Item{
		ID: "t1-user:1b", ThreadID: "t1", TurnIndex: 1, ItemIndex: 5,
		Kind: "user_text", Role: "user", CreatedAt: 10,
	}); err != nil {
		t.Fatalf("insert same-turn sibling: %v", err)
	}
	for _, row := range []Checkpoint{
		{ID: "cp-1a", ThreadID: "t1", UserItemID: "t1-user:1", TurnIndex: 1, RefName: "refs/1a", WorkspacePath: "/w", CapturedAt: 900},
		{ID: "cp-1b", ThreadID: "t1", UserItemID: "t1-user:1b", TurnIndex: 1, RefName: "refs/1b", WorkspacePath: "/w", CapturedAt: 100},
		{ID: "cp-0", ThreadID: "t1", UserItemID: "t1-user:0", TurnIndex: 0, RefName: "refs/0", WorkspacePath: "/w", CapturedAt: 500},
	} {
		if err := s.SaveCheckpoint(row); err != nil {
			t.Fatalf("save %s: %v", row.ID, err)
		}
	}

	list, err := s.ListCheckpoints("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := make([]string, len(list))
	for i, cp := range list {
		got[i] = cp.ID
	}
	want := []string{"cp-0", "cp-1a", "cp-1b"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("checkpoint order = %v, want %v (timeline position, not captured_at)", got, want)
	}
}

func mustCreateThreadForCheckpoint(t *testing.T, s *Store, id string) {
	t.Helper()
	if err := s.CreateThread(makeThread(id, "claude")); err != nil {
		t.Fatalf("create thread %s: %v", id, err)
	}
	for turn := 0; turn < 3; turn++ {
		if err := s.InsertItem(Item{
			ID:        id + "-user:" + string(rune('0'+turn)),
			ThreadID:  id,
			TurnIndex: turn,
			ItemIndex: 0,
			Kind:      "user_text",
			Role:      "user",
			CreatedAt: int64(turn + 1),
		}); err != nil {
			t.Fatalf("insert user item turn %d for %s: %v", turn, id, err)
		}
	}
}
