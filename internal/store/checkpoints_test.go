package store

import (
	"fmt"
	"sync"
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

// TestSaveCheckpointRejectsDuplicateThreadTurn verifies migration v8's
// UNIQUE(thread_id, turn_index). Two capture attempts for the same
// (thread, turn) — even with different ref names — must not be allowed to
// coexist. Adversarial guard: a racing capture should get a clean error,
// not a silent duplicate row.
func TestSaveCheckpointRejectsDuplicateThreadTurn(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")

	c1 := Checkpoint{
		ID: "chk-1", ThreadID: "t1", TurnIndex: 0,
		RefName: "refs/a", CapturedAt: 1, WorkspacePath: "/a",
	}
	if err := s.SaveCheckpoint(c1); err != nil {
		t.Fatalf("first save: %v", err)
	}
	c2 := Checkpoint{
		ID: "chk-2", ThreadID: "t1", TurnIndex: 0,
		RefName: "refs/b", CapturedAt: 2, WorkspacePath: "/a",
	}
	if err := s.SaveCheckpoint(c2); err == nil {
		t.Errorf("expected unique constraint violation for duplicate (thread_id, turn_index)")
	}
}

// TestSaveCheckpointAllowsSameRefAcrossThreads documents that ref_name is
// no longer globally unique after migration v8: the only uniqueness we
// enforce is (thread_id, turn_index). In practice refs are path-unique via
// the thread id embedded in the ref path, but the DB should not be the
// thing blocking that.
func TestSaveCheckpointAllowsSameRefAcrossThreads(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")
	mustCreateThreadForCheckpoint(t, s, "t2")

	c1 := Checkpoint{
		ID: "chk-1", ThreadID: "t1", TurnIndex: 0,
		RefName: "refs/agent-overflow/shared", CapturedAt: 1, WorkspacePath: "/a",
	}
	if err := s.SaveCheckpoint(c1); err != nil {
		t.Fatalf("save t1: %v", err)
	}
	c2 := Checkpoint{
		ID: "chk-2", ThreadID: "t2", TurnIndex: 0,
		RefName: "refs/agent-overflow/shared", CapturedAt: 1, WorkspacePath: "/a",
	}
	if err := s.SaveCheckpoint(c2); err != nil {
		t.Errorf("second save with same ref_name but different thread should succeed after v8: %v", err)
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

	// Three rows across two threads; each (thread, turn) pair unique so the
	// v8 composite UNIQUE is respected.
	seeds := []struct {
		id, thread string
		turn       int
	}{
		{"chk-t1-0", "t1", 0},
		{"chk-t1-1", "t1", 1},
		{"chk-t2-0", "t2", 0},
	}
	for _, sd := range seeds {
		c := Checkpoint{
			ID: sd.id, ThreadID: sd.thread, TurnIndex: sd.turn,
			RefName: "refs/" + sd.id, CapturedAt: 1, WorkspacePath: "/tmp",
		}
		if err := s.SaveCheckpoint(c); err != nil {
			t.Fatalf("save %s: %v", sd.id, err)
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

// TestMigrationV8CollapsesDuplicateThreadTurns seeds a v7-shaped table with
// three rows sharing (thread_id, turn_index) and asserts migration v8 keeps
// only the most recent captured_at. Guards against a bug where the rebuild
// silently drops all duplicates or keeps the wrong one.
func TestMigrationV8CollapsesDuplicateThreadTurns(t *testing.T) {
	db := openSQLiteDB(t)

	// Apply migrations 1..7 manually so we can seed duplicate rows BEFORE
	// v8 enforces the new uniqueness.
	if err := ensureMigrationTable(db); err != nil {
		t.Fatalf("ensure migration table: %v", err)
	}
	for _, m := range migrations {
		if m.Version == 8 {
			break
		}
		if _, err := db.Exec(m.SQL); err != nil {
			t.Fatalf("apply v%d: %v", m.Version, err)
		}
		if _, err := db.Exec(
			"INSERT INTO migration_versions (version, name) VALUES (?, ?)",
			m.Version, m.Name,
		); err != nil {
			t.Fatalf("record v%d: %v", m.Version, err)
		}
	}

	if _, err := db.Exec(`INSERT INTO threads (id, title, provider, workspace_path, model, created_at, updated_at)
		VALUES ('t1', 'T', 'claude', '/tmp', '', 1000, 1000)`); err != nil {
		t.Fatalf("seed thread: %v", err)
	}

	// Three captures for the same (thread, turn) — valid under v7 since
	// only ref_name was UNIQUE.
	seeds := []struct {
		id         string
		ref        string
		capturedAt int64
	}{
		{"chk-early", "refs/a", 100},
		{"chk-mid", "refs/b", 200},
		{"chk-latest", "refs/c", 300},
	}
	for _, s := range seeds {
		if _, err := db.Exec(`INSERT INTO thread_checkpoints
			(id, thread_id, turn_index, ref_name, baseline_sha, captured_at, workspace_path)
			VALUES (?, 't1', 0, ?, '', ?, '/tmp')`, s.id, s.ref, s.capturedAt); err != nil {
			t.Fatalf("seed %s: %v", s.id, err)
		}
	}

	if err := applyMigration(db, migrations[7]); err != nil {
		t.Fatalf("apply v8: %v", err)
	}

	rows, err := db.Query(`SELECT id, ref_name, captured_at FROM thread_checkpoints WHERE thread_id = 't1' AND turn_index = 0`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	var kept []struct {
		id  string
		ref string
		at  int64
	}
	for rows.Next() {
		var r struct {
			id  string
			ref string
			at  int64
		}
		if err := rows.Scan(&r.id, &r.ref, &r.at); err != nil {
			t.Fatalf("scan: %v", err)
		}
		kept = append(kept, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	if len(kept) != 1 {
		t.Fatalf("expected exactly 1 row after v8 migration, got %d: %+v", len(kept), kept)
	}
	if kept[0].id != "chk-latest" {
		t.Errorf("expected most recent row kept, got id=%q", kept[0].id)
	}
	if kept[0].at != 300 {
		t.Errorf("expected captured_at=300 (the latest), got %d", kept[0].at)
	}
}

// TestSaveCheckpointConcurrentSameThreadTurn hammers SaveCheckpoint from
// many goroutines against the same (thread, turn). Exactly one row must
// end up persisted — the losers must return an error, not corrupt state.
// The store's SetMaxOpenConns(1) serialises writes, but we still exercise
// the race to prove the UNIQUE constraint is the gate and that losers
// fail cleanly instead of panicking.
func TestSaveCheckpointConcurrentSameThreadTurn(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")

	var (
		wg         sync.WaitGroup
		successMu  sync.Mutex
		successful int
		failures   int
	)
	const writers = 16
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			c := Checkpoint{
				ID:            fmt.Sprintf("chk-%d", n),
				ThreadID:      "t1",
				TurnIndex:     0,
				RefName:       fmt.Sprintf("refs/a/%d", n),
				CapturedAt:    int64(n + 1),
				WorkspacePath: "/tmp",
			}
			err := s.SaveCheckpoint(c)
			successMu.Lock()
			defer successMu.Unlock()
			if err == nil {
				successful++
			} else {
				failures++
			}
		}(i)
	}
	wg.Wait()

	if successful != 1 {
		t.Errorf("expected exactly 1 SaveCheckpoint to succeed, got %d", successful)
	}
	if failures != writers-1 {
		t.Errorf("expected %d to fail with UNIQUE violation, got %d", writers-1, failures)
	}

	// DB must hold exactly one row.
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM thread_checkpoints`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 row after concurrent saves, got %d", count)
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
