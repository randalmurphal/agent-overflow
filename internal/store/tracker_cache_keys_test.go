package store

import (
	"testing"
)

// A board's lifetime starts and ends with foreign key work keyed on the
// board: the probe of a new board row and the cascade of a deleted one read
// the same pages whether another board of the root holds 1,000 rows or
// 6,000 (both deep enough that the indexes have the same height).
func TestMirrorBoardKeysIgnoreOtherBoardsRows(t *testing.T) {
	t.Parallel()
	pages := map[int][2]int{}
	for _, n := range []int{1000, 6000} {
		s, _, root := newMirror(t)
		writeMirrorBoard(t, s, root, "10", mirrorRows(10000, n, "a"))
		start := pageAccessesForTest(t, s, `INSERT INTO tracker_cache_boards(root_id, board_id) SELECT ?1, ?2`, root, "19")
		end := pageAccessesForTest(t, s, `DELETE FROM tracker_cache_boards WHERE root_id = ?1 AND board_id = ?2`, root, "19")
		pages[n] = [2]int{start, end}
	}
	if pages[6000] != pages[1000] {
		t.Fatalf("pages read by a board's start and end with 1,000 rows on another board %v, with 6,000 %v; want the same", pages[1000], pages[6000])
	}
}
