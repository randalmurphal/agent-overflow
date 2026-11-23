package store

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mirrorReadAt is the read of root 1, the first root a test makes, that
// began at at by its order and wall times, for the package's tests of what
// a write does with a given read time. No production build has it: only
// BeginMirrorRead makes a read there. Zero is no read.
func mirrorReadAt(at int64) MirrorRead { return mirrorReadOf(1, at) }

// mirrorReadOf is mirrorReadAt of root.
func mirrorReadOf(root, at int64) MirrorRead {
	if at == 0 {
		return MirrorRead{}
	}
	return MirrorRead{root: root, rootOrder: at, wall: at}
}

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
// the root stored, though the wall clock stepped back past them: the
// process's first read of the root seeds its clock from what the root
// stores, so a crash between a write and the next read leaves the same
// order as the close here.
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
	if after.At() != newest.At()+1 || after.Wall() != 900_000 {
		t.Fatalf("the first read after the restart began at %d by the wall %d; want %d, after the stored %d, by the wall 900000",
			after.At(), after.Wall(), newest.At()+1, newest.At())
	}
}

// After the wall clock steps back, reads begin a millisecond apart above
// the last one, so a card read after the step replaces the one read
// before it; once the clock passes them again, reads begin at the clock.
// Each read's wall time is the clock's.
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
	if stepped.At() != before.At()+1 || stepped.Wall() != 40_000 {
		t.Fatalf("the read after the step began at %d by the wall %d; want %d by 40000", stepped.At(), stepped.Wall(), before.At()+1)
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
// millisecond a read, each after the last, each at the clock's wall time.
func TestABurstOfReadsRunsAheadOfTheClock(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	const now, burst = 500_000, 100
	last := MirrorRead{}
	for i := range burst {
		read := beginRead(t, s, root, now)
		if read.At() != now+int64(i) || read.At() <= last.At() || read.Wall() != now {
			t.Fatalf("read %d of the burst began at %d by the wall %d after %d; want %d by %d", i, read.At(), read.Wall(), last.At(), now+int64(i), now)
		}
		last = read
	}
}

// hasMirrorReadClock reports whether the process holds a read clock of
// root.
func hasMirrorReadClock(s *Store, root int64) bool {
	s.mirrorReads.mu.Lock()
	defer s.mirrorReads.mu.Unlock()
	_, ok := s.mirrorReads.roots[root]
	return ok
}

// A read of a root that never existed, of a detached root, or of the root
// of a deleted connection fails with ErrTrackerCacheGone and makes no
// clock; a detach and a connection's delete remove the clock a read made.
// After a detach the connection attached again has a new root, whose
// clock is seeded from its own rows: the old root's reads do not order
// it, and the old root stays gone.
func TestAReadClockIsMadeOnlyForALiveRoot(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	if _, err := s.BeginMirrorRead(root+100, 1_000); !errors.Is(err, ErrTrackerCacheGone) || hasMirrorReadClock(s, root+100) {
		t.Fatalf("a read of a root that never existed: %v, clock %v", err, hasMirrorReadClock(s, root+100))
	}
	beginRead(t, s, root, 5_000_000)
	if _, err := s.DetachMirror(jira.ID); err != nil {
		t.Fatal(err)
	}
	if hasMirrorReadClock(s, root) {
		t.Fatal("the detach left the root's clock")
	}
	if _, err := s.BeginMirrorRead(root, 1_000); !errors.Is(err, ErrTrackerCacheGone) || hasMirrorReadClock(s, root) {
		t.Fatalf("a read of a detached root: %v, clock %v", err, hasMirrorReadClock(s, root))
	}
	again, created, err := s.GetOrCreateMirrorRoot(jira.ID)
	if err != nil || !created || again == root {
		t.Fatalf("attach again = %d, %v, %v", again, created, err)
	}
	if read := beginRead(t, s, again, 1_000); read.At() != 1_000 {
		t.Fatalf("the new root's first read began at %d; want the clock's 1000", read.At())
	}
	if _, err := s.BeginMirrorRead(root, 1_000); !errors.Is(err, ErrTrackerCacheGone) {
		t.Fatalf("a read of the old root after the attach: %v", err)
	}
	if _, err := s.DeleteTrackerConnection(jira.ID); err != nil {
		t.Fatal(err)
	}
	if hasMirrorReadClock(s, again) {
		t.Fatal("the connection's delete left its root's clock")
	}
	if _, err := s.BeginMirrorRead(again, 2_000); !errors.Is(err, ErrTrackerCacheGone) || hasMirrorReadClock(s, again) {
		t.Fatalf("a read of a deleted connection's root: %v, clock %v", err, hasMirrorReadClock(s, again))
	}
}

// The drain of a detached root removes its clock with the root: a clock
// left for it (here put in by hand, which no path makes) does not outlive
// the drain.
func TestADrainRemovesTheRootsClock(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	if _, err := s.DetachMirror(jira.ID); err != nil {
		t.Fatal(err)
	}
	s.mirrorReads.mu.Lock()
	s.mirrorReads.roots = map[int64]int64{root: 9_000_000}
	s.mirrorReads.mu.Unlock()
	if err := s.FinishMirrorCleanup(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if hasMirrorReadClock(s, root) {
		t.Fatal("the drain left the root's clock")
	}
}

// A restore removes every root's clock with the mirror, and a second
// restore removes the clock of the root made after the first; no
// pre-restore root reads again.
func TestARestoreRemovesTheReadClocks(t *testing.T) {
	t.Parallel()
	st := snapshotTestStore(t)
	jira := mustCreateTrackerConnection(t, st, jiraConnection(mirrorSite, "user1@example.com", 10))
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := st.SnapshotTo(snap); err != nil {
		t.Fatal(err)
	}
	var roots []int64
	for restore := range 2 {
		root, _, err := st.GetOrCreateMirrorRoot(jira.ID)
		if err != nil {
			t.Fatal(err)
		}
		beginRead(t, st, root, 1_000)
		if _, err := st.RestoreFrom(snap); err != nil {
			t.Fatalf("restore %d: %v", restore, err)
		}
		roots = append(roots, root)
		for _, r := range roots {
			if hasMirrorReadClock(st, r) {
				t.Fatalf("after restore %d the clock of root %d is held", restore, r)
			}
			if _, err := st.BeginMirrorRead(r, 2_000); !errors.Is(err, ErrTrackerCacheGone) || hasMirrorReadClock(st, r) {
				t.Fatalf("after restore %d a read of root %d: %v", restore, r, err)
			}
		}
	}
}

// Ten minutes of reads a second cost one statement, the process's first
// read of the root, which seeds its clock, and no row write.
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
	if statements != 1 || len(writes) != 0 {
		t.Fatalf("600 reads over 10 minutes: %d statements, rows written %v; pinned 1 and none", statements, writes)
	}
}

// mirrorNotReadTimes are the INTEGER columns of the mirror's tables, other
// than root_id, that hold no read's order time, each with what it holds
// instead.
var mirrorNotReadTimes = map[string]string{
	"tracker_cache_roots.id":                       "the root",
	"tracker_cache_roots.created_at":               "when the root was made, by the wall clock",
	"tracker_cache_roots.sweep":                    "a counter",
	"tracker_cache_statuses.status_group":          "an enum",
	"tracker_cache_boards.lifetime":                "a counter",
	"tracker_cache_boards.plans_sprints":           "a flag",
	"tracker_cache_boards.estimate_unit":           "an enum",
	"tracker_cache_boards.revision":                "a counter",
	"tracker_cache_boards.totals_revision":         "a counter",
	"tracker_cache_boards.retired":                 "a flag",
	"tracker_cache_boards.structure_wall":          "a wall time (MirrorRead.Wall)",
	"tracker_cache_sync.read_wall":                 "a wall time (MirrorRead.Wall)",
	"tracker_cache_sync.failed_wall":               "a wall time (MirrorRead.Wall)",
	"tracker_cache_board_unexplained.kind":         "an enum",
	"tracker_cache_board_columns.position":         "a position",
	"tracker_cache_board_column_statuses.position": "a position",
	"tracker_cache_board_column_statuses.rank":     "a position",
	"tracker_cache_sprints.position":               "a position",
	"tracker_cache_sprints.state":                  "an enum",
	"tracker_cache_sprints.start_at":               "the tracker's clock",
	"tracker_cache_sprints.end_at":                 "the tracker's clock",
	"tracker_cache_sprints.completed_at":           "the tracker's clock",
	"tracker_cache_issues.type_level":              "a level",
	"tracker_cache_issues.created_at":              "the tracker's clock",
	"tracker_cache_issues.updated_at":              "the tracker's clock",
	"tracker_cache_issues.resolved_at":             "the tracker's clock",
	"tracker_cache_issues.status_since":            "the tracker's clock",
	"tracker_cache_issues.history_from":            "the tracker's clock",
	"tracker_cache_issues.history_for":             "the tracker's clock",
	"tracker_cache_issues.history_answered_at":     "the steady clock of the process that answered",
	"tracker_cache_issues.history_wait":            "a duration",
	"tracker_cache_issues.history_receipt":         "a counter",
	"tracker_cache_issues.history_gen":             "a counter",
	"tracker_cache_issues.history_dirty":           "a flag",
	"tracker_cache_issues.boards_rev":              "a counter",
	"tracker_cache_issues.history_boards_rev":      "a counter",
	"tracker_cache_issues.board_rows":              "a count",
	"tracker_cache_changes.gen":                    "a counter",
	"tracker_cache_changes.kind":                   "an enum",
	"tracker_cache_changes.seq":                    "a position",
	"tracker_cache_changes.at":                     "the tracker's clock",
	"tracker_cache_issues.status_final_for":        "the tracker's clock",
	"tracker_cache_issues.body_updated_at":         "the tracker's clock",
	"tracker_cache_issues.body_wall":               "a wall time (MirrorRead.Wall)",
	"tracker_cache_worklogs.spent_seconds":         "a duration",
	"tracker_cache_worklogs.updated_at":            "the tracker's clock",
	"tracker_cache_sync.reconciled_wall":           "a wall time (MirrorRead.Wall)",
	"tracker_cache_open_tickets.session":           "a process session",
	"tracker_cache_open_tickets.pump":              "a counter",
}

// Every INTEGER column of every mirror table is a read time the clock's
// seed reads (mirrorReadTimeColumns), a read time its table's CHECK bounds
// by another (mirrorBoundedReadTimes), or is named as no read time: a
// column added to a mirror table fails here until it is classified. Each listed
// column moves the seed: a process's first read of the root begins after
// a value put in that column alone.
func TestEveryReadTimeColumnSeedsTheClock(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	seedFullMirror(t, s, root)
	listed := map[string]bool{}
	for _, tc := range mirrorReadTimeColumns {
		for _, c := range tc.columns {
			listed[tc.table+"."+c] = true
		}
	}
	tables, err := queryIDs(s.db, `SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'tracker\_cache\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		columns, err := queryIDs(s.db, `SELECT name FROM pragma_table_info(?) WHERE type = 'INTEGER' ORDER BY cid`, table)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range columns {
			name := table + "." + c
			_, other := mirrorNotReadTimes[name]
			if check, ok := mirrorBoundedReadTimes[name]; ok {
				var sql string
				if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE name = ?`, table).Scan(&sql); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(sql, "CHECK("+check+")") || listed[name] || other {
					t.Errorf("%s is bounded by %q, which its table does not check, or is classified twice", name, check)
				}
				continue
			}
			switch {
			case c == "root_id" || other:
				if listed[name] {
					t.Errorf("%s is both a read time and %q", name, mirrorNotReadTimes[name])
				}
			case !listed[name]:
				t.Errorf("%s is not classified: list it in mirrorReadTimeColumns or name it in mirrorNotReadTimes", name)
			}
			delete(listed, name)
		}
	}
	for name := range listed {
		t.Errorf("mirrorReadTimeColumns lists %s, which the schema does not have", name)
	}
	high := int64(9_000_000)
	for _, tc := range mirrorReadTimeColumns {
		for _, c := range tc.columns {
			high += 1_000
			result, err := s.db.Exec(`UPDATE `+tc.table+` SET `+c+` = ?2 WHERE root_id = ?1`, root, high)
			if err != nil {
				t.Fatalf("%s.%s: %v", tc.table, c, err)
			}
			if n, err := result.RowsAffected(); err != nil || n == 0 {
				t.Fatalf("%s.%s: the fixture holds no row to set: %d, %v", tc.table, c, n, err)
			}
			s.forgetMirrorReads()
			if read := beginRead(t, s, root, 1_000); read.At() != high+1 {
				t.Errorf("with %s.%s at %d the first read began at %d; want %d", tc.table, c, high, read.At(), high+1)
			}
		}
	}
}

