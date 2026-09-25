package store

import (
	"database/sql"
	"fmt"
	"reflect"
	"testing"
)

// A pointer fork's write-time aggregates: the subagent cards of the anchors
// it copies and the turn-error pair of the rows it inherits.

// seedForkAgentSource creates src with an agent launch A in turn 1, two
// children in that turn and one in turn 2, each written with A's card.
func seedForkAgentSource(t *testing.T, s *Store, src string) {
	t.Helper()
	seedLinearSource(t, s, src, 1)
	for _, row := range []stampFixtureRow{
		{id: "A", kind: "tool_call", tool: "Agent", summary: "Agent: A", turn: 1},
		{id: "A-c1", kind: "tool_call", tool: "Bash", summary: "Bash: one", parent: "A", turn: 1, index: 1},
		{id: "A-c2", kind: "assistant_text", summary: "two", parent: "A", turn: 1, index: 2},
		{id: "A-c3", kind: "tool_call", tool: "Read", summary: "Read: three", parent: "A", turn: 2},
	} {
		if err := insertCarded(s, row.item(src)); err != nil {
			t.Fatalf("insert %s: %v", row.id, err)
		}
	}
}

// forkAnchorCard is the card thread serves on A, with its descendant count.
func forkAnchorCard(t *testing.T, s *Store, threadID string) (subagentCard, float64) {
	t.Helper()
	card := subagentCardsForTest(t, s, s.reader(), threadID)["A"]
	count, _ := card[metaKeySubagentDescendantCount].(float64)
	return card, count
}

