package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	sqlite3 "modernc.org/sqlite/lib"
)

// exportConn is a connection to a new private in-memory database, the
// kind trackerdash hands the export.
func exportConn(t *testing.T) *sql.Conn {
	t.Helper()
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close the export's connection: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Errorf("close the export's database: %v", err)
		}
	})
	return conn
}

func exportRows(t *testing.T, conn *sql.Conn, query string) []string {
	t.Helper()
	rows, err := conn.QueryContext(context.Background(), query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close the rows of %s: %v", query, err)
		}
	}()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// An export holds one board: its rows, the changes of their issues, the
// memberships of its sprints, and the statuses and sprints it names; the
// other board's rows are not in it. Its sprint members count done by the
// pace's rule, so the export's last five closed sprints add up to the
// board's pace, regrouped or not; an active sprint's members count done by
// their status now. A ticket started at its first move into progress and
// finished at its last move into done, if it is done now. The file is detached when it returns,
// and a board the mirror does not show exports nothing.
func TestMirrorExportHoldsTheBoardAlone(t *testing.T) {
	t.Parallel()
	s, root := paceFixture(t)
	regroup, err := NewMirrorRegroup(map[string]int{"6": MirrorGroupDone, "10001": MirrorGroupInProgress})
	if err != nil {
		t.Fatal(err)
	}
	// Per regrouping: the active sprint's members with whether each is done
	// now, and the start and finish of the tickets that have a history
	// (ticket|started|done), NULL as "-".
	active := [][]string{{"10008|1", "10010|1"}, {"10008|0", "10010|0"}}
	times := [][]string{{"10001|-|-", "10002|-|650", "10009|100|-"}, {"10001|550|-", "10002|650|-", "10009|100|-"}}
	for i, g := range []MirrorRegroup{{}, regroup} {
		conn := exportConn(t)
		export, ok, err := s.ExportMirrorBoard(context.Background(), conn, root, "10", g, MirrorExportTables)
		if err != nil || !ok {
			t.Fatalf("export board 10: %v, %v", ok, err)
		}
		if export.EstimateUnit != MirrorEstimatePoints || !export.PlansSprints {
			t.Fatalf("export = %+v; want a points board that plans sprints", export)
		}
		if got := exportRows(t, conn, `SELECT name FROM pragma_database_list ORDER BY seq`); !reflect.DeepEqual(got, []string{"main"}) {
			t.Fatalf("databases after the export = %v; want main alone", got)
		}
		tickets := exportRows(t, conn, `SELECT id FROM tickets ORDER BY id`)
		want := []string{"10001", "10002", "10003", "10005", "10006", "10008", "10009", "10010", "10011", "10012"}
		if !reflect.DeepEqual(tickets, want) {
			t.Fatalf("tickets = %v; want board 10's rows %v", tickets, want)
		}
		if got := exportRows(t, conn, `SELECT DISTINCT ticket FROM changes ORDER BY ticket`); !reflect.DeepEqual(got, []string{"10001", "10002", "10009"}) {
			t.Fatalf("changes of %v; want those of board 10's issues with a history", got)
		}
		if got := exportRows(t, conn, `SELECT count(*) FROM sprint_tickets WHERE ticket NOT IN (SELECT id FROM tickets) OR sprint NOT IN (SELECT id FROM sprints)`); got[0] != "0" {
			t.Fatalf("%s memberships outside the board's tickets and sprints", got[0])
		}
		if got := exportRows(t, conn, `SELECT ticket || '|' || done FROM sprint_finished WHERE sprint = '21' ORDER BY ticket`); !reflect.DeepEqual(got, active[i]) {
			t.Fatalf("active sprint's members = %v; want %v", got, active[i])
		}
		if got := exportRows(t, conn, `SELECT id || '|' || coalesce(started_at, '-') || '|' || coalesce(done_at, '-') FROM tickets
		  WHERE id IN ('10001', '10002', '10009') ORDER BY id`); !reflect.DeepEqual(got, times[i]) {
			t.Fatalf("ticket times = %v; want %v (the first move into progress, the last into done while done now)", got, times[i])
		}
		pace := paceOf(t, s, root, "10", g)
		got := map[string]MirrorSprintFinished{}
		rows, err := conn.QueryContext(context.Background(), `SELECT f.sprint, count(*), total(f.done), total(iif(f.done, t.points, 0))
		  FROM sprint_finished f JOIN tickets t ON t.id = f.ticket JOIN sprints s ON s.id = f.sprint AND s.state = 'closed' GROUP BY f.sprint`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var f MirrorSprintFinished
			var done float64
			if err := rows.Scan(&f.SprintID, &f.Members, &done, &f.Estimate); err != nil {
				t.Fatal(err)
			}
			f.Done = int(done)
			got[f.SprintID] = f
		}
		if err := errorsOf(rows); err != nil {
			t.Fatal(err)
		}
		for _, p := range pace {
			e := got[p.SprintID]
			e.SprintID = p.SprintID
			if e != p {
				t.Fatalf("export's sprint %s = %+v; the pace counts %+v", p.SprintID, e, p)
			}
		}
	}

	conn := exportConn(t)
	listMirrorBoards(t, s, defaultTestProjectID, mustConnectionOfRoot(t, s, root), "11")
	if _, ok, err := s.ExportMirrorBoard(context.Background(), conn, root, "10", MirrorRegroup{}, MirrorExportTables); err != nil || ok {
		t.Fatalf("export of a board no project lists = %v, %v; want nothing", ok, err)
	}
	if got := exportRows(t, conn, `SELECT count(*) FROM tickets`); got[0] != "0" {
		t.Fatalf("an unlisted board exported %s tickets", got[0])
	}
	if got := exportRows(t, conn, `SELECT name FROM pragma_database_list ORDER BY seq`); !reflect.DeepEqual(got, []string{"main"}) {
		t.Fatalf("databases after an export of nothing = %v; want main alone", got)
	}
}

