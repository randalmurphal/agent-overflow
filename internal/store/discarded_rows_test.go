package store

import (
	"context"
	"strconv"
	"testing"
)

// seedAbandonedResponse seeds turn 0 of thread t: the user message, two
// text blocks of response msg_1, the record naming the second block, and
// the continuation under msg_2. It returns the ids in timeline order.
func seedAbandonedResponse(t *testing.T, s *Store, withRecord bool) []string {
	t.Helper()
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	rows := []Item{
		{ID: "u0", Kind: "user_text", Role: "user"},
		{ID: "a1", Kind: "assistant_text", Role: "assistant", Meta: `{"api_message_id":"msg_1","block_index":0}`},
		{ID: "a2", Kind: "assistant_text", Role: "assistant", Meta: `{"api_message_id":"msg_1","block_index":1}`},
		abandonedBlocksRecord(0, "", "msg_1", 1, "a2"),
		{ID: "a4", Kind: "assistant_text", Role: "assistant", Meta: `{"api_message_id":"msg_2","block_index":0}`},
	}
	var ids []string
	for i, item := range rows {
		if !withRecord && isAbandonedBlocksRecord(item) {
			continue
		}
		item.ThreadID, item.TurnIndex, item.ItemIndex = "t", 0, i
		item.Status, item.Summary, item.CreatedAt = "completed", item.ID, int64(i+1)
		if err := insertCarded(s, item); err != nil {
			t.Fatalf("insert %s: %v", item.ID, err)
		}
		ids = append(ids, item.ID)
	}
	return ids
}

func abandonedBlocksRecord(turnIndex int, parentID, apiMessageID string, from int, itemIDs ...string) Item {
	list, _ := jsonList(itemIDs)
	return Item{
		ID:       AbandonedBlocksRecordID(turnIndex, parentID, apiMessageID),
		Kind:     "notification",
		Role:     "system",
		ToolName: AbandonedBlocksNotificationKind,
		ParentID: parentID,
		Meta:     `{"kind":"abandoned_blocks","api_message_id":"` + apiMessageID + `","from_block_index":` + strconv.Itoa(from) + `,"item_ids":` + list + `}`,
	}
}

func discardedByOf(items []Item) map[string]string {
	out := make(map[string]string, len(items))
	for _, item := range items {
		out[item.ID] = item.DiscardedBy
	}
	return out
}

// A page that holds a discarded row without its record still serves the
// row marked: the record is read by the id the row's own coordinates
// imply, not found among the page's rows. The earlier block of the same
// response, which the record does not name, stays unmarked.
func TestDiscardedRowsReadMarkedWithoutTheRecordOnThePage(t *testing.T) {
	s := newTestStore(t)
	seedAbandonedResponse(t, s, true)
	recordID := AbandonedBlocksRecordID(0, "", "msg_1")

	page, err := s.ListThreadSliceAround(context.Background(), "t", "u0", 3, 10, TimelineSelection{})
	if err != nil {
		t.Fatalf("slice: %v", err)
	}
	marks := discardedByOf(page.Items)
	if _, holdsRecord := marks[recordID]; holdsRecord || len(page.Items) != 3 {
		t.Fatalf("slice should end before the record, got %v", marks)
	}
	if marks["a2"] != recordID {
		t.Errorf("a2 discardedBy = %q, want the record %q", marks["a2"], recordID)
	}
	if marks["a1"] != "" {
		t.Errorf("a1 (block 0, kept) discardedBy = %q, want unmarked", marks["a1"])
	}

	tail, err := s.ListThreadSliceAround(context.Background(), "t", "", 10, 10, TimelineSelection{})
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	marks = discardedByOf(tail.Items)
	if marks["a2"] != recordID || marks["a1"] != "" || marks["a4"] != "" || marks[recordID] != "" {
		t.Errorf("tail marks = %v", marks)
	}

	wire, err := s.ListWireItems("t", []string{"a2", "a4"})
	if err != nil {
		t.Fatalf("wire: %v", err)
	}
	marks = discardedByOf(wire)
	if marks["a2"] != recordID || marks["a4"] != "" {
		t.Errorf("wire marks = %v", marks)
	}
}