// ownsRow reports whether threadID stores id itself.
func ownsRow(t *testing.T, s *Store, threadID, id string) bool {
	t.Helper()
	var owned bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM items WHERE thread_id = ? AND id = ?)`, threadID, id).Scan(&owned); err != nil {
		t.Fatal(err)
	}
	return owned
}

// TestPointerForkCopiedAnchorKeepsItsCard: a fork reads an inherited anchor
// at read time. When its own write takes a copy, the local copy is served
// from a stamp, so the copy carries one and serves the card it served
// before.
func TestPointerForkCopiedAnchorKeepsItsCard(t *testing.T) {
	s := newTestStore(t)
	seedForkAgentSource(t, s, "S")
	mustPointerFork(t, s, "S", "F", ForkCut{})
	want, count := forkAnchorCard(t, s, "F")
	if count != 3 {
		t.Fatalf("fixture: the fork serves A with %v descendants, want 3 (card %v)", count, want)
	}
	if ownsRow(t, s, "F", "A") {
		t.Fatal("fixture: the fork owns A before the write")
	}

	summary := "Agent: A in the fork"
	if _, err := s.UpdateItemFields("F", "A", ItemPartialUpdate{Summary: &summary}); err != nil {
		t.Fatal(err)
	}

	if !ownsRow(t, s, "F", "A") {
		t.Fatal("the write did not copy A into the fork")
	}
	if _, stamped := subagentStampRowsForTest(t, s, "F")["A"]; !stamped {
		t.Error("the fork's copy of A carries no stamp")
	}
	got, _ := forkAnchorCard(t, s, "F")
	delete(got, metaKeySubagentLatestChildSummary)
	delete(want, metaKeySubagentLatestChildSummary)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the copied anchor serves %v, before the copy %v", got, want)
	}
	assertSubagentStampParity(t, s, "F", "the fork's own rewrite", true)
}

// TestPointerForkSettledCopiesKeepTheirCards: fork creation copies and
// settles the rows still running in its source. A settled child can be its
// agent's newest, so the settle moves the copied anchor's card.
func TestPointerForkSettledCopiesKeepTheirCards(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 1)
	for _, row := range []stampFixtureRow{
		{id: "A", kind: "tool_call", tool: "Agent", summary: "Agent: A", status: "running", turn: 1},
		{id: "A-c1", kind: "tool_call", tool: "Bash", summary: "Bash: done", parent: "A", turn: 1, index: 1},
		{id: "A-c2", kind: "tool_call", tool: "Bash", summary: "Bash: running", status: "running", parent: "A", turn: 1, index: 2},
	} {
		if err := insertCarded(s, row.item("S")); err != nil {
			t.Fatalf("insert %s: %v", row.id, err)
		}
	}
	mustPointerFork(t, s, "S", "F", ForkCut{})
	if !ownsRow(t, s, "F", "A") || !ownsRow(t, s, "F", "A-c2") {
		t.Fatal("fixture: the fork did not copy the running rows")
	}
	card, count := forkAnchorCard(t, s, "F")
	if count != 2 {
		t.Errorf("A serves %v descendants in the fork, want 2", count)
	}
	if got, want := card[metaKeySubagentLatestChildSummary], testInterruptedSummary("Bash: running"); got != want {
		t.Errorf("A's latest child in the fork is %v, want the settled %q", got, want)
	}
	assertSubagentStampParity(t, s, "F", "fork of a running agent", true)
}

// TestPointerForkCardUnderAnInheritedAnchorCopiesIt: the fork's own agent
// rows under an inherited anchor are written with its card. Opening the
// card copies the anchor, so the card keeps the copy's stamp, and the
// source's A is no longer one the fork shows.
func TestPointerForkCardUnderAnInheritedAnchorCopiesIt(t *testing.T) {
	s := newTestStore(t)
	seedForkAgentSource(t, s, "S")
	mustPointerFork(t, s, "S", "F", ForkCut{})
	session := newCardSessionForTest(t, s, "F")

	write := func(id string, index int) {
		t.Helper()
		row := stampFixtureRow{id: id, kind: "tool_call", tool: "Bash", summary: "Bash: " + id, parent: "A", turn: 3, index: index}.item("F")
		row.SubagentCard = session.card("A")
		if err := s.InsertItem(row); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	write("F-c1", 0)
	if !ownsRow(t, s, "F", "A") {
		t.Fatal("opening the card under an inherited anchor did not copy it")
	}
	session.flush()
	assertSubagentStampParity(t, s, "F", "first fork child", true)

	// A source rewrite of A finds no fork reading it; the open card keeps
	// counting what the fork writes.
	summary := "Agent: A renamed"
	if _, err := s.UpdateItemFields("S", "A", ItemPartialUpdate{Summary: &summary}); err != nil {
		t.Fatal(err)
	}
	write("F-c2", 1)
	session.flush()
	if _, count := forkAnchorCard(t, s, "F"); count != 5 {
		t.Errorf("A serves %v descendants in the fork, want 5", count)
	}
	assertSubagentStampParity(t, s, "F", "second fork child", true)
	session.closeAll()
}

// TestPointerForkViewChangesRecomputeCopiedStamps: a fork's stamped copy of
// an anchor counts children it reads from its source. A write that stops the
// fork reading some of them (its delete of an inherited child, a revert of
// inherited rows) recomputes the copy's stamp. The source's deletion keeps
// the rows the fork reads, so the stamp stays exact.
func TestPointerForkViewChangesRecomputeCopiedStamps(t *testing.T) {
	for _, tc := range []struct {
		name  string
		want  float64
		write func(t *testing.T, s *Store)
	}{
		{"delete of an inherited child", 2, func(t *testing.T, s *Store) {
			if err := s.DeleteThreadItem("F", "A-c2"); err != nil {
				t.Fatal(err)
			}
		}},
		{"revert of inherited rows", 2, func(t *testing.T, s *Store) {
			if _, _, err := s.DeleteConversationFromTurn("F", 2); err != nil {
				t.Fatal(err)
			}
		}},
		{"source deletion", 3, func(t *testing.T, s *Store) {
			if err := s.DeleteThread("S"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			seedForkAgentSource(t, s, "S")
			mustPointerFork(t, s, "S", "F", ForkCut{})
			summary := "Agent: A in the fork"
			if _, err := s.UpdateItemFields("F", "A", ItemPartialUpdate{Summary: &summary}); err != nil {
				t.Fatal(err)
			}
			if _, stamped := subagentStampRowsForTest(t, s, "F")["A"]; !stamped {
				t.Fatal("fixture: the fork's copy of A carries no stamp")
			}

			tc.write(t, s)

			if _, count := forkAnchorCard(t, s, "F"); count != tc.want {
				t.Errorf("A serves %v descendants in the fork, want %v", count, tc.want)
			}
			assertSubagentStampParity(t, s, "F", tc.name, true)
		})
	}
}

// seedFailedForkSource creates src whose newest turn, turn 1, failed: an
// error row after its prompt and a later user row, both in turn 1.
func seedFailedForkSource(t *testing.T, s *Store, src string) {
	t.Helper()
	seedForkSource(t, s, src, []Item{
		{ID: "u0", TurnIndex: 0, ItemIndex: 0, Kind: "user_text", Role: "user", Status: "completed", Summary: "ask", Meta: "{}"},
		{ID: "a0", TurnIndex: 0, ItemIndex: 1, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "reply", Meta: "{}"},
		{ID: "u1", TurnIndex: 1, ItemIndex: 0, Kind: "user_text", Role: "user", Status: "completed", Summary: "ask again", Meta: "{}"},
		{ID: "err", TurnIndex: 1, ItemIndex: 1, Kind: "error", Role: "assistant", Status: "completed", Summary: "failed", Meta: "{}"},
		{ID: "u1b", TurnIndex: 1, ItemIndex: 2, Kind: "user_text", Role: "user", Status: "completed", Summary: "queued", Meta: "{}"},
	})
	for turn := range 2 {
		completed := int64(1)
		if err := s.InsertTurn(Turn{TurnID: fmt.Sprintf("%s:%d", src, turn), ThreadID: src, TurnIndex: turn, StartedAt: 1, CompletedAt: &completed}); err != nil {
			t.Fatal(err)
		}
	}
}

// forkTurnErrorAt reads a thread's newest_turn_error_at.
func forkTurnErrorAt(t *testing.T, s *Store, threadID string) sql.NullInt64 {
	t.Helper()
	var at sql.NullInt64
	if err := s.db.QueryRow(`SELECT newest_turn_error_at FROM threads WHERE id = ?`, threadID).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// failedPillAfterUnread marks the thread unread and reports its Failed pill.
func failedPillAfterUnread(t *testing.T, s *Store, threadID string) bool {
	t.Helper()
	thread, _, err := s.MarkThreadUnread(threadID)
	if err != nil {
		t.Fatal(err)
	}
	return thread.HasFailedTurn
}

// TestPointerForkTurnErrorsReadTheLineage: a fork of a failed turn reads the
// error row from its source, so its Failed pill lights like the source's
// once it is unread. Each write that changes which rows the fork reads from
// its source recomputes the pair: its delete of the error, a revert of it.
// The source's deletion keeps what the fork reads. A new turn empties the
// set.
func TestPointerForkTurnErrorsReadTheLineage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, s *Store)
		want  bool
	}{
		{"fork", func(*testing.T, *Store) {}, true},
		{"delete of the inherited error", func(t *testing.T, s *Store) {
			if err := s.DeleteThreadItem("F", "err"); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"revert of the inherited error", func(t *testing.T, s *Store) {
			if _, _, err := s.DeleteConversationFromItem("F", "err"); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"source deletion", func(t *testing.T, s *Store) {
			if err := s.DeleteThread("S"); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"new turn", func(t *testing.T, s *Store) {
			if err := s.InsertTurn(Turn{TurnID: "F:2", ThreadID: "F", TurnIndex: 2, StartedAt: 2}); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			seedFailedForkSource(t, s, "S")
			if !failedPillAfterUnread(t, s, "S") {
				t.Fatal("fixture: the source does not show Failed")
			}
			mustPointerFork(t, s, "S", "F", throughTurn(1))
			if got := forkTurnErrorAt(t, s, "F"); !got.Valid || got != forkTurnErrorAt(t, s, "S") {
				t.Fatalf("the fork's turn-error pair is %v, the source's %v", got, forkTurnErrorAt(t, s, "S"))
			}

			tc.write(t, s)

			if got := failedPillAfterUnread(t, s, "F"); got != tc.want {
				t.Errorf("the fork shows Failed = %v, want %v (pair %v)", got, tc.want, forkTurnErrorAt(t, s, "F"))
			}
		})
	}
}

// TestPointerForkTurnErrorTriggersKeepInheritedErrors: a trigger that
// recomputes a fork's pair after a write to the fork's own rows or turns
// counts the rows the fork reads through its lineage too, so the inherited
// error keeps the fork Failed after its own error goes and after a revert
// of its newest turn.
func TestPointerForkTurnErrorTriggersKeepInheritedErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, s *Store)
	}{
		{"delete of its own error", func(t *testing.T, s *Store) {
			own := Item{ID: "F-err", ThreadID: "F", TurnIndex: 1, ItemIndex: 10, Kind: "error", Role: "assistant",
				Status: "completed", Summary: "fork failed", Meta: "{}", CreatedAt: 5, UpdatedAt: 5}
			if err := s.InsertItem(own); err != nil {
				t.Fatal(err)
			}
			if got := forkTurnErrorAt(t, s, "F"); got.Int64 != 5 {
				t.Fatalf("fixture: the fork's own error left the pair at %v", got)
			}
			if err := s.DeleteThreadItem("F", "F-err"); err != nil {
				t.Fatal(err)
			}
		}},
		{"revert of its newest turn", func(t *testing.T, s *Store) {
			if err := s.InsertTurn(Turn{TurnID: "F:2", ThreadID: "F", TurnIndex: 2, StartedAt: 2}); err != nil {
				t.Fatal(err)
			}
			if got := forkTurnErrorAt(t, s, "F"); got.Valid {
				t.Fatalf("fixture: a new turn left the pair at %v", got)
			}
			if _, _, err := s.DeleteConversationFromTurn("F", 2); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			seedFailedForkSource(t, s, "S")
			mustPointerFork(t, s, "S", "F", throughTurn(1))

			tc.write(t, s)

			if got, want := forkTurnErrorAt(t, s, "F"), forkTurnErrorAt(t, s, "S"); !got.Valid || got != want {
				t.Errorf("the fork's turn-error pair is %v, want the inherited error's %v", got, want)
			}
			if !failedPillAfterUnread(t, s, "F") {
				t.Error("the fork does not show Failed for the error it inherits")
			}
		})
	}
}
