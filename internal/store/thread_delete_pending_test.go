package store

import (
	"database/sql"
	"errors"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// seedPendingDeleteFixture opens a store holding S, a 1200-row thread that
// drains in several chunks, K, a thread every check finds, and F, a
// pointer fork of S. S and K both answer "bilby" by title and by message.
func seedPendingDeleteFixture(t *testing.T) *Store {
	t.Helper()
	s := openStoreAt(t)
	for _, id := range []string{"S", "K"} {
		thread := makeThread(id, "claude")
		thread.Title = "bilby " + id
		if err := s.CreateThread(thread); err != nil {
			t.Fatal(err)
		}
		if err := s.InsertItem(Item{
			ID: id + "-msg", ThreadID: id, TurnIndex: 120, ItemIndex: 0, Kind: "user_text",
			Role: "user", Status: "completed", Summary: "bilby report", CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
		seedCompletedTurn(t, s, id, 120, 1_000, 1_005)
	}
	mustExec(t, s.db, `WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < 1199)
		INSERT INTO items(thread_id,id,turn_index,item_index,kind,role,status,summary,meta,created_at,updated_at)
		SELECT 'S', 'r' || i, i / 10, i % 10, 'assistant_text', 'assistant', 'completed', 'row', '{}', 1, 1 FROM n`)
	mustPointerFork(t, s, "S", "F", ForkCut{})
	requireFoundOnlyAt(t, s, []string{"K", "S"})
	return s
}

// requireFoundOnlyAt checks that among S and K exactly want, sorted, are
// listed, read and searched, that S is pending deletion exactly when it is
// not found and its row remains, and that an S not found refuses forks, and
// transfers while its row remains.
func requireFoundOnlyAt(t *testing.T, s *Store, want []string) {
	t.Helper()
	pick := func(ids []string) []string {
		var out []string
		for _, id := range ids {
			if (id == "S" || id == "K") && !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
		slices.Sort(out)
		return out
	}
	threads, err := s.ListThreads()
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, thread := range threads {
		listed = append(listed, thread.ID)
	}
	requireIDs(t, "ListThreads", pick(listed), want)
	sidebar, err := s.ListThreadsWithItems()
	if err != nil {
		t.Fatal(err)
	}
	listed = listed[:0]
	for _, thread := range sidebar {
		listed = append(listed, thread.ID)
	}
	requireIDs(t, "ListThreadsWithItems", pick(listed), want)
	byActivity, err := s.ListThreadsByActivity(ThreadSearchFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	listed = listed[:0]
	for _, thread := range byActivity {
		listed = append(listed, thread.ID)
	}
	requireIDs(t, "ListThreadsByActivity", pick(listed), want)
	var hits []string
	for _, hit := range mustSearch(t, s, "bilby", ThreadSearchFilter{Limit: 10}) {
		hits = append(hits, hit.ThreadID)
	}
	requireIDs(t, "SearchThreads", pick(hits), want)
	messages, err := s.SearchThreadMessages("bilby", 20)
	if err != nil {
		t.Fatal(err)
	}
	hits = hits[:0]
	for _, hit := range messages {
		hits = append(hits, hit.ThreadID)
	}
	requireIDs(t, "SearchThreadMessages", pick(hits), want)

	gone := !slices.Contains(want, "S")
	if _, err := s.GetOwnedThread("S"); errors.Is(err, sql.ErrNoRows) != gone {
		t.Fatalf("GetOwnedThread(S) = %v, gone %v", err, gone)
	}
	pending, err := s.ListPendingThreadDeletes()
	if err != nil {
		t.Fatal(err)
	}
	_, rowErr := s.GetThread("S")
	if got, want := slices.Contains(pending, "S"), gone && rowErr == nil; got != want {
		t.Fatalf("ListPendingThreadDeletes = %v, want S pending %v", pending, want)
	}
	if !gone {
		return
	}
	for fork, cut := range map[string]ForkCut{"turn": throughTurn(3), "item": {BeforeItemID: "r905"}} {
		err := s.CreatePointerFork(makeThread(fork, "claude"), "S", cut, testInterruptedSummary, 999)
		if !errors.Is(err, ErrForkSourceDeleted) {
			t.Errorf("%s fork of a pending delete = %v, want ErrForkSourceDeleted", fork, err)
		}
		if _, err := s.GetThread(fork); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("a refused fork left thread %s: %v", fork, err)
		}
	}
	for _, kind := range []string{"move", "copy"} {
		if rowErr != nil {
			break
		}
		if _, err := s.CreateThreadTransfer(transferRequest("S", kind, "outgoing")); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("%s of a pending delete = %v, want sql.ErrNoRows", kind, err)
		}
	}
	requireIDs(t, "F lineage", forkLineage(t, s, "F"), nil)
	if _, origin := forkDivider(t, s, "F"); !origin.SourceDeleted {
		t.Errorf("F divider = %+v", origin)
	}
}

// reopenAndFinish closes s, reopens its file as a boot does, checks S is
// still pending and hidden, and completes the delete.
func reopenAndFinish(t *testing.T, s *Store) {
	t.Helper()
	path := s.path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close reopened store: %v", err)
		}
	})
	requireFoundOnlyAt(t, s, []string{"K"})
	if err := s.DeleteThread("S"); err != nil {
		t.Fatalf("completing the pending delete = %v", err)
	}
	if _, err := s.GetThread("S"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("S after the completed delete: %v", err)
	}
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM items WHERE thread_id = 'S'`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("S rows after the completed delete = %d, err %v", rows, err)
	}
	if pending, err := s.ListPendingThreadDeletes(); err != nil || len(pending) != 0 {
		t.Fatalf("pending after the completed delete = %v, err %v", pending, err)
	}
	requireFoundOnlyAt(t, s, []string{"K"})
}

// sourceRows counts S's own rows.
func sourceRows(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM items WHERE thread_id = 'S'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestInterruptedThreadDeleteStaysGone: a delete stopped after its first
// chunk, as a crash stops it, leaves a thread nothing lists, reads,
// searches, forks or transfers, and a reopened store finishes it.
func TestInterruptedThreadDeleteStaysGone(t *testing.T) {
	s := seedPendingDeleteFixture(t)
	type outcome struct {
		returned bool
		err      error
	}
	stopped := make(chan outcome, 1)
	go func() {
		var got outcome
		defer func() { stopped <- got }()
		got.err = s.DeleteThreadPaced("S", func() { runtime.Goexit() })
		got.returned = true
	}()
	if got := <-stopped; got.returned {
		t.Fatalf("the delete returned %v instead of stopping at its first pause", got.err)
	}
	if n := sourceRows(t, s); n != 701 {
		t.Fatalf("S holds %d rows, want the 701 the first chunk left", n)
	}
	requireFoundOnlyAt(t, s, []string{"K"})
	reopenAndFinish(t, s)
}

// TestFailedThreadDeleteStaysGone: a delete whose second chunk fails
// returns the error and leaves the same state as one a crash stopped.
func TestFailedThreadDeleteStaysGone(t *testing.T) {
	s := seedPendingDeleteFixture(t)
	// The writer is one connection, so a TEMP trigger on it sees every
	// delete. The first chunk leaves 701 of S's rows.
	mustExec(t, s.db, `CREATE TEMP TRIGGER fail_second_chunk BEFORE DELETE ON main.items
		WHEN OLD.thread_id = 'S' AND (SELECT COUNT(*) FROM main.items WHERE thread_id = 'S') <= 701
		BEGIN SELECT RAISE(ABORT, 'injected chunk failure'); END`)
	err := s.DeleteThread("S")
	if err == nil || !strings.Contains(err.Error(), "injected chunk failure") {
		t.Fatalf("the delete = %v, want the injected failure", err)
	}
	mustExec(t, s.db, `DROP TRIGGER temp.fail_second_chunk`)
	if n := sourceRows(t, s); n != 701 {
		t.Fatalf("S holds %d rows, want the 701 the first chunk left", n)
	}
	requireFoundOnlyAt(t, s, []string{"K"})
	reopenAndFinish(t, s)
}

// TestPointerForkOfAMovedSourceSaysItMoved: fork admission reads sources
// through owned_threads, which also leaves out a thread this computer gave
// away. Such a fork is refused with the move, not as a deletion.
func TestPointerForkOfAMovedSourceSaysItMoved(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 1)
	seedTransfer(t, s, "S", "outgoing", "move", "complete", 128)
	err := s.CreatePointerFork(makeThread("F", "claude"), "S", ForkCut{}, testInterruptedSummary, 999)
	var moved *ThreadTransferError
	if !errors.As(err, &moved) || !moved.Moved || errors.Is(err, ErrForkSourceDeleted) {
		t.Fatalf("fork of a moved source = %v, want the move", err)
	}
	if _, err := s.GetThread("F"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a refused fork left thread F: %v", err)
	}
}
