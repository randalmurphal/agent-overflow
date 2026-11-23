package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// A card, detail or history read outside a board names the root's sweep
// captured before the read. After its issue is unkept, swept and kept
// again, the late write is refused and stores nothing; the same write
// naming the sweep after it is stored.
func TestMirrorSweepFencesReadsOutsideABoard(t *testing.T) {
	t.Parallel()
	history := MirrorHistory{IssueID: "10001", Changes: []MirrorChange{{Kind: MirrorChangeStatus, At: 2, From: "1", To: "3"}}}
	for _, kind := range []struct {
		name   string
		stored string
		write  func(s *Store, root, sweep int64) error
	}{
		{"card", `tracker_cache_issues`, func(s *Store, root, sweep int64) error {
			_, err := s.UpsertMirrorCards(root, sweep, []MirrorCard{webCard("10001", 9)}, MirrorCatalog{}, mirrorReadAt(2))
			return err
		}},
		{"body", `tracker_cache_issue_bodies`, func(s *Store, root, sweep int64) error {
			_, err := storeTicket(t, s, root, sweep, "10001", 9, 9, []byte(`{}`))
			return err
		}},
		{"history", `tracker_cache_changes`, func(s *Store, root, sweep int64) error {
			_, _, err := s.ReplaceMirrorHistories(root, sweep, dueHistories(s, root, []MirrorHistory{history}), MirrorCatalog{}, mirrorReadAt(0))
			return err
		}},
		{"staged history", `tracker_cache_changes`, func(s *Store, root, sweep int64) error {
			// The stage after the batch transaction, where a sweep can land.
			in, err := newMirrorHistoryInput(answeredHistory(statusHistory("10001", clearMirrorChunk+100)))
			if err != nil {
				return err
			}
			s.mirrorCleanup.Lock()
			defer s.mirrorCleanup.Unlock()
			return s.replaceStagedHistory(root, sweep, in, &MirrorHistoryResult{})
		}},
	} {
		s, _, root := newMirror(t)
		mustReplaceStructure(t, s, root, webStructure(), true)
		stored := fmt.Sprintf(`SELECT count(*) > 0 FROM %s WHERE root_id = %d AND issue_id = '10001'`, kind.stored, root)
		linkIssue(t, s, "linked", "10001")
		read := currentSweep(s, root)
		if _, err := s.UpsertMirrorCards(root, read, []MirrorCard{webCard("10001", 5)}, MirrorCatalog{}, mirrorReadAt(1)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClearThreadTicket("linked"); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishMirrorCleanup(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		if got := currentSweep(s, root); got != read+1 {
			t.Fatalf("%s: sweep %d after the cleanup; want %d", kind.name, got, read+1)
		}
		assertRows(t, s.db, fmt.Sprintf(`SELECT count(*) FROM tracker_cache_issues WHERE root_id = %d`, root), "0")
		linkIssue(t, s, "relinked", "10001")
		swept := currentSweep(s, root)
		if kind.name != "card" {
			if _, err := s.UpsertMirrorCards(root, swept, []MirrorCard{webCard("10001", 7)}, MirrorCatalog{}, mirrorReadAt(3)); err != nil {
				t.Fatal(err)
			}
		}
		if err := kind.write(s, root, read); !errors.Is(err, ErrTrackerCacheSwept) {
			t.Fatalf("%s: a write of a read from before the sweep: %v; want it refused", kind.name, err)
		}
		assertRows(t, s.db, stored, "0")
		if err := kind.write(s, root, swept+1); !errors.Is(err, ErrInvalidTrackerCache) {
			t.Fatalf("%s: a write naming a sweep never reached: %v", kind.name, err)
		}
		if kind.name != "staged history" {
			if err := kind.write(s, root, 0); !errors.Is(err, ErrInvalidTrackerCache) {
				t.Fatalf("%s: a write naming no sweep: %v", kind.name, err)
			}
			assertRows(t, s.db, stored, "0")
		}
		if err := kind.write(s, root, swept); err != nil {
			t.Fatalf("%s: a write of a read after the sweep: %v", kind.name, err)
		}
		assertRows(t, s.db, stored, "1")
	}
}

// A sweep page that deletes nothing leaves the sweep, so a read of an
// issue kept again before its page is not refused.
func TestMirrorSweepThatDeletesNothingLeavesTheSweep(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	linkIssue(t, s, "linked", "10001")
	read := currentSweep(s, root)
	if _, err := s.UpsertMirrorCards(root, read, []MirrorCard{webCard("10001", 5)}, MirrorCatalog{}, mirrorReadAt(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClearThreadTicket("linked"); err != nil {
		t.Fatal(err)
	}
	pauses := 0
	if err := s.FinishMirrorCleanup(context.Background(), func() {
		if pauses++; pauses == 1 {
			linkIssue(t, s, "relinked", "10001")
		}
	}); err != nil {
		t.Fatal(err)
	}
	if got := currentSweep(s, root); pauses == 0 || got != read {
		t.Fatalf("sweep %d after %d sweep pages that deleted nothing; want %d", got, pauses, read)
	}
	assertRows(t, s.db, fmt.Sprintf(`SELECT count(*) FROM tracker_cache_issues WHERE root_id = %d`, root), "1")
}

// A sweep that lands after a batch's transaction, before its long history
// is staged, refuses the staging: the batch hands its sweep on. A trigger
// on the batch's write stands in for that sweep.
func TestMirrorSweepBetweenBatchAndStaging(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	linkIssue(t, s, "long", "10001")
	linkIssue(t, s, "short", "10002")
	read := currentSweep(s, root)
	if _, err := s.UpsertMirrorCards(root, read, []MirrorCard{webCard("10001", 5), webCard("10002", 5)}, MirrorCatalog{}, mirrorReadAt(1)); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s.db, `CREATE TRIGGER test_sweep_between AFTER UPDATE OF history_for ON tracker_cache_issues WHEN NEW.issue_id = '10002'
		BEGIN UPDATE tracker_cache_roots SET sweep = sweep + 1; END`)
	short := MirrorHistory{IssueID: "10002", Changes: []MirrorChange{{Kind: MirrorChangeStatus, At: 2, From: "1", To: "3"}}}
	long := statusHistory("10001", clearMirrorChunk+100)
	if _, _, err := s.ReplaceMirrorHistories(root, read, dueHistories(s, root, []MirrorHistory{long, short}), MirrorCatalog{}, mirrorReadAt(0)); !errors.Is(err, ErrTrackerCacheSwept) {
		t.Fatalf("a staging after a sweep: %v; want it refused", err)
	}
	changes := func(issue string) string {
		return fmt.Sprintf(`SELECT count(*) > 0 FROM tracker_cache_changes WHERE root_id = %d AND issue_id = '%s'`, root, issue)
	}
	assertRows(t, s.db, changes("10001"), "0")
	assertRows(t, s.db, changes("10002"), "1")
	mustExec(t, s.db, `DROP TRIGGER test_sweep_between`)
	if _, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, []MirrorHistory{long}), MirrorCatalog{}, mirrorReadAt(0)); err != nil {
		t.Fatal(err)
	}
	assertRows(t, s.db, changes("10001"), "1")
}
