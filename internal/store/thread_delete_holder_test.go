package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/threadmode"
)

// TestSourceDeleteKeepsItAsAHolder: a deleted thread its forks read keeps
// the rows they read as a holder, in place, and loses everything else as
// its delete would have removed it. The forks read exactly what they read
// before and are told nothing. A fork of a fork keeps its history when the
// fork between goes too, and the last reader's delete releases every
// holder it kept.
func TestSourceDeleteKeepsItAsAHolder(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 2)
	mustPointerFork(t, s, "S", "F", ForkCut{})
	for _, it := range []Item{
		{ID: "u2", ThreadID: "S", TurnIndex: 2, Kind: "user_text", Role: "user", Status: "completed", Summary: "past the cut"},
		{ID: "f2", ThreadID: "F", TurnIndex: 2, Kind: "user_text", Role: "user", Status: "completed", Summary: "fork 2"},
	} {
		if _, err := appendCarded(s, it); err != nil {
			t.Fatal(err)
		}
	}
	mustPointerFork(t, s, "F", "G", ForkCut{})
	if _, err := s.db.Exec(`DELETE FROM threads WHERE id = 'S'`); err == nil || !strings.Contains(err.Error(), "kept as a holder") {
		t.Fatalf("deleting a source that forks read: %v", err)
	}
	shapes := map[string][]string{"F": timelineShape(t, s, "F"), "G": timelineShape(t, s, "G")}
	stamps := map[string]HistoryStamp{"F": historyStampOf(t, s, "F"), "G": historyStampOf(t, s, "G")}
	requireForks := func(what string, forks ...string) {
		t.Helper()
		for _, id := range forks {
			requireShape(t, s, id, shapes[id])
			if now := historyStampOf(t, s, id); now != stamps[id] {
				t.Fatalf("%s moved %s's stamp %+v -> %+v", what, id, stamps[id], now)
			}
		}
	}
	released := 0
	s.OnHoldersReleased(func() { released++ })

	// S reads nothing through a lineage, so its delete removes no lineage
	// row and reports nothing.
	if err := s.DeleteThread("S"); err != nil {
		t.Fatal(err)
	}
	requireForks("the source's delete", "F", "G")
	if released != 0 {
		t.Fatalf("the source's delete reported %d possible releases", released)
	}
	requireIDs(t, "held rows", ownIDs(t, s, "S"), []string{"u0", "a0", "u1", "a1"})
	requireHolder(t, s, "S", "Thread S")
	fork, err := s.GetThread("F")
	if err != nil || fork.ForkedFromThreadID != "" {
		t.Fatalf("F forked from %q, %v; the link to a deleted thread clears", fork.ForkedFromThreadID, err)
	}
	var origin string
	if err := s.db.QueryRow(`SELECT fork_source_title FROM threads WHERE id = 'F'`).Scan(&origin); err != nil || origin != "Thread S" {
		t.Fatalf("F's origin title = %q, %v", origin, err)
	}
	err = s.CreatePointerFork(makeThread("late", "claude"), "S", ForkCut{}, testInterruptedSummary, 999)
	if !errors.Is(err, ErrForkSourceDeleted) {
		t.Fatalf("a fork of a holder = %v, want ErrForkSourceDeleted", err)
	}

	if err := s.DeleteThread("F"); err != nil {
		t.Fatal(err)
	}
	requireForks("the middle fork's delete", "G")
	requireHolder(t, s, "F", "Thread F")
	requireIDs(t, "released holders while G reads them", releasedHolders(t, s), nil)

	released = 0
	if err := s.DeleteThread("G"); err != nil {
		t.Fatal(err)
	}
	if released != 1 {
		t.Fatalf("the last reader's delete reported %d possible releases, want 1", released)
	}
	holders := releasedHolders(t, s)
	requireIDs(t, "released holders", holders, []string{"F", "S"})
	pending, err := s.ListPendingThreadDeletes()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range holders {
		if !slices.Contains(pending, id) {
			t.Fatalf("pending deletes %v leave out released holder %s", pending, id)
		}
		if err := s.DeleteThread(id); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetThread(id); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("holder %s after its delete: %v", id, err)
		}
	}
	var rows int
	if err := s.db.QueryRow(`SELECT count(*) FROM items`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("left %d rows, %v", rows, err)
	}
}

