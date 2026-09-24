package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// turnErrorOracleSQL is the definition of the pair, stated over the
// timeline_items view and independent of the arm SQL the triggers use.
const turnErrorOracleSQL = `SELECT MAX(created_at), MAX(turn_index) FROM timeline_items
 WHERE thread_id = ?1 AND kind = 'error'
   AND turn_index >= COALESCE((SELECT MAX(turn_index) FROM turns WHERE thread_id = ?1), 0)`

type turnErrorPair struct{ at, turn sql.NullInt64 }

func (p turnErrorPair) String() string {
	show := func(v sql.NullInt64) string {
		if !v.Valid {
			return "NULL"
		}
		return fmt.Sprint(v.Int64)
	}
	return "(" + show(p.at) + ", " + show(p.turn) + ")"
}

// turnErrorMismatches compares each thread's stored pair with the oracle.
func turnErrorMismatches(t *testing.T, db *sql.DB, threads ...string) []string {
	t.Helper()
	var out []string
	for _, thread := range threads {
		var stored, want turnErrorPair
		if err := db.QueryRow(`SELECT newest_turn_error_at, newest_turn_error_turn FROM threads WHERE id = ?`, thread).
			Scan(&stored.at, &stored.turn); err != nil {
			t.Fatalf("read stored pair of %s: %v", thread, err)
		}
		if err := db.QueryRow(turnErrorOracleSQL, thread).Scan(&want.at, &want.turn); err != nil {
			t.Fatalf("read oracle pair of %s: %v", thread, err)
		}
		if stored != want {
			out = append(out, fmt.Sprintf("%s stored %s, want %s", thread, stored, want))
		}
	}
	return out
}

