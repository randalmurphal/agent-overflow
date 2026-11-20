package store

import (
	"fmt"
	"reflect"
	"testing"
)

// A detail the cap evicted starts a new lifetime when it is opened again:
// the opening that creates its row is its read watermark, so a document
// from a read that began before it, the eviction's lifetime included, is
// superseded, and one from a read that began at or after it is stored. A
// write that opens the detail itself stores its document only when its read
// began no earlier than its opening.
func TestMirrorReopenedDetailRefusesReadsFromBeforeTheOpening(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", mirrorRows(10000, mirrorDetailCap+2, "a"))
	sweep := currentSweep(s, root)
	got, err := s.StoreMirrorDetail(root, sweep, "10000", 10, 50, []byte(`{"v":"new"}`))
	mustDetailWrite(t, "first opening", got, err, MirrorDetailWrite{Stored: true, Changed: true})
	for i := range mirrorDetailCap {
		if _, err := s.StoreMirrorDetail(root, sweep, fmt.Sprint(10001+i), int64(100+i), int64(100+i), []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_details WHERE issue_id = '10000'`, "0")
	got, err = s.StoreMirrorDetail(root, sweep, "10000", 400, 300, []byte(`{"v":"stale"}`))
	mustDetailWrite(t, "opened again, its read earlier", got, err, MirrorDetailWrite{Stored: true, Superseded: true, Dropped: []string{"10001"}})
	got, err = s.StoreMirrorDetail(root, sweep, "10000", 0, 20, []byte(`{"v":"old"}`))
	mustDetailWrite(t, "a read from before the eviction", got, err, MirrorDetailWrite{Stored: true, Superseded: true})
	if d, ok, err := s.GetMirrorDetail(root, "10000"); err != nil || ok {
		t.Fatalf("detail after a superseded read = %+v, %v, %v; want none", d, ok, err)
	}
	got, err = s.StoreMirrorDetail(root, sweep, "10000", 0, 400, []byte(`{"v":"now"}`))
	mustDetailWrite(t, "a read from the opening", got, err, MirrorDetailWrite{Stored: true, Changed: true})
	assertRows(t, s.db, `SELECT opened_at || ' ' || read_at || ' ' || (data ->> 'v') FROM tracker_cache_issue_details WHERE issue_id = '10000'`, "400 400 now")

	got, err = s.StoreMirrorDetail(root, sweep, "10201", 500, 450, []byte(`{"v":"old"}`))
	mustDetailWrite(t, "an opening later than its own read", got, err, MirrorDetailWrite{Stored: true, Superseded: true, Dropped: []string{"10002"}})
	assertRows(t, s.db, `SELECT opened_at || ' ' || read_at || ' ' || (data IS NULL) FROM tracker_cache_issue_details WHERE issue_id = '10201'`, "500 500 1")
}

// A board row that leaves the board's window (a done issue aging out, read
// as missing) and returns keeps its board's watermarks: the removal time
// fences a read that began before it from adding the row back, and the
// returned row's read time fences one from changing it.
func TestMirrorRowThatLeavesAndReturnsRefusesAnEarlierRead(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a")})
	write := func(readAt int64, rows []MirrorRow, missing ...string) MirrorChanges {
		t.Helper()
		changes, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Rows: rows, Missing: missing, ReadAt: readAt}))
		if err != nil {
			t.Fatal(err)
		}
		return changes
	}
	write(30, nil, "10001")
	if got := write(20, []MirrorRow{webRow("10001", 5, "b")}); !reflect.DeepEqual(got.Superseded, []string{"10001"}) {
		t.Fatalf("a read from before the removal, while the row is gone = %+v", got)
	}
	write(40, []MirrorRow{webRow("10001", 5, "c")})
	if got := write(20, []MirrorRow{webRow("10001", 5, "b")}); !reflect.DeepEqual(got.Superseded, []string{"10001"}) {
		t.Fatalf("a read from before the removal, after the row returned = %+v", got)
	}
	assertRows(t, s.db, `SELECT order_key FROM tracker_cache_board_issues WHERE issue_id = '10001'`, "c")
}

// A sprint that leaves the structure's window (closed long ago) and
// returns is fenced by the board's structure watermark, which its removal
// does not reset: a structure read that began before the return stores
// nothing.
func TestMirrorSprintThatLeavesAndReturnsRefusesAnEarlierStructure(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	structure := func(readAt int64, sprints ...MirrorSprint) MirrorStructure {
		st := webStructure()
		st.Sprints, st.ReadAt = sprints, readAt
		return st
	}
	closed := webStructure().Sprints[2]
	mustReplaceStructure(t, s, root, structure(10, closed), true)
	mustReplaceStructure(t, s, root, structure(30), true)
	returned := closed
	returned.Name = "WEB Sprint 1 renamed"
	mustReplaceStructure(t, s, root, structure(40, returned), true)
	if got, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, structure(20, closed))); err != nil || got.MirrorWrite != (MirrorWrite{Superseded: true}) {
		t.Fatalf("a structure read from before the return = %+v, %v; want superseded", got, err)
	}
	assertRows(t, s.db, `SELECT name FROM tracker_cache_sprints WHERE sprint_id = '20'`, "WEB Sprint 1 renamed")
}
