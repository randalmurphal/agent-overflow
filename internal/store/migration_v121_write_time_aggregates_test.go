package store

import (
	"context"
	"database/sql"
	"testing"
)

// forkOfRunningAgentAtV120 opens a store upgraded to v121 from a v120
// database holding a pointer fork F of a running agent: F owns a settled
// copy of the agent's launch A and reads A's two children, and a failed
// turn's error, from its source S. v121's deferred phase has not run.
func forkOfRunningAgentAtV120(t *testing.T) *Store {
	t.Helper()
	s := openStoreAt(t)
	seedForkSource(t, s, "S", []Item{
		{ID: "u0", TurnIndex: 0, ItemIndex: 0, Kind: "user_text", Role: "user", Status: "completed", Summary: "ask", Meta: "{}"},
		{ID: "A", TurnIndex: 0, ItemIndex: 1, Kind: "tool_call", ToolName: "Agent", Role: "assistant", Status: "running", Summary: "Agent: A", Meta: "{}"},
		{ID: "A-c1", TurnIndex: 0, ItemIndex: 2, Kind: "tool_call", ToolName: "Bash", Role: "assistant", Status: "completed", Summary: "Bash: one", ParentID: "A", Meta: "{}"},
		{ID: "A-c2", TurnIndex: 0, ItemIndex: 3, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "two", ParentID: "A", Meta: "{}"},
		{ID: "err", TurnIndex: 0, ItemIndex: 4, Kind: "error", Role: "assistant", Status: "completed", Summary: "failed", Meta: "{}"},
	})
	if err := s.InsertTurn(Turn{TurnID: "S:0", ThreadID: "S", TurnIndex: 0, StartedAt: 1}); err != nil {
		t.Fatal(err)
	}
	mustPointerFork(t, s, "S", "F", ForkCut{})
	if !ownsRow(t, s, "F", "A") || ownsRow(t, s, "F", "A-c1") || ownsRow(t, s, "F", "A-c2") || ownsRow(t, s, "F", "err") {
		t.Fatal("fixture: the fork should own its settled copy of A and inherit A's children and the error")
	}
	if _, count := forkAnchorCard(t, s, "F"); count != 2 {
		t.Fatalf("fixture: the fork serves A with %v descendants, want 2", count)
	}

	path := s.path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	downgradeSchema(t, db, migrateThrough(t, 120), 120)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close upgraded store: %v", err)
		}
	})
	if _, mode := subagentStampStateForTest(t, s, "F", "A"); mode != subagentUnstamped {
		t.Fatalf("fixture: the fork's copy of A is stamped (mode %d) before the deferred phase", mode)
	}
	return s
}

// TestMigrationV121CountsAForkLineage: v121's backfill gives the fork the
// inherited error's turn-error pair, and its deferred phase stamps the
// fork's copy of A with the children it inherits.
func TestMigrationV121CountsAForkLineage(t *testing.T) {
	s := forkOfRunningAgentAtV120(t)
	if got, want := forkTurnErrorAt(t, s, "F"), forkTurnErrorAt(t, s, "S"); !got.Valid || got != want {
		t.Errorf("v121 gave the fork the turn-error pair %v, want the inherited error's %v", got, want)
	}

	if err := s.RunDeferredMigrations(context.Background(), DeferredHost{}); err != nil {
		t.Fatal(err)
	}
	if deferredPending(t, s) || deferredFailureOf(t, s) != nil {
		t.Fatalf("the phase left watermark %d, failure %+v", deferredWatermarkOf(t, s), deferredFailureOf(t, s))
	}
	if _, mode := subagentStampStateForTest(t, s, "F", "A"); mode != subagentStampClean {
		t.Errorf("the fork's copy of A ends mode %d, want clean", mode)
	}
	if _, count := forkAnchorCard(t, s, "F"); count != 2 {
		t.Errorf("the fork serves A with %v descendants, want the 2 it inherits", count)
	}
	for _, thread := range []string{"S", "F"} {
		assertSubagentStampParity(t, s, thread, thread+" stamped", true)
	}
}

// TestMigrationV121CardOnAnUnstampedForkCopyCountsItsLineage: a card
// opened on the fork's unstamped copy before the deferred phase reaches it
// seeds from a recompute, because the copy has children the fork inherits,
// so its first flush counts them with the child it writes.
func TestMigrationV121CardOnAnUnstampedForkCopyCountsItsLineage(t *testing.T) {
	s := forkOfRunningAgentAtV120(t)
	session := newCardSessionForTest(t, s, "F")
	row := stampFixtureRow{id: "F-c3", kind: "tool_call", tool: "Bash", summary: "Bash: three", parent: "A", turn: 1}.item("F")
	row.SubagentCard = session.card("A")
	if err := s.InsertItem(row); err != nil {
		t.Fatal(err)
	}
	session.flush()
	if _, count := forkAnchorCard(t, s, "F"); count != 3 {
		t.Errorf("the fork serves A with %v descendants, want the 2 it inherits and its own", count)
	}
	assertSubagentStampParity(t, s, "F", "card on the unstamped copy", false)
	session.closeAll()
}