// requireHolder checks a deleted thread kept as a holder: hidden, with its
// title and no project, and gone to every client read and execution.
func requireHolder(t *testing.T, s *Store, id, title string) {
	t.Helper()
	thread, err := s.GetThread(id)
	if err != nil {
		t.Fatal(err)
	}
	if thread.Mode != threadmode.ModeHolder || thread.Title != title || thread.ProjectID != "" || thread.ForkedFromThreadID != "" {
		t.Fatalf("holder %s = mode %q title %q project %q forked from %q", id, thread.Mode, thread.Title, thread.ProjectID, thread.ForkedFromThreadID)
	}
	var deleting bool
	if err := s.db.QueryRow(`SELECT deleting FROM threads WHERE id = ?`, id).Scan(&deleting); err != nil || deleting {
		t.Fatalf("holder %s deleting=%v err=%v", id, deleting, err)
	}
	if _, err := s.GetOwnedThread(id); err == nil {
		t.Fatalf("holder %s reads as an owned thread", id)
	}
	if err := s.CheckThreadExecutionAccess(thread); !errors.Is(err, ErrThreadGone) {
		t.Fatalf("running in holder %s = %v, want ErrThreadGone", id, err)
	}
	sync, err := s.SyncThreadWindow(context.Background(), id, "", 50, testRunWindowRows, HistoryStamp{}, nil, TimelineSelection{})
	if err != nil || sync.Status != SyncGone {
		t.Fatalf("sync of holder %s = %s, %v; want gone", id, sync.Status, err)
	}
	if _, found, err := s.ThreadHistoryStamp(id); err != nil || found {
		t.Fatalf("holder %s stamp found=%v err=%v", id, found, err)
	}
	listed, err := s.ListThreads()
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range listed {
		if other.ID == id {
			t.Fatalf("holder %s is listed", id)
		}
	}
	old, err := s.ThreadIDsOlderThan(time.Now().Add(time.Hour).UnixMilli())
	if err != nil || slices.Contains(old, id) {
		t.Fatalf("retention would delete holder %s: %v %v", id, old, err)
	}
}

// TestSourceDeleteDrainsOnlyRowsNoForkReads: a long source drains in
// chunks, and only the rows at or after its forks' last cut go, so between
// chunks and after the fork reads what it read.
func TestSourceDeleteDrainsOnlyRowsNoForkReads(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "S")
	mustExec(t, s.db, `WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < 1799)
		INSERT INTO items(thread_id,id,turn_index,item_index,kind,role,status,summary,meta,created_at,updated_at)
		SELECT 'S', 'r' || i, i / 10, i % 10, 'assistant_text', 'assistant', 'completed', 'row', '{}', 1, 1 FROM n`)
	mustPointerFork(t, s, "S", "F", throughTurn(59))
	fork := itemIDs(forkRows(t, s, "F"))
	if len(fork) != 600 {
		t.Fatalf("F reads %d rows, want 600", len(fork))
	}
	pauses := 0
	if err := s.DeleteThreadPaced("S", func() {
		pauses++
		requireIDs(t, fmt.Sprintf("F rows at pause %d", pauses), itemIDs(forkRows(t, s, "F")), fork)
	}); err != nil {
		t.Fatal(err)
	}
	if pauses == 0 {
		t.Fatal("the source drained in one chunk; the fixture must span several")
	}
	requireIDs(t, "F rows", itemIDs(forkRows(t, s, "F")), fork)
	requireIDs(t, "held rows", ownIDs(t, s, "S"), fork)
	requireHolder(t, s, "S", "Thread S")
}

