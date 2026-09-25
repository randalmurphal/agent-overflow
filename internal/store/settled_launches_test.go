package store

import (
	"testing"
)

// settledLaunchCuts are the two conversation cuts that can delete a
// launch's ending sibling and keep the launch: Claude's item cut and
// Codex's turn cut, each at the second prompt u1.
var settledLaunchCuts = []struct {
	name string
	cut  func(*Store, string) error
}{
	{"item cut", func(s *Store, thread string) error {
		_, _, err := s.DeleteConversationFromItem(thread, "u1")
		return err
	}},
	{"turn cut", func(s *Store, thread string) error {
		_, _, err := s.DeleteConversationFromTurn(thread, 1)
		return err
	}},
}

// seedLaunchCompletedAfterACut writes a thread whose turn 0 launches a
// background command and whose turn 1, after the prompt u1, holds the
// command's ending sibling.
func seedLaunchCompletedAfterACut(t *testing.T, s *Store, thread string) {
	t.Helper()
	seedForkSource(t, s, thread, []Item{
		{ID: "u0", TurnIndex: 0, ItemIndex: 0, Kind: "user_text", Role: "user", Status: "completed", Summary: "go", Meta: "{}"},
		{ID: "launch", TurnIndex: 0, ItemIndex: 1, Kind: "tool_call", Role: "assistant", Status: "running", IsBackground: true, ToolName: "Bash", Summary: "Bash", Meta: `{"task_id":"task-1"}`},
		{ID: "u1", TurnIndex: 1, ItemIndex: 0, Kind: "user_text", Role: "user", Status: "completed", Summary: "more", Meta: "{}"},
		{ID: "done", TurnIndex: 1, ItemIndex: 1, Kind: "tool_completion", Role: "assistant", Status: "completed", IsBackground: true, CompletionOf: "launch", ToolName: "Bash", Summary: "done", Meta: "{}"},
	})
}

// assertLaunchSettledWithoutSibling fails unless thread keeps launch as
// settled history: its flag is false, no row of the thread names it
// through completion_of, and no reader of live work returns it, the
// session-end settle's included.
func assertLaunchSettledWithoutSibling(t *testing.T, s *Store, thread, launch string) {
	t.Helper()
	assertSettled(t, s, thread, launch)
	siblings, err := queryIDs(s.db, `SELECT id FROM items WHERE thread_id = ? AND completion_of = ? AND completion_of <> ''`, thread, launch)
	if err != nil {
		t.Fatal(err)
	}
	if len(siblings) != 0 {
		t.Fatalf("%s/%s has siblings %v, want none", thread, launch, siblings)
	}
	if ids := liveTaskIDs(t, s, thread); len(ids) != 0 {
		t.Fatalf("tray rows of %s = %v, want none", thread, ids)
	}
	if rows, err := s.ListBackgroundTrayRows(thread, 0, []string{launch}); err != nil || len(rows) != 0 {
		t.Fatalf("tray rows of %s/%s = %v, %v; want none", thread, launch, rows, err)
	}
	if n, err := s.CountLiveRunningBackgroundToolCalls(thread); err != nil || n != 0 {
		t.Fatalf("CountLiveRunningBackgroundToolCalls(%s) = %d, %v; want 0", thread, n, err)
	}
	if live, err := s.HasLiveBackgroundToolCall(thread); err != nil || live {
		t.Fatalf("HasLiveBackgroundToolCall(%s) = %v, %v; want false", thread, live, err)
	}
	if recoverable, err := s.ListRecoverableClaudeBackgroundLaunchesForThread(thread); err != nil || len(recoverable) != 0 {
		t.Fatalf("launches the session-end settle would end in %s = %v, %v; want none", thread, recoverable, err)
	}
}

// TestConversationCutLeavesALaunchWhoseSiblingItDeletedSettled: a cut that
// deletes a launch's ending sibling and keeps the launch leaves it settled
// history with no result. No reader of live work returns it, so nothing
// writes it a sibling.
func TestConversationCutLeavesALaunchWhoseSiblingItDeletedSettled(t *testing.T) {
	for _, tc := range settledLaunchCuts {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			seedLaunchCompletedAfterACut(t, s, "t")
			assertSettled(t, s, "t", "launch")

			if err := tc.cut(s, "t"); err != nil {
				t.Fatalf("cut: %v", err)
			}
			requireIDs(t, "kept rows", ownIDs(t, s, "t"), []string{"u0", "launch"})
			assertLaunchSettledWithoutSibling(t, s, "t", "launch")
		})
	}
}

