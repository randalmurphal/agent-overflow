package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// unkeptIssueOfSprints stores issue 10001's card naming n sprints while a
// thread links it, then unlinks it, so the next cleanup sweeps it, and
// returns the card.
func unkeptIssueOfSprints(t *testing.T, s *Store, root int64, n int) MirrorCard {
	t.Helper()
	mustReplaceStructure(t, s, root, webStructure(), true)
	linkIssue(t, s, "linked", "10001")
	card := webCard("10001", 5, sprintsOf(100_000, n)...)
	if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{card}, MirrorCatalog{}, mirrorReadAt(1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ClearThreadTicket("linked"); err != nil {
		t.Fatal(err)
	}
	return card
}

// sweepPauses runs the cleanup and returns its transactions (pauses),
// running at, if set, at the pause of that number. A cleanup that runs
// over limit transactions fails the test rather than spinning.
func sweepPauses(t *testing.T, s *Store, ctx context.Context, limit int, at map[int]func()) (int, error) {
	t.Helper()
	pauses := 0
	err := s.FinishMirrorCleanup(ctx, func() {
		pauses++
		if pauses > limit {
			t.Fatalf("the cleanup ran over %d transactions", limit)
		}
		if fn := at[pauses]; fn != nil {
			fn()
		}
	})
	return pauses, err
}

// An unkept issue of more memberships than a sweep transaction deletes is
// removed a chunk of them a transaction, and the sweep ends: an issue of
// 1,200 memberships takes three, of 500, 500 and the last 200 with the
// issue. Before the sweep could only cascade an issue's memberships, so
// such an issue's page deleted nothing, moved past nothing and was read
// again for ever.
func TestTheSweepRemovesManyMembershipsAcrossTransactions(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	unkeptIssueOfSprints(t, s, root, 1200)
	recorder := recordMirrorWrites(t, s)
	pauses, err := sweepPauses(t, s, context.Background(), 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pauses != 3 {
		t.Fatalf("%d sweep transactions; want 3", pauses)
	}
	if most, total := recorder.mostDeletedInATransaction(); most > clearMirrorChunk || total != 1201 {
		t.Fatalf("the sweep deleted %d rows, at most %d in a transaction", total, most)
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issues`, "0")
	if n := totalSprints(t, s, root); n != 0 {
		t.Fatalf("%d memberships left", n)
	}
}

// An issue kept again while the sweep removes its memberships stops being
// swept, keeps those left marked removed in part, and its next card write
// that the mirror admits, the same card read again, restores them all.
func TestAnIssueKeptAgainMidRemovalGetsItsSprintsBack(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	card := unkeptIssueOfSprints(t, s, root, 1200)
	pauses, err := sweepPauses(t, s, context.Background(), 10, map[int]func(){2: func() { linkIssue(t, s, "relinked", "10001") }})
	if err != nil {
		t.Fatal(err)
	}
	if pauses != 2 {
		t.Fatalf("%d sweep transactions; want the one before the link and the one that finds it kept", pauses)
	}
	assertRows(t, s.db, `SELECT count(*) || ':' || (SELECT hex(sprint_set) FROM tracker_cache_issues WHERE issue_id = '10001')
		FROM tracker_cache_issue_sprints WHERE issue_id = '10001'`, "700:"+mirrorSprintSetPartial)
	changes, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{card}, MirrorCatalog{}, mirrorReadAt(2))
	if err != nil || len(changes.Cards) != 1 {
		t.Fatalf("the card read again = %+v, %v; want it stored", changes, err)
	}
	if got := storedSprints(t, s, root, "10001"); len(got) != 1200 {
		t.Fatalf("the card's write left %d memberships; want 1,200", len(got))
	}
	assertRows(t, s.db, `SELECT lower(hex(sprint_set)) FROM tracker_cache_issues WHERE issue_id = '10001'`, mirrorSprintSet(card.Sprints))
}

// A cleanup whose context ends stops before its next transaction and
// returns the context's cause; what it left is finished by the next.
func TestACleanupStopsBetweenTransactions(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	unkeptIssueOfSprints(t, s, root, 1200)
	ctx, cancel := context.WithCancelCause(context.Background())
	stop := errors.New("the app stops")
	pauses, err := sweepPauses(t, s, ctx, 10, map[int]func(){2: func() { cancel(stop) }})
	if !errors.Is(err, stop) || !errors.Is(err, errMirrorCleanupStopped) {
		t.Fatalf("a stopped cleanup returned %v", err)
	}
	if pauses != 2 {
		t.Fatalf("%d pauses; want the stop at the second to end the cleanup", pauses)
	}
	if n := totalSprints(t, s, root); n != 1200-clearMirrorChunk {
		t.Fatalf("the stopped cleanup left %d memberships; want the first transaction's done", n)
	}
	if _, err := sweepPauses(t, s, ctx, 10, nil); !errors.Is(err, stop) {
		t.Fatalf("a cleanup of an ended context returned %v", err)
	}
	if _, err := sweepPauses(t, s, context.Background(), 10, nil); err != nil {
		t.Fatal(err)
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issues`, "0")
	if n := totalSprints(t, s, root); n != 0 {
		t.Fatal(fmt.Sprint(n, " memberships left"))
	}
}
