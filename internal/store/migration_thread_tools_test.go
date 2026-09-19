package store

import (
	"database/sql"
	"strings"
	"testing"
)

// threadToolsFixtureModes is every mode a v100 database can hold. The v101
// rebuild copies the table, so a mode missing from the new CHECK would fail
// the copy rather than the test's own assertions.
var threadToolsFixtureModes = []string{
	"chat", "plan", "discussion", "terminal", "workflow", "workflow-studio", "workflow-triage",
}

// seedThreadToolsV100Fixture populates a v100 database with one thread per
// mode and an imported session attached to the first of them, written with
// SQL because store accessors are written against the current schema.
func seedThreadToolsV100Fixture(t *testing.T, db *sql.DB) {
	t.Helper()
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at)
		VALUES ('p-tools', '/tmp/tools', 'Tools', 1, 1)`)
	for i, mode := range threadToolsFixtureModes {
		mustExec(t, db, `
			INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
				created_at, updated_at, archived, mode, import_source)
			VALUES (?, 'p-tools', ?, 'claude', '/tmp/tools', 'test-model', ?, ?, 0, ?, ?)`,
			"t-"+mode, "Thread "+mode, int64(i+1), int64(i+1), mode, importSourceFor(i))
	}
	mustExec(t, db, `INSERT INTO import_history_chunks (id, item_count, min_turn_index, max_turn_index)
		VALUES ('chunk-tools', 2, 0, 0)`)
	mustExec(t, db, `
		INSERT INTO import_history_items (chunk_id, id, turn_index, item_index, kind, role,
			status, summary, created_at, updated_at)
		VALUES ('chunk-tools', 'imported-user', 0, 0, 'user_text', 'user', 'completed',
			'imported question about migrations', 1, 1)`)
	mustExec(t, db, `
		INSERT INTO import_history_items (chunk_id, id, turn_index, item_index, kind, role,
			status, summary, created_at, updated_at)
		VALUES ('chunk-tools', 'imported-answer', 0, 1, 'assistant_text', 'assistant', 'completed',
			'imported answer about migrations', 1, 1)`)
	mustExec(t, db, `INSERT INTO thread_import_chunks (thread_id, chunk_order, chunk_id)
		VALUES ('t-chat', 0, 'chunk-tools')`)
	mustExec(t, db, `
		INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
			summary, meta, created_at, updated_at)
		VALUES ('local-user', 't-plan', 0, 0, 'user_text', 'user', 'completed',
			'local question about migrations', '{}', 1, 1)`)
}

// importSourceFor marks only the thread that carries imported history, so the
// fixture also proves the rebuild preserves an empty import_source.
func importSourceFor(index int) string {
	if index == 0 {
		return "claude"
	}
	return ""
}

// The thread tools chain rebuilds `threads` (v101) and adds four tables. A
// rebuild that lost a column, index, trigger or view would take the whole
// database with it, so this drives the real chain over a populated v100.
func TestThreadToolsMigrationsPreservePopulatedDatabase(t *testing.T) {
	db := migrateThrough(t, 100)
	seedThreadToolsV100Fixture(t, db)
	migrateFrom(t, db, 100)

	for _, mode := range threadToolsFixtureModes {
		var stored string
		if err := db.QueryRow(`SELECT mode FROM threads WHERE id = ?`, "t-"+mode).Scan(&stored); err != nil {
			t.Fatalf("read thread mode %s: %v", mode, err)
		}
		if stored != mode {
			t.Errorf("thread t-%s mode = %q, want %q", mode, stored, mode)
		}
	}

	var importSource string
	if err := db.QueryRow(`SELECT import_source FROM threads WHERE id = 't-chat'`).Scan(&importSource); err != nil {
		t.Fatalf("read import source: %v", err)
	}
	if importSource != "claude" {
		t.Errorf("import_source = %q, want claude", importSource)
	}

	// The imported session still reads through the logical timeline, which
	// means the rebuild kept the chunk attachment and its foreign keys.
	var imported int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM timeline_items WHERE thread_id = 't-chat'`).Scan(&imported); err != nil {
		t.Fatalf("count imported timeline rows: %v", err)
	}
	if imported != 2 {
		t.Errorf("imported timeline rows = %d, want 2", imported)
	}

	if _, err := db.Exec(`UPDATE threads SET mode = 'scratch' WHERE id = 't-plan'`); err != nil {
		t.Fatalf("scratch mode must be accepted after v101: %v", err)
	}
	if _, err := db.Exec(`UPDATE threads SET mode = 'scratchy' WHERE id = 't-plan'`); err == nil {
		t.Fatal("an unknown mode must still violate the CHECK")
	}

	for _, table := range []string{
		"scratch_threads", "thread_requests", "thread_request_receipts",
		"thread_search_rows", "thread_search", "thread_search_build",
	} {
		var exists int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, table).Scan(&exists); err != nil {
			t.Fatalf("probe %s: %v", table, err)
		}
		if exists == 0 {
			t.Errorf("table %s missing after migration", table)
		}
	}

	var building int
	if err := db.QueryRow(`SELECT COUNT(*) FROM thread_search_build WHERE id = 1`).Scan(&building); err != nil {
		t.Fatalf("read build progress: %v", err)
	}
	if building != 1 {
		t.Errorf("thread_search_build rows = %d, want 1", building)
	}

	// The rebuild drops and recreates these; a missing one would only show
	// up much later as a lost history stamp or an unenforceable override.
	for _, object := range []string{
		"trg_items_rev_insert", "trg_items_rev_update", "trg_items_rev_delete",
		"trg_items_require_import_override", "owned_threads",
		"idx_threads_project", "idx_threads_updated",
	} {
		var exists int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, object).Scan(&exists); err != nil {
			t.Fatalf("probe %s: %v", object, err)
		}
		if exists == 0 {
			t.Errorf("schema object %s missing after the threads rebuild", object)
		}
	}

	var owned int
	if err := db.QueryRow(`SELECT COUNT(*) FROM owned_threads`).Scan(&owned); err != nil {
		t.Fatalf("read owned_threads: %v", err)
	}
	if owned != len(threadToolsFixtureModes) {
		t.Errorf("owned_threads rows = %d, want %d", owned, len(threadToolsFixtureModes))
	}

	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign key check: %v", err)
	}
	defer rows.Close()
	var violations []string
	for rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fkid int
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			t.Fatalf("scan foreign key violation: %v", err)
		}
		violations = append(violations, table+" -> "+parent)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate foreign key violations: %v", err)
	}
	if len(violations) > 0 {
		t.Errorf("foreign key violations after migration: %s", strings.Join(violations, ", "))
	}
}