func errorsOf(rows *sql.Rows) error {
	err := rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	return err
}

func mustConnectionOfRoot(t *testing.T, s *Store, root int64) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(`SELECT connection_id FROM tracker_cache_roots WHERE id = ?`, root).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// Every fill statement searches the file by the board's keys: no step
// scans a table of the file, so the export's cost follows the board's
// rows, not the mirror's or the database's size. A scan of the snapshot's
// own tables (the board's rows, already exported), of a bound JSON list
// or of a CTE is allowed.
func TestMirrorExportSearchesByTheBoard(t *testing.T) {
	t.Parallel()
	s, _, _ := newMirror(t)
	conn := exportConn(t)
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, MirrorExportSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `ATTACH ? AS `+mirrorExportSchemaName, mirrorExportURI(s.path)); err != nil {
		t.Fatal(err)
	}
	snapshot := map[string]bool{"l": true, "x": true, "o": true}
	for _, table := range MirrorExportTables {
		snapshot[table] = true
	}
	plans := append([]mirrorExportFill{{name: "board state", sql: mirrorExportBoardSQL}}, mirrorExportFills...)
	for _, fill := range plans {
		rows, err := conn.QueryContext(ctx, `EXPLAIN QUERY PLAN `+fill.sql, 1, "10", "{}", 0, 1)
		if err != nil {
			t.Fatalf("plan %s: %v", fill.name, err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		if err := errorsOf(rows); err != nil {
			t.Fatal(err)
		}
		for _, step := range plan {
			fields := strings.Fields(step)
			if len(fields) < 2 || fields[0] != "SCAN" {
				continue
			}
			if !snapshot[strings.TrimPrefix(fields[1], "main.")] && !strings.HasPrefix(fields[1], "json_each") && fields[1] != "CONSTANT" {
				t.Errorf("the export of %s scans %s:\n%s", fill.name, fields[1], strings.Join(plan, "\n"))
			}
		}
		if !slices.ContainsFunc(plan, func(step string) bool { return strings.HasPrefix(step, "SEARCH") }) {
			t.Errorf("the export of %s searches nothing:\n%s", fill.name, strings.Join(plan, "\n"))
		}
	}
}

// An export says the board's first read has finished only once a
// reconcile of it completed: not while its pages are being stored, nor
// after a read alone. A board unlisted and listed again reads first again
// until its next reconcile completes.
func TestMirrorExportSaysWhenTheFirstReadIsDone(t *testing.T) {
	t.Parallel()
	s, c, root := newMirror(t)
	done := func(step string, want bool) {
		t.Helper()
		export, ok, err := s.ExportMirrorBoard(context.Background(), exportConn(t), root, "10", MirrorRegroup{}, nil)
		if err != nil || !ok || export.FirstReadDone != want {
			t.Fatalf("%s: export = %+v, %v, %v; want the first read done %v", step, export, ok, err, want)
		}
	}
	record := func(step string, a MirrorSyncAttempt) {
		t.Helper()
		if _, _, err := s.RecordMirrorSyncAttempt(root, "10", currentAttempt(s, root, "10", a)); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 5, "a")), []byte("c1"))
	done("a page stored", false)
	record("a read", MirrorSyncAttempt{ReadAt: mirrorReadAt(10)})
	done("a read", false)
	record("a reconcile", MirrorSyncAttempt{ReconciledAt: mirrorReadAt(20)})
	done("a reconcile", true)

	listMirrorBoards(t, s, defaultTestProjectID, c.ID, "11")
	if _, ok, err := s.ExportMirrorBoard(context.Background(), exportConn(t), root, "10", MirrorRegroup{}, nil); err != nil || ok {
		t.Fatalf("export of the unlisted board = %v, %v; want nothing", ok, err)
	}
	listMirrorBoards(t, s, defaultTestProjectID, c.ID, "10")
	if err := s.FinishMirrorCleanup(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a")})
	done("listed again", false)
	record("a reconcile after the listing", MirrorSyncAttempt{ReconciledAt: mirrorReadAt(40)})
	done("a reconcile after the listing", true)
}

