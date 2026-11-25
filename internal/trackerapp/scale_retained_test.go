package trackerapp

import (
	"fmt"
	"runtime"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// scaleShownBoards and scaleShownTickets are what the retained-memory
// measurement shows of a service: five boards of the same n issues and
// 50 of their tickets.
const (
	scaleShownBoards  = 5
	scaleShownTickets = 50
)

// heapInUse is the live heap after two collections, and the goroutines.
func heapInUse() (heap uint64, goroutines int) {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc, runtime.NumGoroutine()
}

// retainedAtScale measures what a running service holds with five boards
// of n issues each and 50 tickets shown, against the same service with
// nothing shown, and then the round of the shown tickets: idle, and with
// 10 changed, on a tracker whose update time moves with its detail (Jira)
// and on one where it does not (Linear).
func retainedAtScale(t *testing.T, n int) {
	svc, p, scope := benchService(t)
	svc.cadence = Cadence{Delta: time.Hour, Ticket: time.Hour}.withDefaults()
	cs := newCountingStore(svc.store.(*store.Store))
	svc.store = cs
	board := newScaleBoard(n)
	board.serve(p, true)
	var boards []tracker.BoardID
	for b := range scaleShownBoards {
		boards = append(boards, tracker.BoardID(fmt.Sprint(b+1)))
	}
	listBoards(t, cs.Store, scope.Connection, boards...)
	start := time.Now()
	for _, b := range boards {
		pumpTicks(t, svc, Scope{Connection: scope.Connection, Board: b})
	}
	t.Logf("first sync of %d boards of %d issues each: %s", len(boards), n, time.Since(start).Round(time.Millisecond))
	idleHeap, idleGoroutines := heapInUse()

	sections := []Section{{Kind: SectionColumn}, {Kind: SectionBacklog}, {Kind: SectionSprint, Sprint: "1"}}
	var handles []string
	for _, b := range boards {
		sub := must[BoardSubscription](t)(svc.SubscribeBoard("boards-"+scope.Connection, Scope{Connection: scope.Connection, Board: b}, sections, true, untied))
		handles = append(handles, sub.ID)
	}
	tickets := board.ids(spread(n, scaleShownTickets))
	for _, id := range tickets {
		sub := must[TicketSubscription](t)(svc.SubscribeTicket(scope.Connection, id, untied))
		handles = append(handles, sub.ID)
	}
	settled(t, svc)
	idleBoards(t, svc, p)
	shownHeap, shownGoroutines := heapInUse()
	svc.subMu.Lock()
	pumps, ticketPumps, rounds := len(svc.subs.boards), len(svc.subs.tickets), len(svc.subs.rounds)
	svc.subMu.Unlock()
	t.Logf("shown: %d board pumps, %d ticket pumps and %d rounds, each with one timer; heap %+.0f KB and %+d goroutines over nothing shown (%.0f KB, %d)",
		pumps, ticketPumps, rounds, (float64(shownHeap)-float64(idleHeap))/1024, shownGoroutines-idleGoroutines, float64(idleHeap)/1024, idleGoroutines)

	m := &scaleMeter{t: t, svc: svc, p: p, cs: cs}
	rd := svc.roundOf(scope.Connection)
	round := func(int) {
		if res := svc.roundTick(rd); res.failed {
			t.Fatalf("round %+v", res)
		}
	}
	rev := 1000
	changed := func(int) {
		rev++
		board.edit(spread(n, scaleShownTickets)[:10], rev)
	}
	for _, jira := range []bool{true, false} {
		board.serve(p, jira)
		round(0)
		kind := map[bool]string{true: "Jira", false: "Linear"}[jira]
		t.Logf("%-32s %s", fmt.Sprintf("%s round of %d, idle", kind, scaleShownTickets), m.measure(nil, round, false))
		t.Logf("%-32s %s", fmt.Sprintf("%s round of %d, 10 changed", kind, scaleShownTickets), m.measure(changed, round, false))
	}

	for _, h := range handles {
		svc.ReleaseSubscription(h)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		heap, goroutines := heapInUse()
		if goroutines <= idleGoroutines || time.Now().After(deadline) {
			t.Logf("released: heap %+.0f KB and %+d goroutines over nothing shown", (float64(heap)-float64(idleHeap))/1024, goroutines-idleGoroutines)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
}
