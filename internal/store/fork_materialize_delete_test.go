package store

import (
	"context"
	"database/sql"
	"errors"
	"runtime"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/itemmeta"
)

// seedMaterializeDeleteFixture creates S, a fork F of it and a fork G of
// F. S holds an agent A with three children (turns 1 and 2), a tool row
// with a payload (turn 3), a message showing S's attachment att (turn 4)
// and rows r0 to r1199 (turns 10 to 129). F took its own copy of A by
// renaming it and wrote f-own past its cut. F reads 1,206 rows its
// materialization copies in batches, three of them, and att-msg, which
// its last transaction copies.
func seedMaterializeDeleteFixture(t *testing.T, s *Store) {
	t.Helper()
	seedForkAgentSource(t, s, "S")
	if err := insertWithPayloadCarded(s,
		Item{ID: "tool", ThreadID: "S", TurnIndex: 3, Kind: "tool_call", Role: "assistant", Status: "completed", Summary: "tool", PayloadID: "p", Meta: "{}"},
		Payload{ID: "p", Kind: "text", Meta: "{}", Data: []byte("output")},
	); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertAttachment(Attachment{ID: "att", ThreadID: "S", Kind: AttachmentKindImage, Filename: "a.png", MimeType: "image/png", RelativePath: "S/att.png"}); err != nil {
		t.Fatal(err)
	}
	if err := insertCarded(s, Item{ID: "att-msg", ThreadID: "S", TurnIndex: 4, Kind: "user_text", Role: "user", Status: "completed", Summary: "look", Meta: `{"attachments":["att"]}`}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s.db, `WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < 1199)
		INSERT INTO items(thread_id,id,turn_index,item_index,kind,role,status,summary,meta,created_at,updated_at)
		SELECT 'S', 'r' || i, 10 + i / 10, i % 10, 'assistant_text', 'assistant', 'completed', 'row ' || i, '{}', 1, 1 FROM n`)
	mustPointerFork(t, s, "S", "F", ForkCut{})
	summary := "Agent: A in the fork"
	if _, err := s.UpdateItemFields("F", "A", ItemPartialUpdate{Summary: &summary}); err != nil {
		t.Fatal(err)
	}
	if _, err := appendCarded(s, Item{ID: "f-own", ThreadID: "F", TurnIndex: 200, Kind: "user_text", Role: "user", Status: "completed", Summary: "fork only"}); err != nil {
		t.Fatal(err)
	}
	mustPointerFork(t, s, "F", "G", ForkCut{})
	if n := ownRowCount(t, s, "F"); n != 3 {
		t.Fatalf("fixture: F stores %d rows, want its divider, A and f-own", n)
	}
}

