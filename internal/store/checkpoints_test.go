package store

import (
	"testing"
	"time"
)

func mustCreateThreadForCheckpoint(t *testing.T, s *Store, id string) {
	t.Helper()
	if err := s.CreateThread(makeThread(id, "claude")); err != nil {
		t.Fatalf("create thread %s: %v", id, err)
	}
}

func TestSaveAndGetCheckpointRoundTrip(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")

	c := Checkpoint{
		ID:            "chk-1",
		ThreadID:      "t1",
		TurnIndex:     0,
		RefName:       "refs/agent-overflow/checkpoints/dDE/turn/0",
		BaselineSHA:   "deadbeef",
		CapturedAt:    time.Now().UnixMilli(),
		WorkspacePath: "/tmp/workspace",
	}
	if err := s.SaveCheckpoint(c); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, ok, err := s.GetCheckpoint("t1", 0)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !ok {
		t.Fatalf("checkpoint should exist")
	}
	if got.ID != c.ID || got.RefName != c.RefName || got.BaselineSHA != c.BaselineSHA {
		t.Errorf("round-trip mismatch: got %+v want %+v", got, c)
	}
}

func TestGetCheckpointMissingReturnsFalse(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")

	_, ok, err := s.GetCheckpoint("t1", 42)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ok {
		t.Errorf("no checkpoint should exist yet")
	}
}

func TestSaveCheckpointRejectsMissingIDs(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")

	cases := []Checkpoint{
		{ID: "", ThreadID: "t1", RefName: "refs/x"},
		{ID: "chk", ThreadID: "", RefName: "refs/x"},
		{ID: "chk", ThreadID: "t1", RefName: ""},
	}
	for i, c := range cases {
		if err := s.SaveCheckpoint(c); err == nil {
			t.Errorf("case %d: expected validation error, got nil for %+v", i, c)
		}
	}
}

func TestSaveCheckpointRejectsDuplicateRefName(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")

	c1 := Checkpoint{
		ID: "chk-1", ThreadID: "t1", TurnIndex: 0,
		RefName: "refs/shared", CapturedAt: 1, WorkspacePath: "/a",
	}
	if err := s.SaveCheckpoint(c1); err != nil {
		t.Fatalf("first save: %v", err)
	}
	c2 := Checkpoint{
		ID: "chk-2", ThreadID: "t1", TurnIndex: 1,
		RefName: "refs/shared", CapturedAt: 2, WorkspacePath: "/a",
	}
	if err := s.SaveCheckpoint(c2); err == nil {
		t.Errorf("expected unique constraint violation for duplicate ref_name")
	}
}

func TestListCheckpointsOrdersByTurnIndex(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")

	// Insert out of order; list should return them sorted ascending.
	for _, turn := range []int{2, 0, 1} {
		c := Checkpoint{
			ID:            "chk-" + string(rune('0'+turn)),
			ThreadID:      "t1",
			TurnIndex:     turn,
			RefName:       "refs/x/" + string(rune('0'+turn)),
			CapturedAt:    int64(turn),
			WorkspacePath: "/tmp",
		}
		if err := s.SaveCheckpoint(c); err != nil {
			t.Fatalf("save turn %d: %v", turn, err)
		}
	}

	list, err := s.ListCheckpoints("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("expected 3 checkpoints, got %d", len(list))
	}
	for i, c := range list {
		if c.TurnIndex != i {
			t.Errorf("index %d: got TurnIndex=%d, want %d", i, c.TurnIndex, i)
		}
	}
}

func TestListCheckpointsScopesToThread(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")
	mustCreateThreadForCheckpoint(t, s, "t2")

	for _, th := range []string{"t1", "t2"} {
		c := Checkpoint{
			ID: "chk-" + th, ThreadID: th, TurnIndex: 0,
			RefName: "refs/" + th, CapturedAt: 1, WorkspacePath: "/tmp",
		}
		if err := s.SaveCheckpoint(c); err != nil {
			t.Fatalf("save %s: %v", th, err)
		}
	}

	list, err := s.ListCheckpoints("t1")
	if err != nil || len(list) != 1 {
		t.Fatalf("expected 1 checkpoint for t1, got %d err=%v", len(list), err)
	}
	if list[0].ThreadID != "t1" {
		t.Errorf("wrong thread: %s", list[0].ThreadID)
	}
}

func TestDeleteCheckpointsForThreadRemovesAllRows(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")
	mustCreateThreadForCheckpoint(t, s, "t2")

	for _, th := range []string{"t1", "t1", "t2"} {
		// three rows: two for t1, one for t2
		c := Checkpoint{
			ID: "chk-" + th + "-" + randID(t), ThreadID: th, TurnIndex: 0,
			RefName: "refs/" + th + "/" + randID(t), CapturedAt: 1, WorkspacePath: "/tmp",
		}
		if err := s.SaveCheckpoint(c); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	if err := s.DeleteCheckpointsForThread("t1"); err != nil {
		t.Fatalf("delete for t1: %v", err)
	}

	t1, _ := s.ListCheckpoints("t1")
	t2, _ := s.ListCheckpoints("t2")
	if len(t1) != 0 {
		t.Errorf("t1 should have no checkpoints after delete, got %d", len(t1))
	}
	if len(t2) != 1 {
		t.Errorf("t2 should retain its checkpoint, got %d", len(t2))
	}
}

func TestDeleteCheckpointsForThreadMissingIsNoop(t *testing.T) {
	s := newTestStore(t)
	if err := s.DeleteCheckpointsForThread("nonexistent"); err != nil {
		t.Errorf("delete for missing thread should be a no-op, got %v", err)
	}
}

func TestThreadDeleteCascadesCheckpoints(t *testing.T) {
	// When the thread is deleted via the store, FK CASCADE should remove
	// the checkpoint rows. This proves the migration wired up ON DELETE
	// CASCADE correctly.
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")

	c := Checkpoint{
		ID: "chk-1", ThreadID: "t1", TurnIndex: 0,
		RefName: "refs/t1/0", CapturedAt: 1, WorkspacePath: "/tmp",
	}
	if err := s.SaveCheckpoint(c); err != nil {
		t.Fatalf("save: %v", err)
	}

	if err := s.DeleteThread("t1"); err != nil {
		t.Fatalf("delete thread: %v", err)
	}

	rows, err := s.db.Query(`SELECT COUNT(*) FROM thread_checkpoints WHERE thread_id = 't1'`)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	defer rows.Close()
	var count int
	if rows.Next() {
		if err := rows.Scan(&count); err != nil {
			t.Fatalf("scan: %v", err)
		}
	}
	if count != 0 {
		t.Errorf("expected FK CASCADE to drop checkpoint rows, still have %d", count)
	}
}

// randID returns a short random-ish id. Not crypto-strong; just unique enough
// within a single test.
var randomCounter int

func randID(t *testing.T) string {
	t.Helper()
	randomCounter++
	return "r" + string(rune('a'+randomCounter%26))
}
