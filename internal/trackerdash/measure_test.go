package trackerdash

import (
	"os"
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