// An export copies the statuses its board names, not the root's catalog:
// the board's columns' and its tickets', and those its changes name only
// when it copies its changes. Board 11 of the same root has statuses of
// its own, which board 10's export holds none of; its people are the
// assignees alone unless it copies the changes.
func TestMirrorExportCopiesTheStatusesItsBoardNames(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	st := webStructure()
	st.Catalog.Statuses = append(st.Catalog.Statuses,
		MirrorStatus{ID: "2", Name: "Backlog", Group: MirrorGroupBacklog}, MirrorStatus{ID: "7", Name: "In Review", Group: MirrorGroupInProgress},
		MirrorStatus{ID: "8", Name: "Testing", Group: MirrorGroupInProgress}, MirrorStatus{ID: "9", Name: "Shipped", Group: MirrorGroupDone})
	st.Catalog.Users = append(st.Catalog.Users, MirrorUser{ID: "acc-user2", Name: "User 2", Data: []byte(`{"active":true}`)})
	mustReplaceStructure(t, s, root, st, true)
	other := webStructure()
	other.Board.ID = "11"
	other.Columns = []MirrorColumn{{Name: "Review", Statuses: []string{"7"}}, {Name: "Shipped", Statuses: []string{"9"}}}
	other.Sprints = nil
	mustReplaceStructure(t, s, root, other, true)
	mustWriteRowPage(t, s, root, webPage("10", sectionRow("10001", "a", "2", 1)), nil)
	mustWriteRowPage(t, s, root, webPage("11", sectionRow("10002", "a", "7", 1)), nil)
	h := []MirrorHistory{{IssueID: "10001", Changes: []MirrorChange{
		{Kind: MirrorChangeStatus, At: 5, AuthorID: "acc-user2", From: "8", To: "2"},
	}}}
	if _, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, h), MirrorCatalog{}, mirrorReadOf(root, 10)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name             string
		relations        []string
		statuses, people []string
		changes          string
	}{
		{"without its changes", []string{"statuses", "people"}, []string{"1", "10001", "2", "3", "6"}, []string{"acc-user1"}, "0"},
		{"with its changes", []string{"statuses", "people", "changes"}, []string{"1", "10001", "2", "3", "6", "8"}, []string{"acc-user1", "acc-user2"}, "1"},
	} {
		conn := exportConn(t)
		if _, ok, err := s.ExportMirrorBoard(context.Background(), conn, root, "10", MirrorRegroup{}, c.relations); err != nil || !ok {
			t.Fatalf("%s: export = %v, %v", c.name, ok, err)
		}
		if got := exportRows(t, conn, `SELECT id FROM statuses ORDER BY id`); !reflect.DeepEqual(got, c.statuses) {
			t.Errorf("%s: statuses = %v; want %v", c.name, got, c.statuses)
		}
		if got := exportRows(t, conn, `SELECT id FROM people ORDER BY id`); !reflect.DeepEqual(got, c.people) {
			t.Errorf("%s: people = %v; want %v", c.name, got, c.people)
		}
		if got := exportRows(t, conn, `SELECT count(*) FROM changes`); got[0] != c.changes {
			t.Errorf("%s: %s changes; want %s", c.name, got[0], c.changes)
		}
	}
	// An export of the epics fills the tickets their fill reads, and no
	// relation nobody asked for.
	conn := exportConn(t)
	if _, ok, err := s.ExportMirrorBoard(context.Background(), conn, root, "10", MirrorRegroup{}, []string{"epics"}); err != nil || !ok {
		t.Fatalf("export of the epics = %v, %v", ok, err)
	}
	if got := exportRows(t, conn, `SELECT (SELECT count(*) FROM tickets) || '|' || (SELECT count(*) FROM statuses) || '|' || (SELECT count(*) FROM sprints)`); got[0] != "1|0|3" {
		t.Fatalf("an export of the epics holds tickets|statuses|sprints %s; want the one ticket, no status, the board's 3 sprints", got[0])
	}
	if _, _, err := s.ExportMirrorBoard(context.Background(), exportConn(t), root, "10", MirrorRegroup{}, []string{"labels"}); err == nil ||
		!strings.Contains(err.Error(), `"labels" is not one of the export's relations`) {
		t.Fatalf("an export of an unknown relation = %v", err)
	}
}