// runTurnErrorScenario drives every write that can change a pair, through
// the store API where one exists and raw SQL for the key updates no caller
// makes today, and calls check after each step.
func runTurnErrorScenario(t *testing.T, s *Store, check func(step string)) {
	t.Helper()
	const local, imported, other = "te-local", "te-import", "te-other"
	mustCreateThread(t, s, local)
	mustCreateThread(t, s, other)
	newImportTargetThread(t, s, imported)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	item := func(id string, turn, index int, kind string, createdAt int64) Item {
		return Item{ID: id, ThreadID: local, TurnIndex: turn, ItemIndex: index, Kind: kind,
			Role: "assistant", Status: "completed", Summary: id, CreatedAt: createdAt, UpdatedAt: createdAt}
	}
	insert := func(it Item) {
		t.Helper()
		if err := s.InsertItem(it); err != nil {
			t.Fatalf("InsertItem(%s): %v", it.ID, err)
		}
	}
	upsert := func(it Item) {
		t.Helper()
		if _, err := s.UpsertItem(it, nil); err != nil {
			t.Fatalf("UpsertItem(%s): %v", it.ID, err)
		}
	}
	turn := func(thread string, index int) {
		t.Helper()
		if err := s.InsertTurn(Turn{TurnID: fmt.Sprintf("%s:%d", thread, index), ThreadID: thread, TurnIndex: index, StartedAt: 1}); err != nil {
			t.Fatalf("InsertTurn(%s, %d): %v", thread, index, err)
		}
	}

	insert(item("e0", 0, 0, "error", 100))
	check("orphan error before any turn")
	turn(local, 1)
	check("a newer turn passes the orphan")
	insert(item("e3", 3, 0, "error", 300))
	insert(item("e1", 1, 0, "error", 400))
	check("errors on the newest turn and past it")
	turn(local, 2)
	check("a newer turn below an orphan error")
	insert(item("e2", 2, 0, "error", 250))
	if err := s.DeleteThreadItem(local, "e3"); err != nil {
		t.Fatal(err)
	}
	check("the newest error deleted")
	insert(item("t2", 2, 1, "assistant_text", 600))
	upsert(item("t2", 2, 1, "error", 600))
	check("a row upserted into an error")
	upsert(item("t2", 2, 1, "assistant_text", 600))
	check("an error upserted into another kind")
	exec(`UPDATE turns SET turn_index = 7 WHERE thread_id = ? AND turn_index = 2`, local)
	check("the newest turn renumbered past the errors")
	exec(`UPDATE turns SET turn_index = 2 WHERE thread_id = ? AND turn_index = 7`, local)
	check("the newest turn renumbered back")
	if _, _, err := s.DeleteConversationFromTurn(local, 2); err != nil {
		t.Fatal(err)
	}
	check("a rollback exposes the previous turn's error")
	exec(`UPDATE items SET thread_id = ? WHERE thread_id = ? AND id = 'e1'`, other, local)
	check("an error row moved to another thread")

	row := func(id string, turn, index int, kind string, createdAt int64) ImportRow {
		return ImportRow{Item: Item{ID: id, TurnIndex: turn, ItemIndex: index, Kind: kind,
			Role: "assistant", Status: "completed", Summary: id, CreatedAt: createdAt, UpdatedAt: createdAt}}
	}
	batch := func(turnIndex int, rows ...ImportRow) {
		t.Helper()
		if err := s.ApplyImportBatch(imported, ImportBatch{
			Turns: []Turn{{TurnID: fmt.Sprintf("%s:%d", imported, turnIndex), ThreadID: imported, TurnIndex: turnIndex, StartedAt: 1}},
			Rows:  rows,
		}); err != nil {
			t.Fatalf("ApplyImportBatch(turn %d): %v", turnIndex, err)
		}
	}
	batch(0, row("imp-a-text", 0, 0, "assistant_text", 990), row("imp-a-err", 0, 1, "error", 1000))
	check("an imported chunk with an error")
	older := item("loc-err", 0, 5, "error", 980)
	older.ThreadID = imported
	insert(older)
	check("an older local error beside the imported one")
	batch(1, row("imp-b-err", 1, 0, "error", 900), row("imp-b-text", 1, 1, "assistant_text", 910))
	check("a second imported turn")
	exec(`INSERT INTO thread_import_item_overrides (thread_id, item_id) VALUES (?, 'imp-b-err')`, imported)
	check("an override hides the imported error")
	exec(`DELETE FROM thread_import_item_overrides WHERE thread_id = ? AND item_id = 'imp-b-err'`, imported)
	check("the override released")
	exec(`INSERT INTO thread_import_item_overrides (thread_id, item_id) VALUES (?, 'imp-b-text')`, imported)
	exec(`UPDATE thread_import_item_overrides SET item_id = 'imp-b-err' WHERE thread_id = ? AND item_id = 'imp-b-text'`, imported)
	check("an override moved onto the imported error")
	exec(`UPDATE thread_import_item_overrides SET item_id = 'imp-b-text' WHERE thread_id = ? AND item_id = 'imp-b-err'`, imported)
	check("the override moved off it")
	var chunkB string
	if err := s.db.QueryRow(`SELECT chunk_id FROM thread_import_chunks WHERE thread_id = ? ORDER BY chunk_order DESC LIMIT 1`, imported).Scan(&chunkB); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE thread_import_chunks SET thread_id = ? WHERE thread_id = ? AND chunk_id = ?`, other, imported, chunkB)
	check("a chunk moved to another thread")
	exec(`UPDATE thread_import_chunks SET thread_id = ? WHERE thread_id = ? AND chunk_id = ?`, imported, other, chunkB)
	check("the chunk moved back")
	exec(`DELETE FROM thread_import_chunks WHERE thread_id = ? AND chunk_id = ?`, imported, chunkB)
	check("the chunk detached and collected")
	batch(2, row("imp-c-err", 2, 0, "error", 950))
	check("a third imported turn")
	if _, _, err := s.DeleteConversationFromTurn(imported, 1); err != nil {
		t.Fatal(err)
	}
	check("a rollback into imported history")
	if err := s.DeleteThread(imported); err != nil {
		t.Fatal(err)
	}
}

// TestThreadTurnErrorAggregateParity keeps the write-time pair equal to its
// definition after every write that can change it.
func TestThreadTurnErrorAggregateParity(t *testing.T) {
	s := newTestStore(t)
	steps := 0
	runTurnErrorScenario(t, s, func(step string) {
		steps++
		for _, mismatch := range turnErrorMismatches(t, s.db, "te-local", "te-import", "te-other") {
			t.Errorf("%s: %s", step, mismatch)
		}
	})
	if steps == 0 {
		t.Fatal("the scenario checked nothing")
	}
}

// TestThreadTurnErrorTriggersAreEachLoadBearing drops one trigger at a time
// and requires the scenario to catch it, so each trigger has a step that
// fails without it and a new trigger joins the list by being installed.
func TestThreadTurnErrorTriggersAreEachLoadBearing(t *testing.T) {
	names := regexp.MustCompile(`CREATE TRIGGER (\w+)`).FindAllStringSubmatch(threadTurnErrorTriggersSQL, -1)
	if len(names) != 12 {
		t.Fatalf("found %d turn-error triggers, want 12", len(names))
	}
	for _, name := range names {
		t.Run(name[1], func(t *testing.T) {
			s := newTestStore(t)
			if _, err := s.db.Exec(`DROP TRIGGER ` + name[1]); err != nil {
				t.Fatal(err)
			}
			caught := ""
			runTurnErrorScenario(t, s, func(step string) {
				if caught == "" {
					if mismatches := turnErrorMismatches(t, s.db, "te-local", "te-import", "te-other"); len(mismatches) > 0 {
						caught = step + ": " + mismatches[0]
					}
				}
			})
			if caught == "" {
				t.Fatalf("the scenario passes without %s", name[1])
			}
			t.Logf("caught at %s", caught)
		})
	}
}

// TestThreadTurnErrorStatementPlans pins every probe the triggers run to
// its index, and the two thread-row reads to no timeline probe at all.
func TestThreadTurnErrorStatementPlans(t *testing.T) {
	s := newTestStore(t)
	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{"recompute", `SELECT MAX(created_at), MAX(turn_index) FROM (` + turnErrorRowsSQL("?1", "") + `)`,
			[]string{"idx_items_thread_error", "idx_import_history_items_error"}},
		{"recompute without a chunk", `SELECT MAX(created_at), MAX(turn_index) FROM (` + turnErrorRowsSQL("?1", "?2") + `)`,
			[]string{"idx_items_thread_error", "idx_import_history_items_error"}},
		{"chunk rows", turnErrorChunkRowsSQL("?1", "?2"), []string{"idx_import_history_items_error"}},
		{"imported row", turnErrorImportedRowSQL("?1", "?2"), []string{"idx_import_history_items_id (id=?)"}},
	}
	for _, tc := range cases {
		plan := explainPlan(t, s, tc.query, "t", "c")
		text := planText(plan)
		for _, r := range plan {
			if strings.HasPrefix(r.detail, "SCAN ") && !strings.HasPrefix(r.detail, "SCAN (subquery") {
				t.Errorf("%s scans: %q\n%s", tc.name, r.detail, text)
			}
		}
		for _, want := range tc.want {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not use %s:\n%s", tc.name, want, text)
			}
		}
	}
	for name, query := range map[string]string{
		"read state": threadReadStateQuery,
		"thread row": `SELECT ` + threadColumns + ` FROM threads WHERE id = ?1`,
	} {
		text := planText(explainPlan(t, s, query, "t"))
		for _, probe := range []string{"idx_items_thread_error", "idx_import_history_items_error", "turn_index>?"} {
			if strings.Contains(text, probe) {
				t.Errorf("%s probes the timeline for turn errors (%s):\n%s", name, probe, text)
			}
		}
	}
}

// seedImportedChunkThread attaches `chunks` chunks to a new imported
// thread, each with one error row and one payload so both chunk-keyed
// tables span many pages, puts the thread's actionable plan in the last
// chunk and opens a turn past every error.
func seedImportedChunkThread(t *testing.T, s *Store, thread string, chunks int) {
	t.Helper()
	newImportTargetThread(t, s, thread)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < chunks; i++ {
		chunk := fmt.Sprintf("%s-chunk-%04d", thread, i)
		for _, stmt := range []struct {
			query string
			args  []any
		}{
			{`INSERT INTO import_history_chunks (id, item_count, min_turn_index, max_turn_index) VALUES (?, 1, 0, 0)`, []any{chunk}},
			{`INSERT INTO import_history_payloads (chunk_id, id, kind, data, created_at) VALUES (?, ?, 'command_output', zeroblob(300), 5)`, []any{chunk, chunk + "-output"}},
			{`INSERT INTO import_history_items (chunk_id, id, turn_index, item_index, kind, role, summary, created_at, updated_at)
			  VALUES (?, ?, 0, ?, 'error', 'assistant', 'failed', 5, 5)`, []any{chunk, chunk + "-err", i}},
			{`INSERT INTO thread_import_chunks (thread_id, chunk_order, chunk_id) VALUES (?, ?, ?)`, []any{thread, i, chunk}},
		} {
			if _, err := tx.Exec(stmt.query, stmt.args...); err != nil {
				t.Fatalf("seed %s: %v", chunk, err)
			}
		}
	}
	last := fmt.Sprintf("%s-chunk-%04d", thread, chunks-1)
	for _, stmt := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO import_history_payloads (chunk_id, id, kind, data, created_at) VALUES (?, 'plan-payload', 'proposed_plan', X'00', 5)`, []any{last}},
		{`INSERT INTO import_history_items (chunk_id, id, turn_index, item_index, kind, role, status, summary, payload_id, created_at, updated_at)
		  VALUES (?, 'plan-item', 0, ?, 'assistant_text', 'assistant', 'completed', 'plan', 'plan-payload', 6, 6)`, []any{last, chunks}},
		{`INSERT INTO proposed_plans (item_id, thread_id, version, created_at, updated_at) VALUES ('plan-item', ?, 1, 6, 6)`, []any{thread}},
		{`INSERT INTO turns (turn_id, thread_id, turn_index, started_at) VALUES (?, ?, 1, 10)`, []any{thread + ":1", thread}},
	} {
		if _, err := tx.Exec(stmt.query, stmt.args...); err != nil {
			t.Fatalf("seed %s plan: %v", thread, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestThreadRowReadDoesNotScaleWithImportedChunks: the thread row of a
// 400-chunk import reads the same pages as a 4-chunk one. Every chunk
// carries an error the newest turn has passed, so a per-read error probe
// would visit each, and the thread's actionable plan is imported, so a
// payload-kind probe that walked the chunk list would too. The negative
// control runs the per-read error probe the pair replaced and requires it
// to scale, so the measurement can see what it guards against.
func TestThreadRowReadDoesNotScaleWithImportedChunks(t *testing.T) {
	s := newTestStore(t)
	seedImportedChunkThread(t, s, "te-small", 4)
	seedImportedChunkThread(t, s, "te-large", 400)
	if mismatches := turnErrorMismatches(t, s.db, "te-small", "te-large"); len(mismatches) > 0 {
		t.Fatalf("fixture pairs: %v", mismatches)
	}
	for _, thread := range []string{"te-small", "te-large"} {
		got, err := s.GetThread(thread)
		if err != nil {
			t.Fatal(err)
		}
		if !got.HasActionableProposedPlan || got.HasFailedTurn {
			t.Fatalf("%s: actionable plan %v, failed %v; want a plan and no failure or the fixture proves nothing",
				thread, got.HasActionableProposedPlan, got.HasFailedTurn)
		}
	}
	measure := func(query string) (small, large int) {
		t.Helper()
		pageAccessesForTest(t, s, query, "te-small")
		pageAccessesForTest(t, s, query, "te-large")
		return pageAccessesForTest(t, s, query, "te-small"), pageAccessesForTest(t, s, query, "te-large")
	}
	for name, query := range map[string]string{
		"thread row": `SELECT ` + threadColumns + ` FROM threads WHERE id = ?`,
		"read state": threadReadStateQuery,
	} {
		if small, large := measure(query); large > small+2 {
			t.Errorf("%s reads %d pages at 400 chunks and %d at 4", name, large, small)
		}
	}
	perReadProbe := `SELECT EXISTS (SELECT 1 FROM timeline_items AS errors
	   WHERE errors.thread_id = threads.id AND errors.kind = 'error'
	     AND errors.turn_index >= COALESCE((SELECT MAX(turn_index) FROM turns WHERE thread_id = threads.id), 0))
	  FROM threads WHERE id = ?`
	if small, large := measure(perReadProbe); large < small+400 {
		t.Fatalf("the per-read probe reads %d pages at 400 chunks and %d at 4; the measurement cannot see a per-chunk probe", large, small)
	}
}

// TestMigrationV121BackfillsThreadTurnErrors checks the inline backfill
// against the definition on rows v121 finds: an orphan error, an error a
// newer turn passed, one on the newest turn, and an imported one.
func TestMigrationV121BackfillsThreadTurnErrors(t *testing.T) {
	db := migrateThrough(t, 115)
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	for _, id := range []string{"t-orphan", "t-passed", "t-newest", "t-imported", "t-clean"} {
		mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode) VALUES ('`+id+`', 'p', 'T', 'claude', '/tmp', '', 1, 1, 0, 'chat')`)
	}
	row := func(thread, id string, turn, index int, kind string, createdAt int) {
		mustExec(t, db, `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
			summary, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 'assistant', 'completed', ?, ?, ?)`, id, thread, turn, index, kind, id, createdAt, createdAt)
	}
	turn := func(thread string, index int) {
		mustExec(t, db, `INSERT INTO turns (turn_id, thread_id, turn_index, started_at) VALUES (?, ?, ?, 1)`,
			fmt.Sprintf("%s:%d", thread, index), thread, index)
	}
	row("t-orphan", "o", 0, 0, "error", 10)
	turn("t-passed", 0)
	row("t-passed", "p0", 0, 0, "error", 20)
	turn("t-passed", 1)
	turn("t-newest", 0)
	turn("t-newest", 1)
	row("t-newest", "n0", 0, 0, "error", 50)
	row("t-newest", "n1", 1, 0, "error", 30)
	row("t-newest", "n1b", 1, 1, "error", 40)
	turn("t-imported", 0)
	mustExec(t, db, `INSERT INTO import_history_chunks (id, item_count, min_turn_index, max_turn_index) VALUES ('c', 1, 0, 0)`)
	mustExec(t, db, `INSERT INTO import_history_items (chunk_id, id, turn_index, item_index, kind, role, summary, created_at, updated_at)
		VALUES ('c', 'i0', 0, 0, 'error', 'assistant', 'failed', 60, 60)`)
	mustExec(t, db, `INSERT INTO thread_import_chunks (thread_id, chunk_order, chunk_id) VALUES ('t-imported', 0, 'c')`)
	row("t-clean", "text", 0, 0, "assistant_text", 70)

	migrateFrom(t, db, 115)

	threads := []string{"t-orphan", "t-passed", "t-newest", "t-imported", "t-clean"}
	for _, mismatch := range turnErrorMismatches(t, db, threads...) {
		t.Error(mismatch)
	}
	var lit int
	if err := db.QueryRow(`SELECT count(*) FROM threads WHERE newest_turn_error_at IS NOT NULL`).Scan(&lit); err != nil || lit != 3 {
		t.Errorf("threads with a turn error = %d (%v), want 3: the fixture would prove nothing", lit, err)
	}
	var triggers int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'trigger' AND name LIKE '%turn_error%'`).Scan(&triggers); err != nil || triggers != 12 {
		t.Errorf("turn-error triggers installed = %d (%v), want 12", triggers, err)
	}
}

// TestRestoreFromKeepsTurnErrorPairs restores a snapshot taken with one
// error and requires the snapshot's pair back, then a live trigger: an
// error written after the restore must still move the pair.
func TestRestoreFromKeepsTurnErrorPairs(t *testing.T) {
	st := snapshotTestStore(t)
	seedSnapshotFixture(t, st, "t1", "x")
	insertError := func(id string, createdAt int64) {
		t.Helper()
		if err := st.InsertItem(Item{ID: id, ThreadID: "t1", TurnIndex: 1, ItemIndex: int(createdAt), Kind: "error",
			Role: "assistant", Status: "completed", Summary: id, CreatedAt: createdAt, UpdatedAt: createdAt}); err != nil {
			t.Fatalf("InsertItem(%s): %v", id, err)
		}
	}
	insertError("before-snapshot", 10)
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := st.SnapshotTo(snap); err != nil {
		t.Fatalf("SnapshotTo: %v", err)
	}
	insertError("after-snapshot", 20)
	if _, err := st.RestoreFrom(snap); err != nil {
		t.Fatalf("RestoreFrom: %v", err)
	}
	var at sql.NullInt64
	read := func() int64 {
		t.Helper()
		if err := st.db.QueryRow(`SELECT newest_turn_error_at FROM threads WHERE id = 't1'`).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at.Int64
	}
	if got := read(); got != 10 {
		t.Fatalf("restored newest_turn_error_at = %v, want the snapshot's 10", at)
	}
	insertError("after-restore", 30)
	if got := read(); got != 30 {
		t.Fatalf("newest_turn_error_at after a post-restore error = %v, want 30: the triggers are not live", at)
	}
	for _, mismatch := range turnErrorMismatches(t, st.db, "t1") {
		t.Error(mismatch)
	}
}