// Without a record no response-stamped row is marked, and the read costs
// no record parse: the stamp alone admits a row to the lookup.
func TestDiscardedRowsStayUnmarkedWithoutARecord(t *testing.T) {
	s := newTestStore(t)
	seedAbandonedResponse(t, s, false)
	page, err := s.ListThreadSliceAround(context.Background(), "t", "", 10, 10, TimelineSelection{})
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	for id, by := range discardedByOf(page.Items) {
		if by != "" {
			t.Errorf("%s discardedBy = %q without a record", id, by)
		}
	}
}

// Writing the record moves the revision of every row it names and only
// those, so a client holding the rows at their old revision cannot prove
// its window fresh (thread-replica-sync.md §3.1).
func TestAbandonmentRecordStampsTheRowsItNames(t *testing.T) {
	s := newTestStore(t)
	seedAbandonedResponse(t, s, false)
	revOf := func(id string) int64 {
		item, found, err := s.GetThreadItem("t", id)
		if err != nil || !found {
			t.Fatalf("read %s: found=%v err=%v", id, found, err)
		}
		return item.Rev
	}
	before := map[string]int64{"a1": revOf("a1"), "a2": revOf("a2"), "a4": revOf("a4")}
	stampBefore, _, err := s.ThreadHistoryStamp("t")
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}

	record := abandonedBlocksRecord(0, "", "msg_1", 1, "a2")
	record.ThreadID, record.Status, record.Summary, record.CreatedAt, record.UpdatedAt = "t", "completed", "cut short", 10, 10
	if _, err := s.UpsertItem(record, nil); err != nil {
		t.Fatalf("upsert record: %v", err)
	}

	if got := revOf("a2"); got <= before["a2"] {
		t.Errorf("a2 rev %d did not move past %d when its record was written", got, before["a2"])
	}
	for _, id := range []string{"a1", "a4"} {
		if got := revOf(id); got != before[id] {
			t.Errorf("%s rev moved %d -> %d; the record does not name it", id, before[id], got)
		}
	}
	stampAfter, _, err := s.ThreadHistoryStamp("t")
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if stampAfter.Rev <= stampBefore.Rev {
		t.Errorf("thread history_rev %d did not advance past %d", stampAfter.Rev, stampBefore.Rev)
	}
	// The stamped row now reads marked at its new revision.
	wire, err := s.ListWireItems("t", []string{"a2"})
	if err != nil || len(wire) != 1 {
		t.Fatalf("wire: rows=%d err=%v", len(wire), err)
	}
	if wire[0].DiscardedBy != record.ID || wire[0].Rev != revOf("a2") {
		t.Errorf("a2 served as discardedBy=%q rev=%d, want %q at %d", wire[0].DiscardedBy, wire[0].Rev, record.ID, revOf("a2"))
	}
}

// A response-stamped row is admitted to the decorated set, and reads
// decorated only once its record exists: before, its write read-back is
// the page read; after, the emitter must read it again.
func TestProbeWireItemReadsAResponseStampedRowOnceItsRecordExists(t *testing.T) {
	s := newTestStore(t)
	seedAbandonedResponse(t, s, false)
	rowOf := func(id string) Item {
		item, found, err := s.GetThreadItem("t", id)
		if err != nil || !found {
			t.Fatalf("read %s: found=%v err=%v", id, found, err)
		}
		return item
	}
	for _, id := range []string{"a1", "a2", "a4"} {
		if !ItemReadIsDecorated(rowOf(id)) {
			t.Errorf("%s: a response-stamped row must be admitted", id)
		}
		probe, err := s.ProbeWireItem(rowOf(id))
		if err != nil {
			t.Fatalf("probe %s: %v", id, err)
		}
		if probe.NeedsDecoration {
			t.Errorf("%s: needs decoration before any record exists", id)
		}
	}
	record := abandonedBlocksRecord(0, "", "msg_1", 1, "a2")
	record.ThreadID, record.Status, record.Summary, record.CreatedAt, record.UpdatedAt = "t", "completed", "cut short", 10, 10
	if _, err := s.UpsertItem(record, nil); err != nil {
		t.Fatalf("upsert record: %v", err)
	}
	for id, want := range map[string]bool{"a1": true, "a2": true, "a4": false} {
		probe, err := s.ProbeWireItem(rowOf(id))
		if err != nil {
			t.Fatalf("probe %s: %v", id, err)
		}
		if probe.NeedsDecoration != want {
			t.Errorf("%s: needs decoration = %v, want %v with the msg_1 record present", id, probe.NeedsDecoration, want)
		}
	}
}
