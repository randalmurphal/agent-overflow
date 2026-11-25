package trackerapp

import (
	"fmt"
	"runtime"
	"runtime/metrics"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// The service at the size of a large workspace, against the scripted
// provider (scaleBoard), whose answers cost no decoding, so the figures
// are the service's and the store's own: a board's first sync as its pump
// runs it, the ticks a shown board runs after, opening it, and what a
// running service retains. Each path reports its provider calls and store
// calls (exact), its store write statements and transactions, its
// allocations and the median and spread of its time over scaleReps runs.
// The requests a real provider sends for the same paths are the
// trackerfake scale test's. Unset, AO_TRACKER_SCALE skips it.
//
//	AO_TRACKER_SCALE=5000,20000 make go-test GO_TEST_PKGS=./internal/trackerapp \
//	  GO_TEST_FLAGS="-count=1 -timeout 60m -v -run TestServiceAtScale"
func TestServiceAtScale(t *testing.T) {
	for _, n := range scaleSizes(t) {
		t.Run(fmt.Sprint(n), func(t *testing.T) { serviceAtScale(t, n) })
		t.Run(fmt.Sprint(n, "/retained"), func(t *testing.T) { retainedAtScale(t, n) })
	}
}

const scaleReps = 5

// scaleCost is one path's measured cost; the counts are its last run's.
type scaleCost struct {
	provider, store          map[string]int
	writes, commits          uint64
	allocBytes, allocObjects uint64
	peakHeap                 uint64
	times                    []time.Duration
}

func (c scaleCost) String() string {
	sorted := slices.Sorted(slices.Values(c.times))
	return fmt.Sprintf("%9s (%s to %s) %7.0f KB in %6d allocations, peak heap %5.1f MB, %3d write statements in %3d transactions\n\tprovider %s\n\tstore %s",
		sorted[len(sorted)/2].Round(time.Microsecond), sorted[0].Round(time.Microsecond), sorted[len(sorted)-1].Round(time.Microsecond),
		float64(c.allocBytes)/1024, c.allocObjects, float64(c.peakHeap)/(1<<20), c.writes, c.commits,
		describeCounts(c.provider), describeCounts(c.store))
}

// heapPeak samples the live heap every millisecond until stop, returning
// the most it saw.
func heapPeak() (stop func() uint64) {
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	done, peak := make(chan struct{}), make(chan uint64)
	go func() {
		var most uint64
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			metrics.Read(sample)
			most = max(most, sample[0].Value.Uint64())
			select {
			case <-done:
				peak <- most
				return
			case <-tick.C:
			}
		}
	}()
	return func() uint64 { close(done); return <-peak }
}

// scaleMeter measures paths of one service.
type scaleMeter struct {
	t   *testing.T
	svc *Service
	p   *scripted
	cs  *countingStore
}

// measure runs op scaleReps times (once when once is set), each after
// its unmeasured prepare (if any).
func (m *scaleMeter) measure(prepare, op func(rep int), once bool) scaleCost {
	m.t.Helper()
	var c scaleCost
	reps := scaleReps
	if once {
		reps = 1
	}
	st := m.cs.Store
	for rep := range reps {
		if prepare != nil {
			prepare(rep)
		}
		runtime.GC()
		calls := len(m.p.callsInOrder())
		m.cs.take()
		writes, commits := st.WriteCount(), st.CommitCount()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		peak := heapPeak()
		start := time.Now()
		op(rep)
		c.times = append(c.times, time.Since(start))
		c.peakHeap = peak()
		runtime.ReadMemStats(&after)
		c.allocBytes, c.allocObjects = after.TotalAlloc-before.TotalAlloc, after.Mallocs-before.Mallocs
		c.provider, c.store = countsOf(m.p.callsInOrder()[calls:]), m.cs.take()
		c.writes, c.commits = st.WriteCount()-writes, st.CommitCount()-commits
	}
	return c
}

// pumpTicks runs scope's ticks as its pump would, from what the sync
// state says is due, until one reports no More, and returns how many ran.
func pumpTicks(t *testing.T, svc *Service, scope Scope) int {
	t.Helper()
	p := &boardPump{scope: scope}
	for ticks := 1; ; ticks++ {
		out, _, err := svc.syncBoard(t.Context(), p, Background, svc.boardDue)
		if err != nil || out.Failure != nil || out.Ended {
			t.Fatalf("tick %d: %+v, %v", ticks, out, err)
		}
		if !out.More() {
			return ticks
		}
	}
}