// TestPointerForkOfADeletingSourceIsRefused: from the start of a paced
// delete, a fork of the thread is refused, whatever its cut, and leaves no
// thread behind; so is a fork of the thread once it is gone.
func TestPointerForkOfADeletingSourceIsRefused(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "S")
	mustExec(t, s.db, `WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < 1199)
		INSERT INTO items(thread_id,id,turn_index,item_index,kind,role,status,summary,meta,created_at,updated_at)
		SELECT 'S', 'r' || i, i / 10, i % 10, 'assistant_text', 'assistant', 'completed', 'row', '{}', 1, 1 FROM n`)
	refused := func(fork string, cut ForkCut) {
		t.Helper()
		err := s.CreatePointerFork(makeThread(fork, "claude"), "S", cut, testInterruptedSummary, 999)
		if !errors.Is(err, ErrForkSourceDeleted) {
			t.Errorf("fork %s of a deleting source = %v, want ErrForkSourceDeleted", fork, err)
		}
		if _, err := s.GetThread(fork); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("a refused fork left thread %s: %v", fork, err)
		}
	}
	pauses := 0
	if err := s.DeleteThreadPaced("S", func() {
		pauses++
		refused(fmt.Sprintf("N%d", pauses), ForkCut{})
		refused(fmt.Sprintf("E%d", pauses), throughTurn(-1))
	}); err != nil {
		t.Fatal(err)
	}
	if pauses == 0 {
		t.Fatal("the source drained in one chunk; the fixture must span several")
	}
	refused("after", ForkCut{})
	if err := s.DeleteThreadPaced("missing", nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleting a missing thread = %v, want sql.ErrNoRows", err)
	}
}

// TestPointerForkAdmittedAsTheDeleteBeginsIsKept: a fork whose transaction
// is open when the source's delete begins passed the check before the
// delete marked the source, and the delete's mark waits for the writer
// connection, so the fork commits first and the delete finds it a reader:
// the source keeps what the fork reads as a holder.
func TestPointerForkAdmittedAsTheDeleteBeginsIsKept(t *testing.T) {
	s := newTestStore(t)
	seedForkSource(t, s, "S", []Item{
		{ID: "u0", TurnIndex: 0, ItemIndex: 0, Kind: "user_text", Role: "user", Status: "completed", Summary: "hi", Meta: "{}"},
		{ID: "a0", TurnIndex: 0, ItemIndex: 1, Kind: "assistant_text", Role: "assistant", Status: "streaming", Summary: "partial", Meta: "{}"},
	})
	deleted := make(chan error, 1)
	var started bool
	// The fork settles S's streaming row inside its transaction, which is
	// where the delete begins.
	summarise := func(summary string) string {
		if !started {
			started = true
			waits := s.db.Stats().WaitCount
			go func() { deleted <- s.DeleteThreadPaced("S", nil) }()
			for deadline := time.Now().Add(5 * time.Second); s.db.Stats().WaitCount == waits; {
				if time.Now().After(deadline) {
					t.Fatal("the delete never waited for the writer connection")
				}
				time.Sleep(time.Millisecond)
			}
		}
		return testInterruptedSummary(summary)
	}
	if err := s.CreatePointerFork(makeThread("N", "claude"), "S", ForkCut{}, summarise, 999); err != nil {
		t.Fatalf("a fork admitted before the delete = %v", err)
	}
	if !started {
		t.Fatal("the fork settled no row, so the delete never began inside it")
	}
	if err := <-deleted; err != nil {
		t.Fatalf("delete = %v", err)
	}
	requireHolder(t, s, "S", "Thread S")
	requireIDs(t, "N lineage", forkLineage(t, s, "N"), []string{"1:S:0:2"})
	requireIDs(t, "N rows", itemIDs(forkRows(t, s, "N")), []string{"u0", "a0"})
	requireIDs(t, "N's own rows", ownIDs(t, s, "N"), []string{"a0"})
}

// TestPointerForkSourceEmptiedThenCleanedUp: a source whose history was
// reverted away gave that history to a holder first, so the empty-draft
// cleanup that deletes it leaves its forks whole. A cleanup that keeps the
// thread leaves its forks linked.
func TestPointerForkSourceEmptiedThenCleanedUp(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 1)
	mustPointerFork(t, s, "S", "F", ForkCut{})
	if _, _, err := s.DeleteConversationFromTurn("S", 0); err != nil {
		t.Fatal(err)
	}
	requireIDs(t, "F rows", itemIDs(forkRows(t, s, "F")), []string{"u0", "a0"})
	h := holderOf(t, s, "F")
	requireIDs(t, "F lineage", forkLineage(t, s, "F"), []string{"1:" + h + ":0:2"})
	if deleted, err := s.DeleteEmptyDraftThread("S"); err != nil || !deleted {
		t.Fatalf("cleanup deleted=%v err=%v", deleted, err)
	}
	if _, err := s.GetThread("S"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("the emptied source after its cleanup: %v", err)
	}
	requireIDs(t, "F rows", itemIDs(forkRows(t, s, "F")), []string{"u0", "a0"})

	seedLinearSource(t, s, "kept", 1)
	mustPointerFork(t, s, "kept", "reader", ForkCut{})
	if deleted, err := s.DeleteEmptyDraftThread("kept"); err != nil || deleted {
		t.Fatalf("cleanup of a thread with history deleted=%v err=%v", deleted, err)
	}
	requireIDs(t, "reader lineage", forkLineage(t, s, "reader"), []string{"1:kept:0:2"})
}

