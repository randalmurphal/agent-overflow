package store

import (
	"testing"
)

// Every card writer, a board's rows and cards read by ID (an open
// ticket's check, the linked issues' read, a card a document is owed),
// marks the references its cards name that the mirror lacks on each board
// the issue has a row on, so a card that moves an issue to a sprint or a
// status no structure read has stored makes the board due a structure
// read (MirrorSync.StructureDueAt) whichever read brought it. Until that
// read the board counts the issue where its known references put it (the
// backlog); after it, where the new sprint or Done status does, and the
// board is no longer due.
func TestEveryCardWriterMarksWhatItsCardsLack(t *testing.T) {
	t.Parallel()
	writers := map[string]func(s *Store, root int64, card MirrorCard) error{
		"board rows": func(s *Store, root int64, card MirrorCard) error {
			_, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{{Card: card, OrderKey: "a"}}, ReadAt: mirrorReadAt(3)}))
			return err
		},
		"cards by ID": func(s *Store, root int64, card MirrorCard) error {
			_, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{card}, MirrorCatalog{}, mirrorReadAt(3))
			return err
		},
	}
	moves := map[string]struct {
		move      func(c *MirrorCard)
		structure func(st *MirrorStructure)
		placed    func(c MirrorSectionCounts) bool
	}{
		"a new active sprint": {
			move: func(c *MirrorCard) { c.Sprints = []string{"23"} },
			structure: func(st *MirrorStructure) {
				st.Sprints = append(st.Sprints, MirrorSprint{ID: "23", OriginBoardID: "10", Name: "WEB Sprint 4", State: MirrorSprintActive})
			},
			placed: func(c MirrorSectionCounts) bool { return c.Sprints["23"].Rows == 1 },
		},
		"a new Done status": {
			move: func(c *MirrorCard) { c.StatusID = "10002" },
			structure: func(st *MirrorStructure) {
				st.Columns[2].Statuses = append(st.Columns[2].Statuses, "10002")
				st.Catalog.Statuses = append(st.Catalog.Statuses, MirrorStatus{ID: "10002", Name: "Shipped", Group: MirrorGroupDone})
			},
			placed: func(c MirrorSectionCounts) bool { return c.Columns[2].Rows == 1 },
		},
	}
	for writer, write := range writers {
		for name, m := range moves {
			t.Run(writer+", "+name, func(t *testing.T) {
				t.Parallel()
				s, _, root := newMirror(t)
				mustReplaceStructure(t, s, root, webStructure(), true)
				mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 5, "a")), nil)
				counts := func() MirrorSectionCounts {
					t.Helper()
					c, err := s.CountMirrorSections(root, "10", MirrorRegroup{})
					if err != nil {
						t.Fatal(err)
					}
					return c
				}
				if c := counts(); c.Backlog.Rows != 1 || structureDue(t, s, root) != 0 {
					t.Fatalf("before the move: backlog %d, due %d", c.Backlog.Rows, structureDue(t, s, root))
				}
				card := webCard("10001", 6)
				m.move(&card)
				if err := write(s, root, card); err != nil {
					t.Fatal(err)
				}
				if c := counts(); c.Backlog.Rows != 1 || m.placed(c) {
					t.Fatalf("before the structure read: %+v; want the issue in the backlog", c)
				}
				if due := structureDue(t, s, root); due != 3 {
					t.Fatalf("after the move the board is due %d; want 3, the card's read", due)
				}
				st := webStructure()
				st.ReadAt = mirrorReadAt(4)
				m.structure(&st)
				mustReplaceStructure(t, s, root, st, true)
				if c := counts(); c.Backlog.Rows != 0 || !m.placed(c) {
					t.Fatalf("after the structure read: %+v; want the issue placed", c)
				}
				if due := structureDue(t, s, root); due != 0 {
					t.Fatalf("after the structure read the board is due %d", due)
				}
			})
		}
	}
}

// A card of an issue on several boards marks a sprint it names on each
// board that lacks it, and a reference the mirror holds on none.
func TestACardMarksEachBoardItsIssueIsOn(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	other := webStructure()
	other.Board.ID = "11"
	other.Sprints = append(other.Sprints, MirrorSprint{ID: "23", OriginBoardID: "10", Name: "WEB Sprint 4", State: MirrorSprintActive})
	mustReplaceStructure(t, s, root, other, true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 5, "a")), nil)
	mustWriteRowPage(t, s, root, webPage("11", webRow("10001", 5, "a")), nil)
	card := webCard("10001", 6, "23")
	if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{card}, MirrorCatalog{}, mirrorReadAt(3)); err != nil {
		t.Fatal(err)
	}
	assertRows(t, s.db, `SELECT group_concat(board_id || ':' || kind || ':' || ref_id || '@' || named_at, ',') FROM
		(SELECT * FROM tracker_cache_board_unexplained ORDER BY board_id, kind, ref_id)`, "10:3:23@3")
}