func serviceAtScale(t *testing.T, n int) {
	svc, p, scope := benchService(t)
	cs := newCountingStore(svc.store.(*store.Store))
	svc.store = cs
	board := newScaleBoard(n)
	board.serve(p, true)
	m := &scaleMeter{t: t, svc: svc, p: p, cs: cs}
	logCost := func(name string, c scaleCost) { t.Logf("%-32s %s", name, c) }

	var ticks int
	logCost("first sync", m.measure(nil, func(int) { ticks = pumpTicks(t, svc, scope) }, true))
	t.Logf("first sync: %d ticks", ticks)
	if v := must[BoardView](t)(svc.Board(scope, StatusGroups{}, nil)); v.All.Rows != n {
		t.Fatalf("the board holds %d rows of %d", v.All.Rows, n)
	}
	// A hidden subscription, so the change funnel looks up the boards of
	// what a tick changed as it does for a client, and no pump ticks.
	sub := must[BoardSubscription](t)(svc.SubscribeBoard("boards-"+scope.Connection, scope, nil, untied))
	idleBoards(t, svc, p)
	if err := svc.SetSubscriptionVisible(sub.ID, false); err != nil {
		t.Fatal(err)
	}
	tick := func(int) { pumpTicks(t, svc, scope) }
	rev := 0
	edit := func(k int) func(int) {
		return func(int) {
			rev++
			board.edit(spread(n, k), rev)
		}
	}
	settle := func(rep int) { pumpTicks(t, svc, scope) }
	logCost("idle tick", m.measure(nil, tick, false))
	for _, k := range []int{1, 100} {
		logCost(fmt.Sprintf("delta tick, %d changed", k), m.measure(func(rep int) { settle(rep); edit(k)(rep) }, func(int) {
			if out := must[Outcome](t)(svc.syncTick(t.Context(), scope, BoardDue{Delta: true}, Background)); len(out.Changes().Cards) != k {
				t.Fatalf("the delta stored %d cards, want %d", len(out.Changes().Cards), k)
			}
		}, false))
		logCost(fmt.Sprintf("history tick, %d due", k), m.measure(func(rep int) {
			settle(rep)
			edit(k)(rep)
			must[Outcome](t)(svc.syncTick(t.Context(), scope, BoardDue{Delta: true}, Background))
		}, func(int) {
			if out := must[Outcome](t)(svc.syncTick(t.Context(), scope, BoardDue{History: true}, Background)); len(out.Changes().History) != k {
				t.Fatalf("the history stored %d, want %d", len(out.Changes().History), k)
			}
		}, false))
	}
	settle(0)
	logCost("reconcile", m.measure(nil, func(int) {
		must[Outcome](t)(svc.syncTick(t.Context(), scope, BoardDue{Reconcile: true}, Background))
	}, false))
	sections := []Section{{Kind: SectionColumn}, {Kind: SectionBacklog}, {Kind: SectionSprint, Sprint: "1"}}
	logCost("board view, 3 sections", m.measure(nil, func(int) { must[BoardView](t)(svc.Board(scope, StatusGroups{}, sections)) }, false))
	// The page that starts halfway down the backlog.
	var deep *SectionCursor
	for page := must[SectionPage](t)(svc.SectionRows(scope, StatusGroups{}, sections[1], nil)); page.Next != nil && len(page.Rows) > 0; {
		if strings.Compare(page.Rows[0].Order, fmt.Sprintf("0|i%05d:", n/2)) >= 0 {
			break
		}
		deep = page.Next
		page = must[SectionPage](t)(svc.SectionRows(scope, StatusGroups{}, sections[1], deep))
	}
	logCost("section page, deep", m.measure(nil, func(int) { must[SectionPage](t)(svc.SectionRows(scope, StatusGroups{}, sections[1], deep)) }, false))
	v := must[BoardView](t)(svc.Board(scope, StatusGroups{}, sections))
	for _, held := range []TotalsAt{{}, {Lifetime: v.Lifetime, Revision: v.TotalsRevision}} {
		kind := map[bool]string{true: "totals moved", false: "totals held"}[held == TotalsAt{}]
		for _, k := range []int{1, 100} {
			ids := board.ids(spread(n, k))
			logCost(fmt.Sprintf("patch read, %d changed, %s", k, kind), m.measure(nil, func(int) {
				must[BoardPatch](t)(svc.BoardPatch(scope, StatusGroups{}, sections, ids, held))
			}, false))
		}
	}
	opened := board.ids(spread(n, 1))[0]
	must3(t)(svc.IssueDetail(t.Context(), scope.Connection, opened, Interactive))
	logCost("ticket open, stored", m.measure(nil, func(int) {
		must3(t)(svc.IssueDetail(t.Context(), scope.Connection, opened, Interactive))
	}, false))
	logCost("ticket first paint", m.measure(nil, func(int) { must3(t)(svc.mirroredDetail(scope.Connection, opened)) }, false))
	logCost("cleanup pass", m.measure(nil, func(int) {
		if err := svc.FinishMirrorCleanup(); err != nil {
			t.Fatal(err)
		}
	}, false))
	logCost("trackerapp.New", m.measure(nil, func(int) {
		other, err := New(testCallbacks(Deps{
			Store: cs, Changed: func() {}, CredentialsDir: svc.credentialsDir, ConfigDir: svc.configDir, Origin: APIOrigin, Version: "1", Now: time.Now, Life: t.Context(),
		}, newEventLog()))
		if err != nil {
			t.Fatal(err)
		}
		other.Stop()
	}, false))
}

// must3 fails t on a call's error and drops its values.
func must3(t *testing.T) func(any, any, error) {
	return func(_, _ any, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
}

// idleBoards waits until no tick of svc is in flight and p has had no call for
// 50 milliseconds.
func idleBoards(t *testing.T, svc *Service, p *scripted) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	last, since := -1, time.Now()
	for time.Now().Before(deadline) {
		svc.mu.Lock()
		busy := len(svc.locks) > 0
		svc.mu.Unlock()
		if calls := len(p.callsInOrder()); calls != last || busy {
			last, since = calls, time.Now()
		} else if time.Since(since) > 50*time.Millisecond {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the service did not go quiet")
}