// TestRetireToHolderClassifiesEveryThreadReference: every foreign key to
// threads is either kept by a holder or cleared when a thread retires to
// one, so a table added later cannot keep a deleted thread's state alive
// in its holder unnoticed.
func TestRetireToHolderClassifiesEveryThreadReference(t *testing.T) {
	s := newTestStore(t)
	rows, err := s.db.Query(`SELECT m.name, f."from" FROM sqlite_master m, pragma_foreign_key_list(m.name) f
		WHERE m.type = 'table' AND f."table" = 'threads' ORDER BY m.name, f."from"`)
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, table+"."+column)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	classified := map[string]bool{
		// retireToHolderTx clears the links to the thread as the delete's
		// ON DELETE SET NULL would.
		"threads.forked_from_thread_id": true,
		"threads.parent_thread_id":      true,
	}
	for _, table := range holderKeptTables {
		classified[table+".thread_id"] = true
	}
	for _, cleared := range holderClearedRows {
		if classified[cleared.table+"."+cleared.column] {
			t.Errorf("%s.%s is both kept and cleared", cleared.table, cleared.column)
		}
		classified[cleared.table+"."+cleared.column] = true
	}
	for _, ref := range refs {
		if !classified[ref] {
			t.Errorf("%s references threads and a holder neither keeps nor clears it", ref)
		}
		delete(classified, ref)
	}
	for ref := range classified {
		t.Errorf("%s is classified but no foreign key names it", ref)
	}
}

