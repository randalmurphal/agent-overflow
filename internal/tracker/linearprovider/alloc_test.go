package linearprovider

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
)

// costOf is what one call of f allocates, counted as testing.AllocsPerRun
// counts: a warm-up call, then runs calls on one P.
func costOf(runs int, f func()) (allocs, bytes uint64) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	f()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range runs {
		f()
	}
	runtime.ReadMemStats(&after)
	return (after.Mallocs - before.Mallocs) / uint64(runs), (after.TotalAlloc - before.TotalAlloc) / uint64(runs)
}

// The rows of a 250-issue page, each issue with two labels Linear named
// with it, a cycle and an estimate, allocate no more than pinned, with
// no request.
func TestRowsAllocations(t *testing.T) {
	if raceOn {
		t.Skip("the race detector changes what allocates")
	}
	p, _ := newProvider(t)
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	three := 3.0
	issues := make([]linear.Issue, 250)
	for i := range issues {
		issues[i] = linear.Issue{
			ID: issueID(i + 1), Identifier: fmt.Sprintf("WEB-%d", i+1), Title: fmt.Sprintf("Issue %d", i+1), Priority: 2, Estimate: &three,
			SortOrder: float64(i), TeamID: web, TeamEstimateScale: linear.Estimation{Scale: "fibonacci"}, StateID: "state-todo", AssigneeID: "user-1", CycleID: "cycle-1",
			LabelIDs: []string{"label-bug", "label-ui"}, Labels: []linear.LabelRef{{ID: "label-bug", Name: "Bug"}, {ID: "label-ui", Name: "UI"}},
			CreatedAt: at, UpdatedAt: at,
		}
	}
	var err error
	allocs, bytes := costOf(10, func() {
		var rows []tracker.Row
		if rows, err = p.rows(context.Background(), web, issues); err == nil && len(rows) != len(issues) {
			err = fmt.Errorf("%d rows", len(rows))
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("rows of 250 issues: %d allocations, %d bytes a call", allocs, bytes)
	if allocs > rowsPin.allocs || bytes > rowsPin.bytes {
		t.Errorf("rows of 250 issues: %d allocations and %d bytes a call, pinned at most %d and %d", allocs, bytes, rowsPin.allocs, rowsPin.bytes)
	}
}

// rowsPin is the rows' measured allocations and bytes a call, with about
// 5% added so a run's noise passes and one more allocation an issue
// fails.
var rowsPin = struct{ allocs, bytes uint64 }{1064, 340_000} // measured 1,014 and 323,771
