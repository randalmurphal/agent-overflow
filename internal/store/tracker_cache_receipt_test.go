package store

import (
	"fmt"
	"testing"
)

// storedHistoryShape is issue 10001's stored history as its wait, answer
// time and change count, with the changes of other generations and the
// staging mark: what a write that was refused must leave as it was.
func storedHistoryShape(t *testing.T, s *Store, root int64) string {
	t.Helper()
	var shape string
	err := s.db.QueryRow(`SELECT 'wait ' || i.history_wait || ' answered ' || i.history_answered_at
		  || ' changes ' || (SELECT count(*) FROM tracker_cache_changes c WHERE c.root_id = i.root_id AND c.issue_id = i.issue_id AND c.gen = i.history_gen)
		  || ' other ' || (SELECT count(*) FROM tracker_cache_changes c WHERE c.root_id = i.root_id AND c.issue_id = i.issue_id AND c.gen <> i.history_gen)
		  || ' dirty ' || i.history_dirty
		FROM tracker_cache_issues i WHERE i.root_id = ?1 AND i.issue_id = '10001'`, root).Scan(&shape)
	if err != nil {
		t.Fatal(err)
	}
	return shape
}

// Two reads of one issue's history listed at the same receipt, for the
// same update time and boards, whose answers land in either order: the
// first to land is written against the state both were listed from and
// moves the receipt; the second is superseded and changes nothing,
// whatever its times say. The older answer is not final and lacks a
// change the tracker listed late; the newer is final and holds it. Short
// histories go through the batch, long ones through staging: switched in
// when no history was stored, kept in place when one was. Over a stored
// history the older answer holds its changes, so landing first it stores
// times alone, its receipt moved: changed, as it is not final, which
// credit reads (mirrorHistoryReadForCard). With the
// stored history's own times, the older answer is that history again and
// writes nothing, so the newer one, its changes alone changed, is stored
// after it and refuses the older one after it.
func TestMirrorHistoryOverlappingReadsLandInEitherOrder(t *testing.T) {
	t.Parallel()
	late := MirrorChange{Kind: MirrorChangeStatus, At: 100_000, From: "1", To: "3"}
	for _, size := range []struct {
		name string
		n    int
	}{{"short", 3}, {"staged", clearMirrorChunk + 100}} {
		for _, tc := range []struct {
			name              string
			stored, sameTimes bool
			olderFirst        bool
			first, second     string
		}{
			{"none stored, older first", false, false, true, "changed", "superseded"},
			{"none stored, newer first", false, false, false, "changed", "superseded"},
			{"stored, older first", true, false, true, "changed", "superseded"},
			{"stored, newer first", true, false, false, "changed", "superseded"},
			{"stored, same times, older first", true, true, true, "unchanged", "changed"},
			{"stored, same times, newer first", true, true, false, "changed", "superseded"},
		} {
			t.Run(size.name+" "+tc.name, func(t *testing.T) {
				t.Parallel()
				s, _, root := newMirror(t)
				writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 100_000, "a")})
				base := statusHistory("10001", size.n)
				if tc.stored {
					mustReplaceHistory(t, s, root, base, MirrorWrite{Changed: true})
				}
				listed := receiptOf(t, s, root, "10001")
				older := listedAt(s, root, base, listed)
				if !tc.sameTimes {
					older.AnsweredAt, older.Wait = 1_000_000, 60_000
				}
				newer := listedAt(s, root, base, listed)
				newer.Changes = append(append([]MirrorChange(nil), base.Changes...), late)
				if !tc.sameTimes {
					newer.AnsweredAt = 1_060_000
				}
				order := []MirrorHistory{newer, older}
				if tc.olderFirst {
					order = []MirrorHistory{older, newer}
				}
				for i, want := range []string{tc.first, tc.second} {
					results, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), []MirrorHistory{order[i]}, MirrorCatalog{}, nextMirrorRead(s, root))
					if err != nil {
						t.Fatal(err)
					}
					got := map[MirrorHistoryResult]string{
						{IssueID: "10001"}: "unchanged", {IssueID: "10001", Changed: true}: "changed", {IssueID: "10001", Superseded: true}: "superseded",
					}[results[0]]
					if got != want {
						t.Fatalf("answer %d landing: %+v; want %s", i+1, results[0], want)
					}
				}
				// The newer answer's shape, or the older one's when it landed
				// first with times of its own.
				want := fmt.Sprintf("wait 0 answered 1060000 changes %d other 0 dirty 0", size.n+1)
				switch {
				case tc.sameTimes:
					want = fmt.Sprintf("wait 0 answered %d changes %d other 0 dirty 0", size.n+1, size.n+1)
				case tc.olderFirst:
					want = fmt.Sprintf("wait 60000 answered 1000000 changes %d other 0 dirty 0", size.n)
				}
				if got := storedHistoryShape(t, s, root); got != want {
					t.Fatalf("stored history %q; want %q", got, want)
				}
			})
		}
	}
}

