package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestGetOrCreateMirrorRoot(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	if got, created, err := s.GetOrCreateMirrorRoot(jira.ID); err != nil || created || got != root {
		t.Fatalf("again = %d, %v, %v; want %d, not created", got, created, err, root)
	}
	if got, ok, err := s.GetMirrorRoot(jira.ID); err != nil || !ok || got != root {
		t.Fatalf("read = %d, %v, %v", got, ok, err)
	}
	tempo := mustCreateTrackerConnection(t, s, NewTrackerConnection{Kind: TrackerKindTempo, Site: "https://api.eu.tempo.io", ParentID: jira.ID, CreatedAt: 11})
	if _, _, err := s.GetOrCreateMirrorRoot(tempo.ID); !errors.Is(err, ErrInvalidTrackerConnection) {
		t.Fatalf("tempo root: %v", err)
	}
	if _, _, err := s.GetOrCreateMirrorRoot("missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing connection: %v", err)
	}
	linear := mustCreateTrackerConnection(t, s, NewTrackerConnection{Kind: TrackerKindLinear, Site: "workspace-a", CreatedAt: 12})
	if _, _, err := s.SetTrackerConnectionEnabled(linear.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.GetOrCreateMirrorRoot(linear.ID); !errors.Is(err, ErrTrackerCacheGone) {
		t.Fatalf("root of a connection turned off: %v", err)
	}
	if _, ok, err := s.GetMirrorRoot(linear.ID); err != nil || ok {
		t.Fatalf("connection turned off has a root: %v, %v", ok, err)
	}
	// A detached root's ID is never given again.
	if detached, err := s.DetachMirror(jira.ID); err != nil || !detached {
		t.Fatalf("detach = %v, %v", detached, err)
	}
	if err := s.FinishMirrorCleanup(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if again, _, err := s.GetOrCreateMirrorRoot(jira.ID); err != nil || again <= root {
		t.Fatalf("root after a detach = %d, %v; want one after %d", again, err, root)
	}
}

// Detaching is one statement that deletes nothing: every write naming the
// root fails with ErrTrackerCacheGone, every read is empty, and the
// connection takes a new root at once. The cleanup then drains the old
// root in batches with a pause before each, never more than a batch per
// transaction.
func TestDetachMirrorThenCleanupDrainsInBatches(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	seedFullMirror(t, s, root)
	rows := make([]MirrorRow, 0, mirrorMaxPageRows)
	for i := range mirrorMaxPageRows {
		rows = append(rows, webRow(fmt.Sprintf("2%04d", i), 5, fmt.Sprintf("b%04d", i)))
	}
	for _, board := range []string{"10", "11"} {
		st := webStructure()
		st.Board.ID = board
		if _, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, st)); err != nil {
			t.Fatal(err)
		}
		mustWriteRowPage(t, s, root, webPage(board, rows...), nil)
	}
	before := countMirrorRows(t, s.db)
	recorder := recordMirrorWrites(t, s)
	if detached, err := s.DetachMirror(jira.ID); err != nil || !detached {
		t.Fatalf("detach = %v, %v", detached, err)
	}
	if after := countMirrorRows(t, s.db); !reflect.DeepEqual(after, before) {
		t.Fatalf("the detach deleted rows: %v, was %v", after, before)
	}
	if _, err := s.WriteMirrorRowPage(root, currentPage(s, root, webPage("10", webRow("30001", 9, "c"))), nil); !errors.Is(err, ErrTrackerCacheGone) {
		t.Fatalf("write after the detach: %v", err)
	}
	if cards, err := s.ListMirrorCards(root, []string{"10001"}); err != nil || len(cards) != 0 {
		t.Fatalf("cards of a detached root = %v, %v", cards, err)
	}
	if _, ok, err := s.GetMirrorBoard(root, "10"); err != nil || ok {
		t.Fatalf("board of a detached root: %v, %v", ok, err)
	}
	newRoot, created, err := s.GetOrCreateMirrorRoot(jira.ID)
	if err != nil || !created {
		t.Fatalf("new root after the detach: %v, %v", created, err)
	}
	if again, err := s.DetachMirror("missing"); err != nil || again {
		t.Fatalf("detach of no mirror = %v, %v", again, err)
	}

	pauses := 0
	if err := s.FinishMirrorCleanup(context.Background(), func() { pauses++ }); err != nil {
		t.Fatal(err)
	}
	if most, total := recorder.mostDeletedInATransaction(); most > clearMirrorChunk || total != mirrorRowTotal(before) {
		t.Fatalf("drain deleted %d rows, at most %d in one transaction; want all %d, at most %d", total, most, mirrorRowTotal(before), clearMirrorChunk)
	}
	if pauses < len(mirrorTableKeys)+2 {
		t.Fatalf("%d pauses; want one before each of at least %d transactions", pauses, len(mirrorTableKeys)+2)
	}
	if counts := countMirrorRows(t, s.db); counts["tracker_cache_roots"] != 1 || mirrorRowTotal(counts) != 1+counts["tracker_cache_boards"] {
		t.Fatalf("after the cleanup: %v; want only the new root and its boards", counts)
	}
	assertRows(t, s.db, fmt.Sprintf(`SELECT count(*) FROM tracker_cache_boards WHERE root_id IS NOT %d OR read_at > 0`, newRoot), "0")
	if got, ok, err := s.GetMirrorRoot(jira.ID); err != nil || !ok || got != newRoot {
		t.Fatalf("root after the cleanup = %d, %v, %v; want %d", got, ok, err, newRoot)
	}
}

