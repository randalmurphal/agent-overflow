package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
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
		fill := func(ctx context.Context, conn *sql.Conn) (store.MirrorExport, bool, error) {
			return s.ExportMirrorBoard(ctx, conn, root, board, regroup)
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
