package trackerdash

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/testutil/procstate"
)

// What a hostile metric costs the computer before its deadline stops it,
// and what its session leaves once closed: a cross join of 3,000 changes
// grouped, and the same made distinct, whose sorter and ephemeral table
// spill to temporary files past the session's 4 MiB page cache. (Sorted
// with a LIMIT, SQLite keeps only the top rows and spills nothing.) A
// watcher sums the sizes of the open temporary files (SQLite deletes each
// as it opens it, so they are read through /proc/self/fd) every 5 ms.
// Heap in use, goroutines and open files are read before the session
// opens and after it closes, with a forced collection. Linux only; unset,
// AO_TRACKER_SCALE skips it.
//
//	AO_TRACKER_SCALE=2000 make go-test GO_TEST_PKGS=./internal/trackerdash \
//	  GO_TEST_FLAGS="-count=1 -v -run TestHostileQuerySpill"
func TestHostileQuerySpill(t *testing.T) {
	if os.Getenv("AO_TRACKER_SCALE") == "" {
		t.Skip("AO_TRACKER_SCALE names no sizes")
	}
	before := procstate.Read(t, true)
	s := sealedOpen(t)
	for rep, q := range []string{
		`SELECT a.ticket AS ticket FROM changes a, changes b, changes c GROUP BY a.seq * 100000000 + b.seq * 10000 + c.seq`,
		`SELECT DISTINCT a.seq * 100000000 + b.seq * 10000 + c.seq AS value, a.ticket AS ticket FROM changes a, changes b, changes c`,
		`SELECT a.ticket AS ticket FROM changes a, changes b, changes c GROUP BY a.seq * 100000000 + b.seq * 10000 + c.seq`,
		`SELECT DISTINCT a.seq * 100000000 + b.seq * 10000 + c.seq AS value, a.ticket AS ticket FROM changes a, changes b, changes c`,
	} {
		var peak atomic.Int64
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			tick := time.NewTicker(5 * time.Millisecond)
			defer tick.Stop()
			for {
				if n := procstate.DeletedBytes(t); n > peak.Load() {
					peak.Store(n)
				}
				select {
				case <-stop:
					return
				case <-tick.C:
				}
			}
		}()
		start := time.Now()
		_, err := runSQL(s, metricOf(q))
		elapsed := time.Since(start)
		close(stop)
		<-done
		after := procstate.DeletedBytes(t)
		t.Logf("%.40s… run %d: stopped after %v (%v), temporary files at most %.1f MB, %d bytes after", q, rep, elapsed.Round(time.Millisecond), err, float64(peak.Load())/(1<<20), after)
		if err == nil {
			t.Fatal("the hostile query finished; it should have been stopped")
		}
		if after != 0 {
			t.Fatalf("%d bytes of temporary files left open after the stopped query", after)
		}
	}
	open := procstate.Read(t, false)
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	closed := procstate.Settled(t, before, 2*time.Second)
	t.Logf("before the session %v; after the four queries, uncollected, %v; after it closed, collected, %v", before, open, closed)
	if left := closed.Added(before); len(left) > 0 {
		t.Errorf("files left open by the closed session: %v", left)
	}
	if closed.Goroutines > before.Goroutines {
		t.Errorf("%d goroutines after the session closed, %d before it", closed.Goroutines, before.Goroutines)
	}
}

// What a tile's set of 5,000 tickets costs: a board of 5,000 tickets all
// stuck, drilled for their IDs as a tile's filter asks, once with Jira's
// IDs (numbers) and once with Linear's (UUIDs). It logs the answer's
// JSON bytes, the rows the metric returned and the time the drill took.
//
//	AO_TRACKER_SCALE=5000 make go-test GO_TEST_PKGS=./internal/trackerdash \
//	  GO_TEST_FLAGS="-count=1 -v -run TestATicketSetsCost"
func TestATicketSetsCost(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("AO_TRACKER_SCALE"))
	if n <= 0 {
		t.Skip("AO_TRACKER_SCALE names no sizes")
	}
	for _, shape := range []struct {
		name string
		id   func(i int) string
	}{
		{"Jira", func(i int) string { return strconv.Itoa(10000 + i) }},
		{"Linear", func(i int) string { return fmt.Sprintf("%08x-0000-4000-8000-%012x", i, i) }},
	} {
		f := sprintBoard()
		for i := range n {
			f.ticket(ticket{id: shape.id(i), key: fmt.Sprintf("WEB-%d", i), status: "3", group: "in_progress", assignee: "u1", created: ago(20), since: ago(5)})
		}
		e := evaluator()
		start := time.Now()
		d, err := e.Drill(context.Background(), f.board(), DrillRequest{Request: Request{Dashboard: builtinSprintsID, TimeZone: "UTC", Widgets: []string{"stuck"}}, Widget: "stuck", IDs: true})
		took := time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %d tickets, %d rows returned, answer %d bytes (%.1f a ticket), drilled in %v", shape.name, len(d.IDs), d.Count, len(body), float64(len(body))/float64(len(d.IDs)), took.Round(time.Millisecond))
		if len(d.IDs) != n {
			t.Fatalf("%d IDs; want %d", len(d.IDs), n)
		}
	}
}
