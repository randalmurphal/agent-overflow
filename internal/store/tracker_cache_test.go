package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const mirrorSite = "https://example.atlassian.net"

// newMirror is a store with a Jira connection and its mirror root.
func newMirror(t *testing.T) (*Store, TrackerConnection, string) {
	t.Helper()
	s := newTestStore(t)
	c := mustCreateTrackerConnection(t, s, jiraConnection(mirrorSite, "user1@example.com", 10))
	root, created, err := s.GetOrCreateMirrorRoot(c.ID)
	if err != nil || !created {
		t.Fatalf("create mirror root: %v, %v", created, err)
	}
	return s, c, root
}

// webStructure is board 10: three columns, an active, a future and a
// closed sprint.
func webStructure() MirrorStructure {
	return MirrorStructure{
		Board: MirrorBoard{ID: "10", ProjectID: "100", Name: "WEB board", PlansSprints: true, EstimateUnit: MirrorEstimatePoints, Data: []byte(`{"choices":[1,2,3]}`)},
		Columns: []MirrorColumn{
			{Name: "To do", Statuses: []string{"1"}},
			{Name: "In progress", Statuses: []string{"3"}},
			{Name: "Done", Statuses: []string{"10001", "6"}},
		},
		Sprints: []MirrorSprint{
			{ID: "21", OriginBoardID: "10", Name: "WEB Sprint 2", State: MirrorSprintActive, StartAt: 1000, EndAt: 2000},
			{ID: "22", OriginBoardID: "10", Name: "WEB Sprint 3", State: MirrorSprintFuture},
			{ID: "20", OriginBoardID: "10", Name: "WEB Sprint 1", State: MirrorSprintClosed, StartAt: 100, EndAt: 900, CompletedAt: 950},
		},
		Catalog: MirrorCatalog{
			Projects: []MirrorProject{{ID: "100", Key: "WEB", Name: "Web", Data: []byte(`{"audience":false}`)}},
			Users:    []MirrorUser{{ID: "acc-user1", Name: "User 1", Data: []byte(`{"active":true}`)}},
			Statuses: []MirrorStatus{
				{ID: "1", Name: "To Do", Group: MirrorGroupToDo},
				{ID: "3", Name: "In Progress", Group: MirrorGroupInProgress},
				{ID: "10001", Name: "Done", Group: MirrorGroupDone},
				{ID: "6", Name: "Won't do", Group: MirrorGroupCanceled},
			},
		},
	}
}

func webCard(id string, updated int64, sprints ...string) MirrorCard {
	return MirrorCard{IssueID: id, Key: "WEB-" + id, ProjectID: "100", TypeID: "10001", StatusID: "1", AssigneeID: "acc-user1",
		PriorityID: "3", CreatedAt: 1, UpdatedAt: updated, Sprints: sprints, Data: []byte(`{"title":"Issue ` + id + `"}`)}
}

func webRow(id string, updated int64, order string, sprints ...string) MirrorRow {
	return MirrorRow{Card: webCard(id, updated, sprints...), OrderKey: order}
}

func mustReplaceStructure(t *testing.T, s *Store, root string, st MirrorStructure, want bool) {
	t.Helper()
	if changed, err := s.ReplaceMirrorStructure(root, st); err != nil || changed != want {
		t.Fatalf("replace board %s structure: changed %v, %v; want %v", st.Board.ID, changed, err, want)
	}
}

func mustWriteRowPage(t *testing.T, s *Store, root string, page MirrorRowPage, cursor []byte) MirrorChanges {
	t.Helper()
	changes, err := s.WriteMirrorRowPage(root, page, cursor)
	if err != nil {
		t.Fatalf("write board %s rows: %v", page.BoardID, err)
	}
	return changes
}