// mirrorExportStepsPerRow is the most VM steps a fill may take per row of
// its board on the sized fixture (100 sprints, benchChanges changes a
// ticket), and a fill of the changes per row and change. Measured: the
// tickets fill, the largest, takes 289 a row; the membership fill took 731
// a row when it probed once per sprint for each ticket, and takes 37 by
// each ticket's key.
const mirrorExportStepsPerRow = 400

// Each fill reads what it copies by the rows' keys: its VM steps per board
// row stay within mirrorExportStepsPerRow on a board of 100 sprints, where
// a plan that every step searches (TestMirrorExportSearchesByTheBoard)
// can still probe once per sprint for every ticket. Each fill's steps are
// its statement's own counter (SQLITE_STMTSTATUS_VM_STEP).
func TestMirrorExportReadsEachRowOnce(t *testing.T) {
	t.Parallel()
	s, root, _ := openSizedMirror(t, benchMirrorSize{boards: 1, issues: 400})
	conn := exportConn(t)
	ctx := context.Background()
	for _, q := range []string{MirrorExportSchema, `ATTACH '` + mirrorExportURI(s.path) + `' AS ` + mirrorExportSchemaName} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	args := []driver.NamedValue{{Ordinal: 1, Value: root}, {Ordinal: 2, Value: benchBoardID(0)}, {Ordinal: 3, Value: "{}"}, {Ordinal: 4, Value: int64(0)}, {Ordinal: 5, Value: int64(1)}}
	steps := map[string]int{}
	err := conn.Raw(func(dc any) error {
		prep := dc.(driver.ConnPrepareContext)
		for _, fill := range mirrorExportFills {
			stmt, err := prep.PrepareContext(ctx, fill.sql)
			if err != nil {
				return fmt.Errorf("prepare %s: %w", fill.name, err)
			}
			if _, err := stmt.(driver.StmtExecContext).ExecContext(ctx, args); err != nil {
				return errors.Join(fmt.Errorf("fill %s: %w", fill.name, err), stmt.Close())
			}
			tls, pstmt, err := sqliteStmtHandles(stmt)
			if err != nil {
				return errors.Join(err, stmt.Close())
			}
			steps[fill.name] = int(sqlite3.Xsqlite3_stmt_status(tls, pstmt, sqlite3.SQLITE_STMTSTATUS_VM_STEP, 0))
			if err := stmt.Close(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := 400
	if got := exportRows(t, conn, `SELECT count(*) FROM tickets`); got[0] != strconv.Itoa(rows) {
		t.Fatalf("the board's copy holds %s tickets; want %d", got[0], rows)
	}
	for _, fill := range mirrorExportFills {
		bound := mirrorExportStepsPerRow
		if fill.relation == "changes" {
			bound *= 1 + benchChanges
		}
		t.Logf("%-26s %8d steps, %5d a row", fill.name, steps[fill.name], steps[fill.name]/rows)
		if steps[fill.name] > bound*rows {
			t.Errorf("the export of %s took %d VM steps for %d board rows, %d a row; want at most %d a row",
				fill.name, steps[fill.name], rows, steps[fill.name]/rows, bound)
		}
	}
}
