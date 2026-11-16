package store

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// A restore writes the composer, deletes the queue rows and retires the
// unconfirmed rows with the review comments they claimed in one
// transaction: a failure in any part leaves all of them as they were.
func TestRestoreFlushQueueToDraftRetiresRowsInTheSameTransaction(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "restore")
	owner := ThreadReviewOwner("restore")
	if _, err := s.CreateDiffReviewComment(DiffReviewComment{
		ID: "c", ThreadID: "restore", Scope: string(DiffReviewScopeWorkspace), SourceKey: "diff",
		FilePath: "a.go", NewLine: 1, Side: "new", Body: "Rename it.", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimDiffReviewCommentsSent(owner, string(DiffReviewScopeWorkspace), "diff",
		[]DiffReviewCommentSnapshot{{ID: "c", Body: "Rename it."}},
		DiffReviewDelivery{SentAt: 2, TurnID: "restore:1", ThreadID: "restore", ItemID: "user:1:flush:1", Queued: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendItem(Item{
		ID: "user:1:flush:1", ThreadID: "restore", TurnIndex: 1, Kind: "user_text", Role: "user",
		Status: "completed", Summary: "unconfirmed", Meta: `{"pendingFlush":true}`, CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertFlushQueueItem(FlushQueueItem{ID: "queue", ThreadID: "restore", Message: "queued", EnqueuedAt: 1}); err != nil {
		t.Fatal(err)
	}
	expected, _, err := s.GetThreadDraft("restore")
	if err != nil {
		t.Fatal(err)
	}
	merged := expected
	merged.Content = "unconfirmed\n\nqueued"
	restore := func() (DraftWrite, []DiffReviewReturnedComments, error) {
		written, err := s.RestoreFlushQueueToDraft(QueueDraftRestore{
			Expected: expected, Merged: merged, QueueItemIDs: []string{"queue"}, RetireItemIDs: []string{"user:1:flush:1"},
		})
		return written.Draft, written.Returned, err
	}
	state := func() string {
		t.Helper()
		draft, _, err := s.GetThreadDraft("restore")
		if err != nil {
			t.Fatal(err)
		}
		_, row, err := s.GetThreadItem("restore", "user:1:flush:1")
		if err != nil {
			t.Fatal(err)
		}
		queued, err := s.ListFlushQueueItems("restore")
		if err != nil {
			t.Fatal(err)
		}
		comment, err := s.GetDiffReviewComment(owner, "c")
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("draft=%q row=%v queued=%d comment=%s", draft.Content, row, len(queued), comment.Status)
	}
	before := state()

	if _, err := s.db.Exec(`CREATE TRIGGER refuse_row_delete BEFORE DELETE ON items WHEN OLD.id = 'user:1:flush:1' BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := restore(); err == nil {
		t.Fatal("RestoreFlushQueueToDraft succeeded with its retire refused")
	}
	if after := state(); after != before {
		t.Fatalf("a failed restore wrote part of itself:\nbefore %s\nafter  %s", before, after)
	}
	if _, err := s.db.Exec(`DROP TRIGGER refuse_row_delete`); err != nil {
		t.Fatal(err)
	}

	// The thread was listed by its unconfirmed row and stays listed by the
	// draft, so the restore lists nothing.
	write, returned, err := restore()
	if err != nil || write != (DraftWrite{Changed: true}) {
		t.Fatalf("RestoreFlushQueueToDraft = %+v, %v; want changed only", write, err)
	}
	if got, want := state(), `draft="unconfirmed\n\nqueued" row=false queued=0 comment=draft`; got != want {
		t.Fatalf("after the restore %s, want %s", got, want)
	}
	if len(returned) != 1 || len(returned[0].Comments) != 1 || returned[0].Comments[0] != (DiffReviewCommentSnapshot{ID: "c", Body: "Rename it."}) {
		t.Fatalf("returned = %+v, want comment c", returned)
	}
}

// A restore or a retire never takes back a marked row that is history: one
// a later turn follows. The whole restore is refused, so neither the row nor
// its text moves, and the same restore of a residue row goes through.
func TestRestoreFlushQueueToDraftKeepsAMarkedHistoryRow(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "history")
	now := time.Now().UnixMilli()
	for _, item := range []Item{
		{ID: "user:9:flush:1", TurnIndex: 9, Kind: "user_text", Role: "user", Summary: "shown as sent", Meta: `{"pendingFlush":true}`},
		{ID: "user:10", TurnIndex: 10, Kind: "user_text", Role: "user", Summary: "a later turn"},
		{ID: "user:10:flush:1", TurnIndex: 10, Kind: "user_text", Role: "user", Summary: "residue", Meta: `{"pendingFlush":true}`},
	} {
		item.ThreadID, item.Status, item.CreatedAt, item.UpdatedAt = "history", "completed", now, now
		if _, err := s.AppendItem(item); err != nil {
			t.Fatal(err)
		}
	}
	expected, _, err := s.GetThreadDraft("history")
	if err != nil {
		t.Fatal(err)
	}
	merged := expected
	merged.Content = "shown as sent"
	if _, err := s.RestoreFlushQueueToDraft(QueueDraftRestore{Expected: expected, Merged: merged, RetireItemIDs: []string{"user:9:flush:1"}}); err == nil || !strings.Contains(err.Error(), "is history") {
		t.Fatalf("restore of a history row = %v, want refused", err)
	}
	if _, err := s.RetireUndeliveredUserRow("history", "user:9:flush:1"); err == nil || !strings.Contains(err.Error(), "is history") {
		t.Fatalf("retire of a history row = %v, want refused", err)
	}
	if _, found, err := s.GetThreadItem("history", "user:9:flush:1"); err != nil || !found {
		t.Fatalf("history row after a refused restore: found=%v err=%v", found, err)
	}
	if draft, _, err := s.GetThreadDraft("history"); err != nil || draft.Content != "" {
		t.Fatalf("draft after a refused restore = %q (%v), want empty", draft.Content, err)
	}
	merged.Content = "residue"
	if _, err := s.RestoreFlushQueueToDraft(QueueDraftRestore{Expected: expected, Merged: merged, RetireItemIDs: []string{"user:10:flush:1"}}); err != nil {
		t.Fatalf("restore of a residue row: %v", err)
	}
	if _, found, err := s.GetThreadItem("history", "user:10:flush:1"); err != nil || found {
		t.Fatalf("residue row after its restore: found=%v err=%v", found, err)
	}
}

func TestFindQueuedSendUsesItsThreadAndSparseIndex(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "queue-a")
	mustCreateThread(t, s, "queue-b")
	for _, row := range []FlushQueueItem{
		{ID: "a", ThreadID: "queue-a", SendID: "same-send", Message: "A", Payload: json.RawMessage(`{"attachmentIds":["image"]}`)},
		{ID: "b", ThreadID: "queue-b", SendID: "same-send", Message: "B"},
		{ID: "injector", ThreadID: "queue-a", Message: "No client identity"},
	} {
		if err := s.InsertFlushQueueItem(row); err != nil {
			t.Fatal(err)
		}
	}
	row, found, err := s.FindFlushQueueItemBySendID("queue-a", "same-send")
	if err != nil || !found || row.ID != "a" || row.Message != "A" || string(row.Payload) != `{"attachmentIds":["image"]}` {
		t.Fatalf("queued identity: %+v found=%v err=%v", row, found, err)
	}
	for _, id := range []string{"", "missing"} {
		if _, found, err := s.FindFlushQueueItemBySendID("queue-a", id); err != nil || found {
			t.Fatalf("unexpected match for %q: %v %v", id, found, err)
		}
	}
	if err := s.DeleteFlushQueueItem("a"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.FindFlushQueueItemBySendID("queue-a", "same-send"); err != nil || found {
		t.Fatalf("another thread's identity matched: %v %v", found, err)
	}
	var plan string
	var id, parent, unused int
	if err := s.db.QueryRow("EXPLAIN QUERY PLAN "+findQueuedSendSQL, "queue-a", "same-send").Scan(&id, &parent, &unused, &plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "idx_flush_queue_send_id") {
		t.Fatalf("queued identity lookup did not use its index: %s", plan)
	}
}

func TestFlushQueueItemsRoundTripInQueueOrder(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "thread-a")

	rows := []FlushQueueItem{
		{ID: "queue:2", ThreadID: "thread-a", SendID: "send-2", Message: "second", EnqueuedAt: 200},
		{ID: "queue:1", ThreadID: "thread-a", SendID: "send-1", Message: "first",
			Payload: json.RawMessage(`{"attachmentIds":["att-1"]}`), EnqueuedAt: 100},
		// Same millisecond as the row before it: insertion order is the
		// tiebreak, because two messages queued inside one millisecond still
		// have a first one and the queue is a FIFO.
		{ID: "queue:3", ThreadID: "thread-a", Message: "third", EnqueuedAt: 200},
	}
	for _, row := range rows {
		if err := s.InsertFlushQueueItem(row); err != nil {
			t.Fatalf("insert %s: %v", row.ID, err)
		}
	}

	got, err := s.ListFlushQueueItems("thread-a")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("rows: got %d, want 3", len(got))
	}
	if got[0].ID != "queue:1" || got[1].ID != "queue:2" || got[2].ID != "queue:3" {
		t.Fatalf("order: got %s, %s, %s", got[0].ID, got[1].ID, got[2].ID)
	}
	if got[0].Message != "first" || got[0].SendID != "send-1" {
		t.Fatalf("row 1: got %+v", got[0])
	}
	if string(got[0].Payload) != `{"attachmentIds":["att-1"]}` {
		t.Fatalf("payload: got %q", string(got[0].Payload))
	}
	// A row with no payload reads back as no payload, not as the four bytes
	// "null": the boot sweep tells them apart to decide what it can restore.
	if got[2].Payload != nil {
		t.Fatalf("row 3 payload: got %q, want nil", string(got[2].Payload))
	}
	if got[2].SendID != "" {
		t.Fatalf("row 3 send id: got %q, want empty", got[2].SendID)
	}
}

func TestFlushQueueItemsAreScopedToTheirThread(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "thread-a")
	mustCreateThread(t, s, "thread-b")

	for _, row := range []FlushQueueItem{
		{ID: "queue:a1", ThreadID: "thread-a", Message: "a1", EnqueuedAt: 1},
		{ID: "queue:a2", ThreadID: "thread-a", Message: "a2", EnqueuedAt: 2},
		{ID: "queue:b1", ThreadID: "thread-b", Message: "b1", EnqueuedAt: 1},
	} {
		if err := s.InsertFlushQueueItem(row); err != nil {
			t.Fatalf("insert %s: %v", row.ID, err)
		}
	}

	threads, err := s.ListThreadsWithFlushQueueItems()
	if err != nil {
		t.Fatalf("list threads: %v", err)
	}
	if len(threads) != 2 || threads[0] != "thread-a" || threads[1] != "thread-b" {
		t.Fatalf("threads: got %v", threads)
	}

	// One id, one row: the other thread's queue is untouched.
	if err := s.DeleteFlushQueueItem("queue:a1"); err != nil {
		t.Fatalf("delete one: %v", err)
	}
	// Already gone is success — the two durable endpoints can settle the
	// same item and neither owes the other a check.
	if err := s.DeleteFlushQueueItem("queue:a1"); err != nil {
		t.Fatalf("delete again: %v", err)
	}
	if err := s.DeleteFlushQueueItem(""); err != nil {
		t.Fatalf("delete empty id: %v", err)
	}

	if err := s.DeleteFlushQueueItemsForThread("thread-a", ""); err != nil {
		t.Fatalf("delete thread: %v", err)
	}
	remaining, err := s.ListFlushQueueItems("thread-a")
	if err != nil {
		t.Fatalf("list a: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("thread-a rows: got %d, want 0", len(remaining))
	}
	other, err := s.ListFlushQueueItems("thread-b")
	if err != nil {
		t.Fatalf("list b: %v", err)
	}
	if len(other) != 1 || other[0].ID != "queue:b1" {
		t.Fatalf("thread-b rows: got %+v", other)
	}
}

// The presence read names exactly the asked rows that stand in the asked
// thread's queue: not a deleted one, not another thread's, not one never
// asked about.
func TestFlushQueueItemsPresentNamesTheThreadsStandingRows(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "thread-a")
	mustCreateThread(t, s, "thread-b")
	for _, row := range []FlushQueueItem{
		{ID: "queue:a1", ThreadID: "thread-a", Message: "a1", EnqueuedAt: 1},
		{ID: "queue:a2", ThreadID: "thread-a", Message: "a2", EnqueuedAt: 2},
		{ID: "queue:a3", ThreadID: "thread-a", Message: "a3", EnqueuedAt: 3},
		{ID: "queue:b1", ThreadID: "thread-b", Message: "b1", EnqueuedAt: 1},
	} {
		if err := s.InsertFlushQueueItem(row); err != nil {
			t.Fatalf("insert %s: %v", row.ID, err)
		}
	}
	if err := s.DeleteFlushQueueItem("queue:a2"); err != nil {
		t.Fatal(err)
	}
	present, err := s.FlushQueueItemsPresent("thread-a", []string{"queue:a1", "queue:a2", "queue:b1", "queue:none"})
	if err != nil {
		t.Fatal(err)
	}
	if len(present) != 1 || !present["queue:a1"] {
		t.Fatalf("present = %v, want queue:a1 alone", present)
	}
	if present, err := s.FlushQueueItemsPresent("thread-a", nil); err != nil || len(present) != 0 {
		t.Fatalf("present for no ids = %v, %v; want none", present, err)
	}
}

// Deleting a thread takes its queue with it. Without the cascade a deleted
// thread's rows would outlive every path that could ever clear them, and the
// boot sweep would try to write a draft for a thread that no longer exists.
func TestFlushQueueItemsCascadeOnThreadDelete(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "thread-a")
	if err := s.InsertFlushQueueItem(FlushQueueItem{
		ID: "queue:a1", ThreadID: "thread-a", Message: "a1", EnqueuedAt: 1,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := s.DeleteThread("thread-a"); err != nil {
		t.Fatalf("delete thread: %v", err)
	}

	threads, err := s.ListThreadsWithFlushQueueItems()
	if err != nil {
		t.Fatalf("list threads: %v", err)
	}
	if len(threads) != 0 {
		t.Fatalf("threads: got %v, want none", threads)
	}
}

func TestFlushQueueItemInsertRefusesAnUnidentifiedRow(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "thread-a")

	if err := s.InsertFlushQueueItem(FlushQueueItem{ThreadID: "thread-a", Message: "x"}); err == nil {
		t.Fatal("expected an error for a row with no id")
	}
	if err := s.InsertFlushQueueItem(FlushQueueItem{ID: "queue:x", Message: "x"}); err == nil {
		t.Fatal("expected an error for a row with no thread")
	}
}

// A wholesale drop keeps the rows it names by id, beside its prefix, so
// their owners can take them back; a row of the same kind it did not name
// goes with the rest.
func TestDropKeepsTheRowsItNames(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "thread-a")
	mustCreateThread(t, s, "thread-b")
	for _, row := range []FlushQueueItem{
		{ID: "queue:1", ThreadID: "thread-a", SendID: "person-1", Message: "mine", EnqueuedAt: 1},
		{ID: "queue:2", ThreadID: "thread-a", SendID: "agent-send:x", Message: "outside", EnqueuedAt: 2},
		{ID: "queue:3", ThreadID: "thread-a", SendID: "thread-wake:t1", Message: "answer", EnqueuedAt: 3},
		{ID: "queue:4", ThreadID: "thread-a", SendID: "remote-completion:c:r", Message: "done", EnqueuedAt: 4},
		{ID: "queue:5", ThreadID: "thread-a", SendID: "thread-wake:t2", Message: "later answer", EnqueuedAt: 5},
		{ID: "queue:6", ThreadID: "thread-b", SendID: "person-2", Message: "other thread", EnqueuedAt: 6},
	} {
		if err := s.InsertFlushQueueItem(row); err != nil {
			t.Fatalf("insert %s: %v", row.ID, err)
		}
	}
	if err := s.DeleteFlushQueueItemsForThread("thread-a", "agent-send:", "queue:3", "queue:4", "queue:6"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	var kept []string
	for _, thread := range []string{"thread-a", "thread-b"} {
		rows, err := s.ListFlushQueueItems(thread)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			kept = append(kept, row.ID)
		}
	}
	if want := []string{"queue:2", "queue:3", "queue:4", "queue:6"}; !slices.Equal(kept, want) {
		t.Fatalf("kept %q, want %q", kept, want)
	}
}

// A wholesale drop can keep the rows under one send id prefix for their
// owner to remove, and of two removals of one row only one finds it.
func TestDropKeepsAPrefixAndRemoveReportsWhoRemoved(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "thread-a")
	for _, row := range []FlushQueueItem{
		{ID: "queue:1", ThreadID: "thread-a", SendID: "person-1", Message: "mine", EnqueuedAt: 1},
		{ID: "queue:2", ThreadID: "thread-a", SendID: "agent-send:x", Message: "outside", EnqueuedAt: 2},
		{ID: "queue:3", ThreadID: "thread-a", Message: "no id", EnqueuedAt: 3},
		{ID: "queue:4", ThreadID: "thread-a", SendID: "agent-wake:w:1", Message: "wake", EnqueuedAt: 4},
	} {
		if err := s.InsertFlushQueueItem(row); err != nil {
			t.Fatalf("insert %s: %v", row.ID, err)
		}
	}
	if err := s.DeleteFlushQueueItemsForThread("thread-a", "agent-send:"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	rows, err := s.ListFlushQueueItems("thread-a")
	if err != nil || len(rows) != 1 || rows[0].ID != "queue:2" {
		t.Fatalf("kept %+v %v, want only the agent-send row", rows, err)
	}
	if removed, err := s.RemoveFlushQueueItem("queue:2"); err != nil || !removed {
		t.Fatalf("first remove: %v %v", removed, err)
	}
	if removed, err := s.RemoveFlushQueueItem("queue:2"); err != nil || removed {
		t.Fatalf("second remove: %v %v", removed, err)
	}
}

// A requeue keeps a row that is still there and writes one that is gone.
func TestEnsureFlushQueueItemKeepsAnExistingRow(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "thread-a")
	if err := s.InsertFlushQueueItem(FlushQueueItem{ID: "queue:1", ThreadID: "thread-a", Message: "original", EnqueuedAt: 1}); err != nil {
		t.Fatal(err)
	}
	for _, item := range []FlushQueueItem{
		{ID: "queue:1", ThreadID: "thread-a", Message: "rewritten", EnqueuedAt: 9},
		{ID: "queue:2", ThreadID: "thread-a", Message: "requeued", EnqueuedAt: 2},
	} {
		if err := s.EnsureFlushQueueItem(item); err != nil {
			t.Fatalf("EnsureFlushQueueItem(%s): %v", item.ID, err)
		}
	}
	rows, err := s.ListFlushQueueItems("thread-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Message != "original" || rows[1].Message != "requeued" {
		t.Fatalf("rows = %+v, want the original row kept and the requeued one added", rows)
	}
}

// The boot restore reads exactly the main-timeline user rows still marked
// pendingFlush that are residue: in their thread's last turn and written by
// that thread. A marked row a later turn follows, or one copied in from
// another thread, is history and is not listed. The scan goes through the
// sparse user-row index.
func TestListUnconfirmedUserRows(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"unconfirmed-a", "unconfirmed-b", "history-row", "history-turn", "copied"} {
		mustCreateThread(t, s, id)
	}
	now := time.Now().UnixMilli()
	for _, item := range []Item{
		{ID: "user:2:flush:2", ThreadID: "unconfirmed-a", TurnIndex: 2, Kind: "user_text", Role: "user", Meta: `{"pendingFlush":true}`},
		{ID: "user:2:flush:1", ThreadID: "unconfirmed-a", TurnIndex: 2, Kind: "user_text", Role: "user", Meta: `{"sendId":"s","pendingFlush":true}`},
		{ID: "assistant:2", ThreadID: "unconfirmed-a", TurnIndex: 2, Kind: "assistant_text", Role: "assistant"},
		{ID: "user:1:confirmed", ThreadID: "unconfirmed-a", TurnIndex: 1, Kind: "user_text", Role: "user", Meta: `{"provider_item_id":"u"}`},
		{ID: "user:1:false", ThreadID: "unconfirmed-a", TurnIndex: 1, Kind: "user_text", Role: "user", Meta: `{"pendingFlush":false}`},
		{ID: "assistant:1", ThreadID: "unconfirmed-a", TurnIndex: 1, Kind: "assistant_text", Role: "assistant", Meta: `{"pendingFlush":true}`},
		{ID: "user:3:flush:1", ThreadID: "unconfirmed-b", TurnIndex: 3, Kind: "user_text", Role: "user", Meta: `{"pendingFlush":true}`},
		{ID: "user:9:flush:1", ThreadID: "history-row", TurnIndex: 9, Kind: "user_text", Role: "user", Meta: `{"pendingFlush":true}`},
		{ID: "user:10", ThreadID: "history-row", TurnIndex: 10, Kind: "user_text", Role: "user"},
		{ID: "user:3:flush:1", ThreadID: "history-turn", TurnIndex: 3, Kind: "user_text", Role: "user", Meta: `{"pendingFlush":true}`},
		{ID: "user:4:flush:1", ThreadID: "copied", TurnIndex: 4, Kind: "user_text", Role: "user", Meta: `{"pendingFlush":true}`, CreatedAt: 1},
	} {
		item.Status = "completed"
		if item.CreatedAt == 0 {
			item.CreatedAt = now
		}
		if _, err := s.AppendItem(item); err != nil {
			t.Fatalf("AppendItem(%s): %v", item.ID, err)
		}
	}
	// A later turn with no main-timeline row yet is a later session too.
	if err := s.InsertTurn(Turn{TurnID: "turn-4", ThreadID: "history-turn", TurnIndex: 4, StartedAt: now}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	rows, err := s.ListUnconfirmedUserRows()
	if err != nil {
		t.Fatalf("ListUnconfirmedUserRows: %v", err)
	}
	want := []UnconfirmedUserRow{
		{ThreadID: "unconfirmed-a", ItemID: "user:2:flush:2"},
		{ThreadID: "unconfirmed-a", ItemID: "user:2:flush:1"},
		{ThreadID: "unconfirmed-b", ItemID: "user:3:flush:1"},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v, want %+v", rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("rows = %+v, want %+v", rows, want)
		}
	}
	plan, err := s.db.Query("EXPLAIN QUERY PLAN " + listUnconfirmedUserRowsSQL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = plan.Close() }()
	var steps []string
	for plan.Next() {
		var id, parent, unused int
		var detail string
		if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		steps = append(steps, detail)
	}
	if !strings.Contains(strings.Join(steps, "\n"), "idx_items_user_text") {
		t.Fatalf("the unconfirmed-row scan does not use the user-row index:\n%s", strings.Join(steps, "\n"))
	}
}