// TestSourceRevertKeepsItsLaunchSettledWhenAHolderTakesTheCompletion: a
// source cut that keeps a background launch whose completion its forks
// show moves the completion to a holder, with a copy of the launch. The
// source keeps the launch settled with no sibling, and the fork reads the
// launch and its completion as before.
func TestSourceRevertKeepsItsLaunchSettledWhenAHolderTakesTheCompletion(t *testing.T) {
	for _, tc := range settledLaunchCuts {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			seedLaunchCompletedAfterACut(t, s, "S")
			assertSettled(t, s, "S", "launch")
			mustPointerFork(t, s, "S", "F", ForkCut{})
			fork := timelineShape(t, s, "F")

			if err := tc.cut(s, "S"); err != nil {
				t.Fatalf("cut: %v", err)
			}
			requireShape(t, s, "F", fork)
			h := holderOf(t, s, "F")
			requireIDs(t, "holder rows", ownIDs(t, s, h), []string{"launch", "u1", "done"})
			assertSettled(t, s, h, "launch")
			requireIDs(t, "source rows", ownIDs(t, s, "S"), []string{"u0", "launch"})
			assertLaunchSettledWithoutSibling(t, s, "S", "launch")
		})
	}
}

// TestMidHistoryForkShowsALaunchSettledWithoutASibling: a fork cut before
// the source's tail shows a launch a cut left settled with no ending
// sibling, as the source does: not hidden, read in place, no holder. A
// launch whose ending sibling lies beyond the fork's cut is still hidden,
// with the sibling (forkHiddenClosureTx).
func TestMidHistoryForkShowsALaunchSettledWithoutASibling(t *testing.T) {
	for _, tc := range settledLaunchCuts {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			seedLaunchCompletedAfterACut(t, s, "S")
			if err := tc.cut(s, "S"); err != nil {
				t.Fatalf("cut: %v", err)
			}
			for _, it := range []Item{
				{ID: "u2", TurnIndex: 1, ItemIndex: 0, Kind: "user_text", Role: "user", Status: "completed", Summary: "again", Meta: "{}"},
				{ID: "later", TurnIndex: 1, ItemIndex: 1, Kind: "tool_call", Role: "assistant", Status: "running", IsBackground: true, ToolName: "Bash", Summary: "Bash", Meta: `{"task_id":"task-2"}`},
				{ID: "u3", TurnIndex: 2, ItemIndex: 0, Kind: "user_text", Role: "user", Status: "completed", Summary: "last", Meta: "{}"},
				{ID: "later-done", TurnIndex: 2, ItemIndex: 1, Kind: "tool_completion", Role: "assistant", Status: "completed", IsBackground: true, CompletionOf: "later", ToolName: "Bash", Summary: "done", Meta: "{}"},
			} {
				it.ThreadID = "S"
				it.CreatedAt, it.UpdatedAt = 1, 1
				if err := insertCarded(s, it); err != nil {
					t.Fatalf("insert %s: %v", it.ID, err)
				}
			}
			assertSettled(t, s, "S", "launch")
			assertSettled(t, s, "S", "later")

			mustPointerFork(t, s, "S", "F", throughTurn(1))

			var shown []string
			for _, it := range forkRows(t, s, "F") {
				shown = append(shown, it.ID)
			}
			requireIDs(t, "fork rows", shown, []string{"u0", "launch", "u2"})
			assertSettled(t, s, "F", "launch")
			hidden, err := queryIDs(s.db, `SELECT item_id FROM thread_fork_hidden WHERE thread_id = ? ORDER BY item_id`, "F")
			if err != nil {
				t.Fatal(err)
			}
			requireIDs(t, "fork hides", hidden, []string{"later", "later-done"})
			requireIDs(t, "fork's own rows", ownIDs(t, s, "F"), nil)
			requireIDs(t, "holders", holderIDs(t, s), nil)
			if ids := liveTaskIDs(t, s, "F"); len(ids) != 0 {
				t.Fatalf("tray rows of F = %v, want none", ids)
			}
		})
	}
}
