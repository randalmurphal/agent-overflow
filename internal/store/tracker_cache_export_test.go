package store

import (
	"context"
	"database/sql"
	"reflect"
	"slices"
	"strings"
	"testing"
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
		export, ok, err := s.ExportMirrorBoard(context.Background(), conn, root, "10", g)
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
	if _, ok, err := s.ExportMirrorBoard(context.Background(), conn, root, "10", MirrorRegroup{}); err != nil || ok {
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
	plans := append([]struct{ table, sql string }{{"board state", mirrorExportBoardSQL}}, mirrorExportFills...)
	for _, fill := range plans {
		rows, err := conn.QueryContext(ctx, `EXPLAIN QUERY PLAN `+fill.sql, 1, "10", "{}", 0, 1)
		if err != nil {
			t.Fatalf("plan %s: %v", fill.table, err)
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
				t.Errorf("the export of %s scans %s:\n%s", fill.table, fields[1], strings.Join(plan, "\n"))
			}
		}
		if !slices.ContainsFunc(plan, func(step string) bool { return strings.HasPrefix(step, "SEARCH") }) {
			t.Errorf("the export of %s searches nothing:\n%s", fill.table, strings.Join(plan, "\n"))
		}
	}
}