// countMirrorRows counts the rows of every tracker_cache_* table.
func countMirrorRows(t *testing.T, db sqlQueryer) map[string]int {
	t.Helper()
	tables, err := queryIDs(db, `SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'tracker\_cache\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, table := range tables {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		counts[table] = n
	}
	return counts
}

func mirrorRowTotal(counts map[string]int) int {
	total := 0
	for _, n := range counts {
		total += n
	}
	return total
}

// seedFullMirror writes a row into every mirror table under root.
func seedFullMirror(t *testing.T, s *Store, root string) {
	t.Helper()
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{webRow("10001", 5, "a", "21")}}, []byte("c1"))
	if _, err := s.ReplaceMirrorHistory(root, MirrorHistory{IssueID: "10001", CompleteTo: 5, Changes: []MirrorChange{{Kind: MirrorChangeStatus, At: 2, From: "1", To: "3"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StoreMirrorDetail(root, MirrorDetail{IssueID: "10001", OpenedAt: 7, Data: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-05", []MirrorWorklog{
		{ID: "w1", IssueID: "10001", AuthorID: "acc-user1", Day: "2026-01-05", SpentSeconds: 3600}}); err != nil {
		t.Fatal(err)
	}
	for table, n := range countMirrorRows(t, s.db) {
		if n == 0 && table != "tracker_cache_roots" {
			t.Fatalf("seed left %s empty", table)
		}
	}
}

func TestGetOrCreateMirrorRoot(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	if got, created, err := s.GetOrCreateMirrorRoot(jira.ID); err != nil || created || got != root {
		t.Fatalf("again = %s, %v, %v; want %s, not created", got, created, err, root)
	}
	if got, ok, err := s.GetMirrorRoot(jira.ID); err != nil || !ok || got != root {
		t.Fatalf("read = %s, %v, %v", got, ok, err)
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
}

// A clear detaches the root first, so a write in flight during the drain
// is refused, and a new root can be made before the drain ends. Each
// transaction deletes at most clearMirrorChunk rows.
func TestClearMirrorDrainsInBatches(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	seedFullMirror(t, s, root)
	rows := make([]MirrorRow, 0, 2*clearMirrorChunk+10)
	for i := range 2*clearMirrorChunk + 10 {
		rows = append(rows, webRow(fmt.Sprintf("2%04d", i), 5, fmt.Sprintf("b%04d", i)))
	}
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: rows}, nil)

	before := mirrorRowTotal(countMirrorRows(t, s.db))
	last, pauses := before, 0
	var newRoot string
	cleared, err := s.ClearMirror(jira.ID, func() {
		pauses++
		now := mirrorRowTotal(countMirrorRows(t, s.db))
		if last-now > clearMirrorChunk {
			t.Errorf("one transaction deleted %d rows; the bound is %d", last-now, clearMirrorChunk)
		}
		last = now
		if pauses == 1 {
			if _, err := s.WriteMirrorRowPage(root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{webRow("30001", 9, "c")}}, nil); !errors.Is(err, ErrTrackerCacheGone) {
				t.Errorf("write during the drain: %v", err)
			}
			var err error
			if newRoot, _, err = s.GetOrCreateMirrorRoot(jira.ID); err != nil {
				t.Errorf("new root during the drain: %v", err)
			}
		}
	})
	if err != nil || !cleared {
		t.Fatalf("clear: %v, %v", cleared, err)
	}
	// Three batches of the board's rows and of the cards, and one of
	// each other table, at least.
	if pauses < 6 {
		t.Fatalf("%d pauses for %d rows", pauses, before)
	}
	counts := countMirrorRows(t, s.db)
	if counts["tracker_cache_roots"] != 1 || mirrorRowTotal(counts) != 1 {
		t.Fatalf("after the clear: %v; want only the new root", counts)
	}
	if got, ok, err := s.GetMirrorRoot(jira.ID); err != nil || !ok || got != newRoot || got == root {
		t.Fatalf("root after the clear = %s, %v, %v; want the new %s", got, ok, err, newRoot)
	}
	if cleared, err := s.ClearMirror("missing", nil); err != nil || cleared {
		t.Fatalf("clear of no mirror: %v, %v", cleared, err)
	}
}

// A clear that stopped part way leaves a detached root, which the next
// start finishes.
func TestResumeMirrorClearsFinishesADetachedRoot(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	seedFullMirror(t, s, root)
	mustExec(t, s.db, `UPDATE tracker_cache_roots SET connection_id = NULL WHERE id = ?`, root)
	live, _, err := s.GetOrCreateMirrorRoot(jira.ID)
	if err != nil {
		t.Fatal(err)
	}
	mustReplaceStructure(t, s, live, webStructure(), true)
	resumed, err := s.ResumeMirrorClears(nil)
	if err != nil || !slices.Equal(resumed, []string{root}) {
		t.Fatalf("resume = %v, %v; want %s", resumed, err, root)
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issues`, "0")
	assertRows(t, s.db, `SELECT DISTINCT root_id FROM tracker_cache_boards`, live)
	if resumed, err := s.ResumeMirrorClears(nil); err != nil || len(resumed) != 0 {
		t.Fatalf("second resume = %v, %v", resumed, err)
	}
}

