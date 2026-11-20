package store

import (
	"fmt"
	"testing"
)

// longHistory is n status changes of issue, one a time unit from 1, the
// last to the card's status "1"; final through n+extra.
func longHistory(issue string, n int, extra int64) MirrorHistory {
	h := MirrorHistory{IssueID: issue, CompleteTo: int64(n) + extra}
	for i := range n {
		to := []string{"1", "3"}[(n-1-i)%2]
		h.Changes = append(h.Changes, MirrorChange{Kind: MirrorChangeStatus, At: int64(i + 1), To: to})
	}
	return h
}

// mirrorHistoryShown is what a read sees of issue 10001's history: its
// generation's change count, its final time and its status time, and
// whether other generations or a staging mark are left.
func mirrorHistoryShown(t *testing.T, s *Store) string {
	t.Helper()
	var shown string
	err := s.db.QueryRow(`SELECT (SELECT count(*) FROM tracker_cache_changes c WHERE c.issue_id = i.issue_id AND c.gen = i.history_gen)
		  || ' to ' || i.history_to || ' since ' || i.status_since
		  || ' other ' || (SELECT count(*) FROM tracker_cache_changes c WHERE c.issue_id = i.issue_id AND c.gen <> i.history_gen)
		  || ' dirty ' || i.history_dirty
		FROM tracker_cache_issues i WHERE i.issue_id = '10001'`).Scan(&shown)
	if err != nil {
		t.Fatal(err)
	}
	return shown
}

// A history has no length bound. One longer than a chunk is staged in
// transactions of at most clearMirrorChunk changes and made the issue's
// history in one more, so no read sees part of it and no transaction
// writes or deletes more than a chunk; an identical one writes nothing; a
// longer one that keeps the stored changes appends in place.
func TestMirrorLongHistoryIsStagedWhole(t *testing.T) {
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a")})
	const n = 5000
	recorder := recordMirrorWrites(t, s)
	for _, step := range []struct {
		name               string
		history            MirrorHistory
		changed            bool
		statements, chunks int
		shown              string
	}{
		{"new", longHistory("10001", n, 1), true, 31, n / clearMirrorChunk, "5000 to 5001 since 5000 other 0 dirty 0"},
		{"identical", longHistory("10001", n, 1), false, 14, 0, "5000 to 5001 since 5000 other 0 dirty 0"},
		{"appended", longHistory("10001", n+100, 1), true, 22, 1, "5100 to 5101 since 5100 other 0 dirty 0"},
		{"shortened", longHistory("10001", 10, 6000), true, 34, 0, "10 to 6010 since 10 other 0 dirty 0"},
		{"grown again", longHistory("10001", n, 7000), true, 34, 0, "5000 to 12000 since 5000 other 0 dirty 0"},
	} {
		recorder.reset()
		var results []MirrorHistoryResult
		var err error
		sweep := currentSweep(s, root)
		statements := countMirrorStatements(t, func() { results, err = s.ReplaceMirrorHistories(root, sweep, []MirrorHistory{step.history}) })
		if err != nil || len(results) != 1 || results[0].Changed != step.changed {
			t.Fatalf("%s: %+v, %v", step.name, results, err)
		}
		most, transactions := recorder.mostChangesInATransaction()
		if most > clearMirrorChunk || (step.chunks > 0 && transactions < step.chunks) || (!step.changed && transactions > 0) {
			t.Errorf("%s: at most %d changes in one of %d transactions", step.name, most, transactions)
		}
		if deleted, _ := recorder.mostDeletedInATransaction(); deleted > clearMirrorChunk {
			t.Errorf("%s: deleted %d in one transaction", step.name, deleted)
		}
		if step.statements > 0 && statements != step.statements {
			t.Errorf("%s: %d statements; want %d", step.name, statements, step.statements)
		}
		if got := mirrorHistoryShown(t, s); got != step.shown {
			t.Errorf("%s: history %q; want %q", step.name, got, step.shown)
		}
	}
}

// A staging stopped between its chunks leaves the stored history what
// every read sees, before its switch, and the new one after it; the
// cleanup, or the issue's next history, deletes what it left.
func TestMirrorStagingStoppedBetweenChunks(t *testing.T) {
	t.Parallel()
	for _, stop := range []struct {
		name, trigger string
		shown         string
		finish        func(s *Store, root int64) error
	}{
		{"before the switch", `BEFORE INSERT ON tracker_cache_changes WHEN NEW.gen = 3 AND NEW.seq = 3000`,
			"10 to 6010 since 10 other 3000 dirty 1",
			func(s *Store, _ int64) error { return s.FinishMirrorCleanup(nil) }},
		{"after the switch", `BEFORE DELETE ON tracker_cache_changes WHEN OLD.gen = 1 AND OLD.seq = 2000`,
			"10 to 6010 since 10 other 3000 dirty 1",
			func(s *Store, root int64) error {
				_, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), []MirrorHistory{longHistory("10001", 10, 9000)})
				return err
			}},
	} {
		s, _, root := newMirror(t)
		writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a")})
		mustReplaceHistory(t, s, root, longHistory("10001", 5000, 1), MirrorWrite{Changed: true})
		next := longHistory("10001", 10, 6000)
		if stop.name == "before the switch" {
			mustReplaceHistory(t, s, root, next, MirrorWrite{Changed: true})
			next = longHistory("10001", 5000, 7000)
		}
		mustExec(t, s.db, fmt.Sprintf(`CREATE TRIGGER test_stop_staging %s BEGIN SELECT RAISE(ABORT, 'stopped'); END`, stop.trigger))
		if _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), []MirrorHistory{next}); err == nil {
			t.Fatalf("%s: the staging was not stopped", stop.name)
		}
		if got := mirrorHistoryShown(t, s); got != stop.shown {
			t.Errorf("%s: history %q; want %q", stop.name, got, stop.shown)
		}
		mustExec(t, s.db, `DROP TRIGGER test_stop_staging`)
		recorder := recordMirrorWrites(t, s)
		if err := stop.finish(s, root); err != nil {
			t.Fatalf("%s: finish: %v", stop.name, err)
		}
		if deleted, _ := recorder.mostDeletedInATransaction(); deleted > clearMirrorChunk {
			t.Errorf("%s: the finish deleted %d in one transaction", stop.name, deleted)
		}
		var other, dirty int
		if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM tracker_cache_changes c WHERE c.gen <> i.history_gen), i.history_dirty
			FROM tracker_cache_issues i`).Scan(&other, &dirty); err != nil || other != 0 || dirty != 0 {
			t.Errorf("%s: after the finish %d changes of other generations, dirty %d, %v", stop.name, other, dirty, err)
		}
	}
}
