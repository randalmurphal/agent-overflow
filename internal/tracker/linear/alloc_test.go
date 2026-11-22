package linear

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"agent-overflow/internal/tracker/linear/lineartest"
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

// costHistoryBody is a batch history answer of issues issues with entries
// entries each, every one a state change.
func costHistoryBody(issues, entries int) []byte {
	var nodes []string
	for i := range issues {
		es := make([]string, entries)
		for j := range es {
			es[j] = lineartest.HistoryEntry{ID: uuidN(10000 + i*entries + j), CreatedAt: "2026-11-05T10:00:00.000Z", Actor: uuidN(2),
				FromState: uuidN(3), ToState: uuidN(4)}.JSON()
		}
		nodes = append(nodes, historiesNode(uuidN(i+1), uuidN(5), es, false, ""))
	}
	return []byte(lineartest.Data(lineartest.PageJSON(nodes, false, "")))
}

// decodeHistories is IssueHistories's decode of one batch answer.
func decodeHistories(body []byte, issues, entries int) error {
	var data rootPage[wireIssueChanges]
	if err := (&Client{}).readAnswer(answer{op: opIssueHistories, status: 200, scrub: func(s string) string { return s }}, body, &data); err != nil {
		return err
	}
	p, err := page(opIssueHistories, *data.Page, Usage{}, same[wireIssueChanges])
	if err != nil {
		return err
	}
	for _, w := range p.Items {
		changes, err := (&Client{}).issueChanges(context.Background(), w, time.Time{})
		if err != nil {
			return err
		}
		if len(changes.Entries) != entries {
			return fmt.Errorf("%d entries, want %d", len(changes.Entries), entries)
		}
	}
	if len(p.Items) != issues {
		return fmt.Errorf("%d issues, want %d", len(p.Items), issues)
	}
	return nil
}

// The decode of a 250-issue page and of a batch of 20 issues' 100 history
// entries each allocate no more than pinned: allocations and bytes a
// call. A pin is the measured cost and about 5% (see hotPins).
func TestHotPathAllocations(t *testing.T) {
	if raceOn {
		t.Skip("the race detector changes what allocates")
	}
	issues := costIssuePage(250, false)
	histories := costHistoryBody(20, 100)
	for _, c := range []struct {
		name string
		call func() error
	}{
		{"page of 250 issues", func() error { return decodeIssuePage(issues, 250) }},
		{"20 histories of 100 entries", func() error { return decodeHistories(histories, 20, 100) }},
	} {
		var err error
		allocs, bytes := costOf(10, func() {
			if e := c.call(); e != nil {
				err = e
			}
		})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		pin := hotPins[c.name]
		t.Logf("%s: %d allocations, %d bytes a call (pinned %d, %d)", c.name, allocs, bytes, pin.allocs, pin.bytes)
		if allocs > pin.allocs || bytes > pin.bytes {
			t.Errorf("%s: %d allocations and %d bytes a call, pinned at most %d and %d", c.name, allocs, bytes, pin.allocs, pin.bytes)
		}
	}
}

// hotPins are the hot paths' measured allocations and bytes a call, with
// about 5% added so a run's noise passes and one more allocation an
// issue or an entry fails.
var hotPins = map[string]struct{ allocs, bytes uint64 }{
	"page of 250 issues":          {3737, 428_000},     // measured 3,559 and 407,728
	"20 histories of 100 entries": {15_030, 2_668_000}, // measured 14,312 and 2,540,742
}
