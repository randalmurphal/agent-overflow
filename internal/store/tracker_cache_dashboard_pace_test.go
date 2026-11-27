package store_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/trackerdash"
)

// The dashboard's usual pace over the last five sprints is the Backlog's
// pace: the same sprints, the same members counted done at each
// completion, the same estimates, plain and regrouped; on a board without
// estimates both count the tickets. The Backlog's pace is the average of
// its sprints' sums (trackerapp's PaceView), computed here as it does.
func TestDashboardPaceIsTheBacklogsPace(t *testing.T) {
	t.Parallel()
	s, root := store.PaceFixtureForTest(t)
	regroup, err := store.NewMirrorRegroup(map[string]int{"6": store.MirrorGroupDone, "10001": store.MirrorGroupInProgress})
	if err != nil {
		t.Fatal(err)
	}
	e := trackerdash.NewEvaluator(func() time.Time { return time.UnixMilli(100_000) })
	for _, c := range []struct {
		name, board string
		regroup     store.MirrorRegroup
		rows        bool
	}{{"points", "10", store.MirrorRegroup{}, false}, {"regrouped", "10", regroup, false}, {"no estimates", "11", store.MirrorRegroup{}, true}} {
		pace := store.PaceForTest(t, s, root, c.board, c.regroup)
		backlog := 0.0
		for _, f := range pace {
			if c.rows {
				backlog += float64(f.Done)
			} else {
				backlog += f.Estimate
			}
		}
		backlog /= float64(len(pace))
		fill := func(ctx context.Context, conn *sql.Conn, relations []string) (store.MirrorExport, bool, error) {
			return s.ExportMirrorBoard(ctx, conn, root, c.board, c.regroup, store.OwnUnitForTest, relations, store.CardShapeForTest)
		}
		a, err := e.Evaluate(context.Background(), trackerdash.Board{Fill: fill, Epics: true},
			trackerdash.Request{Dashboard: "overview_sprints", Range: trackerdash.RangeSprints5, TimeZone: "UTC"})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var got *trackerdash.Num
		for _, w := range a.Widgets {
			if w.ID == "pace" {
				got = w.Value
			}
		}
		if got == nil || got.V == nil || *got.V != backlog || len(a.Range.Periods) != len(pace) {
			t.Fatalf("%s: the dashboard's pace = %+v over %d sprints; the Backlog's is %v over %d (%+v)", c.name, got, len(a.Range.Periods), backlog, len(pace), pace)
		}
	}
}