// The scratch mode exists so an ephemeral fork is invisible everywhere a
// hidden mode is, and the return mode is recorded beside it because nothing
// else remembers what the thread was forked out of.
func TestScratchThreadPromoteRestoresTheRecordedMode(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("t-scratch", "claude")
	thread.Mode = "scratch"
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("create scratch thread: %v", err)
	}
	if err := s.InsertScratchThread(ScratchThread{
		ThreadID:       "t-scratch",
		SourceThreadID: "t-source",
		ReturnMode:     "plan",
		CreatedAt:      5,
		RequestToken:   "tok-scratch",
	}); err != nil {
		t.Fatalf("insert scratch record: %v", err)
	}

	row, found, err := s.GetScratchThread("t-scratch")
	if err != nil || !found {
		t.Fatalf("get scratch record: found=%v err=%v", found, err)
	}
	if row.SourceThreadID != "t-source" || row.ReturnMode != "plan" || row.RequestToken != "tok-scratch" {
		t.Fatalf("scratch record = %+v", row)
	}
	listed, err := s.ListScratchThreads()
	if err != nil {
		t.Fatalf("list scratch records: %v", err)
	}
	if len(listed) != 1 || listed[0].ThreadID != "t-scratch" {
		t.Fatalf("listed scratch records = %+v", listed)
	}

	promoted, err := s.PromoteScratchThread("t-scratch")
	if err != nil {
		t.Fatalf("promote scratch thread: %v", err)
	}
	if promoted.Mode != "plan" {
		t.Errorf("promoted mode = %q, want plan", promoted.Mode)
	}
	if _, found, err := s.GetScratchThread("t-scratch"); err != nil || found {
		t.Errorf("scratch record survived promotion: found=%v err=%v", found, err)
	}
	if _, err := s.PromoteScratchThread("t-scratch"); err == nil {
		t.Error("promoting a promoted thread must fail")
	}

	// Deleting the thread takes the record with it; nothing else does.
	second := makeThread("t-scratch-2", "claude")
	second.Mode = "scratch"
	if err := s.CreateThread(second); err != nil {
		t.Fatalf("create second scratch thread: %v", err)
	}
	if err := s.InsertScratchThread(ScratchThread{
		ThreadID: "t-scratch-2", SourceThreadID: "t-source", ReturnMode: "chat",
	}); err != nil {
		t.Fatalf("insert second scratch record: %v", err)
	}
	if err := s.DeleteThread("t-scratch-2"); err != nil {
		t.Fatalf("delete scratch thread: %v", err)
	}
	if _, found, err := s.GetScratchThread("t-scratch-2"); err != nil || found {
		t.Errorf("scratch record outlived its thread: found=%v err=%v", found, err)
	}
}

// A scratch thread is hidden from the project thread list exactly as the
// workflow modes are: the same clause answers for all of them.
func TestScratchThreadIsHiddenFromThreadLists(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-visible")
	scratch := makeThread("t-hidden-scratch", "claude")
	scratch.Mode = "scratch"
	if err := s.CreateThread(scratch); err != nil {
		t.Fatalf("create scratch thread: %v", err)
	}
	threads, err := s.ListThreadsByProject(defaultTestProjectID)
	if err != nil {
		t.Fatalf("list threads: %v", err)
	}
	for _, thread := range threads {
		if thread.ID == "t-hidden-scratch" {
			t.Fatal("a scratch thread must not appear in the sidebar list")
		}
	}
}
