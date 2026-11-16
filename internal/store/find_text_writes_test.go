package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// requireFindTextCurrent holds every row of threadID to the candidate text
// its content gives: exactly for a row whose text is its summary and meta,
// and, for a reasoning row whose stream appended to it, as a text that
// holds the loose form of its summary and of its payload's content.
func requireFindTextCurrent(t *testing.T, s *Store, threadID, step string) {
	t.Helper()
	rows, err := s.db.Query(`SELECT items.id, items.kind, items.payload_id IS NOT NULL, items.find_text, `+findTextSQL("items")+`,
		       items.summary, COALESCE(`+findReasoningContentSQL("items")+`, '')
		  FROM items WHERE items.thread_id = ?`, threadID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, kind, want, summary, content string
		var paid bool
		var stored *string
		if err := rows.Scan(&id, &kind, &paid, &stored, &want, &summary, &content); err != nil {
			t.Fatal(err)
		}
		switch {
		case stored == nil:
			t.Errorf("%s: row %s has no find text", step, id)
		case findReasoningKind(kind) && paid:
			if !strings.Contains(*stored, findLoose(summary)) || !strings.Contains(*stored, findLoose(content)) {
				t.Errorf("%s: reasoning row %s find text %q misses its summary %q or content %q", step, id, *stored, summary, content)
			}
		case *stored != want:
			t.Errorf("%s: row %s find text = %q, want %q", step, id, *stored, want)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func findTextOf(t *testing.T, s *Store, threadID, id string) string {
	t.Helper()
	var text string
	if err := s.db.QueryRow(`SELECT find_text FROM items WHERE thread_id = ? AND id = ?`, threadID, id).Scan(&text); err != nil {
		t.Fatal(err)
	}
	return text
}

// TestFindTextFollowsEveryWrite: each way a row's text, meta or payload is
// written leaves its stored candidate text current, the inline writers and
// the triggers alike.
func TestFindTextFollowsEveryWrite(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "T")
	check := func(step string) { t.Helper(); requireFindTextCurrent(t, s, "T", step) }

	if err := insertCarded(s, Item{ID: "a", ThreadID: "T", Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "**foo**bar"}); err != nil {
		t.Fatal(err)
	}
	check("insert")
	if _, err := upsertCarded(s, Item{ID: "a", ThreadID: "T", Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "[docs](https://example.com/x)suffix"}, nil); err != nil {
		t.Fatal(err)
	}
	check("upsert over a row")
	if strings.Contains(findTextOf(t, s, "T", "a"), "foobar") {
		t.Fatal("the upsert kept the text it replaced")
	}

	if err := insertCarded(s, Item{ID: "s", ThreadID: "T", ItemIndex: 1, Kind: "assistant_text", Role: "assistant", Status: "streaming", Summary: "hel"}); err != nil {
		t.Fatal(err)
	}
	for _, delta := range []string{"lo [x](https://e.com)", "y <b>z</b>w"} {
		if _, err := s.AppendItemSummary("T", "s", delta, 2); err != nil {
			t.Fatal(err)
		}
	}
	check("streaming append")
	summary := "settled **text**"
	if _, err := s.UpdateItemFields("T", "s", ItemPartialUpdate{Summary: &summary}); err != nil {
		t.Fatal(err)
	}
	check("field update")

	if err := seedPayloadRow(s, "T", Payload{ID: "pay-th", Kind: "thinking", Meta: "{}", Data: []byte("start "), CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := insertCarded(s, Item{ID: "th", ThreadID: "T", ItemIndex: 2, Kind: "thinking", Role: "assistant", Status: "streaming", Summary: "start ", PayloadID: "pay-th"}); err != nil {
		t.Fatal(err)
	}
	for _, chunk := range []string{"middle words ", "and a long ", "ending"} {
		if _, err := s.AppendItemSummaryTailAndPayloadData("T", "th", chunk, 8, "pay-th", []byte(chunk), 3); err != nil {
			t.Fatal(err)
		}
	}
	check("thinking stream")
	if text := findTextOf(t, s, "T", "th"); !strings.Contains(text, findLoose("start middle words and a long ending")) {
		t.Fatalf("thinking find text %q misses the content past its tail", text)
	}
	if err := s.ReplacePayloadData("T", "pay-th", []byte("final authoritative thought"), "{}", 4); err != nil {
		t.Fatal(err)
	}
	check("payload replace")
	if text := findTextOf(t, s, "T", "th"); strings.Contains(text, "middle") {
		t.Fatalf("thinking find text %q keeps the chunks the replace cleared", text)
	}

	if err := insertCarded(s, Item{ID: "tool", ThreadID: "T", ItemIndex: 3, Kind: "tool_call", ToolName: "Bash", Role: "assistant", Status: "running", Summary: "Bash", Meta: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateItemMeta("T", "tool", `{"input":{"command":"grep -rn needle src/"}}`); err != nil {
		t.Fatal(err)
	}
	check("meta update")
	if !strings.Contains(findTextOf(t, s, "T", "tool"), "greprnneedlesrc") {
		t.Fatal("the tool row's find text misses the command its meta gained")
	}

	if err := insertCarded(s, Item{ID: "cr", ThreadID: "T", ItemIndex: 4, Kind: "compaction_reasoning", Role: "assistant", Status: "completed", Summary: "compacted reasoning"}); err != nil {
		t.Fatal(err)
	}
	check("reasoning row without a payload")

	// A command card reads its command from its payload's meta first.
	if _, err := upsertCarded(s, Item{ID: "cmd", ThreadID: "T", ItemIndex: 5, Kind: "tool_call", ToolName: "Bash", Role: "assistant", Status: "completed", Summary: "Bash", Meta: "{}"},
		&Payload{ID: "pay-cmd", Kind: "command_output", Meta: `{"command":"make payload-meta-only"}`, Data: []byte("out"), CreatedAt: 5}); err != nil {
		t.Fatal(err)
	}
	check("tool row with a payload")
	if !strings.Contains(findTextOf(t, s, "T", "cmd"), "makepayloadmetaonly") {
		t.Fatal("the tool row's find text misses its payload meta's command")
	}
	if err := s.UpdatePayloadMeta("T", "pay-cmd", `{"command":"make renamed-target"}`); err != nil {
		t.Fatal(err)
	}
	check("payload meta update")
	if text := findTextOf(t, s, "T", "cmd"); !strings.Contains(text, "makerenamedtarget") || strings.Contains(text, "payloadmetaonly") {
		t.Fatalf("the tool row's find text %q does not follow its payload meta", text)
	}

	// A write no writer supplies the text for is caught by the trigger.
	mustExec(t, s.db, `UPDATE items SET summary = 'raw <i>q</i>r' WHERE thread_id = 'T' AND id = 'a'`)
	check("raw summary write")

	importedHistoryFixture(t, s, "imp-src", 3)
	if n := countRows(t, s, `SELECT count(*) FROM import_history_items AS items WHERE items.find_text IS NOT `+findTextTables[1].text); n != 0 {
		t.Fatalf("the import wrote %d rows a stale find text", n)
	}
	if n := countRows(t, s, `SELECT count(*) FROM import_history_items WHERE find_text IS NULL`); n != 0 {
		t.Fatalf("the import left %d rows without find text", n)
	}
}

// TestFindTextOfAForkCopy: a fork copies the rows its source still runs.
// A copy keeps the source's candidate text, and a copy of a row the
// backfill had not reached is filled from the fork's own copy of the
// payload.
func TestFindTextOfAForkCopy(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "S")
	if err := seedPayloadRow(s, "S", Payload{ID: "pay", Kind: "thinking", Meta: "{}", Data: []byte("hidden opening, then the end"), CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	for _, item := range []Item{
		{ID: "think", Kind: "thinking", Summary: "the end", PayloadID: "pay"},
		{ID: "text", ItemIndex: 1, Kind: "assistant_text", Summary: "**foo**bar"},
	} {
		item.ThreadID, item.Role, item.Status = "S", "assistant", "streaming"
		if err := insertCarded(s, item); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(t, s.db, `UPDATE items SET find_text = NULL WHERE thread_id = 'S' AND id = 'think'`)
	mustPointerFork(t, s, "S", "F", ForkCut{})
	if n := countRows(t, s, `SELECT count(*) FROM items WHERE thread_id = 'F'`); n != 2 {
		t.Fatalf("the fork copied %d of the source's running rows, want 2", n)
	}
	requireFindTextCurrent(t, s, "F", "fork copy")
	if got := findIDs(t, s, "F", "hidden opening"); got != "[think]" {
		t.Errorf("the fork's copy of the thinking row found %s, want [think]", got)
	}
	if got := findIDs(t, s, "F", "foobar"); got != "[text]" {
		t.Errorf("the fork's copy of the text row found %s, want [text]", got)
	}
}

// TestFindTextSurvivesARestore: a restore copies each row's candidate text
// as the snapshot holds it, runs the backfill again for a snapshot that
// predates it, and leaves the triggers installed for the writes after it.
func TestFindTextSurvivesARestore(t *testing.T) {
	src := findTextAtV149(t)
	snapshot := filepath.Join(t.TempDir(), "snapshot.sqlite")
	if err := src.SnapshotTo(snapshot); err != nil {
		t.Fatal(err)
	}
	live := newTestStore(t)
	if _, err := live.RestoreFrom(snapshot); err != nil {
		t.Fatal(err)
	}
	if !deferredPending(t, live) {
		t.Fatal("the restore of an unfilled snapshot left no backfill pending")
	}
	if got := findIDs(t, live, "T", "foobar"); got != "[md think cmd]" {
		t.Fatalf("the restored unfilled rows found %s, want [md think cmd]", got)
	}
	if err := live.RunDeferredMigrations(context.Background(), DeferredHost{}); err != nil {
		t.Fatal(err)
	}
	if deferredPending(t, live) || deferredFailureOf(t, live) != nil {
		t.Fatalf("the backfill after the restore left failure %+v", deferredFailureOf(t, live))
	}
	if n := staleFindTextRows(t, live); n != 0 {
		t.Fatalf("%d restored rows stale after the backfill", n)
	}
	if err := insertCarded(live, Item{ID: "after", ThreadID: "T", ItemIndex: 9, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "after the restore"}); err != nil {
		t.Fatal(err)
	}
	requireFindTextCurrent(t, live, "T", "write after the restore")
}
