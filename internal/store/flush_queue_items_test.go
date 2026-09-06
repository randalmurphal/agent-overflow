package store

import (
	"encoding/json"
	"strings"
	"testing"
)

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

	if err := s.DeleteFlushQueueItemsForThread("thread-a"); err != nil {
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
