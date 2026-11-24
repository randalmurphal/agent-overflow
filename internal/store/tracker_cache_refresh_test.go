package store

import (
	"errors"
	"reflect"
	"strconv"
	"testing"
)

// The refresh targets are what the mirror holds of the cards, details and
// boards asked, in one statement: the cards' rows on shown boards with
// their lifetimes and the cards kept, the details stored and the boards
// shown, with the root's sweep. A detail asked alone reads no row or kept
// issue, and a card asked alone no detail. A board no project lists, or
// one whose structure is not stored, holds no target; a detached root
// fails as gone; a list over the lookup bound is refused. Not parallel:
// it counts statements.
func TestReadMirrorRefreshTargets(t *testing.T) {
	s, c, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a"), webRow("10002", 5, "b")})
	writeMirrorBoard(t, s, root, "11", []MirrorRow{webRow("10001", 5, "a")})
	linkIssue(t, s, "thread-1", "10003")
	if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{webCard("10003", 5)}, MirrorCatalog{}, mirrorReadAt(5)); err != nil {
		t.Fatal(err)
	}
	if _, err := storeTicket(t, s, root, currentSweep(s, root), "10001", 10, 10, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	read := func(cards, details []string) MirrorRefreshTargets {
		t.Helper()
		var got MirrorRefreshTargets
		statements := countMirrorStatements(t, func() {
			var err error
			got, err = s.ReadMirrorRefreshTargets(root, cards, details, []string{"99", "12", "10"})
			if err != nil {
				t.Fatal(err)
			}
		})
		if statements != 1 {
			t.Fatalf("the read ran %d statements", statements)
		}
		return got
	}
	asked := []string{"10004", "10001", "10002", "10003", "10001"}
	l10, l11 := currentLifetime(s, root, "10"), currentLifetime(s, root, "11")
	want := MirrorRefreshTargets{
		Sweep:   currentSweep(s, root),
		Rows:    []MirrorRowTarget{{"10001", "10", l10}, {"10001", "11", l11}, {"10002", "10", l10}},
		Kept:    []string{"10001", "10002", "10003"},
		Details: []string{"10001"},
		Boards:  []MirrorBoardTarget{{"10", l10}},
	}
	if got := read(asked, asked); !reflect.DeepEqual(got, want) {
		t.Fatalf("targets %+v\nwant %+v", got, want)
	}
	if got, alone := read(nil, asked), (MirrorRefreshTargets{Sweep: want.Sweep, Details: want.Details, Boards: want.Boards}); !reflect.DeepEqual(got, alone) {
		t.Fatalf("details alone %+v\nwant %+v", got, alone)
	}
	if got, alone := read(asked, nil), (MirrorRefreshTargets{Sweep: want.Sweep, Rows: want.Rows, Kept: want.Kept, Boards: want.Boards}); !reflect.DeepEqual(got, alone) {
		t.Fatalf("cards alone %+v\nwant %+v", got, alone)
	}

	// Board 11 unlisted: its rows are no target, and 10001 stays kept
	// through board 10.
	listMirrorBoards(t, s, defaultTestProjectID, c.ID, "10", "12")
	want.Rows = want.Rows[:1:1]
	want.Rows = append(want.Rows, MirrorRowTarget{"10002", "10", l10})
	if got := read(asked, asked); !reflect.DeepEqual(got, want) {
		t.Fatalf("with board 11 unlisted %+v\nwant %+v", got, want)
	}

	over := make([]string, mirrorMaxLookup+1)
	for i := range over {
		over[i] = strconv.Itoa(i)
	}
	if _, err := s.ReadMirrorRefreshTargets(root, nil, over, nil); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Fatalf("a read of %d issues: %v", len(over), err)
	}
	if _, err := s.DetachMirror(c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadMirrorRefreshTargets(root, []string{"10001"}, nil, nil); !errors.Is(err, ErrTrackerCacheGone) {
		t.Fatalf("a read of a detached root: %v", err)
	}
}
