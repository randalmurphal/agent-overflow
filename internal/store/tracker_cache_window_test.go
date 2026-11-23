package store

import (
	"reflect"
	"testing"
)

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
		changes, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Rows: rows, Missing: missing, ReadAt: mirrorReadAt(readAt)}))
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
		st.Sprints, st.ReadAt = sprints, mirrorReadAt(readAt)
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
