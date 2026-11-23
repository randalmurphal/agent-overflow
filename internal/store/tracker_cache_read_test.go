package store

import (
	"errors"
	"maps"
	"testing"
)

// mirrorReadAt is the read that began at at, for the package's tests of
// what a write does with a given read time. No production build has it:
// only BeginMirrorRead makes a read there.
func mirrorReadAt(at int64) MirrorRead { return MirrorRead{at: at} }

// beginRead is BeginMirrorRead that fails the test on an error.
func beginRead(t *testing.T, s *Store, root, now int64) MirrorRead {
	t.Helper()
	read, err := s.BeginMirrorRead(root, now)
	if err != nil {
		t.Fatalf("begin a read at %d: %v", now, err)
	}
	return read
}

// A read begun after the store is opened again begins after every read
// the root stored, though the wall clock stepped back past them. The lease
// is stored before the read that needs it is returned, never at close, so
// a crash between a write and the next read leaves the same lease as the
// close here.
func TestAReadBeginsAfterEveryReadTheRootStoredAcrossARestart(t *testing.T) {
	s, _, root := newMirror(t)
	var newest MirrorRead
	for _, now := range []int64{1_000_000, 1_000_000, 1_030_000} {
		newest = beginRead(t, s, root, now)
	}
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 5, "a")), nil)
	if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{webCard("10001", 5)}, MirrorCatalog{}, newest); err != nil {
		t.Fatal(err)
	}
	s = reopenStore(t, s)
	after := beginRead(t, s, root, 900_000)
	var stored int64
	if err := s.db.QueryRow(`SELECT max(read_at) FROM tracker_cache_issues WHERE root_id = ?`, root).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if after.At() <= stored || after.At() <= newest.At() {
		t.Fatalf("the first read after the restart began at %d; want after the stored %d and the last begun %d", after.At(), stored, newest.At())
	}
}

// After the wall clock steps back, reads begin a millisecond apart above
// the last one, so a card read after the step replaces the one read
// before it; once the clock passes them again, reads begin at the clock.
func TestAReadAfterTheClockStepsBackIsOrderedAfterTheReadsBeforeIt(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 5, "a")), nil)
	before := beginRead(t, s, root, 100_000)
	card := webCard("10001", 7)
	card.Data = []byte(`{"title":"before the step"}`)
	if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{card}, MirrorCatalog{}, before); err != nil {
		t.Fatal(err)
	}
	stepped := beginRead(t, s, root, 40_000)
	if stepped.At() != before.At()+1 {
		t.Fatalf("the read after the step began at %d; want %d", stepped.At(), before.At()+1)
	}
	card.Data = []byte(`{"title":"after the step"}`)
	if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{card}, MirrorCatalog{}, stepped); err != nil {
		t.Fatal(err)
	}
	if got := mustMirrorCards(t, s, root, "10001"); len(got) != 1 || string(got[0].Data) != `{"title":"after the step"}` {
		t.Fatalf("the mirror holds %+v; want the card read after the step", got)
	}
	if next := beginRead(t, s, root, 40_001); next.At() != stepped.At()+1 {
		t.Fatalf("the next read began at %d; want %d", next.At(), stepped.At()+1)
	}
	if caught := beginRead(t, s, root, 100_500); caught.At() != 100_500 {
		t.Fatalf("the read once the clock passed them began at %d; want the clock's 100500", caught.At())
	}
}

// A burst of reads in one millisecond runs ahead of the clock a
// millisecond a read, each after the last; a burst that passes the lease
// stores the next lease before it returns the read that passed it.
func TestABurstOfReadsRunsAheadOfTheClock(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	const now, burst = 500_000, mirrorReadLease + 10
	last := MirrorRead{}
	for i := range burst {
		read := beginRead(t, s, root, now)
		if read.At() != now+int64(i) || read.At() <= last.At() {
			t.Fatalf("read %d of the burst began at %d after %d; want %d", i, read.At(), last.At(), now+int64(i))
		}
		last = read
	}
	var lease int64
	if err := s.db.QueryRow(`SELECT lease FROM tracker_cache_read_leases WHERE root_id = ?`, root).Scan(&lease); err != nil {
		t.Fatal(err)
	}
	if lease < last.At() {
		t.Fatalf("the stored lease %d is below the burst's last read %d", lease, last.At())
	}
}

// A read that stores a lease fails with ErrTrackerCacheGone on a detached
// root and on a root that never existed, and stores nothing.
func TestAReadClockStoresALeaseOnlyOnALiveRoot(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	if _, err := s.BeginMirrorRead(root+100, 1_000); !errors.Is(err, ErrTrackerCacheGone) {
		t.Fatalf("a read of a root that never existed: %v", err)
	}
	if _, err := s.DetachMirror(jira.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginMirrorRead(root, 1_000); !errors.Is(err, ErrTrackerCacheGone) {
		t.Fatalf("a read of a detached root: %v", err)
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_read_leases`, "0")
}

// Ten minutes of reads a second cost one statement for the process's
// first read of the root, which reads the lease, and one statement and
// one row for each lease: ten, one a minute. The other 590 reads touch
// no row.
func TestReadClockCosts(t *testing.T) {
	s, _, root := newMirror(t)
	recorder := recordMirrorWrites(t, s)
	const start = 1_000_000
	var err error
	statements := countMirrorStatements(t, func() {
		for k := range int64(600) {
			if _, err = s.BeginMirrorRead(root, start+k*1000); err != nil {
				return
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	writes := recorder.rowWrites()
	t.Logf("600 reads over 10 minutes: %d statements, rows written %v", statements, writes)
	if want := map[string]int{"tracker_cache_read_leases": 10}; statements != 11 || !maps.Equal(writes, want) {
		t.Fatalf("600 reads over 10 minutes: %d statements, rows written %v; pinned 11 and %v", statements, writes, want)
	}
}
