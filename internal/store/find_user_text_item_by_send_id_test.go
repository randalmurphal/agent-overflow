package store

import (
	"fmt"
	"strings"
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

// A reconnect may outlive more user messages than the old 64-row cutoff.
func TestFindUserTextItemBySendIDSurvivesNewerMessages(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-a")
	ids := make([]string, 129)
	for i := range ids {
		ids[i] = fmt.Sprintf("send-%d", i)
	}
	insertUserTexts(t, s, "t-a", ids...)
	for _, id := range []string{ids[0], ids[len(ids)-1]} {
		item, found, err := s.FindUserTextItemBySendID("t-a", id)
		if err != nil || !found || item.ID != "t-a-user-"+id {
			t.Fatalf("find %s: found=%v item=%q err=%v", id, found, item.ID, err)
		}
		if item.Meta != `{"sendId":"`+id+`"}` {
			t.Fatalf("meta lost: %q", item.Meta)
		}
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

	item, found, err := s.FindUserTextItemBySendID("t-b", "send-1")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if !found || item.ID != "t-b-user-send-1" {
		t.Fatalf("thread scope: found=%v id=%q", found, item.ID)
	}
}

// The guard's premise, pinned. `json_extract` RAISES on malformed JSON, so a
// single unreadable row would fail every send on the
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
	if _, found, err := s.FindUserTextItemBySendID("t-a", "send-absent"); err != nil || found {
		t.Fatalf("absent among keyless rows: found=%v err=%v", found, err)
	}
}

// An empty id is never a query. Every app-internal
// injector leaves the id unset, and matching those against each other would
// collapse unrelated messages into one.
func TestFindUserTextItemBySendIDRefusesAnEmptyQuestion(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-a")
	insertUserTexts(t, s, "t-a", "")

	if _, found, err := s.FindUserTextItemBySendID("t-a", ""); err != nil || found {
		t.Fatalf("empty send id: found=%v err=%v", found, err)
	}
}

func TestSendIdentityLookupUsesBothSparseIndexes(t *testing.T) {
	s := newTestStore(t)
	query, args := sendIdentityQuery("thread", "send-id")
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, index := range []string{"idx_items_send_id", "idx_import_history_items_send_id"} {
		if !strings.Contains(plan.String(), index) {
			t.Fatalf("missing %s in plan:\n%s", index, plan.String())
		}
	}
	if strings.Contains(plan.String(), "SCAN items") {
		t.Fatalf("scanned message history:\n%s", plan.String())
	}
}

func TestSendIdentityLookupHonorsImportedHistoryAndOverrides(t *testing.T) {
	s := newTestStore(t)
	const thread = "imported-send"
	newImportTargetThread(t, s, thread)
	err := s.ApplyImportBatch(thread, ImportBatch{
		Turns: []Turn{{TurnID: "import-turn", ThreadID: thread, TurnIndex: 0, StartedAt: 1}},
		Rows: []ImportRow{{Item: Item{ID: "import-user", TurnIndex: 0, ItemIndex: 0,
			Kind: "user_text", Role: "user", Status: "completed", Summary: "original",
			Meta: `{"sendId":"original-send"}`, CreatedAt: 1, UpdatedAt: 1}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if row, found, err := s.FindUserTextItemBySendID(thread, "original-send"); err != nil || !found || row.ID != "import-user" {
		t.Fatalf("imported identity: found=%v item=%q err=%v", found, row.ID, err)
	}
	meta := `{"sendId":"replacement-send"}`
	if err := s.UpdateItemFields(thread, "import-user", ItemPartialUpdate{Meta: &meta}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.FindUserTextItemBySendID(thread, "original-send"); err != nil || found {
		t.Fatalf("hidden imported identity matched: found=%v err=%v", found, err)
	}
	if _, found, err := s.FindUserTextItemBySendID(thread, "replacement-send"); err != nil || !found {
		t.Fatalf("local override identity missing: found=%v err=%v", found, err)
	}
}

func TestMigrationV89IndexesExistingSendIdentities(t *testing.T) {
	db := migrateThrough(t, 88)
	s := &Store{db: db}
	if _, err := s.CreateProject(Project{ID: defaultTestProjectID, Path: t.TempDir(), Name: "Existing project", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	mustCreateThread(t, s, "existing-send")
	insertUserTexts(t, s, "existing-send", "before-upgrade", "")
	if err := s.InsertFlushQueueItem(FlushQueueItem{ID: "before-queue", ThreadID: "existing-send", SendID: "queued-before-upgrade", Message: "Waiting"}); err != nil {
		t.Fatal(err)
	}
	if err := applyMigration(db, migrationByVersion(t, 89)); err != nil {
		t.Fatal(err)
	}
	if row, found, err := s.FindUserTextItemBySendID("existing-send", "before-upgrade"); err != nil || !found || row.ID != "existing-send-user-before-upgrade" {
		t.Fatalf("upgraded lookup: found=%v item=%q err=%v", found, row.ID, err)
	}
	if row, found, err := s.FindFlushQueueItemBySendID("existing-send", "queued-before-upgrade"); err != nil || !found || row.Message != "Waiting" {
		t.Fatalf("upgraded queued lookup: found=%v item=%q err=%v", found, row.ID, err)
	}
}
