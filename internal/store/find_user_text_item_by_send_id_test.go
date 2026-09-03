package store

import (
	"testing"
	"time"
)

// insertUserTexts writes one user message per (turn, sendId) pair, newest
// last, with an assistant row between each so the filter has something to
// exclude.
func insertUserTexts(t *testing.T, s *Store, threadID string, sendIDs ...string) {
	t.Helper()
	now := time.Now().UnixMilli()
	for i, sendID := range sendIDs {
		meta := ""
		if sendID != "" {
			meta = `{"sendId":"` + sendID + `"}`
		}
		rows := []Item{
			{ID: threadID + "-user-" + sendID, ThreadID: threadID, TurnIndex: i, ItemIndex: 0,
				Kind: "user_text", Role: "user", Summary: "msg", Meta: meta},
			{ID: threadID + "-answer-" + sendID, ThreadID: threadID, TurnIndex: i, ItemIndex: 1,
				Kind: "assistant_text", Role: "assistant", Summary: "answer"},
		}
		for _, row := range rows {
			row.CreatedAt, row.UpdatedAt = now, now
			if err := s.InsertItem(row); err != nil {
				t.Fatalf("insert %s: %v", row.ID, err)
			}
		}
	}
}

// The window is what makes the lookup cheap, so its edge is the behaviour
// worth pinning: inside it a repeat is recognised, past it the send is new.
func TestFindUserTextItemBySendIDSeesOnlyTheNewestWindow(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-a")
	insertUserTexts(t, s, "t-a", "send-old", "send-mid", "send-new")

	item, found, err := s.FindUserTextItemBySendID("t-a", "send-new", 2)
	if err != nil {
		t.Fatalf("find newest: %v", err)
	}
	if !found || item.ID != "t-a-user-send-new" {
		t.Fatalf("newest: found=%v id=%q", found, item.ID)
	}
	// The id rides in `meta`, so it has to survive hydration too: a read that
	// dropped it would leave the caller unable to say what it matched.
	if item.Meta != `{"sendId":"send-new"}` {
		t.Fatalf("meta: got %q", item.Meta)
	}

	// One older row is still inside a window of two, counting user messages
	// only — the assistant rows between them are not sends.
	if _, found, err := s.FindUserTextItemBySendID("t-a", "send-mid", 2); err != nil || !found {
		t.Fatalf("mid: found=%v err=%v", found, err)
	}

	// Past the window the answer is "not found", which makes the send new.
	// A retry follows its own failed frame by seconds; nothing this old can
	// be one.
	if _, found, err := s.FindUserTextItemBySendID("t-a", "send-old", 2); err != nil || found {
		t.Fatalf("old: found=%v err=%v", found, err)
	}
	if _, found, err := s.FindUserTextItemBySendID("t-a", "send-old", 64); err != nil || !found {
		t.Fatalf("old within a wider window: found=%v err=%v", found, err)
	}
}

// One thread's send id says nothing about another's: the ids are minted per
// send by a client, and two threads are two conversations.
func TestFindUserTextItemBySendIDStaysOnItsThread(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-a")
	mustCreateThread(t, s, "t-b")
	insertUserTexts(t, s, "t-a", "send-1")
	insertUserTexts(t, s, "t-b", "send-1")

	item, found, err := s.FindUserTextItemBySendID("t-b", "send-1", 64)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if !found || item.ID != "t-b-user-send-1" {
		t.Fatalf("thread scope: found=%v id=%q", found, item.ID)
	}
}

// The guard's premise, pinned. `json_extract` RAISES on malformed JSON, so a
// single unreadable row inside the window would fail every send on the
// thread. What stops that is not the lookup but the WRITE: both physical arms
// carry an expression index over `json_extract(meta, '$.task_id')`, which
// refuses malformed JSON at insert time, and `items` coalesces an empty meta
// to `{}`. The `json_valid` guard in the lookup is the backstop for the day
// one of those indexes is dropped — and this test is what says so out loud,
// because a backstop nobody can reach is otherwise indistinguishable from
// dead code.
func TestBothTimelineArmsRefuseMetaTheLookupCouldNotRead(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-a")
	now := time.Now().UnixMilli()

	for i, meta := range []string{"not json at all", "{"} {
		err := s.InsertItem(Item{
			ID: "local-broken", ThreadID: "t-a", TurnIndex: i, ItemIndex: 0,
			Kind: "user_text", Role: "user", Summary: "broken", Meta: meta,
			CreatedAt: now, UpdatedAt: now,
		})
		if err == nil {
			t.Fatalf("local arm accepted meta %q; the lookup's json_valid guard is now load-bearing", meta)
		}
		_, err = s.db.Exec(`INSERT INTO import_history_chunks
			(id, item_count, min_turn_index, max_turn_index) VALUES ('chunk-broken', 1, 9, 9)`)
		if err != nil && i == 0 {
			t.Fatalf("seed chunk: %v", err)
		}
		if _, err := s.db.Exec(`INSERT INTO import_history_items (
			chunk_id, id, turn_index, item_index, kind, role, status, summary,
			meta, created_at, updated_at
		) VALUES ('chunk-broken', 'imported-broken', 9, 0, 'user_text', 'user', 'completed',
			'broken', ?, 1, 1)`, meta); err == nil {
			t.Fatalf("imported arm accepted meta %q; the lookup's json_valid guard is now load-bearing", meta)
		}
	}

	// The shapes that ARE reachable read as "no send id" rather than as an
	// error: an empty meta lands as `{}`, and a row can hold valid JSON with
	// no such key at all.
	for i, meta := range []string{"", "{}", "null", `{"attachments":[]}`} {
		if err := s.InsertItem(Item{
			ID: "readable-" + string(rune('a'+i)), ThreadID: "t-a", TurnIndex: i, ItemIndex: 1,
			Kind: "user_text", Role: "user", Summary: "readable", Meta: meta,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("insert readable meta %q: %v", meta, err)
		}
	}
	if _, found, err := s.FindUserTextItemBySendID("t-a", "send-absent", 64); err != nil || found {
		t.Fatalf("absent among keyless rows: found=%v err=%v", found, err)
	}
}

// Neither an empty id nor an empty window is a query. Every app-internal
// injector leaves the id unset, and matching those against each other would
// collapse unrelated messages into one.
func TestFindUserTextItemBySendIDRefusesAnEmptyQuestion(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-a")
	insertUserTexts(t, s, "t-a", "")

	if _, found, err := s.FindUserTextItemBySendID("t-a", "", 64); err != nil || found {
		t.Fatalf("empty send id: found=%v err=%v", found, err)
	}
	for _, window := range []int{0, -1} {
		if _, found, err := s.FindUserTextItemBySendID("t-a", "send-1", window); err != nil || found {
			t.Fatalf("window %d: found=%v err=%v", window, found, err)
		}
	}
}
