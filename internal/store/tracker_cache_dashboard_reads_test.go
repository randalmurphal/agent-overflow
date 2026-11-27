package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/trackerdash"
)

// countRelations are the relations every count copies beside its
// metrics' reads (trackerdash's boardRelations): what the answer names.
var countRelations = []string{"epics", "people", "statuses", "tickets"}

// A count copies what the statement it runs reads as SQLite plans it and
// nothing else, and "How this is counted" names those relations: each
// case says the relations its query reads, tickets always (the count
// looks up each row's ticket on the board), and the on-demand columns it
// reads. Its
// rows on that copy are its rows on a reference copy of every relation
// and column, so a read the count missed is a different answer here and
// not a silent zero. A new export relation or on-demand column adds rows
// for its USING, NATURAL and ON reads.
//
// The reference copies changes only for a query that reads them: copying
// them adds the people they name to people (the export's contract), which
// a query that reads people and no changes must not count.
func TestADashboardCountCopiesWhatItsQueryReads(t *testing.T) {
	t.Parallel()
	s, root := store.DashboardReadsFixtureForTest(t)
	e := trackerdash.NewEvaluator(func() time.Time { return time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC) })
	bom := string(rune(0xFEFF))
	sum := trackerdash.ReduceSum
	viaTickets := `WITH c AS (SELECT id AS ticket FROM tickets) SELECT c.ticket FROM c `
	cases := []struct {
		name, sql string
		reduce    trackerdash.Reducer
		reads     []string // what How this is counted names
		columns   []string // the on-demand columns asked for
		rows      int
	}{
		{"type USING", `SELECT t.id AS ticket FROM tickets t JOIN (SELECT 'Bug' AS type) k USING(type)`, "", []string{"tickets"}, []string{"tickets.type"}, 1},
		{"type NATURAL", `SELECT id AS ticket FROM tickets NATURAL JOIN (SELECT 'Bug' AS type)`, "", []string{"tickets"}, []string{"tickets.type"}, 1},
		{"type ON", `SELECT t.id AS ticket FROM tickets t JOIN (SELECT 'Bug' AS k) k ON k.k = t.type`, "", []string{"tickets"}, []string{"tickets.type"}, 1},
		{"priority USING", `SELECT t.id AS ticket FROM tickets t JOIN (SELECT 'High' AS priority) k USING(priority)`, "", []string{"tickets"}, []string{"tickets.priority"}, 1},
		{"priority NATURAL", `SELECT id AS ticket FROM tickets NATURAL JOIN (SELECT 'High' AS priority)`, "", []string{"tickets"}, []string{"tickets.priority"}, 1},
		{"priority ON", `SELECT t.id AS ticket FROM tickets t JOIN (SELECT 'High' AS k) k ON k.k = t.priority`, "", []string{"tickets"}, []string{"tickets.priority"}, 1},
		{"labels USING", viaTickets + `JOIN labels USING(ticket)`, "", []string{"labels", "tickets"}, nil, 1},
		{"labels NATURAL", viaTickets + `NATURAL JOIN labels`, "", []string{"labels", "tickets"}, nil, 1},
		{"labels ON", `SELECT t.id AS ticket FROM tickets t JOIN labels l ON l.ticket = t.id`, "", []string{"labels", "tickets"}, nil, 1},
		{"sprint_tickets USING", viaTickets + `JOIN sprint_tickets USING(ticket)`, "", []string{"sprint_tickets", "tickets"}, nil, 2},
		{"sprint_tickets NATURAL", viaTickets + `NATURAL JOIN sprint_tickets`, "", []string{"sprint_tickets", "tickets"}, nil, 2},
		{"sprint_tickets ON", `SELECT t.id AS ticket FROM tickets t JOIN sprint_tickets m ON m.ticket = t.id`, "", []string{"sprint_tickets", "tickets"}, nil, 2},
		{"changes USING", viaTickets + `JOIN changes USING(ticket)`, "", []string{"changes", "tickets"}, nil, 1},
		{"changes NATURAL", viaTickets + `NATURAL JOIN changes`, "", []string{"changes", "tickets"}, nil, 1},
		{"changes ON", `SELECT t.id AS ticket FROM tickets t JOIN changes c ON c.ticket = t.id`, "", []string{"changes", "tickets"}, nil, 1},
		{"sprint_finished USING", viaTickets + `JOIN sprint_finished USING(ticket)`, "", []string{"sprint_finished", "tickets"}, nil, 2},
		{"sprint_finished NATURAL", viaTickets + `NATURAL JOIN sprint_finished`, "", []string{"sprint_finished", "tickets"}, nil, 2},
		{"sprint_finished ON", `SELECT t.id AS ticket FROM tickets t JOIN sprint_finished f ON f.ticket = t.id`, "", []string{"sprint_finished", "tickets"}, nil, 2},
		{"a CTE named changes", `WITH changes AS (SELECT id AS ticket FROM tickets) SELECT ticket FROM changes`, "", []string{"tickets"}, nil, 3},
		{"a CTE named labels", `WITH labels AS (SELECT id AS ticket FROM tickets) SELECT ticket FROM labels`, "", []string{"tickets"}, nil, 3},
		{"a label filter naming changes", `SELECT ticket FROM labels WHERE name IN ('frontend', 'changes')`, "", []string{"labels", "tickets"}, nil, 1},
		{"a product", `SELECT id AS ticket, points * 2 AS value FROM tickets`, sum, []string{"tickets"}, nil, 3},
		{"count(*)", `SELECT id AS ticket FROM tickets WHERE (SELECT count(*) FROM tickets) > 0`, "", []string{"tickets"}, nil, 3},
		{"a windowed count(*)", `SELECT id AS ticket, count(*) OVER () AS value FROM tickets`, sum, []string{"tickets"}, nil, 3},
		{"* through a CTE", `WITH c AS (SELECT * FROM tickets) SELECT id AS ticket FROM c`, "", []string{"tickets"}, nil, 3},
		{"U+FEFF before labels", `SELECT ticket FROM ` + bom + `labels`, "", []string{"labels", "tickets"}, nil, 1},
		{"U+FEFF before type", `SELECT id AS ticket FROM tickets WHERE ` + bom + `type = 'Bug'`, "", []string{"tickets"}, []string{"tickets.type"}, 1},
		{"people beside a CTE named changes", `WITH changes AS (SELECT id AS ticket FROM tickets WHERE open = 1) SELECT ticket FROM changes WHERE (SELECT count(*) FROM people) = 1`, "", []string{"people", "tickets"}, nil, 2},
		{"an unread LEFT JOIN of changes under DISTINCT", `SELECT DISTINCT t.id AS ticket FROM tickets t LEFT JOIN changes c ON c.ticket = t.id`, "", []string{"tickets"}, nil, 3},
		{"an unread LEFT JOIN of labels under DISTINCT", `SELECT DISTINCT t.id AS ticket FROM tickets t LEFT JOIN labels l ON l.ticket = t.id`, "", []string{"tickets"}, nil, 3},
		{"an unread LEFT JOIN of a parent under DISTINCT", `SELECT DISTINCT t.id AS ticket FROM tickets t LEFT JOIN tickets p ON p.id = t.parent AND p.priority = 'High'`, "", []string{"tickets"}, nil, 3},
		{"people beside an unread LEFT JOIN of changes", `WITH t AS (SELECT id AS ticket FROM tickets) SELECT DISTINCT t.ticket FROM t LEFT JOIN changes USING (ticket) WHERE (SELECT count(*) FROM people) = 1`, "", []string{"people", "tickets"}, nil, 3},
		{"an index hint", `SELECT ticket FROM sprint_tickets INDEXED BY sqlite_autoindex_sprint_tickets_1 WHERE sprint = '21'`, "", []string{"sprint_tickets", "tickets"}, nil, 2},
		{"an index hint on tickets", `SELECT id AS ticket FROM tickets INDEXED BY sqlite_autoindex_tickets_1`, "", []string{"tickets"}, nil, 3},
		{"a bare rowid, the board's", `SELECT t.id AS ticket FROM board, tickets t WHERE rowid > 0`, "", []string{"board", "tickets"}, nil, 3},
		// The known limit: the program SQLite emits can hold reads it never
		// runs. A grouped join whose only use SQLite removed keeps its
		// materialization in the program, so the count asks for changes.
		{"known limit: a grouped join SQLite never runs", `WITH enriched AS (SELECT t.id, c.newest FROM tickets t LEFT JOIN (SELECT ticket, max(at) AS newest FROM changes GROUP BY ticket) c ON c.ticket = t.id) SELECT DISTINCT id AS ticket FROM enriched`, "", []string{"changes", "tickets"}, nil, 3},
		{"the same grouped join, read", `WITH enriched AS (SELECT t.id, c.newest FROM tickets t LEFT JOIN (SELECT ticket, max(at) AS newest FROM changes GROUP BY ticket) c ON c.ticket = t.id) SELECT DISTINCT id AS ticket FROM enriched WHERE newest IS NOT NULL`, "", []string{"changes", "tickets"}, nil, 1},
		{"people beside a CTE named c", `WITH c AS (SELECT id AS ticket FROM tickets WHERE open = 1) SELECT ticket FROM c WHERE (SELECT count(*) FROM people) = 1`, "", []string{"people", "tickets"}, nil, 2},
	}
	try := func(t *testing.T, m trackerdash.Metric, fill func([]string) []string) (trackerdash.Tried, []string) {
		t.Helper()
		var asked []string
		b := trackerdash.Board{Fill: func(ctx context.Context, conn *sql.Conn, relations []string) (store.MirrorExport, bool, error) {
			asked = relations
			return s.ExportMirrorBoard(ctx, conn, root, "10", store.MirrorRegroup{}, fill(relations), 0, store.CardShapeForTest)
		}}
		tried, err := e.Try(context.Background(), b, trackerdash.Widget{Form: trackerdash.FormNumber, Width: 3}, m, "UTC")
		if err != nil {
			t.Fatal(err)
		}
		return tried, asked
	}
	answer := func(t *testing.T, tried trackerdash.Tried) string {
		t.Helper()
		ids := make([]string, len(tried.Rows))
		for i, r := range tried.Rows {
			ids[i] = r.ID
		}
		value, err := json.Marshal(tried.Value.Value)
		if err != nil {
			t.Fatal(err)
		}
		return string(value) + " " + strings.Join(ids, ",")
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := trackerdash.Metric{Name: "m", Title: c.name, Words: []string{"A count."}, SQL: c.sql, Reduce: cmpOr(c.reduce, trackerdash.ReduceCount), Unit: trackerdash.UnitTickets}
			tried, asked := try(t, m, func(relations []string) []string { return relations })
			want := slices.Concat(countRelations, c.reads, c.columns)
			slices.Sort(want)
			if want = slices.Compact(want); !slices.Equal(asked, want) {
				t.Errorf("the count asked for %v; want %v", asked, want)
			}
			if !slices.Equal(tried.View.How.Reads, c.reads) {
				t.Errorf("How this is counted reads %v; want %v", tried.View.How.Reads, c.reads)
			}
			if tried.Count != c.rows {
				t.Errorf("%d rows; want %d", tried.Count, c.rows)
			}
			reference, _ := try(t, m, func([]string) []string {
				all := slices.Concat(store.MirrorExportTables, store.MirrorExportColumns)
				if !slices.Contains(c.reads, "changes") {
					all = slices.DeleteFunc(all, func(r string) bool { return r == "changes" })
				}
				return all
			})
			if got, ref := answer(t, tried), answer(t, reference); got != ref {
				t.Errorf("on the copy it asked for: %s; on every relation and column: %s", got, ref)
			}
		})
	}
}

func cmpOr[T comparable](a, b T) T {
	var zero T
	if a != zero {
		return a
	}
	return b
}
