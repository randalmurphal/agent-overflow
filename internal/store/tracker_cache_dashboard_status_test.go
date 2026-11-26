package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/trackerdash"
)

// A dashboard counts a ticket open, or not at all, as the board's sections
// do, through a real export: a Backlog and an unclassified status are To
// do, a status the project leaves out of counts is in no widget, Created
// included, and a canceled ticket is not open. On a sprint board the
// active sprint's done tickets are Done.
func TestDashboardCountsStatusesAsTheSectionsDo(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	s, root, regroup := store.DashboardStatusFixtureForTest(t, now.Add(-24*time.Hour).UnixMilli())
	e := trackerdash.NewEvaluator(func() time.Time { return now })
	evaluate := func(board, dashboard, rng string) trackerdash.Answer {
		t.Helper()
		fill := func(ctx context.Context, conn *sql.Conn, relations []string) (store.MirrorExport, bool, error) {
			return s.ExportMirrorBoard(ctx, conn, root, board, regroup, relations)
		}
		a, err := e.Evaluate(context.Background(), trackerdash.Board{Fill: fill, Epics: true},
			trackerdash.Request{Dashboard: dashboard, Range: rng, TimeZone: "UTC"})
		if err != nil {
			t.Fatalf("board %s: %v", board, err)
		}
		return a
	}
	widget := func(a trackerdash.Answer, id string) trackerdash.WidgetValue {
		t.Helper()
		for _, w := range a.Widgets {
			if w.ID == id {
				if w.Error != "" {
					t.Fatalf("%s failed: %s", id, w.Error)
				}
				return w
			}
		}
		t.Fatalf("no widget %s", id)
		return trackerdash.WidgetValue{}
	}
	cells := func(w trackerdash.WidgetValue) []string {
		var out []string
		for _, b := range w.Buckets {
			row := b.Label
			for _, c := range b.Cells {
				row += fmt.Sprintf(" %g", *c.V)
			}
			out = append(out, row)
		}
		return out
	}

	sprints := evaluate("10", "overview_sprints", trackerdash.RangeSprints5)
	if got := cells(widget(sprints, "carrying")); !reflect.DeepEqual(got, []string{"User 1 3 4 5"}) {
		t.Fatalf("sprint board's carrying = %v; want To do WEB-1 (Backlog) and WEB-2 (unclassified), Doing WEB-4, Done WEB-5", got)
	}
	if got := widget(sprints, "open_by_epic").Buckets; len(got) != 1 || *got[0].Total.V != 7 {
		t.Fatalf("sprint board's open work = %+v; want WEB-1, WEB-2 and WEB-4", got)
	}

	weeks := evaluate("11", "overview_weeks", trackerdash.RangeWeeks2)
	if got := cells(widget(weeks, "carrying")); !reflect.DeepEqual(got, []string{"User 1 3 4"}) {
		t.Fatalf("week board's carrying = %v; want To do WEB-11 (Backlog) and WEB-12 (unclassified), Doing WEB-14", got)
	}
	created := 0.0
	for _, b := range widget(weeks, "created_finished").Buckets {
		created += *b.Cells[0].V
	}
	if created != 5 {
		t.Fatalf("created = %v; want every ticket but WEB-13, whose status is left out of counts", created)
	}
}

// Start to done counts what a ticket's history shows, through a real
// export: a Linear ticket whose history begins as To Do shows its start,
// though its history begins minutes after its creation; one made in
// progress before its history begins does not, and is listed as not
// counted; a Jira history, which begins at creation, shows its start, and
// one of a ticket made in done shows none, in words true of both.
func TestStartToDoneCountsWhatTheHistoryShows(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	day := int64(24 * time.Hour / time.Millisecond)
	// Monday 16 November, noon: WEB-21 is in progress Tuesday to Thursday,
	// WEB-23 Tuesday to Wednesday.
	s, root := store.DashboardHistoryFixtureForTest(t, time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC).UnixMilli(), day)
	b := trackerdash.Board{Fill: func(ctx context.Context, conn *sql.Conn, relations []string) (store.MirrorExport, bool, error) {
		return s.ExportMirrorBoard(ctx, conn, root, "11", store.MirrorRegroup{}, relations)
	}}
	r := trackerdash.Request{Dashboard: "overview_weeks", Range: trackerdash.RangeWeeks2, TimeZone: "UTC"}
	d, err := trackerdash.NewEvaluator(func() time.Time { return now }).Drill(context.Background(), b, trackerdash.DrillRequest{Request: r, Widget: "start_to_done"})
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, row := range d.Rows {
		v := "-"
		if row.Value != nil {
			v = fmt.Sprint(*row.Value)
		}
		rows = append(rows, fmt.Sprintf("%s %s %s", row.Key, v, row.Note))
	}
	want := []string{"WEB-21 2 ", "WEB-23 1 ", "WEB-22 - its history shows no start", "WEB-24 - its history shows no start"}
	if d.Value.V == nil || *d.Value.V != 1.5 || d.Value.NC != 2 || !reflect.DeepEqual(rows, want) {
		t.Fatalf("start to done = %+v with rows %q; want the median 1.5 of WEB-21 and WEB-23, WEB-22 and WEB-24 not counted: %q", d.Value, rows, want)
	}
}

// A count copies what its metrics read, through a real export of a board
// whose tickets have changes: no built-in reads the changes, so neither
// the Overview's copy nor a Stuck drill's holds any, and the drill asks
// for the relations its metric and the rows' labels read, no sprint's.
func TestACountCopiesWhatItsMetricsRead(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	s, root := store.DashboardHistoryFixtureForTest(t, time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC).UnixMilli(), int64(24*time.Hour/time.Millisecond))
	var asked [][]string
	var changes []int
	b := trackerdash.Board{Fill: func(ctx context.Context, conn *sql.Conn, relations []string) (store.MirrorExport, bool, error) {
		export, ok, err := s.ExportMirrorBoard(ctx, conn, root, "11", store.MirrorRegroup{}, relations)
		var n int
		if err == nil {
			err = conn.QueryRowContext(ctx, `SELECT count(*) FROM changes`).Scan(&n)
		}
		asked, changes = append(asked, relations), append(changes, n)
		return export, ok, err
	}}
	e := trackerdash.NewEvaluator(func() time.Time { return now })
	r := trackerdash.Request{Dashboard: "overview_weeks", Range: trackerdash.RangeWeeks2, TimeZone: "UTC"}
	if _, err := e.Evaluate(context.Background(), b, r); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Drill(context.Background(), b, trackerdash.DrillRequest{Request: r, Widget: "stuck"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(changes, []int{0, 0}) || slices.Contains(asked[0], "changes") {
		t.Fatalf("the copies held %v changes, the Overview asked for %v; want none copied", changes, asked[0])
	}
	if want := []string{"epics", "people", "statuses", "tickets"}; !reflect.DeepEqual(asked[1], want) {
		t.Fatalf("the Stuck drill asked for %v; want %v", asked[1], want)
	}
}
