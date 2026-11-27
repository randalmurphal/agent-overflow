package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil/procstate"
	"agent-overflow/internal/trackerdash"
)

// A board's built-in dashboard at the sizes AO_TRACKER_SCALE names, one
// board of n issues with 10 changes each, five of them status changes,
// and 13 dated two-week sprints, the last active (the store's fixture
// with real times): every issue changed inside the window, and 10% of
// them. It measures the store's fill of a session alone, a whole
// evaluation (open, fill, seal, every metric, reduce) over the last 5
// sprints and over this sprint, a drill's first page, the bytes of each
// answer as JSON, and what an evaluation allocates, once the burndown
// has drawn a value and the sprint table counted a cell. Each is the
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
		t.Run(fmt.Sprint(n, " all changed"), func(t *testing.T) { dashboardAtScale(t, n, 100) })
		t.Run(fmt.Sprint(n, " recent 10%"), func(t *testing.T) { dashboardAtScale(t, n, 10) })
	}
}

// dashboardAtScale measures a board of n issues of the fixture with real
// times, recent percent of them changed inside the window, counted at its
// now.
func dashboardAtScale(t *testing.T, n, recent int) {
	s, root, board := store.ScaleMirrorForTest(t, n, recent)
	window, now := store.BenchWindowForTest, func() time.Time { return time.UnixMilli(store.BenchNowForTest) }
	var fills []time.Duration
	fill := func(ctx context.Context, conn *sql.Conn, relations []string) (store.MirrorExport, bool, error) {
		start := time.Now()
		export, ok, err := s.ExportMirrorBoard(ctx, conn, root, board, store.MirrorRegroup{}, relations, window, math.MaxInt64, store.CardShapeForTest)
		fills = append(fills, time.Since(start))
		return export, ok, err
	}
	b := trackerdash.Board{Fill: fill, Epics: true, Viewer: "acc-user1"}
	e := trackerdash.NewEvaluator(now)
	ctx := context.Background()
	spread := func(ds []time.Duration) string {
		sorted := slices.Sorted(slices.Values(ds))
		return fmt.Sprintf("%v (%v to %v)", sorted[len(sorted)/2].Round(time.Microsecond), sorted[0].Round(time.Microsecond), sorted[len(sorted)-1].Round(time.Microsecond))
	}
	// The Overview's copy alone, as its count asks for it and without the
	// intervals, the copy before they existed: the relations of one
	// count's fill, then 5 fills of each on a new database.
	var asked []string
	b.Fill = func(ctx context.Context, conn *sql.Conn, relations []string) (store.MirrorExport, bool, error) {
		asked = slices.Clone(relations)
		return s.ExportMirrorBoard(ctx, conn, root, board, store.MirrorRegroup{}, relations, window, math.MaxInt64, store.CardShapeForTest)
	}
	if _, err := e.Evaluate(ctx, b, trackerdash.Request{Dashboard: "overview_sprints", Range: trackerdash.RangeSprints5, TimeZone: "UTC"}); err != nil {
		t.Fatal(err)
	}
	b.Fill = fill
	// What is measured draws the burndown and counts the sprint table.
	drawn, err := e.Evaluate(ctx, b, trackerdash.Request{Dashboard: "overview_sprints", Range: trackerdash.RangeSprints5, TimeZone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range drawn.Widgets {
		switch {
		case w.ID == "burndown" && (w.Note != "" || len(w.Buckets) == 0 || w.Buckets[0].Cells[0].V == nil):
			t.Fatalf("the burndown draws no start: %+v", w)
		case w.ID == "sprint_scope" && !slices.ContainsFunc(w.Buckets, func(b trackerdash.BucketValue) bool {
			return slices.ContainsFunc(b.Cells, func(c trackerdash.Num) bool { return c.V != nil && *c.V > 0 })
		}):
			t.Fatalf("the sprint table counts no cell: %+v", w.Buckets)
		}
	}
	fills = nil
	withoutIntervals := slices.DeleteFunc(slices.Clone(asked), func(r string) bool { return strings.HasSuffix(r, "_intervals") })
	for _, copied := range []struct {
		label     string
		relations []string
	}{{"without the intervals", withoutIntervals}, {"with the status intervals", append(slices.Clone(withoutIntervals), "status_intervals")},
		{"with both intervals", append(slices.Clone(withoutIntervals), "status_intervals", "sprint_intervals")}, {"as the Overview asks", asked}} {
		var times []time.Duration
		for range 5 {
			d, err := timedExport(ctx, s, root, board, copied.relations, window)
			if err != nil {
				t.Fatal(err)
			}
			times = append(times, d)
		}
		t.Logf("%d issues, %d%% recent, the Overview's copy %s (%s): %s", n, recent, copied.label, strings.Join(copied.relations, ", "), spread(times))
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
		t.Logf("%d issues, %d%% recent, %s: evaluation %s, of which the fill %s; answer %d bytes as JSON; %.1f MB allocated in %d allocations",
			n, recent, rng, spread(times), spread(fills), len(body), float64(after.TotalAlloc-before.TotalAlloc)/(1<<20), after.Mallocs-before.Mallocs)
	}
	// A drill of the stuck tickets, the widget with the most rows on this
	// fixture: its first page of 100.
	var times []time.Duration
	var drill trackerdash.DrillAnswer
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
	t.Logf("%d issues, %d%% recent, drill of the stuck tickets (%d rows, %d on the page): %s; answer %d bytes as JSON", n, recent, drill.Count, len(drill.Rows), spread(times), len(body))
}

// timedExport is how long one export of relations of board takes, on a
// new private in-memory database as an evaluation's.
func timedExport(ctx context.Context, s *store.Store, root int64, board string, relations []string, window int64) (_ time.Duration, err error) {
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	start := time.Now()
	if _, ok, err := s.ExportMirrorBoard(ctx, conn, root, board, store.MirrorRegroup{}, relations, window, math.MaxInt64, store.CardShapeForTest); err != nil || !ok {
		return 0, fmt.Errorf("export %v: %v, %w", relations, ok, err)
	}
	return time.Since(start), nil
}

// What a board's dashboard leaves in the Go process: heap in use,
// goroutines and open files before the first evaluation, after it, after
// 20 evaluations and 20 drills, and after a forced collection once the
// calls end, as when the screen closes. An evaluation keeps nothing
// between calls; the screen's only other Go state is the board's
// subscription, which the Board screen shares. The files open after the
// calls are compared with those after the first evaluation: its attach
// of the app file leaves one descriptor of it open, because SQLite's unix
// VFS does not close a file whose inode another connection of the process
// still locks (the store's own) and reuses that descriptor at the next
// open, so the count does not grow; it closes with the store. Linux only;
// unset, AO_TRACKER_SCALE skips it.
//
//	AO_TRACKER_SCALE=2000 make go-test GO_TEST_PKGS=./internal/store \
//	  GO_TEST_FLAGS="-count=1 -v -run TestDashboardMemoryAfterClose"
func TestDashboardMemoryAfterClose(t *testing.T) {
	env := os.Getenv("AO_TRACKER_SCALE")
	if env == "" {
		t.Skip("AO_TRACKER_SCALE names no sizes")
	}
	for field := range strings.SplitSeq(env, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || n < 100 {
			t.Fatalf("AO_TRACKER_SCALE=%q: each size is a whole number of at least 100", env)
		}
		t.Run(fmt.Sprint(n), func(t *testing.T) { dashboardMemory(t, n) })
	}
}

func dashboardMemory(t *testing.T, n int) {
	s, root, board := store.ScaleMirrorForTest(t, n, 0)
	fill := func(ctx context.Context, conn *sql.Conn, relations []string) (store.MirrorExport, bool, error) {
		return s.ExportMirrorBoard(ctx, conn, root, board, store.MirrorRegroup{}, relations, 0, math.MaxInt64, store.CardShapeForTest)
	}
	b := trackerdash.Board{Fill: fill, Epics: true, Viewer: "acc-user1"}
	e := trackerdash.NewEvaluator(time.Now)
	ctx := context.Background()
	evaluate := func(rng string) {
		a, err := e.Evaluate(ctx, b, trackerdash.Request{Dashboard: "overview_sprints", Range: rng, TimeZone: "UTC"})
		if err != nil {
			t.Fatal(err)
		}
		if len(a.Widgets) == 0 {
			t.Fatal("the evaluation answered no widgets")
		}
	}
	before := procstate.Read(t, true)
	evaluate(trackerdash.RangeSprints5)
	first := procstate.Read(t, true)
	ranges := []string{trackerdash.RangeSprints5, trackerdash.RangeThisSprint}
	for i := range 20 {
		evaluate(ranges[i%2])
		if _, err := e.Drill(ctx, b, trackerdash.DrillRequest{Request: trackerdash.Request{Dashboard: "overview_sprints", Range: trackerdash.RangeSprints5, TimeZone: "UTC"}, Widget: "stuck", Page: i % 2}); err != nil {
			t.Fatal(err)
		}
	}
	held := procstate.Read(t, false)
	after := procstate.Settled(t, first, 2*time.Second)
	t.Logf("%d issues: before the first evaluation %v; after it %v; after 20 evaluations and 20 drills, uncollected, %v; after a forced collection %v",
		n, before, first, held, after)
	t.Logf("%d issues: opened by the first evaluation and kept: %v", n, first.Added(before))
	if left := after.Added(first); len(left) > 0 {
		t.Errorf("%d issues: files left open by the evaluations: %v", n, left)
	}
	if after.Goroutines > first.Goroutines {
		t.Errorf("%d issues: %d goroutines after the evaluations, %d after the first", n, after.Goroutines, first.Goroutines)
	}
}
