package store

import (
	"context"
	"reflect"
	"testing"
)

// TestPointerForkStampsAndHeldWindows: a fork's stamps are its own. A
// source write after its cut, a revision touch of a row it shows and a
// source delete of such a row (which gives the row to a holder) leave it
// fresh, whether the window it holds is its own rows or holds inherited
// ones, which carry no row stamp.
func TestPointerForkStampsAndHeldWindows(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 2)
	mustPointerFork(t, s, "S", "F", ForkCut{})
	for _, id := range []string{"f1", "f2"} {
		if _, err := appendCarded(s, Item{ID: id, ThreadID: "F", TurnIndex: 2, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: id}); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	page, err := s.ListThreadSliceAround(ctx, "F", "", 200, testRunWindowRows, TimelineSelection{})
	if err != nil {
		t.Fatal(err)
	}
	own := page.Items[len(page.Items)-2:]
	requireIDs(t, "F tail", itemIDs(own), []string{"f1", "f2"})
	for _, it := range page.Items[:len(page.Items)-2] {
		if it.Rev != -1 {
			t.Fatalf("inherited row %s rev=%d", it.ID, it.Rev)
		}
	}
	tail := heldWindowOverItems(own, true, false)
	whole := heldWindowOverItems(page.Items, false, false)
	stamp := historyStampOf(t, s, "F")
	requireFresh := func(what string) {
		t.Helper()
		if now := historyStampOf(t, s, "F"); now != stamp {
			t.Fatalf("%s moved the fork's stamp %+v -> %+v", what, stamp, now)
		}
		for name, held := range map[string]*HeldWindow{"no window": nil, "own rows": &tail, "whole window": &whole} {
			got, err := s.SyncThreadWindow(ctx, "F", "", 200, testRunWindowRows, stamp, held, TimelineSelection{})
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != SyncFresh {
				t.Fatalf("%s: the fork holding %s = %s, want fresh", what, name, got.Status)
			}
		}
	}
	requireFresh("nothing")

	if _, err := appendCarded(s, Item{ID: "late", ThreadID: "S", TurnIndex: 2, Kind: "user_text", Role: "user", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	requireFresh("a source write past the cut")

	touchItemForTest(t, s, "S", "a1")
	requireFresh("a revision touch of a row the fork shows")

	before, sourceStamp := timelineShape(t, s, "F"), historyStampOf(t, s, "S")
	if err := s.DeleteThreadItem("S", "a0"); err != nil {
		t.Fatal(err)
	}
	if historyStampOf(t, s, "S") == sourceStamp {
		t.Fatal("the source's delete left its own stamp")
	}
	requireShape(t, s, "F", before)
	requireFresh("a source delete of a row the fork shows")
	held, found, err := s.GetThreadItem("F", "a0")
	if err != nil || !found || held.Rev != -1 {
		t.Fatalf("the row the holder took reads rev=%d found=%v err=%v", held.Rev, found, err)
	}
}

// touchItemForTest is a revision touch of one row, the write a plan badge or
// a plan comment makes (bumpHistoryRevForItemTx).
func touchItemForTest(t *testing.T, s *Store, threadID, itemID string) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := bumpHistoryRevForItemTx(tx, threadID, itemID, "test touch"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// forkWindow is a thread's decorated window read.
func forkWindow(t *testing.T, s *Store, threadID string) []Item {
	t.Helper()
	page, err := s.ListThreadSliceAround(context.Background(), threadID, "", 200, testRunWindowRows, TimelineSelection{})
	if err != nil {
		t.Fatalf("window of %s: %v", threadID, err)
	}
	return page.Items
}

// TestForkStampsIgnoreEverySourceWrite: no source write moves a fork's
// stamps or changes its window. A write after the fork's cut does not
// reach it, including one whose row stamping reaches a settled row below
// the cut; a revision touch and spans on a payload the forks show change
// nothing they read; a change to a row they show gives them a copy in a
// holder they read in place first; and a revert of rows they show gives
// the rows to that holder.
func TestForkStampsIgnoreEverySourceWrite(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 3)
	for _, it := range []Item{
		{ID: "root", ThreadID: "S", TurnIndex: 1, ItemIndex: 2, Kind: "tool_call", Role: "assistant", Status: "completed", ToolName: "Task", Summary: "Task", Meta: "{}"},
		{ID: "c1", ThreadID: "S", TurnIndex: 1, ItemIndex: 3, ParentID: "root", Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "child 1", Meta: "{}"},
	} {
		if err := insertCarded(s, it); err != nil {
			t.Fatal(err)
		}
	}
	if err := insertWithPayloadCarded(s,
		Item{ID: "tl", ThreadID: "S", TurnIndex: 1, ItemIndex: 4, Kind: "tool_call", Role: "assistant", Status: "completed", ToolName: "Bash", PayloadID: "pt", Meta: "{}"},
		Payload{ID: "pt", Kind: "text", Meta: "{}", Data: []byte("out")},
	); err != nil {
		t.Fatal(err)
	}
	mustPointerFork(t, s, "S", "F", throughTurn(1))
	mustPointerFork(t, s, "F", "G", ForkCut{})

	type view struct {
		stamp  HistoryStamp
		window []Item
	}
	read := func() map[string]view {
		out := map[string]view{}
		for _, id := range []string{"F", "G"} {
			out[id] = view{historyStampOf(t, s, id), forkWindow(t, s, id)}
		}
		return out
	}
	requireUnchanged := func(what string, before map[string]view) {
		t.Helper()
		for id, was := range before {
			now := read()[id]
			if now.stamp != was.stamp {
				t.Errorf("%s moved %s's stamp %+v -> %+v", what, id, was.stamp, now.stamp)
			}
			if !reflect.DeepEqual(now.window, was.window) {
				t.Errorf("%s changed %s's window\n got %+v\nwant %+v", what, id, now.window, was.window)
			}
		}
	}
	rootRev := func() int64 {
		t.Helper()
		root, found, err := s.GetThreadItem("S", "root")
		if err != nil || !found {
			t.Fatalf("source root: found=%v err=%v", found, err)
		}
		return root.Rev
	}

	summary := "reply 2 edited"
	edited := "reply 0 edited"
	for _, write := range []struct {
		name string
		run  func()
	}{
		{"a source append", func() {
			if _, err := appendCarded(s, Item{ID: "late", ThreadID: "S", TurnIndex: 3, Kind: "user_text", Role: "user", Status: "completed", Summary: "late", Meta: "{}"}); err != nil {
				t.Fatal(err)
			}
		}},
		{"a source update past the cut", func() {
			if _, err := s.UpdateItemFields("S", "a2", ItemPartialUpdate{Summary: &summary}); err != nil {
				t.Fatal(err)
			}
		}},
		{"a source delete past the cut", func() {
			if err := s.DeleteThreadItem("S", "u2"); err != nil {
				t.Fatal(err)
			}
		}},
		{"a child past the cut under a root below it", func() {
			was := rootRev()
			if err := insertCarded(s, Item{ID: "c2", ThreadID: "S", TurnIndex: 3, ItemIndex: 1, ParentID: "root", Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "child 2", Meta: "{}"}); err != nil {
				t.Fatal(err)
			}
			if rootRev() == was {
				t.Fatal("the child did not stamp its root below the cut")
			}
		}},
		{"spans on a source payload past the cut", func() {
			if err := insertWithPayloadCarded(s,
				Item{ID: "tl2", ThreadID: "S", TurnIndex: 3, ItemIndex: 2, Kind: "tool_call", Role: "assistant", Status: "completed", ToolName: "Bash", PayloadID: "pl", Meta: "{}"},
				Payload{ID: "pl", Kind: "text", Meta: "{}", Data: []byte("late out")},
			); err != nil {
				t.Fatal(err)
			}
			if err := s.UpdatePayloadSpans("S", "pl", `{"late":1}`, `{"late":2}`); err != nil {
				t.Fatal(err)
			}
		}},
		{"a revision touch of a row the forks show", func() { touchItemForTest(t, s, "S", "a1") }},
		{"a source update below the cut", func() {
			if _, err := s.UpdateItemFields("S", "a0", ItemPartialUpdate{Summary: &edited}); err != nil {
				t.Fatalf("a source update of a row the forks show: %v", err)
			}
		}},
		{"a source revert of rows the forks show", func() {
			if _, _, err := s.DeleteConversationFromTurn("S", 1); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		before := read()
		write.run()
		requireUnchanged(write.name, before)
	}

	// Spans are a cache the client version-checks: they move the holder's
	// stamp alone, whichever thread asks.
	// The revert gave its rows to the holder the update's copy made: the
	// same forks read both, right before the source.
	holder := holderOf(t, s, "F")
	requireIDs(t, "holders", holderIDs(t, s), []string{holder})
	before, holderStamp := read(), holderStampOf(t, s, holder)
	if err := s.UpdatePayloadSpans("F", "pt", `{"preview":1}`, `{"full":1}`); err != nil {
		t.Fatal(err)
	}
	for id, was := range before {
		if now := historyStampOf(t, s, id); now != was.stamp {
			t.Errorf("spans on a payload %s shows moved its stamp %+v -> %+v", id, was.stamp, now)
		}
	}
	if holderStampOf(t, s, holder) == holderStamp {
		t.Error("spans left the stamp of the payload's holder")
	}
	var spans string
	if err := s.db.QueryRow(`SELECT spans FROM payloads WHERE thread_id = ? AND id = 'pt'`, holder).Scan(&spans); err != nil || spans != `{"full":1}` {
		t.Fatalf("holder's spans = %q, %v", spans, err)
	}
}

// holderStampOf reads a holder's stamps, which readHistoryStampTx reports
// as a deleted thread's.
func holderStampOf(t *testing.T, s *Store, holder string) HistoryStamp {
	t.Helper()
	var stamp HistoryStamp
	if err := s.db.QueryRow(`SELECT history_rev, history_epoch FROM threads WHERE id = ?`, holder).Scan(&stamp.Rev, &stamp.Epoch); err != nil {
		t.Fatalf("stamps of holder %s: %v", holder, err)
	}
	return stamp
}

// TestHolderMoveStampsTheRowsItChanges: a row's revision is its thread's
// revision when the row's read last changed. A row that moves to a holder
// changes the read of its completion sibling, which stays: the sibling is
// served at the source's revision after the move.
func TestHolderMoveStampsTheRowsItChanges(t *testing.T) {
	s := newTestStore(t)
	seedForkSource(t, s, "S", []Item{
		{ID: "u0", TurnIndex: 0, ItemIndex: 0, Kind: "user_text", Role: "user", Status: "completed", Summary: "hi", Meta: "{}"},
		{ID: "read", TurnIndex: 0, ItemIndex: 1, Kind: "tool_call", Role: "assistant", Status: "completed", ToolName: "Read", Summary: "Read: a.go", Meta: "{}"},
		{ID: "read-done", TurnIndex: 0, ItemIndex: 2, Kind: "tool_completion", Role: "assistant", Status: "completed", CompletionOf: "read", ToolName: "Read", Summary: "Read: a.go -> done", Meta: "{}"},
	})
	mustPointerFork(t, s, "S", "F", ForkCut{})
	if err := s.DeleteThreadItem("S", "read"); err != nil {
		t.Fatal(err)
	}
	requireIDs(t, "holder rows", ownIDs(t, s, holderOf(t, s, "F")), []string{"read"})
	sibling, found, err := s.GetThreadItem("S", "read-done")
	if err != nil || !found {
		t.Fatalf("the sibling: found=%v err=%v", found, err)
	}
	if stamp := historyStampOf(t, s, "S"); sibling.Rev != stamp.Rev {
		t.Fatalf("the sibling reads at revision %d, the source at %+v", sibling.Rev, stamp)
	}
}
