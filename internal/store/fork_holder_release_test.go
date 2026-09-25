package store

import "testing"

// A pointer fork's own revert or hide of rows it reads from a holder, and
// the holder it stops reading.

// TestForkRevertBelowAHolder: a fork reverts rows it reads from a holder
// as it reverts any inherited row, by lowering its cut there. A holder it
// stops reading is released.
func TestForkRevertBelowAHolder(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 3)
	mustPointerFork(t, s, "S", "F", ForkCut{})
	if _, _, err := s.DeleteConversationFromTurn("S", 1); err != nil {
		t.Fatal(err)
	}
	h := holderOf(t, s, "F")
	source := timelineShape(t, s, "S")
	if _, _, err := s.DeleteConversationFromItem("F", "a2"); err != nil {
		t.Fatal(err)
	}
	requireIDs(t, "F rows", itemIDs(forkRows(t, s, "F")), []string{"u0", "a0", "u1", "a1", "u2"})
	released := 0
	s.OnHoldersReleased(func() { released++ })
	if _, _, err := s.DeleteConversationFromTurn("F", 1); err != nil {
		t.Fatal(err)
	}
	requireIDs(t, "F rows", itemIDs(forkRows(t, s, "F")), []string{"u0", "a0"})
	requireIDs(t, "F lineage", forkLineage(t, s, "F"), []string{"1:S:0:2"})
	requireShape(t, s, "S", source)
	var deleting bool
	if err := s.db.QueryRow(`SELECT deleting FROM threads WHERE id = ?`, h).Scan(&deleting); err != nil || !deleting {
		t.Fatalf("the holder F stopped reading: deleting=%v err=%v", deleting, err)
	}
	if released != 1 {
		t.Fatalf("the revert reported %d releases, want 1", released)
	}
	requireIDs(t, "released", releasedHolders(t, s), []string{h})
}

// TestForkHideReleasesAnEmptyHolder: a fork that deletes the one row it
// reads from a holder hides it, and the holder, which then shows the fork
// nothing, leaves its lineage and is released.
func TestForkHideReleasesAnEmptyHolder(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 3)
	mustPointerFork(t, s, "S", "F", ForkCut{})
	if err := s.DeleteThreadItem("S", "a1"); err != nil {
		t.Fatal(err)
	}
	h := holderOf(t, s, "F")
	requireIDs(t, "holder rows", ownIDs(t, s, h), []string{"a1"})
	released := 0
	s.OnHoldersReleased(func() { released++ })
	if err := s.DeleteThreadItem("F", "a1"); err != nil {
		t.Fatal(err)
	}
	requireIDs(t, "F rows", itemIDs(forkRows(t, s, "F")), []string{"u0", "a0", "u1", "u2", "a2"})
	requireIDs(t, "F lineage", forkLineage(t, s, "F"), []string{"1:S:2:2"})
	if released != 1 {
		t.Fatalf("the hide reported %d releases, want 1", released)
	}
	requireIDs(t, "released", releasedHolders(t, s), []string{h})
}