// An issue the tracker no longer answers for is stored as an empty final
// history. That answer landing after a later read stored the issue's
// changes is superseded and erases nothing.
func TestMirrorHistoryLateMissingIssueAnswerIsSuperseded(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 100_000, "a")})
	listed := receiptOf(t, s, root, "10001")
	missing := listedAt(s, root, MirrorHistory{IssueID: "10001", CompleteFrom: 100_000, AnsweredAt: 1_000_000}, listed)
	found := listedAt(s, root, MirrorHistory{IssueID: "10001", AnsweredAt: 1_060_000,
		Changes: []MirrorChange{{Kind: MirrorChangeStatus, At: 100_000, From: "1", To: "3"}}}, listed)
	for i, h := range []MirrorHistory{found, missing} {
		results, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), []MirrorHistory{h}, MirrorCatalog{}, nextMirrorRead(s, root))
		if err != nil || results[0].Superseded != (i == 1) || results[0].Changed != (i == 0) {
			t.Fatalf("answer %d: %+v, %v", i+1, results, err)
		}
	}
	if got := storedHistoryShape(t, s, root); got != "wait 0 answered 1060000 changes 1 other 0 dirty 0" {
		t.Fatalf("stored history %q; want the found one", got)
	}
}

// A staged history is written against the receipt it was admitted with:
// a write that lands while it stages (here a trigger standing in for one)
// makes the switch refuse it, and what it staged is deleted, leaving the
// issue's history as it was. A write between its admission and its
// staging mark refuses it at the admission that follows the mark, which
// clears the mark.
func TestMirrorStagedHistoryWithAStaleReceiptIsDiscarded(t *testing.T) {
	t.Parallel()
	for name, trigger := range map[string]string{
		"while staging": `CREATE TRIGGER test_write_between AFTER INSERT ON tracker_cache_changes WHEN NEW.gen > 0
			BEGIN UPDATE tracker_cache_issues SET history_receipt = history_receipt + 1 WHERE issue_id = NEW.issue_id; END`,
		"before the mark": `CREATE TRIGGER test_write_between AFTER UPDATE OF history_dirty ON tracker_cache_issues WHEN NEW.history_dirty = 1
			BEGIN UPDATE tracker_cache_issues SET history_receipt = history_receipt + 1 WHERE issue_id = NEW.issue_id; END`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, _, root := newMirror(t)
			writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 100_000, "a")})
			mustReplaceHistory(t, s, root, statusHistory("10001", 3), MirrorWrite{Changed: true})
			before := storedHistoryShape(t, s, root)
			long := listedAt(s, root, statusHistory("10001", 2*clearMirrorChunk+100), receiptOf(t, s, root, "10001"))
			mustExec(t, s.db, trigger)
			results, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), []MirrorHistory{long}, MirrorCatalog{}, nextMirrorRead(s, root))
			if err != nil || results[0] != (MirrorHistoryResult{IssueID: "10001", Superseded: true}) {
				t.Fatalf("a staged history whose receipt went stale: %+v, %v; want superseded", results, err)
			}
			if got := storedHistoryShape(t, s, root); got != before {
				t.Fatalf("stored history %q; want it as it was, %q", got, before)
			}
		})
	}
}
