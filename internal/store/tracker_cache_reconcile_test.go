package store

import (
	"reflect"
	"testing"
)

// A complete listing older than the newest one the board reconciled sends
// no row to a read by ID: the newer listing saw the rows it leaves out.
func TestAnOlderListingSendsNoRowToARead(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", nil)
	row := webPage("10", webRow("10001", 5, "a"))
	row.ReadAt = mirrorReadAt(5)
	mustWriteRowPage(t, s, root, row, nil)
	lifetime := currentLifetime(s, root, "10")
	if got, err := s.ReconcileMirrorBoard(root, "10", lifetime, []string{"10001"}, true, mirrorReadAt(30)); err != nil || !reflect.DeepEqual(got, MirrorReconcile{}) {
		t.Fatalf("the newer listing = %+v, %v", got, err)
	}
	got, err := s.ReconcileMirrorBoard(root, "10", lifetime, []string{"10002"}, true, mirrorReadAt(20))
	if err != nil || !reflect.DeepEqual(got, MirrorReconcile{Missing: []string{"10002"}}) {
		t.Fatalf("the older listing = %+v, %v; want its missing issue alone", got, err)
	}
	assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_board_issues`, "10001")
}
