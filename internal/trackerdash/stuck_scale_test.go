package trackerdash

import (
	"os"
	"runtime"
	"testing"
	"time"
)

// The weeks Overview of a board whose tickets are each stuck now and
// stuck twice before in the range before (stuckBoard), at 6,000 and 10,001
// tickets: Stuck's number, and an evaluation's time and allocation, three
// runs each. Unset, AO_TRACKER_SCALE skips it.
//
//	AO_TRACKER_SCALE=1 make go-test GO_TEST_PKGS=./internal/trackerdash \
//	  GO_TEST_FLAGS="-count=1 -v -run TestStuckWithPastStaysAtScale"
func TestStuckWithPastStaysAtScale(t *testing.T) {
	if os.Getenv("AO_TRACKER_SCALE") == "" {
		t.Skip("AO_TRACKER_SCALE is not set")
	}
	day := func(d int) int64 { return utc(10, d, 0) }
	for _, n := range []int{6_000, 10_001} {
		b := stuckBoard(n, [2]int64{day(12), day(16)}, [2]int64{day(19), day(23)}, [2]int64{ago(10), 0}).board()
		for rep := range 3 {
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			start := time.Now()
			a := mustEvaluate(t, evaluator(), b, Request{Dashboard: builtinWeeksID, TimeZone: "UTC"})
			took := time.Since(start)
			runtime.ReadMemStats(&after)
			w := widgetOf(t, a, "stuck")
			t.Logf("%d tickets, run %d: stuck %s %q; evaluation %v, %.1f MB allocated", n, rep, num(w.Value), w.Error, took.Round(time.Millisecond), float64(after.TotalAlloc-before.TotalAlloc)/(1<<20))
		}
	}
}