// TestRetiredThreadLosesWhatItsDeleteRemoves: the rows a holder does not
// keep go when the thread retires, and the ones its forks read stay.
func TestRetiredThreadLosesWhatItsDeleteRemoves(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 1)
	questionFixture(t, s, "S", "question")
	if err := s.InsertAttachment(Attachment{ID: "att", ThreadID: "S", Kind: AttachmentKindImage, Filename: "a.png", MimeType: "image/png", RelativePath: "S/att.png"}); err != nil {
		t.Fatal(err)
	}
	mustPointerFork(t, s, "S", "F", ForkCut{})
	mustPointerFork(t, s, "F", "child", ForkCut{})
	mustExec(t, s.db, `UPDATE threads SET parent_thread_id = 'S' WHERE id = 'child'`)
	if err := s.DeleteThread("S"); err != nil {
		t.Fatal(err)
	}
	for _, cleared := range holderClearedRows {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM ` + cleared.table + ` WHERE ` + cleared.column + ` = 'S'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("the holder kept %d rows of %s", n, cleared.table)
		}
	}
	var items, owners, links int
	if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM items WHERE thread_id = 'S'),
		(SELECT count(*) FROM attachment_owners WHERE thread_id = 'S'),
		(SELECT count(*) FROM threads WHERE parent_thread_id = 'S' OR forked_from_thread_id = 'S')`).Scan(&items, &owners, &links); err != nil {
		t.Fatal(err)
	}
	if items == 0 || owners != 1 || links != 0 {
		t.Fatalf("holder keeps %d rows and %d attachments, %d threads still link to it", items, owners, links)
	}
}

// TestDeletingAPointerForkLeavesNothing: a throwaway fork (thread_ask, a
// failed fork) removes every row it wrote, and its source reads exactly as
// it did.
func TestDeletingAPointerForkLeavesNothing(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "S")
	questionFixture(t, s, "S", "question")
	if err := s.InsertAttachment(Attachment{ID: "att", ThreadID: "S", Kind: AttachmentKindImage, Filename: "a.png", MimeType: "image/png", RelativePath: "S/att.png"}); err != nil {
		t.Fatal(err)
	}
	if err := insertCarded(s, Item{ID: "prompt", ThreadID: "S", TurnIndex: 1, Kind: "user_text", Role: "user", Status: "completed", Summary: "look", Meta: `{"attachments":["att"]}`}); err != nil {
		t.Fatal(err)
	}
	if err := insertWithPayloadCarded(s,
		Item{ID: "tool", ThreadID: "S", TurnIndex: 1, ItemIndex: 1, Kind: "tool_call", Role: "assistant", Status: "running", PayloadID: "p", Meta: "{}"},
		Payload{ID: "p", Kind: "text", Meta: "{}", Data: []byte("output")},
	); err != nil {
		t.Fatal(err)
	}
	source := timelineShape(t, s, "S")
	stamp := historyStampOf(t, s, "S")

	mustPointerFork(t, s, "S", "F", ForkCut{})
	if err := s.UpdateItemMeta("F", "prompt", `{"attachments":["att"],"edited":true}`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteThreadItem("F", "question"); err != nil {
		t.Fatal(err)
	}
	if _, err := appendWithPayloadCarded(s, Item{ID: "answer", ThreadID: "F", TurnIndex: 2, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "fork answer", PayloadID: "fp", Meta: "{}"},
		Payload{ID: "fp", Kind: "text", Meta: "{}", Data: []byte("fork")}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendPayloadData("F", "fp", []byte(" more"), "{}", 3); err != nil {
		t.Fatal(err)
	}
	var written int
	if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM items WHERE thread_id = 'F') + (SELECT count(*) FROM thread_fork_hidden WHERE thread_id = 'F')
		+ (SELECT count(*) FROM attachment_owners WHERE thread_id = 'F')`).Scan(&written); err != nil || written < 5 {
		t.Fatalf("fixture wrote %d fork rows, %v", written, err)
	}
	mustPointerFork(t, s, "F", "ask", ForkCut{})

	for _, id := range []string{"ask", "F"} {
		if err := s.DeleteThread(id); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"items", "turns", "payloads", "payload_chunks", "edit_file_snapshots", "thread_fork_lineage",
			"thread_fork_hidden", "attachment_owners", "async_questions", "thread_search_rows"} {
			var n int
			if err := s.db.QueryRow(`SELECT count(*) FROM `+table+` WHERE thread_id = ?`, id).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Errorf("deleted fork %s left %d rows in %s", id, n, table)
			}
		}
		if _, err := s.GetThread(id); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("deleted fork %s left its row: %v", id, err)
		}
	}
	requireShape(t, s, "S", source)
	if after := historyStampOf(t, s, "S"); after != stamp {
		t.Fatalf("source stamp moved %+v -> %+v", stamp, after)
	}
	if owned, err := s.OwnsAttachment("S", "att"); err != nil || !owned {
		t.Fatalf("source lost its attachment: %v %v", owned, err)
	}
}

// TestDeleteThreadSearchItemsTakesAnyNumberOfIDs: a revert or delete of
// more rows than SQLite binds parameters removes their index rows in one
// statement.
func TestDeleteThreadSearchItemsTakesAnyNumberOfIDs(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "T")
	const n = 40_001
	mustExec(t, s.db, `WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO thread_search_rows(thread_id, item_id, source, kind) SELECT 'T', 'r' || i, 'item', 'message' FROM n`, n-1)
	mustExec(t, s.db, `INSERT INTO thread_search(rowid, text) SELECT rowid, 'needle' FROM thread_search_rows WHERE item_id <> ''`)
	mustExec(t, s.db, `INSERT INTO thread_search_rows(thread_id, item_id, source, kind) VALUES ('T', 'kept', 'item', 'message')`)
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("r%d", i)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := deleteThreadSearchItemsTx(tx, "T", ids); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	left, err := queryIDs(s.db, `SELECT item_id FROM thread_search_rows WHERE thread_id = 'T' AND item_id <> ''`)
	if err != nil {
		t.Fatal(err)
	}
	requireIDs(t, "index rows left", left, []string{"kept"})
	var text int
	if err := s.db.QueryRow(`SELECT count(*) FROM thread_search WHERE thread_search MATCH 'needle'`).Scan(&text); err != nil || text != 0 {
		t.Fatalf("%d index texts left, %v", text, err)
	}
}
