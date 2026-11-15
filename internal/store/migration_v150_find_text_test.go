package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// findTextBulkRows is how many rows the v149 fixture gives "bulk", more
// than two backfill batches.
const findTextBulkRows = 2*findTextBackfillBatch + 100

// findTextAtV149 opens a store upgraded to v150 from a v149 database. Thread
// T holds a markdown row, a tool call whose command is in its meta, a
// thinking row whose payload holds more than its summary, a plain row and
// a tool call whose command is in its payload's meta;
// "fork" is a pointer fork that shows T's rows; "bulk" holds
// findTextBulkRows local rows and "imp" as many imported rows.
func findTextAtV149(t *testing.T) *Store {
	t.Helper()
	s := openStoreAt(t)
	mustCreateThread(t, s, "T")
	if err := seedPayloadRow(s, "T", Payload{ID: "pay-think", Kind: "thinking", Meta: "{}", Data: []byte("the first words of a long thought"), CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := seedPayloadRow(s, "T", Payload{ID: "pay-cmd", Kind: "command_output", Meta: `{"command":"make payload-meta-cmd"}`, Data: []byte("out"), CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	for _, item := range []Item{
		{ID: "md", ItemIndex: 0, Kind: "assistant_text", Summary: "Read [docs](https://example.com/x)suffix and **foo**bar."},
		{ID: "tool", ItemIndex: 1, Kind: "tool_call", ToolName: "Bash", Summary: "Bash", Meta: `{"input":{"command":"grep -rn needle-in-meta src/"}}`},
		{ID: "think", ItemIndex: 2, Kind: "thinking", Summary: "long thought", PayloadID: "pay-think"},
		{ID: "plain", ItemIndex: 3, Kind: "assistant_text", Summary: "nothing to see"},
		{ID: "cmd", ItemIndex: 4, Kind: "tool_call", ToolName: "Bash", Summary: "Bash", PayloadID: "pay-cmd"},
	} {
		item.ThreadID, item.Role, item.Status, item.CreatedAt, item.UpdatedAt = "T", "assistant", "completed", 1, 1
		if err := insertCarded(s, item); err != nil {
			t.Fatalf("seed %s: %v", item.ID, err)
		}
	}
	mustPointerFork(t, s, "T", "fork", ForkCut{})
	mustCreateThread(t, s, "bulk")
	mustExec(t, s.db, `WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i + 1 < ?)
		INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status, summary, meta, created_at, updated_at)
		SELECT 'b' || i, 'bulk', i, 0, 'assistant_text', 'assistant', 'completed', 'bulk row ' || i, '{}', 1, 1 FROM n`,
		findTextBulkRows)
	importedHistoryFixture(t, s, "imp", findTextBulkRows)

	path := s.path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	downgradeSchema(t, db, migrateThrough(t, 149), 149)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close upgraded store: %v", err)
		}
	})
	return s
}

func findIDs(t *testing.T, s *Store, threadID, query string) string {
	t.Helper()
	answer, err := s.SearchThreadItems(context.Background(), threadID, query, TimelineSelection{}, 0, nil)
	if err != nil {
		t.Fatalf("find %q in %s: %v", query, threadID, err)
	}
	return findHitIDs(answer)
}

func indexExists(t *testing.T, s *Store, name string) bool {
	t.Helper()
	return countRows(t, s, `SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, name) == 1
}

// staleFindTextRows counts the rows of each table whose stored candidate
// text is not the one their content gives.
func staleFindTextRows(t *testing.T, s *Store) int {
	t.Helper()
	stale := 0
	for _, table := range findTextTables {
		stale += countRows(t, s, `SELECT count(*) FROM `+table.name+` AS items WHERE items.find_text IS NOT `+table.text)
	}
	return stale
}

// TestMigrationV150FindsWhileItsBackfillRuns: the upgrade leaves every
// row's candidate text unset and returns at once; find answers from each
// row's content until the deferred phase fills it, which it does a batch at
// a time without moving any thread's history stamp, then drops the
// pending indexes.
func TestMigrationV150FindsWhileItsBackfillRuns(t *testing.T) {
	s := findTextAtV149(t)
	for _, table := range findTextTables {
		if n := countRows(t, s, `SELECT count(*) FROM `+table.name+` WHERE find_text IS NOT NULL`); n != 0 {
			t.Fatalf("the upgrade filled %d %s rows itself", n, table.name)
		}
	}
	if !deferredPending(t, s) || !indexExists(t, s, "idx_items_find_text_pending") || !indexExists(t, s, "idx_import_history_items_find_text_pending") {
		t.Fatal("the upgrade left no backfill pending")
	}
	if !indexExists(t, s, "idx_items_thread_rev") {
		t.Fatal("the upgrade made no revision index")
	}

	// An unfilled row whose candidate text reads its payload is a
	// candidate for every query: the fallback does not read payloads.
	during := map[string]string{
		"docs suffix":      "[md think cmd]",
		"foobar":           "[md think cmd]",
		"needle-in-meta":   "[tool think cmd]",
		"first words":      "[think cmd]",
		"nothing":          "[think plain cmd]",
		"absent":           "[think cmd]",
		"payload-meta-cmd": "[think cmd]",
	}
	for query, want := range during {
		if got := findIDs(t, s, "T", query); got != want {
			t.Errorf("during the backfill %q found %s, want %s", query, got, want)
		}
	}
	if got := findIDs(t, s, "fork", "foobar"); got != "[md think cmd]" {
		t.Errorf("during the backfill the fork found %s, want [md think cmd]", got)
	}

	stamps := map[string]HistoryStamp{}
	for _, id := range []string{"T", "fork", "bulk", "imp"} {
		stamps[id] = historyStampOf(t, s, id)
	}
	pauses := 0
	if err := s.RunDeferredMigrations(context.Background(), DeferredHost{Pause: func() { pauses++ }}); err != nil {
		t.Fatal(err)
	}
	if deferredPending(t, s) || deferredFailureOf(t, s) != nil {
		t.Fatalf("the backfill left watermark %d, failure %+v", deferredWatermarkOf(t, s), deferredFailureOf(t, s))
	}
	if pauses < 4 {
		t.Errorf("the backfill paused %d times over two tables of three batches each", pauses)
	}
	for _, table := range findTextTables {
		if n := countRows(t, s, `SELECT count(*) FROM `+table.name+` WHERE find_text IS NULL`); n != 0 {
			t.Errorf("the backfill left %d %s rows unfilled", n, table.name)
		}
	}
	if n := staleFindTextRows(t, s); n != 0 {
		t.Errorf("the backfill wrote %d rows a candidate text their content does not give", n)
	}
	if indexExists(t, s, "idx_items_find_text_pending") || indexExists(t, s, "idx_import_history_items_find_text_pending") {
		t.Error("the backfill kept its pending indexes")
	}
	for id, stamp := range stamps {
		if got := historyStampOf(t, s, id); got != stamp {
			t.Errorf("the backfill moved %s's history stamp from %+v to %+v", id, stamp, got)
		}
	}

	after := map[string]string{
		"docs suffix":      "[md]",
		"foobar":           "[md]",
		"needle-in-meta":   "[tool]",
		"first words":      "[think]",
		"nothing":          "[plain]",
		"absent":           "[]",
		"payload-meta-cmd": "[cmd]",
	}
	for query, want := range after {
		if got := findIDs(t, s, "T", query); got != want {
			t.Errorf("after the backfill %q found %s, want %s", query, got, want)
		}
	}
	if got := findIDs(t, s, "fork", "first words"); got != "[think]" {
		t.Errorf("after the backfill the fork found %s, want [think]", got)
	}
	if got := strings.Count(findIDs(t, s, "imp", "inherited"), "item-"); got != findTextBulkRows {
		t.Errorf("after the backfill the imported thread found %d rows, want %d", got, findTextBulkRows)
	}
	if got := strings.Count(findIDs(t, s, "bulk", "bulk row"), "b"); got != findTextBulkRows {
		t.Errorf("after the backfill the bulk thread found %d rows, want %d", got, findTextBulkRows)
	}
}

// TestMigrationV150BackfillIsReRunnable: a run over filled rows, as after a
// quit or a restore of a copy whose backfill was done, writes nothing and
// leaves no pending index behind.
func TestMigrationV150BackfillIsReRunnable(t *testing.T) {
	s := findTextAtV149(t)
	run := &deferredRun{}
	if err := backfillFindText(context.Background(), s, run); err != nil || run.failures != 0 {
		t.Fatalf("first run: %v, %d failures", err, run.failures)
	}
	var before int64
	if err := s.db.QueryRow(`SELECT total_changes()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	stamp := historyStampOf(t, s, "T")
	run = &deferredRun{}
	if err := backfillFindText(context.Background(), s, run); err != nil || run.failures != 0 {
		t.Fatalf("second run: %v, %d failures", err, run.failures)
	}
	if n := countRows(t, s, `SELECT count(*) FROM items WHERE find_text IS NULL`); n != 0 {
		t.Fatalf("%d rows unfilled after the second run", n)
	}
	if historyStampOf(t, s, "T") != stamp {
		t.Fatal("the second run moved T's history stamp")
	}
	if indexExists(t, s, "idx_items_find_text_pending") {
		t.Fatal("the second run left its pending index")
	}
}

// TestMigrationV150BackfillReportsARowItCannotFill: a row whose fill fails
// stays unfilled and findable through the fallback, the rest of its batch
// is filled, the phase reports one failure and keeps its pending indexes,
// and the next run fills the row and finishes.
func TestMigrationV150BackfillReportsARowItCannotFill(t *testing.T) {
	s := findTextAtV149(t)
	mustExec(t, s.db, `CREATE TRIGGER zz_refuse_md BEFORE UPDATE OF find_text ON items WHEN OLD.id = 'md'
		BEGIN SELECT RAISE(ABORT, 'refused'); END`)
	if err := s.RunDeferredMigrations(context.Background(), DeferredHost{}); err != nil {
		t.Fatal(err)
	}
	failure := deferredFailureOf(t, s)
	if !deferredPending(t, s) || failure == nil || failure.Version != findTextMigrationVersion || failure.Failures != 1 ||
		!strings.Contains(failure.FirstError, "refused") {
		t.Fatalf("failure = %+v, want v150's one refused row", failure)
	}
	if n := countRows(t, s, `SELECT count(*) FROM items WHERE find_text IS NULL`); n != 1 {
		t.Fatalf("%d rows unfilled, want only md", n)
	}
	if !indexExists(t, s, "idx_items_find_text_pending") {
		t.Fatal("the failed run dropped the pending index the next run needs")
	}
	if got := findIDs(t, s, "T", "foobar"); got != "[md]" {
		t.Fatalf("the unfilled row is not found: %s", got)
	}

	mustExec(t, s.db, `DROP TRIGGER zz_refuse_md`)
	if err := s.RunDeferredMigrations(context.Background(), DeferredHost{}); err != nil {
		t.Fatal(err)
	}
	if deferredPending(t, s) || deferredFailureOf(t, s) != nil {
		t.Fatalf("the second run left failure %+v", deferredFailureOf(t, s))
	}
	if n := staleFindTextRows(t, s); n != 0 {
		t.Fatalf("%d rows stale after the second run", n)
	}
}

// The backfill walks its pending index to the next batch rather than
// scanning the table for unfilled rows.
func TestMigrationV150BackfillWalksItsPendingIndex(t *testing.T) {
	s := findTextAtV149(t)
	for _, table := range findTextTables {
		plan := planText(explainPlan(t, s, table.pendingSQL(), "", -1))
		want := "idx_" + table.name + "_find_text_pending"
		if !strings.Contains(plan, want) || strings.Contains(plan, "TEMP B-TREE") {
			t.Errorf("%s backfill plan does not walk %s:\n%s", table.name, want, plan)
		}
	}
}