// Every table of the mirror is drained by a root's drain, before the
// tables it references, and a board's drain covers every table keyed by
// board.
func TestMirrorDrainCoversEveryTable(t *testing.T) {
	t.Parallel()
	db := migrateThrough(t, migrations[len(migrations)-1].Version)
	tables, err := queryIDs(db, `SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'tracker\_cache\_%' ESCAPE '\'
		AND name <> 'tracker_cache_roots' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var drained []string
	for _, k := range mirrorTableKeys {
		drained = append(drained, k.table)
	}
	if got := slices.Sorted(slices.Values(drained)); !slices.Equal(got, tables) {
		t.Fatalf("mirrorTableKeys = %v\nwant the mirror's tables %v", got, tables)
	}
	for i, table := range drained {
		parents, err := queryIDs(db, `SELECT DISTINCT "table" FROM pragma_foreign_key_list(?)`, table)
		if err != nil {
			t.Fatal(err)
		}
		for _, parent := range parents {
			if j := slices.Index(drained, parent); j >= 0 && j < i {
				t.Errorf("%s is drained before %s, which references it", parent, table)
			}
		}
		if parents, _ := queryIDs(db, `SELECT "table" FROM pragma_foreign_key_list(?) WHERE "table" = 'tracker_cache_boards'`, table); len(parents) > 0 &&
			!slices.Contains(mirrorBoardTables, table) {
			t.Errorf("%s references a board but a board's drain leaves it", table)
		}
	}
	for _, table := range mirrorBoardTables {
		for _, column := range []string{"root_id", "board_id"} {
			if got, _ := queryIDs(db, `SELECT name FROM pragma_table_info(?) WHERE name = ?`, table, column); len(got) == 0 {
				t.Errorf("%s has no %s for a board's drain", table, column)
			}
		}
	}
}

// mirrorWriters are every mirror accessor that writes, each called with a
// valid write for a root holding webStructure.
func mirrorWriters(s *Store) map[string]func(root int64) error {
	return map[string]func(root int64) error{
		"ReplaceMirrorStructure": func(root int64) error {
			_, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, webStructure()))
			return err
		},
		"WriteMirrorRowPage": func(root int64) error {
			_, err := s.WriteMirrorRowPage(root, currentPage(s, root, webPage("10", webRow("10002", 5, "b"))), []byte("c2"))
			return err
		},
		"WriteMirrorRows": func(root int64) error {
			_, err := s.WriteMirrorRows(root, currentPage(s, root, webPage("10", webRow("10003", 5, "c"))))
			return err
		},
		"UpsertMirrorCards": func(root int64) error {
			_, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{webCard("10001", 6)}, MirrorCatalog{}, mirrorReadOf(root, 1))
			return err
		},
		"ReconcileMirrorBoard": func(root int64) error {
			_, err := s.ReconcileMirrorBoard(root, "10", currentLifetime(s, root, "10"), []string{"10001"}, true, mirrorReadOf(root, 1))
			return err
		},
		"ReplaceMirrorHistories": func(root int64) error {
			_, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, []MirrorHistory{{IssueID: "10001"}, {IssueID: "10009"}}), MirrorCatalog{}, nextMirrorRead(s, root))
			return err
		},
		"ReplaceMirrorHistories staged": func(root int64) error {
			_, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, []MirrorHistory{statusHistory("10001", clearMirrorChunk+100)}), MirrorCatalog{}, nextMirrorRead(s, root))
			return err
		},
		"UpsertMirrorCards regroup": func(root int64) error {
			_, err := s.UpsertMirrorCards(root, currentSweep(s, root), nil, MirrorCatalog{Statuses: []MirrorStatus{{ID: "1", Name: "To Do", Group: MirrorGroupDone}}}, mirrorReadOf(root, 2))
			return err
		},
		"StoreMirrorTickets": func(root int64) error {
			_, err := s.StoreMirrorTickets(root, currentSweep(s, root), []MirrorTicket{
				{Card: webCard("10001", 5), Body: []byte(`{"v":2}`)}, {Card: webCard("10002", 5), Body: []byte(`{}`)}}, MirrorCatalog{},
				mirrorReadOf(root, 9), mirrorReadOf(root, 9))
			return err
		},
		"StoreMirrorTickets check": func(root int64) error {
			_, err := s.StoreMirrorTickets(root, currentSweep(s, root), []MirrorTicket{{Card: webCard("10001", 5)}}, MirrorCatalog{},
				mirrorReadOf(root, 0), mirrorReadOf(root, 9))
			return err
		},
		"ReplaceMirrorWorklogs": func(root int64) error {
			_, err := s.ReplaceMirrorWorklogs(root, 0, "acc-user1", "2026-01-05", "2026-01-06", mirrorReadOf(root, 1), nil)
			return err
		},
		"RecordMirrorSyncAttempt": func(root int64) error {
			_, _, err := s.RecordMirrorSyncAttempt(root, "10", currentAttempt(s, root, "10", MirrorSyncAttempt{ReadAt: mirrorReadOf(root, 9)}))
			return err
		},
		"RecordMirrorSyncAttempt reconcile": func(root int64) error {
			_, _, err := s.RecordMirrorSyncAttempt(root, "10", currentAttempt(s, root, "10", MirrorSyncAttempt{ReconciledAt: mirrorReadOf(root, 9)}))
			return err
		},
		"RecordMirrorSyncAttempt reconcile failure": func(root int64) error {
			_, _, err := s.RecordMirrorSyncAttempt(root, "10", currentAttempt(s, root, "10", MirrorSyncAttempt{ReconcileFailure: MirrorReconcileAttempt{
				At: mirrorReadOf(root, 9), Returned: 9, Kind: MirrorFailureTracker, Message: "refused", Refused: true}}))
			return err
		},
		"RecordMirrorSyncAttempt failure": func(root int64) error {
			_, _, err := s.RecordMirrorSyncAttempt(root, "11", currentAttempt(s, root, "11", MirrorSyncAttempt{ReadAt: mirrorReadOf(root, 8), Failure: MirrorAttemptFailure{At: mirrorReadOf(root, 9), Kind: MirrorFailureTransient, Message: "timed out"}}))
			return err
		},
		"PassMirrorUnnamedParents": func(root int64) error { return s.PassMirrorUnnamedParents(root, "10050") },
		"EndMirrorUnnamedParents":  func(root int64) error { return s.EndMirrorUnnamedParents(root) },
		"MarkMirrorIssuesMissing": func(root int64) error {
			_, err := s.MarkMirrorIssuesMissing(root, mirrorReadOf(root, 9), []string{"10001"})
			return err
		},
		"KeepMirrorTicket": func(root int64) error {
			_, _, err := s.KeepMirrorTicket(root, "10001", 2, 1, mirrorReadOf(root, 9))
			return err
		},
		"DropMirrorBodies": func(root int64) error {
			_, err := s.DropMirrorBodies(root, []string{"10001"})
			return err
		},
		"ReplaceMirrorActs": func(root int64) error {
			_, err := s.ReplaceMirrorActs(root, 0, mirrorReadOf(root, 9), []MirrorIssueActs{commentsOf("10001", 5, 2, "c1"),
				{IssueID: "10001", Source: MirrorActWorklog, ReadFor: 5, Acts: []MirrorAct{{ID: "w1", Person: "acc-user2", At: 3, Seconds: 60}}}},
				nil, MirrorCatalog{Users: []MirrorUser{{ID: "acc-user2", Name: "User 2"}}})
			return err
		},
	}
}

// mirrorNonWriters are the mirror's other accessors: the root's lifecycle,
// the cleanup and the reads.
var mirrorNonWriters = []string{
	"GetOrCreateMirrorRoot", "GetMirrorRoot", "DetachMirror", "FinishMirrorCleanup",
	"GetMirrorSync", "ListMirrorHistoryDue", "ListMirrorActsDue", "ListMirrorBodies", "ListMirrorIssueWorklogs",
	"GetMirrorBoard", "GetMirrorBoardStatuses", "GetMirrorCatalog", "ListMirrorCards", "ListMirrorParents", "ListMirrorUnnamedParents", "ListMirrorIssueRows", "ListMirrorTicketRows", "ListMirrorSection", "CountMirrorSections", "GetMirrorBoardPace",
	"ListMirrorOffBoardLinkedIssues", "ReadMirrorBoardView", "ReadMirrorBoardPatch", "ListMirrorIssueBoards", "ListMirrorSprintNames", "LastMirrorPlanRow",
	"ReadMirrorRefreshTargets", "ReadMirrorWorklogDays",
	// A board's rows copied into a dashboard's own database: a read of the
	// file, attached read-only.
	"ExportMirrorBoard",
	// An open ticket's row goes by its pump, whatever became of its root.
	"DropMirrorTicket", "DropMirrorTicketsOfOtherSessions",
	// A read's clock, which stores its lease only when a read passes it
	// (TestAReadClockStoresALeaseOnlyOnALiveRoot).
	"BeginMirrorRead",
	// An open ticket's bind, which is memory: a write naming an ended root
	// fails whatever bind it carries (TestARestoreNeverReusesARootID).
	"ActivateMirrorBind", "EndMirrorBind",
}

// Every writer refuses a detached root, and a root that never existed,
// with ErrTrackerCacheGone and leaves nothing behind. Every Store method
// named for the mirror is listed here, so a new writer cannot skip the
// check.
func TestMirrorWritesAfterADetachFailWithGone(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	writers := mirrorWriters(s)
	storeType := reflect.TypeOf(s)
	for i := range storeType.NumMethod() {
		name := storeType.Method(i).Name
		if !strings.Contains(name, "Mirror") {
			continue
		}
		if _, ok := writers[name]; ok == slices.Contains(mirrorNonWriters, name) {
			t.Errorf("Store.%s must be listed once, as a mirror writer or as a lifecycle or read method", name)
		}
	}

	// Each write succeeds on a live root, so a refusal below is the root's.
	for name := range writers {
		liveStore, _, liveRoot := newMirror(t)
		seedFullMirror(t, liveStore, liveRoot)
		if err := mirrorWriters(liveStore)[name](liveRoot); err != nil {
			t.Errorf("%s on a live root: %v", name, err)
		}
	}

	seedFullMirror(t, s, root)
	if _, err := s.DetachMirror(jira.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishMirrorCleanup(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for name, write := range writers {
		for _, gone := range []int64{root, 9999} {
			if err := write(gone); !errors.Is(err, ErrTrackerCacheGone) {
				t.Errorf("%s on root %d: %v; want ErrTrackerCacheGone", name, gone, err)
			}
		}
	}
	if counts := countMirrorRows(t, s.db); mirrorRowTotal(counts) != 0 {
		t.Fatalf("late writes left %v", counts)
	}
}

// mirrorCascadeViolations walks the schema: every tracker_cache_* table
// reaches tracker_cache_roots through ON DELETE CASCADE keys on NOT NULL
// columns, the root's link to its connection is the only SET NULL key of
// the mirror, and no other table references a mirror table.
func mirrorCascadeViolations(t *testing.T, db *sql.DB) []string {
	t.Helper()
	tables, err := queryIDs(db, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	type key struct {
		parent, onDelete string
		nullable         bool
	}
	keys := map[string][]key{}
	for _, table := range tables {
		err := scanMirrorRows(db, `SELECT fk."table", fk.on_delete, max(coalesce(ti."notnull", 0) = 0)
			FROM pragma_foreign_key_list(?1) fk LEFT JOIN pragma_table_info(?1) ti ON ti.name = fk."from"
			GROUP BY fk.id`, []any{table}, func(r rowScanner) error {
			var k key
			err := r.Scan(&k.parent, &k.onDelete, &k.nullable)
			keys[table] = append(keys[table], k)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	mirror := func(table string) bool { return strings.HasPrefix(table, "tracker_cache_") }
	var violations []string
	rootKey := key{parent: "tracker_connections", onDelete: "SET NULL", nullable: true}
	if !slices.Equal(keys["tracker_cache_roots"], []key{rootKey}) {
		violations = append(violations, "tracker_cache_roots: its one key is not a SET NULL link to its connection")
	}
	reached := map[string]bool{"tracker_cache_roots": true}
	for grew := true; grew; {
		grew = false
		for _, table := range tables {
			if !mirror(table) || reached[table] {
				continue
			}
			for _, k := range keys[table] {
				if reached[k.parent] && k.onDelete == "CASCADE" && !k.nullable {
					reached[table], grew = true, true
					break
				}
			}
		}
	}
	for _, table := range tables {
		if mirror(table) && !reached[table] {
			violations = append(violations, table+": does not reach tracker_cache_roots by cascading keys")
		}
		if mirror(table) {
			for _, k := range keys[table] {
				if k.onDelete == "SET NULL" && table != "tracker_cache_roots" {
					violations = append(violations, table+": sets a key to NULL")
				}
			}
			continue
		}
		for _, k := range keys[table] {
			if mirror(k.parent) {
				violations = append(violations, table+": references "+k.parent)
			}
		}
	}
	return violations
}

// The cascade walk passes the schema and catches each kind of violating
// table.
func TestTrackerCacheTablesCascadeFromTheRoot(t *testing.T) {
	t.Parallel()
	db := migrateThrough(t, trackerMirrorMigrationVersion)
	if violations := mirrorCascadeViolations(t, db); len(violations) > 0 {
		t.Fatalf("schema violations:\n%s", strings.Join(violations, "\n"))
	}
	for name, table := range map[string]string{
		"no key":         `CREATE TABLE tracker_cache_bad (root_id INTEGER NOT NULL)`,
		"set null":       `CREATE TABLE tracker_cache_bad (root_id INTEGER NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE SET NULL)`,
		"nullable key":   `CREATE TABLE tracker_cache_bad (root_id INTEGER REFERENCES tracker_cache_roots(id) ON DELETE CASCADE)`,
		"loose parent":   `CREATE TABLE tracker_cache_bad (root_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE)`,
		"durable child":  `CREATE TABLE app_bad (root_id INTEGER NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE CASCADE)`,
		"durable on row": `CREATE TABLE app_bad (r INTEGER NOT NULL, i TEXT NOT NULL, FOREIGN KEY (r, i) REFERENCES tracker_cache_issues(root_id, issue_id))`,
		"second set null": `CREATE TABLE tracker_cache_bad (root_id INTEGER NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE CASCADE,
			other_root INTEGER REFERENCES tracker_cache_roots(id) ON DELETE SET NULL)`,
	} {
		mustExec(t, db, table)
		violations := mirrorCascadeViolations(t, db)
		if len(violations) == 0 || slices.ContainsFunc(violations, func(v string) bool { return !strings.Contains(v, "_bad") }) {
			t.Errorf("%s: violations %v; want only ones naming the bad table", name, violations)
		}
		mustExec(t, db, `DROP TABLE IF EXISTS tracker_cache_bad`)
		mustExec(t, db, `DROP TABLE IF EXISTS app_bad`)
	}
	// A root that cascades from its connection breaks the rule too.
	mustExec(t, db, `ALTER TABLE tracker_cache_roots RENAME TO tracker_cache_roots_set_null`)
	mustExec(t, db, `CREATE TABLE tracker_cache_roots (id INTEGER PRIMARY KEY, connection_id TEXT UNIQUE REFERENCES tracker_connections(id) ON DELETE CASCADE)`)
	if violations := mirrorCascadeViolations(t, db); len(violations) == 0 || !strings.Contains(violations[0], "tracker_cache_roots:") {
		t.Errorf("a cascading root: violations %v", violations)
	}
}

// A restore empties the mirror, rows written after the snapshot and rows
// it holds alike, keeps the durable tracker tables, and never gives a root
// ID again.
func TestRestoreEmptiesTheMirror(t *testing.T) {
	t.Parallel()
	st := snapshotTestStore(t)
	seedSnapshotFixture(t, st, "t1", "First")
	jira := mustCreateTrackerConnection(t, st, jiraConnection(mirrorSite, "user1@example.com", 10))
	listMirrorBoards(t, st, "p1", jira.ID, "10")
	seedThreadTicket(t, st, ThreadTicket{ThreadID: "t1", Ticket: TicketRef{Kind: TrackerKindJira, Site: mirrorSite, IssueID: "10001"},
		IssueKey: "WEB-1", Origin: TicketOriginManual, LinkedAt: 10})
	root, _, err := st.GetOrCreateMirrorRoot(jira.ID)
	if err != nil {
		t.Fatal(err)
	}
	seedFullMirror(t, st, root)
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := st.SnapshotTo(snap); err != nil {
		t.Fatal(err)
	}
	mustWriteRowPage(t, st, root, webPage("10", webRow("10002", 6, "b")), nil)

	if _, err := st.RestoreFrom(snap); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if counts := countMirrorRows(t, st.db); len(counts) != len(mirrorTableKeys)+1 || mirrorRowTotal(counts) != 0 {
		t.Fatalf("mirror after restore: %v", counts)
	}
	if _, ok, err := st.GetMirrorRoot(jira.ID); err != nil || ok {
		t.Fatalf("root after restore: %v, %v", ok, err)
	}
	if got, err := st.GetTrackerConnection(jira.ID); err != nil || got != jira {
		t.Fatalf("connection after restore = %+v, %v", got, err)
	}
	assertRows(t, st.db, `SELECT board_id FROM project_tracker_boards`, "10")
	assertRows(t, st.db, `SELECT issue_id FROM thread_tickets ORDER BY issue_id`, "10001,10009")
	if _, err := st.WriteMirrorRowPage(root, currentPage(st, root, webPage("10")), nil); !errors.Is(err, ErrTrackerCacheGone) {
		t.Fatalf("write to the pre-restore root: %v", err)
	}
	if again, created, err := st.GetOrCreateMirrorRoot(jira.ID); err != nil || !created || again <= root {
		t.Fatalf("new root after restore = %d, %v, %v; want one after %d", again, created, err, root)
	}
}
