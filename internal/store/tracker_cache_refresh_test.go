package store

import (
	"errors"
	"reflect"
	"strconv"
	"testing"
)

// The refresh targets are what the mirror holds of the issues and boards
// asked, in one statement: the rows on shown boards with their lifetimes,
// the kept issues, the stored details and the shown boards, with the
// root's sweep. A board no project lists, or one whose structure is not
// stored, holds no target; a detached root fails as gone; a list over the
// lookup bound is refused. Not parallel: it counts statements.
func TestReadMirrorRefreshTargets(t *testing.T) {
	s, c, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a"), webRow("10002", 5, "b")})
	writeMirrorBoard(t, s, root, "11", []MirrorRow{webRow("10001", 5, "a")})
	linkIssue(t, s, "thread-1", "10003")
	if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{webCard("10003", 5)}, MirrorCatalog{}, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StoreMirrorDetails(root, currentSweep(s, root), []MirrorDetailDoc{{"10001", []byte(`{}`)}}, 10, 10); err != nil {
		t.Fatal(err)
	}
	read := func() MirrorRefreshTargets {
		t.Helper()
		var got MirrorRefreshTargets
		statements := countMirrorStatements(t, func() {
			var err error
			got, err = s.ReadMirrorRefreshTargets(root, []string{"10004", "10001", "10002", "10003", "10001"}, []string{"99", "12", "10"})
			if err != nil {
				t.Fatal(err)
			}
		})
		if statements != 1 {
			t.Fatalf("the read ran %d statements", statements)
		}
		return got
	}
	l10, l11 := currentLifetime(s, root, "10"), currentLifetime(s, root, "11")
	want := MirrorRefreshTargets{
		Sweep:   currentSweep(s, root),
		Rows:    []MirrorRowTarget{{"10001", "10", l10}, {"10001", "11", l11}, {"10002", "10", l10}},
		Kept:    []string{"10001", "10002", "10003"},
		Details: []string{"10001"},
		Boards:  []MirrorBoardTarget{{"10", l10}},
	}
	if got := read(); !reflect.DeepEqual(got, want) {
		t.Fatalf("targets %+v\nwant %+v", got, want)
	}

	// Board 11 unlisted: its rows are no target, and 10001 stays kept
	// through board 10.
	listMirrorBoards(t, s, defaultTestProjectID, c.ID, "10", "12")
	want.Rows = want.Rows[:1:1]
	want.Rows = append(want.Rows, MirrorRowTarget{"10002", "10", l10})
	if got := read(); !reflect.DeepEqual(got, want) {
		t.Fatalf("with board 11 unlisted %+v\nwant %+v", got, want)
	}

	over := make([]string, mirrorMaxLookup+1)
	for i := range over {
		over[i] = strconv.Itoa(i)
	}
	if _, err := s.ReadMirrorRefreshTargets(root, over, nil); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Fatalf("a read of %d issues: %v", len(over), err)
	}
	if _, err := s.DetachMirror(c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadMirrorRefreshTargets(root, []string{"10001"}, nil); !errors.Is(err, ErrTrackerCacheGone) {
		t.Fatalf("a read of a detached root: %v", err)
	}
}
