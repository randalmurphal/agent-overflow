package store

import (
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

func TestDeleteCheckpointsFromTurnScopesAndReturnsRefs(t *testing.T) {
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

	refs, err := s.DeleteCheckpointsFromTurn("t1", 1)
	if err != nil {
		t.Fatalf("delete checkpoints: %v", err)
	}
	if len(refs) != 2 || refs[0].RefName != "refs/t1/1" || refs[1].RefName != "refs/t1/2" {
		t.Fatalf("refs = %+v", refs)
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

func TestTrackedFilesRejectUnsafePaths(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")
	for _, p := range []string{"../outside.txt", "/tmp/outside.txt", ":/magic", ".git/config"} {
		if err := s.UpsertTrackedFiles("t1", 1, []string{p}); err == nil {
			t.Fatalf("UpsertTrackedFiles(%q) succeeded, want error", p)
		}
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
