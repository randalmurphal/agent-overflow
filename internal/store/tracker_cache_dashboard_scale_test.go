package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/trackerdash"
)

// A board's built-in dashboard at the sizes AO_TRACKER_SCALE names, one
// board of n issues with 10 changes each and 100 sprints (the store's
// scale fixture): the store's fill of a session alone, a whole
// evaluation (open, fill, seal, every metric, reduce) over the last 5
// sprints and over this sprint, a drill's first page, the bytes of
// each answer as JSON, and what an evaluation allocates. Each is the
// median and spread of 5 runs. Unset, the test is skipped.
//
//	AO_TRACKER_SCALE=2000 make go-test GO_TEST_PKGS=./internal/store \
//	  GO_TEST_FLAGS="-count=1 -v -run TestDashboardAtScale"
func TestDashboardAtScale(t *testing.T) {
	env := os.Getenv("AO_TRACKER_SCALE")
	if env == "" {
		t.Skip("AO_TRACKER_SCALE names no sizes")
	}
	for field := range strings.SplitSeq(env, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || n < 100 {
			t.Fatalf("AO_TRACKER_SCALE=%q: each size is a whole number of at least 100", env)
		}
		t.Run(fmt.Sprint(n), func(t *testing.T) { dashboardAtScale(t, n) })
	}
}

func dashboardAtScale(t *testing.T, n int) {
	s, root, board := store.ScaleMirrorForTest(t, n)
	var fills []time.Duration
	fill := func(ctx context.Context, conn *sql.Conn) (store.MirrorExport, bool, error) {
		start := time.Now()
		export, ok, err := s.ExportMirrorBoard(ctx, conn, root, board, store.MirrorRegroup{})
		fills = append(fills, time.Since(start))
		return export, ok, err
	}
	b := trackerdash.Board{Fill: fill, Epics: true, Viewer: "acc-user1"}
	e := trackerdash.NewEvaluator(time.Now)
	ctx := context.Background()
	spread := func(ds []time.Duration) string {
		sorted := slices.Sorted(slices.Values(ds))
		return fmt.Sprintf("%v (%v to %v)", sorted[len(sorted)/2].Round(time.Microsecond), sorted[0].Round(time.Microsecond), sorted[len(sorted)-1].Round(time.Microsecond))
	}
	for _, rng := range []string{trackerdash.RangeSprints5, trackerdash.RangeThisSprint} {
		fills = nil
		var times []time.Duration
		var answer trackerdash.Answer
		var before, after runtime.MemStats
		for rep := range 5 {
			runtime.GC()
			runtime.ReadMemStats(&before)
			start := time.Now()
			a, err := e.Evaluate(ctx, b, trackerdash.Request{Dashboard: "overview_sprints", Range: rng, TimeZone: "UTC"})
			times = append(times, time.Since(start))
			runtime.ReadMemStats(&after)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range a.Widgets {
				if w.Error != "" {
					t.Fatalf("widget %s failed: %s", w.ID, w.Error)
				}
			}
			if rep == 4 {
				answer = a
			}
		}
		body, err := json.Marshal(answer)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%d issues, %s: evaluation %s, of which the fill %s; answer %d bytes as JSON; %.1f MB allocated in %d allocations",
			n, rng, spread(times), spread(fills), len(body), float64(after.TotalAlloc-before.TotalAlloc)/(1<<20), after.Mallocs-before.Mallocs)
	}
	// A drill of the stuck tickets, the widget with the most rows on this
	// fixture: its first page of 100.
	var times []time.Duration
	var drill trackerdash.DrillAnswer
	var err error
	for range 5 {
		start := time.Now()
		drill, err = e.Drill(ctx, b, trackerdash.DrillRequest{Request: trackerdash.Request{Dashboard: "overview_sprints", Range: trackerdash.RangeSprints5, TimeZone: "UTC"}, Widget: "stuck"})
		times = append(times, time.Since(start))
		if err != nil {
			t.Fatal(err)
		}
	}
	body, err := json.Marshal(drill)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d issues, drill of the stuck tickets (%d rows, %d on the page): %s; answer %d bytes as JSON", n, drill.Count, len(drill.Rows), spread(times), len(body))
}
