package store

import (
	"fmt"
	"reflect"
	"testing"
)

// A complete listing older than the newest one the board reconciled
// removes nothing: the newer listing saw the rows it leaves out.
func TestAnOlderListingRemovesNothing(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", nil)
	row := webPage("10", webRow("10001", 5, "a"))
	row.ReadAt = 5
	mustWriteRowPage(t, s, root, row, nil)
	lifetime := currentLifetime(s, root, "10")
	if got, err := s.ReconcileMirrorBoard(root, "10", lifetime, []string{"10001"}, true, 30, 30); err != nil || !reflect.DeepEqual(got, MirrorReconcile{}) {
		t.Fatalf("the newer listing = %+v, %v", got, err)
	}
	got, err := s.ReconcileMirrorBoard(root, "10", lifetime, []string{"10002"}, true, 20, 20)
	if err != nil || !reflect.DeepEqual(got, MirrorReconcile{Missing: []string{"10002"}}) {
		t.Fatalf("the older listing = %+v, %v; want its missing issue and nothing removed", got, err)
	}
	assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_board_issues`, "10001")
}

// The rows a complete listing leaves out are found once and removed a
// chunk per transaction, each judged again in its own: a newer complete
// listing reconciled between two chunks stops the removal, and a row a
// read after the settled bound stored between two chunks stays.
func TestAListingsLaterChunksAreJudgedAgain(t *testing.T) {
	t.Parallel()
	first := mirrorRows(20000, 1, "a")[0].Card.IssueID
	for name, between := range map[string]string{
		"a newer listing": `UPDATE tracker_cache_boards SET listing_read_at = 99 WHERE board_id = '10'`,
		"a newer read":    `UPDATE tracker_cache_board_issues SET read_at = 99 WHERE issue_id = '21000'`,
	} {
		s, _, root := newMirror(t)
		writeMirrorBoard(t, s, root, "10", mirrorRows(20000, clearMirrorChunk+1001, "a"))
		// The first chunk walks the board's order: its first row's delete
		// runs the change in the first chunk's transaction.
		mustExec(t, s.db, fmt.Sprintf(`CREATE TRIGGER test_between AFTER DELETE ON tracker_cache_board_issues WHEN OLD.issue_id = '%s'
			BEGIN %s; END`, first, between))
		got, err := s.ReconcileMirrorBoard(root, "10", currentLifetime(s, root, "10"), nil, true, 20, 20)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var left int
		if err := s.db.QueryRow(`SELECT count(*) FROM tracker_cache_board_issues`).Scan(&left); err != nil {
			t.Fatal(err)
		}
		switch name {
		case "a newer listing":
			if len(got.Removed) != clearMirrorChunk || left != 1001 {
				t.Errorf("%s: removed %d, left %d; want the first chunk alone", name, len(got.Removed), left)
			}
		default:
			if len(got.Removed) != clearMirrorChunk+1000 || left != 1 {
				t.Errorf("%s: removed %d, left %d; want every row but the one read again", name, len(got.Removed), left)
			}
			assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_board_issues`, "21000")
		}
	}
}
