package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/trackerdash"
)

// oldStuckSQL is Stuck right now before the status intervals: the tickets
// in progress now for more than 3 days by their status_since.
const oldStuckSQL = `SELECT t.id AS ticket,
  iif(coalesce(t.status_since, 0) > 0, days(t.status_since, :now), NULL) AS value,
  iif(coalesce(t.status_since, 0) > 0,
      'in ' || coalesce(s.name, t.status) || ' for ' || CAST(days(t.status_since, :now) AS INTEGER) || ' days',
      'how long it has been in ' || coalesce(s.name, t.status) || ' is not known') AS note
FROM tickets t LEFT JOIN statuses s ON s.id = t.status
WHERE t.status_group = 'in_progress' AND (coalesce(t.status_since, 0) = 0 OR days(t.status_since, :now) > 3)`

// Stuck right now over the status intervals counts now what it counted
// over status_since, on real exports: the same number, the same tickets
// not counted, and the same list in the same order with the same notes.
// The intervals fixture holds tickets in progress since before the
// window, since its edge and since inside it, one whose history ends in
// another status than its card's, one with no history, and stays that
// ended, counted at 10 days on and exactly at 3 days after the latest
// stay began (not stuck) and a millisecond later (stuck); the status
// fixture a ticket of each kind of status on a sprint board and on one
// without sprints.
func TestStuckOverTheIntervalsCountsNowAsOverStatusSince(t *testing.T) {
	t.Parallel()
	intervals, intervalsRoot, intervalsWindow := store.IntervalsFixtureForTest(t)
	created := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	statuses, statusesRoot, regroup := store.DashboardStatusFixtureForTest(t, created.UnixMilli())
	edge := int64(12_000 + 3*24*3600*1000)
	for _, c := range []struct {
		name    string
		s       *store.Store
		root    int64
		board   string
		regroup store.MirrorRegroup
		window  int64
		now     time.Time
		want    string
	}{
		{"intervals", intervals, intervalsRoot, "10", store.MirrorRegroup{}, intervalsWindow, time.UnixMilli(20_000 + 10*24*3600*1000), "3, 1 not counted"},
		{"intervals, at 3 days", intervals, intervalsRoot, "10", store.MirrorRegroup{}, intervalsWindow, time.UnixMilli(edge), "2, 1 not counted"},
		{"intervals, past 3 days", intervals, intervalsRoot, "10", store.MirrorRegroup{}, intervalsWindow, time.UnixMilli(edge + 1), "3, 1 not counted"},
		{"statuses, sprints", statuses, statusesRoot, "10", regroup, 0, created.Add(5 * 24 * time.Hour), ""},
		{"statuses, weeks", statuses, statusesRoot, "11", regroup, 0, created.Add(5 * 24 * time.Hour), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := trackerdash.NewEvaluator(func() time.Time { return c.now })
			b := trackerdash.Board{Fill: func(ctx context.Context, conn *sql.Conn, relations []string) (store.MirrorExport, bool, error) {
				return c.s.ExportMirrorBoard(ctx, conn, c.root, c.board, c.regroup, relations, c.window, store.CardShapeForTest)
			}}
			stuck := builtinStuck(t, e, b)
			read := func(sql string, form trackerdash.Form) string {
				m := trackerdash.Metric{Name: "stuck", Title: "Stuck right now", Words: stuck.Words, SQL: sql, Reduce: stuck.Reduce, Unit: stuck.Unit}
				tried, err := e.Try(context.Background(), b, trackerdash.Widget{Form: form, Width: 12}, m, "UTC")
				if err != nil {
					t.Fatal(err)
				}
				out := fmt.Sprintf("%s; %d rows", numText(tried.Value.Value), tried.Count)
				for _, r := range append(tried.Rows, tried.Value.Tickets...) {
					v := "-"
					if r.Value != nil {
						v = fmt.Sprint(*r.Value)
					}
					out += fmt.Sprintf("\n%s %v %s %s", r.Key, r.Counted, v, r.Note)
				}
				return out
			}
			for _, form := range []trackerdash.Form{trackerdash.FormNumber, trackerdash.FormList} {
				old, now := read(oldStuckSQL, form), read(stuck.SQL, form)
				if old != now {
					t.Fatalf("%s: over status_since\n%s\nover the intervals\n%s", form, old, now)
				}
				if form == trackerdash.FormNumber {
					t.Logf("%s", old)
					if c.want != "" && !strings.HasPrefix(old, c.want+";") {
						t.Fatalf("stuck = %s; want %s", old, c.want)
					}
				}
			}
		})
	}
}

// builtinStuck is the built-in Stuck right now metric, as Try counts a
// metric: its SQL, words and reducer, read from the evaluator's listing.
func builtinStuck(t *testing.T, e *trackerdash.Evaluator, b trackerdash.Board) trackerdash.Metric {
	t.Helper()
	l, err := e.Dashboards(context.Background(), trackerdash.Kind{PlansSprints: true}, store.MirrorEstimatePoints, trackerdash.Own{})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range l.Dashboards[0].Widgets {
		if w.ID == "stuck" {
			return trackerdash.Metric{Words: w.How.Words, SQL: w.How.SQL, Reduce: trackerdash.ReduceCount, Unit: trackerdash.UnitTickets}
		}
	}
	t.Fatal("no stuck widget")
	return trackerdash.Metric{}
}

func numText(n *trackerdash.Num) string {
	switch {
	case n == nil:
		return "absent"
	case n.V == nil:
		return fmt.Sprintf("not known, %d not counted", n.NC)
	}
	return fmt.Sprintf("%g, %d not counted", *n.V, n.NC)
}

// The burndown counts a stretch open by the store's one rule of a status
// that is not done, the rule tickets.open follows: on a board with each
// kind of status and a regrouping that leaves In Review out, the
// burndown's start is the estimate of the tickets the export calls open,
// Backlog, the unclassified status and In Progress, 1 + 2 + 4.
func TestTheBurndownCountsOpenAsTheTicketsDo(t *testing.T) {
	t.Parallel()
	created := time.Now().Add(-24 * time.Hour).UnixMilli()
	s, root, regroup := store.DashboardOpenFixtureForTest(t, created)
	e := trackerdash.NewEvaluator(time.Now)
	b := trackerdash.Board{Fill: func(ctx context.Context, conn *sql.Conn, relations []string) (store.MirrorExport, bool, error) {
		return s.ExportMirrorBoard(ctx, conn, root, "10", regroup, relations, 0, store.CardShapeForTest)
	}}
	a, err := e.Evaluate(context.Background(), b, trackerdash.Request{Dashboard: "overview_sprints", TimeZone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	var start string
	for _, w := range a.Widgets {
		if w.ID == "burndown" && len(w.Buckets) > 0 {
			start = numText(&w.Buckets[0].Cells[0])
		}
	}
	m := trackerdash.Metric{Name: "w_1", Title: "Open", Words: []string{"Open tickets' estimate."}, Reduce: trackerdash.ReduceSum, Unit: trackerdash.UnitEstimate,
		SQL: `SELECT t.id AS ticket, t.points AS value FROM tickets t WHERE t.open = 1`}
	tried, err := e.Try(context.Background(), b, trackerdash.Widget{Form: trackerdash.FormNumber, Width: 3}, m, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if open := numText(tried.Value.Value); start != "7, 0 not counted" || open != start {
		t.Fatalf("the burndown's start = %s, the open tickets' estimate %s; want 7 both", start, open)
	}
}
