package browser

import (
	"fmt"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
)

// Network idle must report the same answer whichever of a request's start
// and end is handled first. A redirect reuses the request id for several
// starts before the one end, so the end can be handled between them.
func TestNetworkTrackerSettlesInAnyOrder(t *testing.T) {
	now := time.Unix(1000, 0)
	later := now.Add(time.Hour)
	const quiet = 500 * time.Millisecond
	cases := []struct {
		name     string
		streams  func(tr *networkTracker) [][]eventStep
		wantIdle bool
		// wantKept is how many request ids are left once every event is
		// handled: a settled request leaves nothing behind.
		wantKept int
	}{
		{"a finished request", func(tr *networkTracker) [][]eventStep {
			return [][]eventStep{
				{start(tr, "R1", false, now)},
				{end(tr, "R1", now)},
			}
		}, true, 0},
		{"a redirected request", func(tr *networkTracker) [][]eventStep {
			return [][]eventStep{
				{start(tr, "R1", false, now), start(tr, "R1", true, now), start(tr, "R1", true, now)},
				{end(tr, "R1", now)},
			}
		}, true, 0},
		{"two requests", func(tr *networkTracker) [][]eventStep {
			return [][]eventStep{
				{start(tr, "R1", false, now), start(tr, "R2", false, now)},
				{end(tr, "R2", now), end(tr, "R1", now)},
			}
		}, true, 0},
		{"a request still in flight", func(tr *networkTracker) [][]eventStep {
			return [][]eventStep{
				{start(tr, "R1", false, now), start(tr, "R2", false, now)},
				{end(tr, "R2", now)},
			}
		}, false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The streams are rebuilt per order so each order runs on its
			// own tracker.
			probe := newNetworkTracker(now)
			for i := range interleavings(tc.streams(probe)) {
				tr := newNetworkTracker(now)
				order := interleavings(tc.streams(tr))[i]
				for _, step := range order {
					step.run()
				}
				if got := tr.idle(later, quiet); got != tc.wantIdle {
					t.Errorf("handled as [%s]: idle = %v, want %v", stepNames(order), got, tc.wantIdle)
				}
				if got := tr.retained(); got != tc.wantKept {
					t.Errorf("handled as [%s]: kept %d request ids, want %d", stepNames(order), got, tc.wantKept)
				}
			}
		})
	}
}

func start(tr *networkTracker, id network.RequestID, redirect bool, now time.Time) eventStep {
	name := "start " + string(id)
	if redirect {
		name = "redirect " + string(id)
	}
	return eventStep{name, func() { tr.started(id, redirect, now) }}
}

func end(tr *networkTracker, id network.RequestID, now time.Time) eventStep {
	return eventStep{"end " + string(id), func() { tr.ended(id, now) }}
}

// An end whose start never comes, a request sent before Network.enable, is
// kept only up to orderSlack, and the requests in flight are never dropped
// to make room.
func TestNetworkTrackerBoundsUnmatchedEnds(t *testing.T) {
	now := time.Now()
	tr := newNetworkTracker(now)
	tr.started("live", false, now)
	for i := range orderSlack + 50 {
		tr.ended(network.RequestID(fmt.Sprintf("early-%d", i)), now)
	}
	if got := tr.retained(); got != orderSlack+1 {
		t.Fatalf("kept %d request ids, want the %d newest early ends and the request in flight", got, orderSlack)
	}
	if tr.idle(now.Add(time.Hour), 0) {
		t.Fatal("the request in flight was dropped to make room for early ends")
	}
	// The newest early end is still matched by its late start.
	tr.started(network.RequestID(fmt.Sprintf("early-%d", orderSlack+49)), false, now)
	tr.ended("live", now)
	if !tr.idle(now.Add(time.Hour), 0) {
		t.Fatal("the newest early end no longer settles its late start")
	}
}

// retained reports how many request ids the tracker keeps.
func (t *networkTracker) retained() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.inFlight) + t.endedEarly.len()
}
