package store

import (
	"context"
	"fmt"
	"runtime"
	"testing"
)

// longHistory is n status changes of issue, one a time unit from 1, the
// last to the card's status "1"; answered at n+extra.
func longHistory(issue string, n int, extra int64) MirrorHistory {
	h := MirrorHistory{IssueID: issue, AnsweredAt: int64(n) + extra}
	for i := range n {
		to := []string{"1", "3"}[(n-1-i)%2]
		h.Changes = append(h.Changes, MirrorChange{Kind: MirrorChangeStatus, At: int64(i + 1), To: to})
	}
	return h
}

// mirrorHistoryShown is what a read sees of issue 10001's history: its
// generation's change count, when its answer arrived and its status time, and
// whether other generations or a staging mark are left.
func mirrorHistoryShown(t *testing.T, s *Store) string {
	t.Helper()
	var shown string
	err := s.db.QueryRow(`SELECT (SELECT count(*) FROM tracker_cache_changes c WHERE c.issue_id = i.issue_id AND c.gen = i.history_gen)
		  || ' answered ' || i.history_answered_at || ' since ' || i.status_since
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
// writes or deletes more than a chunk; an identical one writes nothing,
// its status result checked in one transaction of its own
// (writeIdenticalHistory); a longer one that keeps the stored changes
// appends in place.
func TestMirrorLongHistoryIsStagedWhole(t *testing.T) {
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 10_000, "a")})
	const n = 5000
	recorder := recordMirrorWrites(t, s)
	for _, step := range []struct {
		name               string
		history            MirrorHistory
		changed            bool
		statements, chunks int
		shown              string
	}{
		{"new", longHistory("10001", n, 1), true, 31, n / clearMirrorChunk, "5000 answered 5001 since 5000 other 0 dirty 0"},
		{"identical", longHistory("10001", n, 1), false, 17, 0, "5000 answered 5001 since 5000 other 0 dirty 0"},
		{"appended", longHistory("10001", n+100, 1), true, 23, 1, "5100 answered 5101 since 5100 other 0 dirty 0"},
		{"shortened", longHistory("10001", 10, 6000), true, 34, 0, "10 answered 6010 since 10 other 0 dirty 0"},
		{"grown again", longHistory("10001", n, 7000), true, 34, 0, "5000 answered 12000 since 5000 other 0 dirty 0"},
	} {
		recorder.reset()
		var results []MirrorHistoryResult
		var err error
		sweep, histories, read := currentSweep(s, root), dueHistories(s, root, []MirrorHistory{step.history}), nextMirrorRead(s, root)
		statements := countMirrorStatements(t, func() {
			results, _, err = s.ReplaceMirrorHistories(root, sweep, histories, MirrorCatalog{}, read)
		})
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

// A staged history that is not final for the card gives the card's status
// time provisionally, as a batch's does (MirrorStatusSince); the final one
// makes it final.
func TestMirrorStagedHistoryIsProvisionalUntilFinal(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 10_000, "a")})
	const n = 5000
	mustReplaceHistory(t, s, root, longHistory("10001", n, 1), MirrorWrite{Changed: true})
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 10_001, "a")), nil)
	// Every change a time unit later: no stored change is kept, so the
	// history is staged as a new generation and switched to.
	later := func(wait int64) MirrorHistory {
		h := longHistory("10001", n, 2)
		for i := range h.Changes {
			h.Changes[i].At++
		}
		h.Wait = wait
		return h
	}
	gen := func() (g int64) {
		t.Helper()
		if err := s.db.QueryRow(`SELECT history_gen FROM tracker_cache_issues WHERE issue_id = '10001'`).Scan(&g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	before := gen()
	mustReplaceHistory(t, s, root, later(60_000), MirrorWrite{Changed: true})
	if gen() == before {
		t.Fatal("the history was not staged as a new generation")
	}
	if got := statusOf(t, s, root, "10001"); got != fmt.Sprintf("%d false 10001", n+1) {
		t.Fatalf("after a staged history still waiting: %s; want its change within the margin, %d, provisionally", got, n+1)
	}
	mustReplaceHistory(t, s, root, later(0), MirrorWrite{Changed: true})
	if got := statusOf(t, s, root, "10001"); got != fmt.Sprintf("%d true 10001", n+1) {
		t.Fatalf("after the final history: %s; want %d, final", got, n+1)
	}
}

// A history whose changes and times the stored history already holds
// writes the status result it gives the card as any history write does:
// a staged one (too long for a batch) and a batch one end the same, and
// both report the change. Between the two, a ticket's final evidence of
// the same version moved the time.
func TestAnIdenticalHistoryWritesItsStatusResultStagedOrNot(t *testing.T) {
	t.Parallel()
	for name, n := range map[string]int{"batch": 10, "staged": 5000} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, _, root := newMirror(t)
			mustReplaceStructure(t, s, root, webStructure(), true)
			mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 10_000, "a")), nil)
			// The last change enters the card's status 1 at n.
			h := longHistory("10001", n, 1)
			mustReplaceHistory(t, s, root, h, MirrorWrite{Changed: true})
			want := fmt.Sprintf("%d true 10000", n)
			if got := statusOf(t, s, root, "10001"); got != want {
				t.Fatalf("after the history: %s; want %s", got, want)
			}
			card := webCard("10001", 10_000)
			e := MirrorStatusEvidence{Status: card.StatusID, CreatedAt: card.CreatedAt, UpdatedAt: 10_000, LatestTo: card.StatusID, LatestAt: 2, Final: true}
			if _, err := s.StoreMirrorTickets(root, currentSweep(s, root), []MirrorTicket{{Card: card, Body: []byte(`{}`), Status: &e}},
				MirrorCatalog{}, mirrorReadAt(20), mirrorReadAt(20)); err != nil {
				t.Fatal(err)
			}
			if got := statusOf(t, s, root, "10001"); got != "2 true 10000" {
				t.Fatalf("after the ticket's evidence: %s", got)
			}
			mustReplaceHistory(t, s, root, h, MirrorWrite{Changed: true})
			if got := statusOf(t, s, root, "10001"); got != want {
				t.Fatalf("after the same history again: %s; want %s, its result", got, want)
			}
			mustReplaceHistory(t, s, root, h, MirrorWrite{})
		})
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
			"10 answered 6010 since 10 other 3000 dirty 1",
			func(s *Store, _ int64) error { return s.FinishMirrorCleanup(context.Background(), nil) }},
		{"after the switch", `BEFORE DELETE ON tracker_cache_changes WHEN OLD.gen = 1 AND OLD.seq = 2000`,
			"10 answered 6010 since 10 other 3000 dirty 1",
			func(s *Store, root int64) error {
				_, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, []MirrorHistory{longHistory("10001", 10, 9000)}), MirrorCatalog{}, nextMirrorRead(s, root))
				return err
			}},
	} {
		s, _, root := newMirror(t)
		// The card is of an update after every change the histories hold.
		writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 20_000, "a")})
		mustReplaceHistory(t, s, root, longHistory("10001", 5000, 1), MirrorWrite{Changed: true})
		next := longHistory("10001", 10, 6000)
		if stop.name == "before the switch" {
			mustReplaceHistory(t, s, root, next, MirrorWrite{Changed: true})
			next = longHistory("10001", 5000, 7000)
		}
		mustExec(t, s.db, fmt.Sprintf(`CREATE TRIGGER test_stop_staging %s BEGIN SELECT RAISE(ABORT, 'stopped'); END`, stop.trigger))
		if _, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, []MirrorHistory{next}), MirrorCatalog{}, nextMirrorRead(s, root)); err == nil {
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

// mirrorProbeMaxBytes bounds what comparing a history with the stored one
// allocates beyond the input: a chunk of changes, its encoding and what
// the comparison reads back, whatever the history's length.
const mirrorProbeMaxBytes = 512 << 10

// Comparing a long history with the stored one works a chunk at a time and
// stops at the chunk holding the first difference: a 100,000-change history
// whose first change differs allocates no more than mirrorProbeMaxBytes.
func TestMirrorHistoryProbeStopsAtTheFirstDifference(t *testing.T) {
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a")})
	mustReplaceHistory(t, s, root, longHistory("10001", 11, 1), MirrorWrite{Changed: true})
	in, err := newMirrorHistoryInput(answeredHistory(longHistory("10001", 100000, 1)), nextMirrorRead(s, root).rootOrder)
	if err != nil {
		t.Fatal(err)
	}
	var probe mirrorHistoryProbe
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	probe, err = s.probeMirrorHistory(root, in, 0)
	runtime.ReadMemStats(&after)
	if err != nil || !probe.differs {
		t.Fatalf("probe = %+v, %v; want a difference", probe.differs, err)
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	if allocated > mirrorProbeMaxBytes {
		t.Errorf("the probe allocated %d bytes; want at most %d", allocated, mirrorProbeMaxBytes)
	}
	t.Logf("the probe allocated %d bytes", allocated)
}