// forkCopyRecords counts the copies threadID's unfinished materialization
// recorded.
func forkCopyRecords(t *testing.T, s *Store, threadID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM thread_fork_copied WHERE thread_id = ?`, threadID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// requireRecordedCopiesOnly checks that F stores its three own rows plus
// exactly its recorded copies, and hides no row but A and those copies,
// and that it owns no attachment: its materialization has not finished.
func requireRecordedCopiesOnly(t *testing.T, s *Store, records int) {
	t.Helper()
	if n := forkCopyRecords(t, s, "F"); n != records {
		t.Fatalf("F records %d copies, want %d", n, records)
	}
	if n := ownRowCount(t, s, "F"); n != 3+records {
		t.Fatalf("F stores %d rows, want its 3 and %d recorded copies", n, records)
	}
	var strays, owners int
	if err := s.db.QueryRow(`SELECT
		(SELECT count(*) FROM thread_fork_hidden h WHERE h.thread_id = 'F' AND h.item_id <> 'A'
		    AND NOT EXISTS (SELECT 1 FROM thread_fork_copied c WHERE c.thread_id = 'F' AND c.item_id = h.item_id))
		+ (SELECT count(*) FROM items i WHERE i.thread_id = 'F' AND i.id NOT IN ('A', 'f-own', 'fork-origin-F')
		    AND NOT EXISTS (SELECT 1 FROM thread_fork_copied c WHERE c.thread_id = 'F' AND c.item_id = i.id)),
		(SELECT count(*) FROM attachment_owners WHERE thread_id = 'F')`).Scan(&strays, &owners); err != nil {
		t.Fatal(err)
	}
	if strays != 0 || owners != 0 {
		t.Fatalf("F holds %d unrecorded copies or hides and owns %d attachments", strays, owners)
	}
}

// requireForkKeepsNothingOfS checks the state S's delete leaves: F and G
// read only F's own rows, F holds no copy, hide, payload, search row or
// attachment of S's, and both serve the cards the read-time walk derives.
func requireForkKeepsNothingOfS(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.GetThread("S"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("S after its delete: %v", err)
	}
	requireIDs(t, "F lineage", forkLineage(t, s, "F"), nil)
	requireIDs(t, "F rows", itemIDs(forkRows(t, s, "F")), []string{"A", "f-own"})
	requireIDs(t, "G rows", itemIDs(forkRows(t, s, "G")), []string{"A", forkDividerID("F"), "f-own"})
	var records, hides, payloads, search, owners int
	if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM thread_fork_copied),
		(SELECT count(*) FROM thread_fork_hidden WHERE thread_id = 'F' AND item_id <> 'A'),
		(SELECT count(*) FROM payloads WHERE thread_id = 'F'),
		(SELECT count(*) FROM thread_search_rows sr WHERE sr.thread_id = 'F' AND sr.item_id <> ''
		    AND NOT EXISTS (SELECT 1 FROM items i WHERE i.thread_id = 'F' AND i.id = sr.item_id)),
		(SELECT count(*) FROM attachment_owners WHERE thread_id = 'F')`).Scan(&records, &hides, &payloads, &search, &owners); err != nil {
		t.Fatal(err)
	}
	if records != 0 || hides != 0 || payloads != 0 || search != 0 || owners != 0 {
		t.Fatalf("after S's delete: %d records, F keeps %d hides, %d payloads, %d search rows, %d attachments of S's",
			records, hides, payloads, search, owners)
	}
	if _, origin := forkDivider(t, s, "F"); !origin.SourceDeleted {
		t.Fatalf("F divider = %+v", origin)
	}
	if _, count := forkAnchorCard(t, s, "F"); count != 0 {
		t.Fatalf("A serves %v descendants in F, whose children were S's", count)
	}
	assertSubagentStampParity(t, s, "F", "after the delete", true)
	assertSubagentStampParity(t, s, "G", "after the delete", true)
}

// TestForkMaterializationInterleavedWithASourceDelete: the fork keeps no
// part of a source whose delete runs between two batches of its
// materialization. A batch after the mark copies nothing and fails with
// ErrForkSourceDeleting; while the delete rolls back the copies, the fork
// and its fork read what they read before. A materialization that resumes
// after the whole delete completes with what the fork reads without the
// source.
func TestForkMaterializationInterleavedWithASourceDelete(t *testing.T) {
	t.Run("delete in progress", func(t *testing.T) {
		s := newTestStore(t)
		seedMaterializeDeleteFixture(t, s)
		fork, grandchild := timelineShape(t, s, "F"), timelineShape(t, s, "G")
		reports := watchForkMoves(s)
		paused, release, deleted := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		pauses := 0
		pause := func() {
			if pauses++; pauses == 1 {
				close(paused)
				<-release
			}
		}
		batches := 0
		err := s.materializeForkHistory(context.Background(), "F", func() {
			if batches++; batches != 2 {
				return
			}
			requireRecordedCopiesOnly(t, s, 1000)
			stamp := historyStampOf(t, s, "F")
			go func() { deleted <- s.DeleteThreadPaced("S", pause) }()
			<-paused
			// The mark and one rollback chunk have committed.
			if pending, err := s.ListPendingThreadDeletes(); err != nil || !slices.Equal(pending, []string{"S"}) {
				t.Fatalf("pending deletes = %v, %v", pending, err)
			}
			requireRecordedCopiesOnly(t, s, 500)
			// Rows F stored read as inherited again: a held window is rewritten.
			if now := historyStampOf(t, s, "F"); now.Rev <= stamp.Rev || now.Epoch <= stamp.Epoch {
				t.Fatalf("F's stamp %+v -> %+v across the rollback chunk", stamp, now)
			}
			requireShape(t, s, "F", fork)
			requireShape(t, s, "G", grandchild)
			assertSubagentStampParity(t, s, "F", "mid rollback", true)
			if got, want := reports.take(), [][]string{{"F"}}; !slices.EqualFunc(got, want, slices.Equal) {
				t.Fatalf("the rollback chunk reported %v, want %v", got, want)
			}
		})
		if batches != 2 || !errors.Is(err, ErrForkSourceDeleting) {
			t.Fatalf("materialization after %d batches = %v, want ErrForkSourceDeleting after 2", batches, err)
		}
		requireRecordedCopiesOnly(t, s, 500)
		close(release)
		if err := <-deleted; err != nil {
			t.Fatalf("delete = %v", err)
		}
		requireForkKeepsNothingOfS(t, s)
	})

	t.Run("delete completed", func(t *testing.T) {
		s := newTestStore(t)
		seedMaterializeDeleteFixture(t, s)
		batches := 0
		err := s.materializeForkHistory(context.Background(), "F", func() {
			if batches++; batches == 1 {
				if err := s.DeleteThread("S"); err != nil {
					t.Fatalf("delete = %v", err)
				}
				requireForkKeepsNothingOfS(t, s)
			}
		})
		if batches != 3 || err != nil {
			t.Fatalf("materialization after %d batches = %v, want success after 3", batches, err)
		}
		requireForkKeepsNothingOfS(t, s)
		if n := ownRowCount(t, s, "F"); n != 3 {
			t.Fatalf("F stores %d rows after the materialization, want its own 3", n)
		}
	})
}