// Every writer that takes a read refuses a read of another root, and so
// writes nothing to its own; the same write with a read of its root is
// stored.
func TestEveryMirrorWriterRefusesAReadOfAnotherRoot(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	other := mustCreateTrackerConnection(t, s, jiraConnection("https://example2.atlassian.net", "user1@example.com", 10))
	otherRoot, _, err := s.GetOrCreateMirrorRoot(other.ID)
	if err != nil {
		t.Fatal(err)
	}
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 5, "a")), nil)
	sweep := currentSweep(s, root)
	at := int64(1_000)
	for _, w := range []struct {
		name  string
		write func(read MirrorRead) error
	}{
		{"UpsertMirrorCards", func(read MirrorRead) error {
			_, err := s.UpsertMirrorCards(root, sweep, []MirrorCard{webCard("10001", 5)}, MirrorCatalog{}, read)
			return err
		}},
		{"ReplaceMirrorStructure", func(read MirrorRead) error {
			st := currentStructure(s, root, webStructure())
			st.ReadAt = read
			_, err := s.ReplaceMirrorStructure(root, st)
			return err
		}},
		{"WriteMirrorRowPage", func(read MirrorRead) error {
			page := currentPage(s, root, webPage("10", webRow("10001", 5, "a")))
			page.ReadAt = read
			_, err := s.WriteMirrorRowPage(root, page, nil)
			return err
		}},
		{"WriteMirrorRows", func(read MirrorRead) error {
			page := currentPage(s, root, webPage("10", webRow("10001", 5, "a")))
			page.ReadAt = read
			_, err := s.WriteMirrorRows(root, page)
			return err
		}},
		{"ReconcileMirrorBoard", func(read MirrorRead) error {
			_, err := s.ReconcileMirrorBoard(root, "10", currentLifetime(s, root, "10"), []string{"10001"}, true, read)
			return err
		}},
		{"ReplaceMirrorHistories", func(read MirrorRead) error {
			_, _, err := s.ReplaceMirrorHistories(root, sweep, dueHistories(s, root, []MirrorHistory{webHistory("10001")}),
				MirrorCatalog{Users: []MirrorUser{{ID: "acc-user1", Name: "User 1"}}}, read)
			return err
		}},
		{"ReplaceMirrorWorklogs", func(read MirrorRead) error {
			_, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-05", read, nil)
			return err
		}},
		{"StoreMirrorTickets read", func(read MirrorRead) error {
			_, err := s.StoreMirrorTickets(root, sweep, []MirrorTicket{heldTicket(t, s, root, "10001", []byte(`{}`))}, MirrorCatalog{},
				mirrorReadOf(root, 1), read)
			return err
		}},
		{"StoreMirrorTickets opening", func(read MirrorRead) error {
			_, err := s.StoreMirrorTickets(root, sweep, []MirrorTicket{heldTicket(t, s, root, "10001", []byte(`{}`))}, MirrorCatalog{},
				read, mirrorReadOf(root, 1_000_000))
			return err
		}},
		{"ConfirmMirrorBody", func(read MirrorRead) error {
			b, ok, err := s.GetMirrorBody(root, "10001")
			if err != nil || !ok {
				return fmt.Errorf("no body to confirm: %v, %w", ok, err)
			}
			_, err = s.ConfirmMirrorBody(root, b, read)
			return err
		}},
		{"KeepMirrorTicket", func(read MirrorRead) error {
			_, err := s.KeepMirrorTicket(root, "10001", 7, 1, read)
			return err
		}},
		{"RecordMirrorSyncAttempt read", func(read MirrorRead) error {
			a := currentAttempt(s, root, "10", MirrorSyncAttempt{})
			a.ReadAt = read
			_, err := s.RecordMirrorSyncAttempt(root, "10", a)
			return err
		}},
		{"RecordMirrorSyncAttempt reconcile", func(read MirrorRead) error {
			a := currentAttempt(s, root, "10", MirrorSyncAttempt{})
			a.ReconciledAt = read
			_, err := s.RecordMirrorSyncAttempt(root, "10", a)
			return err
		}},
		{"RecordMirrorSyncAttempt failure", func(read MirrorRead) error {
			_, err := s.RecordMirrorSyncAttempt(root, "", MirrorSyncAttempt{Failure: MirrorAttemptFailure{At: read, Kind: MirrorFailureTransient, Message: "m"}})
			return err
		}},
	} {
		at += 1_000
		before := countMirrorRows(t, s.db)
		if err := w.write(mirrorReadOf(otherRoot, at)); !errors.Is(err, ErrInvalidTrackerCache) || !strings.Contains(err.Error(), "a read of tracker mirror") {
			t.Errorf("%s with a read of another root: %v; want it refused", w.name, err)
		}
		if after := countMirrorRows(t, s.db); !maps.Equal(before, after) {
			t.Errorf("%s with a read of another root wrote rows: %v, then %v", w.name, before, after)
		}
		if err := w.write(mirrorReadOf(root, at)); err != nil {
			t.Errorf("%s with a read of its root: %v", w.name, err)
		}
	}
}

// Only At and the root check read a read's order time: a writer reaches
// it through MirrorRead.in, which refuses a read of another root, and no
// file of the package but tracker_cache_read.go builds a read.
func TestOnlyTheRootCheckReadsAReadsOrder(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") || file == "tracker_cache_read.go" {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{".rootOrder", "MirrorRead{root", ".At()"} {
			if strings.Contains(string(data), forbidden) {
				t.Errorf("%s holds %q: a writer takes a read's times through MirrorRead.in", file, forbidden)
			}
		}
	}
}
