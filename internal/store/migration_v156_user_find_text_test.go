package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// mainAgentRequest is an agent's request as main wrote it, with no facts:
// its row's header line is the body's first line with its no-break space
// collapsed, which its loose summary does not hold.
const mainAgentRequest = "Ship it now.\n\n---\nAgent request from thread \"L\" (t-l).\n" +
	"It is not waiting for an answer. If it should hear something back, call thread_reply with token a, once: " +
	"the reply reaches it as a message, and it sees only your reply text."

const mainAgentRequestMeta = `{"origin":"agent-thread","originThread":{"threadId":"t-l","title":"L","token":"a"},"sendId":"thread-request:a"}`

// findTextAtV155 opens a store upgraded to v156 from a v155 database whose
// user rows hold the candidate text v150 gave them, their loose summary.
// Thread T holds main's agent request ("agent"), the person's message
// ("person"), an assistant row ("answer") and a user row nested under it
// ("nested").
func findTextAtV155(t *testing.T) *Store {
	t.Helper()
	s := openStoreAt(t)
	mustCreateThread(t, s, "T")
	for _, item := range []Item{
		{ID: "agent", ItemIndex: 0, Kind: "user_text", Role: "user", Summary: mainAgentRequest, Meta: mainAgentRequestMeta},
		{ID: "person", ItemIndex: 1, Kind: "user_text", Role: "user", Summary: "Ship it later", Meta: `{"sendId":"s-1"}`},
		{ID: "answer", ItemIndex: 2, Kind: "assistant_text", Role: "assistant", Summary: "Shipped."},
		{ID: "nested", ItemIndex: 3, Kind: "user_text", Role: "user", Summary: "Ship it inside", ParentID: "answer"},
	} {
		item.ThreadID, item.Status, item.CreatedAt, item.UpdatedAt = "T", "completed", 1, 1
		if err := insertCarded(s, item); err != nil {
			t.Fatalf("seed %s: %v", item.ID, err)
		}
	}
	mustExec(t, s.db, `UPDATE items SET find_text = find_loose(summary) WHERE kind = 'user_text'`)

	path := s.path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	downgradeSchema(t, db, migrateThrough(t, userFindTextMigrationVersion-1), userFindTextMigrationVersion-1)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close upgraded store: %v", err)
		}
	})
	return s
}

// TestMigrationV156RefillsTheUserRowsFindText: the upgrade unsets the
// candidate text of the top-level user rows, which v150 filled from their
// summary alone, and leaves every other row's; find answers from each
// unset row's content until the deferred phase fills it again with what
// the row shows: an agent's header line read from its meta.
func TestMigrationV156RefillsTheUserRowsFindText(t *testing.T) {
	if strings.Contains(findLoose(mainAgentRequest), findLoose("Ship it now.")) {
		t.Fatal("the fixture's header line is in its loose summary; it proves nothing")
	}
	s := findTextAtV155(t)
	unset := func() string {
		rows, err := s.db.Query(`SELECT id FROM items WHERE thread_id = 'T' AND find_text IS NULL ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		return strings.Join(ids, " ")
	}
	if got := unset(); got != "agent person" {
		t.Fatalf("the upgrade unset %q, want the top-level user rows", got)
	}
	if !deferredPending(t, s) {
		t.Fatal("the upgrade left no refill pending")
	}
	if got := findIDs(t, s, "T", "Ship it now"); got != "[agent]" {
		t.Errorf("during the refill the header line found %s, want [agent]", got)
	}

	stamp := historyStampOf(t, s, "T")
	if err := s.RunDeferredMigrations(context.Background(), DeferredHost{}); err != nil {
		t.Fatal(err)
	}
	if deferredPending(t, s) || deferredFailureOf(t, s) != nil {
		t.Fatalf("the refill left watermark %d, failure %+v", deferredWatermarkOf(t, s), deferredFailureOf(t, s))
	}
	if got := unset(); got != "" {
		t.Fatalf("the refill left %q unset", got)
	}
	if got := countRows(t, s, `SELECT count(*) FROM items AS items WHERE items.thread_id = 'T' AND items.id != 'nested'
		AND items.find_text IS NOT `+findTextSQL("items")); got != 0 {
		t.Errorf("the refill wrote %d rows a candidate text their content does not give", got)
	}
	if indexExists(t, s, "idx_items_find_text_pending") || indexExists(t, s, "idx_import_history_items_find_text_pending") {
		t.Error("the refill kept its pending indexes")
	}
	if got := historyStampOf(t, s, "T"); got != stamp {
		t.Errorf("the refill moved T's history stamp from %+v to %+v", stamp, got)
	}
	if got := findIDs(t, s, "T", "Ship it now"); got != "[agent]" {
		t.Errorf("after the refill the header line found %s, want [agent]", got)
	}
}
