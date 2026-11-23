package store

import (
	"reflect"
	"testing"
	"time"
)

// The reviewers' sequence: a row a delta read just before a restart, which
// the board's search listing does not show yet, is not removed by a
// reconcile begun right after the restart. The row was read less than the
// listing's lag ago by the wall clock, so the listing leaves it to a read
// by ID (Unsettled), whatever order time the restart gave the reconcile.
func TestARestartThenAReconcileRemovesNoLiveRow(t *testing.T) {
	s, _, root := newMirror(t)
	const before = 1_000_000
	writeMirrorBoard(t, s, root, "10", nil)
	listed := currentPage(s, root, webPage("10", webRow("10001", 5, "a")))
	listed.ReadAt = beginRead(t, s, root, before)
	mustWriteRowPage(t, s, root, listed, nil)
	delta := currentPage(s, root, webPage("10", webRow("10002", 6, "b")))
	delta.ReadAt = beginRead(t, s, root, before+1)
	mustWriteRowPage(t, s, root, delta, nil)
	s = reopenStore(t, s)
	read := beginRead(t, s, root, before+1_000)
	got, err := s.ReconcileMirrorBoard(root, "10", currentLifetime(s, root, "10"), []string{"10001"}, true, read, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if want := (MirrorReconcile{Unsettled: []string{"10002"}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("the reconcile after the restart = %+v; want %+v", got, want)
	}
	assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_board_issues ORDER BY issue_id`, "10001,10002")
}