// TestStoppedForkMaterializationRollsBackWithTheSourceDelete: the copies
// of a materialization that was stopped, and of a delete that stopped
// partway through rolling them back, survive a reopen. The reopened store
// refuses to materialize through the pending delete, and completing the
// delete, as the boot does, rolls back the rest.
func TestStoppedForkMaterializationRollsBackWithTheSourceDelete(t *testing.T) {
	s := openStoreAt(t)
	seedMaterializeDeleteFixture(t, s)
	fork := timelineShape(t, s, "F")
	ctx, cancel := context.WithCancel(context.Background())
	batches := 0
	if err := s.materializeForkHistory(ctx, "F", func() {
		if batches++; batches == 2 {
			cancel()
		}
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped materialization = %v", err)
	}
	requireRecordedCopiesOnly(t, s, 1000)
	if owned, err := s.OwnsAttachment("F", "att"); err != nil || !owned {
		t.Fatalf("F reads att: %v %v", owned, err)
	}

	stopped := make(chan bool, 1)
	go func() {
		returned := false
		defer func() { stopped <- returned }()
		_ = s.DeleteThreadPaced("S", func() { runtime.Goexit() })
		returned = true
	}()
	if <-stopped {
		t.Fatal("the delete returned instead of stopping at its first pause")
	}
	s = reopenStore(t, s)
	requireRecordedCopiesOnly(t, s, 500)
	requireShape(t, s, "F", fork)
	if err := s.MaterializeForkHistory(context.Background(), "F"); !errors.Is(err, ErrForkSourceDeleting) {
		t.Fatalf("materialization through a pending delete = %v", err)
	}
	requireRecordedCopiesOnly(t, s, 500)
	if pending, err := s.ListPendingThreadDeletes(); err != nil || !slices.Equal(pending, []string{"S"}) {
		t.Fatalf("pending deletes = %v, %v", pending, err)
	}
	if err := s.DeleteThread("S"); err != nil {
		t.Fatal(err)
	}
	requireForkKeepsNothingOfS(t, s)
}

// TestResumedForkMaterializationCompletes: a materialization run again
// after one was stopped copies the rest, the rows that show attachments
// with their ownership, and settles its records; a later delete of the
// source leaves the fork whole.
func TestResumedForkMaterializationCompletes(t *testing.T) {
	s := newTestStore(t)
	seedMaterializeDeleteFixture(t, s)
	fork, grandchild := timelineShape(t, s, "F"), timelineShape(t, s, "G")
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.materializeForkHistory(ctx, "F", cancel); !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped materialization = %v", err)
	}
	requireRecordedCopiesOnly(t, s, 500)
	if err := s.MaterializeForkHistory(context.Background(), "F"); err != nil {
		t.Fatal(err)
	}
	requireShape(t, s, "F", fork)
	requireShape(t, s, "G", grandchild)
	requireIDs(t, "F lineage", forkLineage(t, s, "F"), nil)
	if n, own := forkCopyRecords(t, s, "F"), ownRowCount(t, s, "F"); n != 0 || own != len(fork) {
		t.Fatalf("F records %d copies and stores %d of its %d rows", n, own, len(fork))
	}
	var owned bool
	if err := s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM attachment_owners WHERE thread_id = 'F' AND attachment_id = 'att')`).Scan(&owned); err != nil || !owned {
		t.Fatalf("F owns att: %v %v", owned, err)
	}

	if err := s.DeleteThread("S"); err != nil {
		t.Fatal(err)
	}
	after := forkRows(t, s, "F")
	if len(after) != len(fork)-1 {
		t.Fatalf("F kept %d of its %d rows", len(after), len(fork)-1)
	}
	if owned, err := s.OwnsAttachment("F", "att"); err != nil || !owned {
		t.Fatalf("F kept att: %v %v", owned, err)
	}
	assertSubagentStampParity(t, s, "F", "after the delete", true)
}

// TestForkMaterializationRefusedThroughADeletingThread: from the mark of
// a delete, a fork that reads through the thread, directly or through a
// nearer fork, copies nothing.
func TestForkMaterializationRefusedThroughADeletingThread(t *testing.T) {
	s := newTestStore(t)
	seedMaterializeDeleteFixture(t, s)
	if err := s.beginThreadDelete("S"); err != nil {
		t.Fatal(err)
	}
	for fork, own := range map[string]int{"F": 3, "G": 1} {
		if err := s.MaterializeForkHistory(context.Background(), fork); !errors.Is(err, ErrForkSourceDeleting) {
			t.Fatalf("materialization of %s = %v, want ErrForkSourceDeleting", fork, err)
		}
		if n, records := ownRowCount(t, s, fork), forkCopyRecords(t, s, fork); n != own || records != 0 {
			t.Fatalf("refused %s stores %d rows and %d records, want %d and none", fork, n, records, own)
		}
	}
	if err := s.DeleteThread("S"); err != nil {
		t.Fatal(err)
	}
	requireForkKeepsNothingOfS(t, s)
}

// TestForkDetachRollsBackAnUnfinishedMaterialization: a detach that finds
// recorded copies, as the draft and import-rollback deletes do, which do
// not pace a rollback first, rolls them all back in its transaction.
func TestForkDetachRollsBackAnUnfinishedMaterialization(t *testing.T) {
	s := newTestStore(t)
	seedMaterializeDeleteFixture(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	batches := 0
	if err := s.materializeForkHistory(ctx, "F", func() {
		if batches++; batches == 2 {
			cancel()
		}
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped materialization = %v", err)
	}
	requireRecordedCopiesOnly(t, s, 1000)
	if err := s.beginThreadDelete("S"); err != nil {
		t.Fatal(err)
	}
	if err := s.detachThreadForks("S"); err != nil {
		t.Fatal(err)
	}
	requireRecordedCopiesOnly(t, s, 0)
	if err := s.DeleteThread("S"); err != nil {
		t.Fatal(err)
	}
	requireForkKeepsNothingOfS(t, s)
}

// TestForkWritesSettleTheCopiesTheyKeep: a fork whose materialization
// copied A and A-c2 of A's three children makes the recorded copies on a
// row's parent chain its own when it writes the row or a row under it, as
// a card's write makes the inherited anchors on its chain its own, so the
// source's delete keeps them with the row. A delete or revert in the fork
// keeps nothing on the copies above the rows it removes, so it settles
// none, and the source's delete leaves the fork no row of the source's.
func TestForkWritesSettleTheCopiesTheyKeep(t *testing.T) {
	fresh := func(id string, parent string) Item {
		return Item{ID: id, ThreadID: "F", TurnIndex: 50, ParentID: parent, Kind: "tool_call", ToolName: "Bash",
			Role: "assistant", Status: "completed", Summary: "Bash: fork", Meta: "{}"}
	}
	for _, tc := range []struct {
		name    string
		write   func(t *testing.T, s *Store) error
		records []string
		rows    []string
	}{
		{"insert under a copy", func(t *testing.T, s *Store) error {
			_, err := appendCarded(s, fresh("f-child", "A"))
			return err
		}, []string{"A-c2"}, []string{"A", "f-child"}},
		{"move under a copy", func(t *testing.T, s *Store) error {
			if _, err := appendCarded(s, fresh("f-row", "")); err != nil {
				return err
			}
			_, err := upsertCarded(s, fresh("f-row", "A"), nil)
			return err
		}, []string{"A-c2"}, []string{"A", "f-row"}},
		{"update of a row under a copy", func(t *testing.T, s *Store) error {
			return s.UpdateItemMeta("F", "A-c1", `{"edited":true}`)
		}, []string{"A-c2"}, []string{"A", "A-c1"}},
		{"update of a copy", func(t *testing.T, s *Store) error {
			return s.UpdateItemMeta("F", "A-c2", `{"edited":true}`)
		}, nil, []string{"A", "A-c2"}},
		{"delete of a copy", func(t *testing.T, s *Store) error {
			return s.DeleteThreadItem("F", "A-c2")
		}, []string{"A"}, nil},
		{"delete of a row under a copy", func(t *testing.T, s *Store) error {
			return s.DeleteThreadItem("F", "A-c1")
		}, []string{"A", "A-c2"}, nil},
		{"revert past a copy", func(t *testing.T, s *Store) error {
			_, _, err := s.DeleteConversationFromTurn("F", 2)
			return err
		}, []string{"A", "A-c2"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			seedForkAgentSource(t, s, "S")
			mustPointerFork(t, s, "S", "F", ForkCut{})
			if err := s.materializeForkRows("F", []string{"A", "A-c2"}); err != nil {
				t.Fatal(err)
			}
			if err := tc.write(t, s); err != nil {
				t.Fatal(err)
			}
			records, err := queryIDs(s.db, `SELECT item_id FROM thread_fork_copied WHERE thread_id = 'F' ORDER BY item_id`)
			if err != nil {
				t.Fatal(err)
			}
			requireIDs(t, "F records after its write", records, tc.records)
			if err := s.DeleteThread("S"); err != nil {
				t.Fatal(err)
			}
			requireIDs(t, "F rows", itemIDs(forkRows(t, s, "F")), tc.rows)
			if n := forkCopyRecords(t, s, "F"); n != 0 {
				t.Fatalf("F records %d copies after the delete", n)
			}
			if tc.rows != nil {
				if _, count := forkAnchorCard(t, s, "F"); count != 1 {
					t.Fatalf("A serves %v descendants in F, want the one the fork kept", count)
				}
			}
			assertSubagentStampParity(t, s, "F", "after the delete", true)
		})
	}
}

// TestForkRevertSettlesNoCopy: a revert that hides an inherited row
// below the fork's cut (a promoted message's revert leaves a queued
// message after it) keeps nothing on the copy above the row, so the
// source's delete leaves the fork none of the source's rows.
func TestForkRevertSettlesNoCopy(t *testing.T) {
	s := newTestStore(t)
	promoted, err := itemmeta.MarkPromotedAtInterrupt("")
	if err != nil {
		t.Fatal(err)
	}
	if promoted, err = itemmeta.MarkPromotedEchoBoundary(promoted, 0); err != nil {
		t.Fatal(err)
	}
	seedForkSource(t, s, "S", []Item{
		{ID: "A", TurnIndex: 1, ItemIndex: 0, Kind: "tool_call", ToolName: "Agent", Role: "assistant", Status: "completed", Summary: "Agent: A", Meta: "{}"},
		{ID: "A-c1", TurnIndex: 1, ItemIndex: 1, ParentID: "A", Kind: "tool_call", ToolName: "Bash", Role: "assistant", Status: "completed", Summary: "Bash: one", Meta: "{}"},
		{ID: "Q", TurnIndex: 1, ItemIndex: 2, Kind: "user_text", Role: "user", Status: "completed", Summary: "queued", Meta: "{}"},
		{ID: "P", TurnIndex: 1, ItemIndex: 3, Kind: "user_text", Role: "user", Status: "completed", Summary: "promoted", Meta: promoted},
	})
	mustPointerFork(t, s, "S", "F", ForkCut{})
	if err := s.materializeForkRows("F", []string{"A"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DeleteConversationFromItem("F", "P"); err != nil {
		t.Fatal(err)
	}
	requireIDs(t, "F rows after the revert", itemIDs(forkRows(t, s, "F")), []string{"A", "Q"})
	if n := forkCopyRecords(t, s, "F"); n != 1 {
		t.Fatalf("F records %d copies after the revert, want A", n)
	}
	if err := s.DeleteThread("S"); err != nil {
		t.Fatal(err)
	}
	requireIDs(t, "F rows", itemIDs(forkRows(t, s, "F")), nil)
	assertSubagentStampParity(t, s, "F", "after the delete", true)
}

// TestForkRollbackKeepsCopiesOfANearerAncestor: a source's delete rolls
// back only the copies of rows the fork stops reading. G's copies of the
// rows it read from F stay recorded, so G keeps showing A as it read it,
// though F renamed A after G copied it, and a later materialization
// completes with them.
func TestForkRollbackKeepsCopiesOfANearerAncestor(t *testing.T) {
	s := newTestStore(t)
	seedMaterializeDeleteFixture(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.materializeForkHistory(ctx, "G", cancel); !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped materialization = %v", err)
	}
	if n := forkCopyRecords(t, s, "G"); n != 500 {
		t.Fatalf("G records %d copies, want a batch", n)
	}
	renamed := "Agent: A renamed in F"
	if _, err := s.UpdateItemFields("F", "A", ItemPartialUpdate{Summary: &renamed}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteThread("S"); err != nil {
		t.Fatal(err)
	}
	records, err := queryIDs(s.db, `SELECT item_id FROM thread_fork_copied WHERE thread_id = 'G' ORDER BY item_id`)
	if err != nil {
		t.Fatal(err)
	}
	requireIDs(t, "G records", records, []string{"A", "f-own", forkDividerID("F")})
	requireIDs(t, "G rows", itemIDs(forkRows(t, s, "G")), []string{"A", forkDividerID("F"), "f-own"})
	shown := func() string {
		t.Helper()
		item, found, err := s.GetThreadItem("G", "A")
		if err != nil || !found {
			t.Fatalf("G's A: found %v, err %v", found, err)
		}
		return item.Summary
	}
	if got := shown(); got != "Agent: A in the fork" {
		t.Fatalf("G shows A as %q", got)
	}
	assertSubagentStampParity(t, s, "G", "after the delete", true)

	if err := s.MaterializeForkHistory(context.Background(), "G"); err != nil {
		t.Fatal(err)
	}
	requireIDs(t, "G lineage", forkLineage(t, s, "G"), nil)
	if n := forkCopyRecords(t, s, "G"); n != 0 {
		t.Fatalf("G records %d copies after its materialization", n)
	}
	if got := shown(); got != "Agent: A in the fork" {
		t.Fatalf("materialized G shows A as %q", got)
	}
	assertSubagentStampParity(t, s, "G", "after the materialization", true)
}

// TestForkCopyRecordQueriesStayIndexed: every item write probes for
// records (forkHoldsCopiesSQL), and the settle, the materialization check
// and the rollback read them, so each probes an index. The rollback chunk
// sorts only the fork's records of the deleted levels.
func TestForkCopyRecordQueriesStayIndexed(t *testing.T) {
	s := newTestStore(t)
	for _, probe := range []struct {
		name, query string
		args        []any
		want        []string
		sorts       bool
	}{
		{name: "holds", query: forkHoldsCopiesSQL, args: []any{"F"},
			want: []string{"SEARCH thread_fork_copied USING ", "(thread_id=?)"}},
		{name: "settle", query: settleForkCopiesSQL, args: []any{"F", `["a"]`},
			want: []string{"SEARCH thread_fork_copied USING PRIMARY KEY (thread_id=? AND item_id=?)"}},
		{name: "rollback chunk", query: forkCopiesToRollBackSQL, args: []any{"F", "S", 500}, sorts: true,
			want: []string{
				"SEARCH gone USING ",
				"SEARCH l USING PRIMARY KEY (thread_id=? AND depth>?)",
				"SEARCH c USING COVERING INDEX idx_thread_fork_copied_source (thread_id=? AND source_id=?)",
				"SEARCH i USING INDEX sqlite_autoindex_items_1 (thread_id=? AND id=?)",
			}},
		{name: "forks with copies", query: forksWithCopiesSQL, args: []any{"S"},
			want: []string{"idx_thread_fork_lineage_ancestor", "idx_thread_fork_copied_source"}},
		{name: "deleting ancestor", query: deletingForkAncestorSQL, args: []any{"F"},
			want: []string{"SEARCH t USING INDEX sqlite_autoindex_threads_1 (id=?)"}},
	} {
		plan := explainPlan(t, s, probe.query, probe.args...)
		text := planText(plan)
		for _, row := range plan {
			scan := strings.HasPrefix(row.detail, "SCAN ") && !strings.Contains(row.detail, "json_each") && row.detail != "SCAN CONSTANT ROW"
			if scan || !probe.sorts && strings.Contains(row.detail, "TEMP B-TREE FOR ORDER BY") {
				t.Errorf("%s: %s\n%s", probe.name, row.detail, text)
			}
		}
		for _, want := range probe.want {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not probe %s\n%s", probe.name, want, text)
			}
		}
	}
}