// Every table of the mirror is drained by a clear, before the tables it
// references.
func TestClearMirrorDrainsEveryTable(t *testing.T) {
	t.Parallel()
	db := migrateThrough(t, trackerMigrationVersion)
	tables, err := queryIDs(db, `SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'tracker\_cache\_%' ESCAPE '\'
		AND name <> 'tracker_cache_roots' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(slices.Values(clearMirrorTables)); !slices.Equal(got, tables) {
		t.Fatalf("clearMirrorTables = %v\nwant the mirror's tables %v", got, tables)
	}
	for i, table := range clearMirrorTables {
		parents, err := queryIDs(db, `SELECT DISTINCT "table" FROM pragma_foreign_key_list(?)`, table)
		if err != nil {
			t.Fatal(err)
		}
		for _, parent := range parents {
			if j := slices.Index(clearMirrorTables, parent); j >= 0 && j < i {
				t.Errorf("%s is drained before %s, which references it", parent, table)
			}
		}
	}
}

// mirrorWriters are every mirror accessor that writes, each called with a
// valid write for a root holding webStructure.
func mirrorWriters(s *Store) map[string]func(root string) error {
	return map[string]func(root string) error{
		"ReplaceMirrorStructure": func(root string) error { _, err := s.ReplaceMirrorStructure(root, webStructure()); return err },
		"DeleteMirrorBoard":      func(root string) error { _, err := s.DeleteMirrorBoard(root, "10"); return err },
		"WriteMirrorRowPage": func(root string) error {
			_, err := s.WriteMirrorRowPage(root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{webRow("10002", 5, "b")}}, []byte("c2"))
			return err
		},
		"WriteMirrorRows": func(root string) error {
			_, err := s.WriteMirrorRows(root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{webRow("10003", 5, "c")}})
			return err
		},
		"UpsertMirrorCards": func(root string) error {
			_, err := s.UpsertMirrorCards(root, []MirrorCard{webCard("10001", 6)}, MirrorCatalog{})
			return err
		},
		"PruneUnkeptMirrorIssues": func(root string) error { _, _, _, err := s.PruneUnkeptMirrorIssues(root, ""); return err },
		"ReconcileMirrorBoard": func(root string) error {
			_, err := s.ReconcileMirrorBoard(root, "10", []string{"10001"}, true)
			return err
		},
		"ReplaceMirrorHistory": func(root string) error {
			_, err := s.ReplaceMirrorHistory(root, MirrorHistory{IssueID: "10001", CompleteTo: 9})
			return err
		},
		"StoreMirrorDetail": func(root string) error {
			_, err := s.StoreMirrorDetail(root, MirrorDetail{IssueID: "10001", OpenedAt: 9, Data: []byte(`{}`)})
			return err
		},
		"ReplaceMirrorWorklogs": func(root string) error {
			_, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-06", nil)
			return err
		},
		"SetMirrorSyncRead":       func(root string) error { _, err := s.SetMirrorSyncRead(root, "10", 9); return err },
		"SetMirrorSyncReconciled": func(root string) error { _, err := s.SetMirrorSyncReconciled(root, "10", 9); return err },
		"SetMirrorSyncFailure": func(root string) error {
			_, err := s.SetMirrorSyncFailure(root, "10", MirrorSyncFailure{At: 9, Kind: MirrorFailureTransient, Message: "timed out"})
			return err
		},
	}
}

// mirrorNonWriters are the mirror's other accessors: the root's lifecycle
// and the reads.
var mirrorNonWriters = []string{
	"GetOrCreateMirrorRoot", "GetMirrorRoot", "ClearMirror", "ResumeMirrorClears",
	"GetMirrorSync", "ListMirrorHistoryDue", "GetMirrorDetail", "ListMirrorWorklogs", "ListMirrorIssueWorklogs",
	"GetMirrorBoard", "GetMirrorCatalog", "ListMirrorCards", "ListMirrorIssueRows", "ListMirrorSection", "CountMirrorSections",
}

// Every writer refuses a cleared root, and a root that never existed, with
// ErrTrackerCacheGone and leaves nothing behind. Every Store method named
// for the mirror is listed here, so a new writer cannot skip the check.
func TestMirrorWritesAfterAClearFailWithGone(t *testing.T) {
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
	if _, err := s.ClearMirror(jira.ID, nil); err != nil {
		t.Fatal(err)
	}
	for name, write := range writers {
		for _, gone := range []string{root, "never"} {
			if err := write(gone); !errors.Is(err, ErrTrackerCacheGone) {
				t.Errorf("%s on root %s: %v; want ErrTrackerCacheGone", name, gone, err)
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
		rows, err := db.Query(`SELECT fk."table", fk.on_delete, max(coalesce(ti."notnull", 0) = 0)
			FROM pragma_foreign_key_list(?1) fk LEFT JOIN pragma_table_info(?1) ti ON ti.name = fk."from"
			GROUP BY fk.id`, table)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var k key
			if err := rows.Scan(&k.parent, &k.onDelete, &k.nullable); err != nil {
				t.Fatal(err)
			}
			keys[table] = append(keys[table], k)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
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
	db := migrateThrough(t, trackerMigrationVersion)
	if violations := mirrorCascadeViolations(t, db); len(violations) > 0 {
		t.Fatalf("schema violations:\n%s", strings.Join(violations, "\n"))
	}
	for name, table := range map[string]string{
		"no key":         `CREATE TABLE tracker_cache_bad (root_id TEXT NOT NULL)`,
		"set null":       `CREATE TABLE tracker_cache_bad (root_id TEXT NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE SET NULL)`,
		"nullable key":   `CREATE TABLE tracker_cache_bad (root_id TEXT REFERENCES tracker_cache_roots(id) ON DELETE CASCADE)`,
		"loose parent":   `CREATE TABLE tracker_cache_bad (root_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE)`,
		"durable child":  `CREATE TABLE app_bad (root_id TEXT NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE CASCADE)`,
		"durable on row": `CREATE TABLE app_bad (r TEXT NOT NULL, i TEXT NOT NULL, FOREIGN KEY (r, i) REFERENCES tracker_cache_issues(root_id, issue_id))`,
		"second set null": `CREATE TABLE tracker_cache_bad (root_id TEXT NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE CASCADE,
			other_root TEXT REFERENCES tracker_cache_roots(id) ON DELETE SET NULL)`,
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
	mustExec(t, db, `CREATE TABLE tracker_cache_roots (id TEXT PRIMARY KEY, connection_id TEXT UNIQUE REFERENCES tracker_connections(id) ON DELETE CASCADE)`)
	if violations := mirrorCascadeViolations(t, db); len(violations) == 0 || !strings.Contains(violations[0], "tracker_cache_roots:") {
		t.Errorf("a cascading root: violations %v", violations)
	}
}

// A restore empties the mirror, rows written after the snapshot and rows
// it holds alike, and keeps the durable tracker tables.
func TestRestoreEmptiesTheMirror(t *testing.T) {
	t.Parallel()
	st := snapshotTestStore(t)
	seedSnapshotFixture(t, st, "t1", "First")
	jira := mustCreateTrackerConnection(t, st, jiraConnection(mirrorSite, "user1@example.com", 10))
	if _, err := st.ReplaceProjectTrackerBoards("p1", []ProjectTrackerBoard{{ConnectionID: jira.ID, BoardID: "10", OpensFirst: true}}); err != nil {
		t.Fatal(err)
	}
	mustSetThreadTicket(t, st, ThreadTicket{ThreadID: "t1", Ticket: TicketRef{Kind: TrackerKindJira, Site: mirrorSite, IssueID: "10001"},
		IssueKey: "WEB-1", Origin: TicketOriginManual, LinkedAt: 10}, true)
	root, _, err := st.GetOrCreateMirrorRoot(jira.ID)
	if err != nil {
		t.Fatal(err)
	}
	seedFullMirror(t, st, root)
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := st.SnapshotTo(snap); err != nil {
		t.Fatal(err)
	}
	mustWriteRowPage(t, st, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{webRow("10002", 6, "b")}}, nil)

	if _, err := st.RestoreFrom(snap); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if counts := countMirrorRows(t, st.db); len(counts) != len(clearMirrorTables)+1 || mirrorRowTotal(counts) != 0 {
		t.Fatalf("mirror after restore: %v", counts)
	}
	if _, ok, err := st.GetMirrorRoot(jira.ID); err != nil || ok {
		t.Fatalf("root after restore: %v, %v", ok, err)
	}
	if got, err := st.GetTrackerConnection(jira.ID); err != nil || got != jira {
		t.Fatalf("connection after restore = %+v, %v", got, err)
	}
	assertRows(t, st.db, `SELECT board_id FROM project_tracker_boards`, "10")
	assertRows(t, st.db, `SELECT issue_id FROM thread_tickets`, "10001")
	if _, err := st.WriteMirrorRowPage(root, MirrorRowPage{BoardID: "10"}, nil); !errors.Is(err, ErrTrackerCacheGone) {
		t.Fatalf("write to the pre-restore root: %v", err)
	}
	if _, created, err := st.GetOrCreateMirrorRoot(jira.ID); err != nil || !created {
		t.Fatalf("new root after restore: %v, %v", created, err)
	}
}
