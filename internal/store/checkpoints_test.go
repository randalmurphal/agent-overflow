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
		ToolPaths:     []string{"src/foo.go", "src/bar.go"},
		CapturedAt:    time.Now().UnixMilli(),
		WorkspacePath: "/tmp/workspace",
	}
	if err := s.SaveCheckpoint(c); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, ok, err := s.GetCheckpointByTurnCount("t1", 0)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !ok {
		t.Fatalf("checkpoint should exist")
	}
	if got.ID != c.ID || got.RefName != c.RefName || got.BaselineSHA != c.BaselineSHA {
		t.Errorf("round-trip mismatch: got %+v want %+v", got, c)
	}
	if len(got.ToolPaths) != 2 || got.ToolPaths[0] != "src/foo.go" || got.ToolPaths[1] != "src/bar.go" {
		t.Errorf("ToolPaths round-trip mismatch: got %v", got.ToolPaths)
	}
}

func TestGetCumulativeToolPathsUnionsAcrossPostTargetRows(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")

	rows := []Checkpoint{
		{ID: "chk-0", ThreadID: "t1", TurnIndex: 0, RefName: "r0", ToolPaths: []string{"baseline-only.go"}, CapturedAt: 0, WorkspacePath: "/w"},
		{ID: "chk-1", ThreadID: "t1", TurnIndex: 1, RefName: "r1", ToolPaths: []string{"a.go", "b.go"}, CapturedAt: 1, WorkspacePath: "/w"},
		{ID: "chk-2", ThreadID: "t1", TurnIndex: 2, RefName: "r2", ToolPaths: []string{"b.go", "c.go"}, CapturedAt: 2, WorkspacePath: "/w"},
		{ID: "chk-3", ThreadID: "t1", TurnIndex: 3, RefName: "r3", ToolPaths: []string{}, CapturedAt: 3, WorkspacePath: "/w"},
	}
	for _, c := range rows {
		if err := s.SaveCheckpoint(c); err != nil {
			t.Fatalf("save %s: %v", c.ID, err)
		}
	}

	got, err := s.GetCumulativeToolPaths("t1", 0)
	if err != nil {
		t.Fatalf("cumulative: %v", err)
	}
	want := []string{"a.go", "b.go", "c.go"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("entry %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestGetCumulativeToolPathsRespectsThreadIsolation(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")
	mustCreateThreadForCheckpoint(t, s, "t2")

	if err := s.SaveCheckpoint(Checkpoint{
		ID: "t1-1", ThreadID: "t1", TurnIndex: 1, RefName: "t1-r1",
		ToolPaths: []string{"t1.go"}, CapturedAt: 0, WorkspacePath: "/w",
	}); err != nil {
		t.Fatalf("t1: %v", err)
	}
	if err := s.SaveCheckpoint(Checkpoint{
		ID: "t2-1", ThreadID: "t2", TurnIndex: 1, RefName: "t2-r1",
		ToolPaths: []string{"t2.go"}, CapturedAt: 0, WorkspacePath: "/w",
	}); err != nil {
		t.Fatalf("t2: %v", err)
	}

	got, err := s.GetCumulativeToolPaths("t1", 0)
	if err != nil {
		t.Fatalf("cumulative: %v", err)
	}
	if len(got) != 1 || got[0] != "t1.go" {
		t.Errorf("cross-thread leak: got %v, want [t1.go]", got)
	}
}

func TestGetCumulativeToolPathsExclusiveLowerBound(t *testing.T) {
	// fromTurnCountExclusive=N must include rows where turnCount > N, NOT
	// the row at N itself. Used by RevertToCheckpoint(target=N) to find
	// what was written AFTER target without rolling back target's own
	// agent edits.
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")

	for i, paths := range [][]string{{"turn-0.go"}, {"turn-1.go"}, {"turn-2.go"}} {
		if err := s.SaveCheckpoint(Checkpoint{
			ID: fmt.Sprintf("chk-%d", i), ThreadID: "t1", TurnIndex: i,
			RefName: fmt.Sprintf("r%d", i), ToolPaths: paths,
			CapturedAt: int64(i), WorkspacePath: "/w",
		}); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}

	got, err := s.GetCumulativeToolPaths("t1", 1)
	if err != nil {
		t.Fatalf("cumulative: %v", err)
	}
	want := []string{"turn-2.go"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("exclusive lower bound broken: got %v, want %v", got, want)
	}
}

func TestGetCumulativeToolPathsEmptyResultIsNil(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")

	got, err := s.GetCumulativeToolPaths("t1", 0)
	if err != nil {
		t.Fatalf("cumulative: %v", err)
	}
	if got != nil {
		t.Errorf("empty result should be nil, got %v", got)
	}
}

func TestGetCheckpointMissingReturnsFalse(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")

	_, ok, err := s.GetCheckpointByTurnCount("t1", 42)
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

func TestDeleteCheckpointsAfterTurnScopesAndReturnsRefs(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")
	mustCreateThreadForCheckpoint(t, s, "t2")

	now := time.Now().UnixMilli()
	saveAt := func(thread string, turn int, ref string) {
		if err := s.SaveCheckpoint(Checkpoint{
			ID:            fmt.Sprintf("chk-%s-%d", thread, turn),
			ThreadID:      thread,
			TurnIndex:     turn,
			RefName:       ref,
			BaselineSHA:   "sha",
			CapturedAt:    now,
			WorkspacePath: "/tmp/w",
		}); err != nil {
			t.Fatalf("save %s turn %d: %v", thread, turn, err)
		}
	}
	// t1: turns 0..3. t2: turns 0..2 (must stay untouched).
	for turn := 0; turn < 4; turn++ {
		saveAt("t1", turn, fmt.Sprintf("refs/t1/turn/%d", turn))
	}
	for turn := 0; turn < 3; turn++ {
		saveAt("t2", turn, fmt.Sprintf("refs/t2/turn/%d", turn))
	}

	// Keep through turn 1 on t1 → delete turns 2 and 3.
	refs, err := s.DeleteCheckpointsAfterTurn("t1", 1)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	want := []string{"refs/t1/turn/2", "refs/t1/turn/3"}
	if len(refs) != len(want) {
		t.Fatalf("returned refs: got %v, want %v", refs, want)
	}
	for i := range want {
		if refs[i].RefName != want[i] {
			t.Errorf("refs[%d] = %q, want %q", i, refs[i].RefName, want[i])
		}
		if refs[i].WorkspacePath != "/tmp/w" {
			t.Errorf("refs[%d].WorkspacePath = %q, want /tmp/w", i, refs[i].WorkspacePath)
		}
	}

	t1Remaining, err := s.ListCheckpoints("t1")
	if err != nil {
		t.Fatalf("list t1: %v", err)
	}
	if len(t1Remaining) != 2 {
		t.Errorf("t1 remaining count: got %d, want 2", len(t1Remaining))
	}
	for _, c := range t1Remaining {
		if c.TurnIndex > 1 {
			t.Errorf("t1 still has turn_index=%d after delete", c.TurnIndex)
		}
	}

	t2Remaining, err := s.ListCheckpoints("t2")
	if err != nil {
		t.Fatalf("list t2: %v", err)
	}
	if len(t2Remaining) != 3 {
		t.Errorf("t2 remaining count: got %d, want 3 (unaffected)", len(t2Remaining))
	}
}

func TestDeleteCheckpointsAfterTurnReturnsWorkspacePerRef(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")

	saveAt := func(turn int, workspace string) {
		if err := s.SaveCheckpoint(Checkpoint{
			ID:            fmt.Sprintf("chk-%d", turn),
			ThreadID:      "t1",
			TurnIndex:     turn,
			RefName:       fmt.Sprintf("refs/t1/turn/%d", turn),
			CapturedAt:    int64(turn),
			WorkspacePath: workspace,
		}); err != nil {
			t.Fatalf("save turn %d: %v", turn, err)
		}
	}
	saveAt(0, "/repo/a")
	saveAt(1, "/repo/a")
	saveAt(2, "/repo/b")
	saveAt(3, "/repo/c")

	refs, err := s.DeleteCheckpointsAfterTurn("t1", 1)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	want := []CheckpointRef{
		{RefName: "refs/t1/turn/2", WorkspacePath: "/repo/b"},
		{RefName: "refs/t1/turn/3", WorkspacePath: "/repo/c"},
	}
	if len(refs) != len(want) {
		t.Fatalf("refs = %+v, want %+v", refs, want)
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Errorf("refs[%d] = %+v, want %+v", i, refs[i], want[i])
		}
	}
}

func TestDeleteCheckpointsAfterTurnReturnsEmptyWhenNoneMatch(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t1")
	if err := s.SaveCheckpoint(Checkpoint{
		ID: "chk-1", ThreadID: "t1", TurnIndex: 0,
		RefName: "refs/t1/turn/0", BaselineSHA: "sha",
		CapturedAt: time.Now().UnixMilli(), WorkspacePath: "/tmp",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	refs, err := s.DeleteCheckpointsAfterTurn("t1", 10)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("returned refs: got %v, want empty", refs)
	}
	remaining, _ := s.ListCheckpoints("t1")
	if len(remaining) != 1 {
		t.Errorf("turn 0 should still exist, got %d rows", len(remaining))
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
