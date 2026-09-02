package store

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// liveFlag reads `meta.live_background_active` off one row. Three answers,
// all of them meaningful: absent (never written), true, false.
func liveFlag(t *testing.T, s *Store, threadID, itemID string) (value bool, present bool) {
	t.Helper()
	item, ok, err := s.GetThreadItem(threadID, itemID)
	if err != nil {
		t.Fatalf("get %s/%s: %v", threadID, itemID, err)
	}
	if !ok {
		t.Fatalf("item %s/%s missing", threadID, itemID)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(item.Meta), &decoded); err != nil {
		t.Fatalf("decode meta of %s (%q): %v", itemID, item.Meta, err)
	}
	raw, ok := decoded["live_background_active"]
	if !ok {
		return false, false
	}
	flag, isBool := raw.(bool)
	if !isBool {
		t.Fatalf("live_background_active on %s is %T, want bool", itemID, raw)
	}
	return flag, true
}

func assertSettled(t *testing.T, s *Store, threadID, itemID string) {
	t.Helper()
	value, present := liveFlag(t, s, threadID, itemID)
	if !present || value {
		t.Fatalf("%s: live_background_active present=%v value=%v, want present=true value=false", itemID, present, value)
	}
}

func assertLive(t *testing.T, s *Store, threadID, itemID string) {
	t.Helper()
	value, present := liveFlag(t, s, threadID, itemID)
	if present && !value {
		t.Fatalf("%s: live_background_active is false, want live", itemID)
	}
}

// seedLaunchWithMeta persists a running background tool_call carrying
// caller-chosen meta, which the shared seedBackgroundItem helper cannot
// express.
func seedLaunchWithMeta(t *testing.T, s *Store, threadID, id string, itemIndex int, meta string) {
	t.Helper()
	if err := s.InsertItem(Item{
		ID:           id,
		ThreadID:     threadID,
		TurnIndex:    0,
		ItemIndex:    itemIndex,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "running",
		Summary:      id,
		IsBackground: true,
		Meta:         meta,
		CreatedAt:    1000,
	}); err != nil {
		t.Fatalf("seed launch %s: %v", id, err)
	}
}

func seedCompletionSibling(t *testing.T, s *Store, threadID, id, launchID string, itemIndex int, createdAt int64) {
	t.Helper()
	if err := s.InsertItem(Item{
		ID:           id,
		ThreadID:     threadID,
		TurnIndex:    0,
		ItemIndex:    itemIndex,
		Kind:         "tool_completion",
		Role:         "assistant",
		Status:       "completed",
		Summary:      id,
		IsBackground: true,
		CompletionOf: launchID,
		CreatedAt:    createdAt,
	}); err != nil {
		t.Fatalf("seed completion %s: %v", id, err)
	}
}

func settleTriggerStore(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	return s
}

// A background launch stays `status='running'` forever (invariant 24),
// so the completion sibling's arrival is the only moment "this is no
// longer live" becomes true. The trigger writes that onto the launch so
// the partial live indexes stop matching it.
func TestCompletionInsertSettlesItsLaunch(t *testing.T) {
	s := settleTriggerStore(t)
	seedLaunchWithMeta(t, s, "t", "launch", 0, `{"task_id":"task-1"}`)
	assertLive(t, s, "t", "launch")

	seedCompletionSibling(t, s, "t", "complete:launch", "launch", 1, 2000)
	assertSettled(t, s, "t", "launch")

	// The launch row's own lifecycle is untouched: status stays running
	// and updated_at is not bumped by a derived stamp.
	item, _, err := s.GetThreadItem("t", "launch")
	if err != nil {
		t.Fatalf("get launch: %v", err)
	}
	if item.Status != "running" {
		t.Fatalf("status = %q, want running", item.Status)
	}
	if !strings.Contains(item.Meta, `"task_id":"task-1"`) {
		t.Fatalf("meta lost its other keys: %q", item.Meta)
	}
}

// Import batches and materializeSharedHistoryTx insert rows in whatever
// order the source gives, so the launch can land after its completion.
func TestLaunchInsertedAfterItsCompletionIsSettled(t *testing.T) {
	s := settleTriggerStore(t)
	seedCompletionSibling(t, s, "t", "complete:launch", "launch", 0, 2000)
	seedLaunchWithMeta(t, s, "t", "launch", 1, `{}`)
	assertSettled(t, s, "t", "launch")
}

// writeBackgroundCompletionSibling inserts the sibling and THEN calls
// persistFinalSubagentProgress, which replaces the launch's meta
// wholesale from an in-memory copy read BEFORE the insert. Without the
// UPDATE trigger that write revives the flag.
func TestWholesaleMetaRewriteOfSettledLaunchIsReStamped(t *testing.T) {
	s := settleTriggerStore(t)
	seedLaunchWithMeta(t, s, "t", "launch", 0, `{}`)
	seedCompletionSibling(t, s, "t", "complete:launch", "launch", 1, 2000)
	assertSettled(t, s, "t", "launch")

	stale := `{"subagentProgress":{"tokens":42}}`
	if err := s.UpdateItemFields("t", "launch", ItemPartialUpdate{Meta: &stale}); err != nil {
		t.Fatalf("wholesale meta rewrite: %v", err)
	}
	assertSettled(t, s, "t", "launch")

	item, _, err := s.GetThreadItem("t", "launch")
	if err != nil {
		t.Fatalf("get launch: %v", err)
	}
	if !strings.Contains(item.Meta, `"tokens":42`) {
		t.Fatalf("re-stamp dropped the caller's meta: %q", item.Meta)
	}
}

// DeleteConversationFromTurn rolls a thread back past a completion. The
// launch is live again afterwards, which is what lets the session-end
// settle and the boot sweep synthesise its terminal exactly as before.
func TestDeletingTheOnlyCompletionRevivesItsLaunch(t *testing.T) {
	s := settleTriggerStore(t)
	seedLaunchWithMeta(t, s, "t", "launch", 0, `{}`)
	seedCompletionSibling(t, s, "t", "complete:launch", "launch", 1, 2000)
	assertSettled(t, s, "t", "launch")

	if err := s.DeleteThreadItem("t", "complete:launch"); err != nil {
		t.Fatalf("delete completion: %v", err)
	}
	if _, present := liveFlag(t, s, "t", "launch"); present {
		t.Fatal("revive left the flag on the row instead of removing it")
	}
	assertLive(t, s, "t", "launch")
}

func TestDeletingOneOfTwoCompletionsLeavesTheLaunchSettled(t *testing.T) {
	s := settleTriggerStore(t)
	seedLaunchWithMeta(t, s, "t", "launch", 0, `{}`)
	seedCompletionSibling(t, s, "t", "complete:launch", "launch", 1, 2000)
	seedCompletionSibling(t, s, "t", "complete:launch:2", "launch", 2, 2500)

	if err := s.DeleteThreadItem("t", "complete:launch"); err != nil {
		t.Fatalf("delete first completion: %v", err)
	}
	assertSettled(t, s, "t", "launch")

	if err := s.DeleteThreadItem("t", "complete:launch:2"); err != nil {
		t.Fatalf("delete second completion: %v", err)
	}
	assertLive(t, s, "t", "launch")
}

// A launch a session teardown marked inactive has no completion sibling,
// so the delete trigger can never reach it — and the revive only ever
// removes a flag whose value is exactly false, so an explicit `true`
// (the Codex projection's live marker) is left alone.
func TestReviveOnlyTouchesSettledLaunches(t *testing.T) {
	s := settleTriggerStore(t)
	seedLaunchWithMeta(t, s, "t", "explicit-live", 0, `{"live_background_active":true}`)
	seedCompletionSibling(t, s, "t", "complete:explicit-live", "explicit-live", 1, 2000)
	// The insert trigger settles it; asserting that first is what makes
	// the delete below a real revive rather than a no-op on a true flag.
	assertSettled(t, s, "t", "explicit-live")

	seedLaunchWithMeta(t, s, "t", "torn-down", 2, `{"live_background_active":false}`)
	// A sibling for a DIFFERENT launch: deleting it must not sweep the
	// torn-down row, which has no sibling of its own.
	if err := s.DeleteThreadItem("t", "complete:explicit-live"); err != nil {
		t.Fatalf("delete completion: %v", err)
	}
	value, present := liveFlag(t, s, "t", "torn-down")
	if !present || value {
		t.Fatalf("torn-down launch flag present=%v value=%v, want the teardown mark intact", present, value)
	}
}

// The Codex spawn card is `status='completed'` on the wire while its
// child thread keeps running (ListLiveCodexSubagentLaunches), so the
// settle triggers must not touch it — its liveness is the Codex
// reconciler's to own.
func TestCodexSpawnCardIsNotSettledByTheTriggers(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "codex")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := s.InsertItem(Item{
		ID:           "spawn",
		ThreadID:     "t",
		TurnIndex:    0,
		ItemIndex:    0,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "completed",
		Summary:      "spawn",
		IsBackground: true,
		ToolName:     "collab_agent",
		Meta:         `{"live_background_active":true,"input":{"tool":"spawn_agent"}}`,
		CreatedAt:    1000,
	}); err != nil {
		t.Fatalf("seed spawn card: %v", err)
	}
	seedCompletionSibling(t, s, "t", "complete:spawn", "spawn", 1, 2000)

	value, present := liveFlag(t, s, "t", "spawn")
	if !present || !value {
		t.Fatalf("spawn card flag present=%v value=%v, want the wire's true untouched", present, value)
	}
}

func TestForegroundLaunchIsNotSettledByTheTriggers(t *testing.T) {
	s := settleTriggerStore(t)
	if err := s.InsertItem(Item{
		ID:        "fg",
		ThreadID:  "t",
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "tool_call",
		Role:      "assistant",
		Status:    "running",
		Summary:   "fg",
		CreatedAt: 1000,
	}); err != nil {
		t.Fatalf("seed foreground launch: %v", err)
	}
	seedCompletionSibling(t, s, "t", "complete:fg", "fg", 1, 2000)

	if _, present := liveFlag(t, s, "t", "fg"); present {
		t.Fatal("a foreground launch acquired a background liveness flag")
	}
}

// items.meta is NOT NULL DEFAULT '{}' and nothing constrains it to
// valid JSON — but for the exact row shape these triggers touch, the
// pre-existing partial indexes already evaluate `json_extract(meta,
// '$.live_background_active')`, so SQLite refuses the write before any
// trigger runs. The triggers therefore cannot introduce a new insert
// failure mode, and no reachable launch row can carry a malformed blob.
func TestMalformedLaunchMetaIsRefusedBeforeTheTriggersSeeIt(t *testing.T) {
	s := settleTriggerStore(t)
	_, err := s.db.Exec(`INSERT INTO items
		(id, thread_id, turn_index, item_index, kind, role, status, summary,
		 parent_id, is_background, completion_of, meta, created_at, updated_at)
		VALUES ('launch', 't', 0, 0, 'tool_call', 'assistant', 'running', 'launch',
		 '', 1, '', 'not json', 1000, 1000)`)
	if err == nil {
		t.Fatal("a running background launch with malformed meta was accepted")
	}
	if !strings.Contains(err.Error(), "malformed JSON") {
		t.Fatalf("error = %v, want a malformed-JSON refusal", err)
	}
}

// The json_valid guard in the stamp expression is the backstop for that:
// json_set returns NULL on a malformed blob, and NULL into a NOT NULL
// column is a failed write on a path nobody is watching. Pinned as an
// expression because no reachable row can reach it (above), and mirrors
// the identical guard in MarkLiveBackgroundToolCallsInactive.
func TestStampExpressionReplacesMalformedMetaInsteadOfBlankingIt(t *testing.T) {
	s := settleTriggerStore(t)
	for _, meta := range []string{"not json", "", `{"task_id":"x"}`} {
		var got string
		if err := s.db.QueryRow(`SELECT json_set(
		      CASE WHEN json_valid(?1) THEN ?1 ELSE '{}' END,
		      '$.live_background_active', json('false'))`, meta).Scan(&got); err != nil {
			t.Fatalf("stamp %q: %v", meta, err)
		}
		if !strings.Contains(got, `"live_background_active":false`) {
			t.Errorf("stamp of %q = %q, want the flag set", meta, got)
		}
	}
}

// Every WHEN clause requires the flag to be in the state the trigger's
// own UPDATE leaves it out of, so the chain terminates at depth one
// whatever `recursive_triggers` is set to. Asserted with the pragma
// forced ON, because the default OFF proves nothing about the guard.
func TestBackgroundSettleTriggersDoNotRecurse(t *testing.T) {
	s := settleTriggerStore(t)
	if _, err := s.db.Exec(`PRAGMA recursive_triggers=ON`); err != nil {
		t.Fatalf("enable recursive triggers: %v", err)
	}
	t.Cleanup(func() { _, _ = s.db.Exec(`PRAGMA recursive_triggers=OFF`) })

	seedLaunchWithMeta(t, s, "t", "launch", 0, `{}`)
	seedCompletionSibling(t, s, "t", "complete:launch", "launch", 1, 2000)
	assertSettled(t, s, "t", "launch")

	if err := s.DeleteThreadItem("t", "complete:launch"); err != nil {
		t.Fatalf("delete completion: %v", err)
	}
	assertLive(t, s, "t", "launch")
}

// The triggers are dropped with `items` by any rebuild and dropped by
// hand around the restore row copy. Both installers come from the same
// const, so the only way they can go missing is a new rebuild that
// forgets to re-run it.
func TestBackgroundSettleTriggersExistAfterTheMigrationChain(t *testing.T) {
	s := newTestStore(t)
	for _, name := range backgroundSettleTriggerNames {
		var count int
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = ?`, name,
		).Scan(&count); err != nil {
			t.Fatalf("find trigger %s: %v", name, err)
		}
		if count != 1 {
			t.Errorf("trigger %s count = %d, want 1", name, count)
		}
	}
}

func TestBackgroundSettleTriggersSurviveSnapshotRestore(t *testing.T) {
	st := snapshotTestStore(t)
	seedSnapshotFixture(t, st, "t1", "before snapshot")

	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := st.SnapshotTo(snap); err != nil {
		t.Fatalf("SnapshotTo: %v", err)
	}
	if _, err := st.RestoreFrom(snap); err != nil {
		t.Fatalf("RestoreFrom: %v", err)
	}

	// Present is not enough: a trigger scoped to a table the restore
	// replaced would pass a name check and stamp nothing.
	seedLaunchWithMeta(t, st, "t1", "launch", 10, `{}`)
	seedCompletionSibling(t, st, "t1", "complete:launch", "launch", 11, 2000)
	assertSettled(t, st, "t1", "launch")
}

// A rebuild of `items` drops every trigger on it. Migration v72 shows
// the shape (drop, rebuild, re-create); this fails the build if a later
// items rebuild lands without re-installing these four.
func TestItemsRebuildMigrationsReinstallBackgroundSettleTriggers(t *testing.T) {
	for _, m := range migrations {
		if m.Version <= backgroundSettleTriggerMigrationVersion {
			continue
		}
		if !strings.Contains(m.SQL, "DROP TABLE items") {
			continue
		}
		if !strings.Contains(m.SQL, backgroundSettleTriggersSQL) {
			t.Errorf("migration v%d (%s) rebuilds items without re-installing backgroundSettleTriggersSQL", m.Version, m.Name)
		}
	}
}

// The backfill is what makes the invariant true for history. Without it
// every launch settled before the migration keeps a set flag and stays
// in the partial live indexes forever, which is the whole defect.
func TestMigrationBackfillsSettledBackgroundLaunches(t *testing.T) {
	db := migrateThrough(t, backgroundSettleTriggerMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at)
		VALUES ('p', '/p', 'p', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (
		id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived
	) VALUES ('t', 'p', 'T', 'claude', '/p', '', 1, 1, 0)`)

	insertItem := func(id, kind, status string, background int, completionOf, meta string, index int) {
		mustExec(t, db, `INSERT INTO items
			(id, thread_id, turn_index, item_index, kind, role, status, summary,
			 parent_id, is_background, completion_of, meta, created_at, updated_at)
			VALUES (?, 't', 0, ?, ?, 'assistant', ?, ?, '', ?, ?, ?, 1000, 1000)`,
			id, index, kind, status, id, background, completionOf, meta)
	}
	insertItem("settled", "tool_call", "running", 1, "", `{"task_id":"x"}`, 0)
	insertItem("complete:settled", "tool_completion", "completed", 1, "settled", `{}`, 1)
	insertItem("live", "tool_call", "running", 1, "", `{}`, 2)
	insertItem("torn-down", "tool_call", "running", 1, "", `{"live_background_active":false}`, 3)
	insertItem("foreground", "tool_call", "running", 0, "", `{}`, 4)
	insertItem("complete:foreground", "tool_completion", "completed", 0, "foreground", `{}`, 5)

	if err := applyMigration(db, migrationByVersion(t, backgroundSettleTriggerMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", backgroundSettleTriggerMigrationVersion, err)
	}

	flag := func(id string) sql.NullString {
		var raw sql.NullString
		if err := db.QueryRow(
			`SELECT json_extract(meta, '$.live_background_active') FROM items WHERE id = ?`, id,
		).Scan(&raw); err != nil {
			t.Fatalf("read flag of %s: %v", id, err)
		}
		return raw
	}
	if got := flag("settled"); !got.Valid || got.String != "0" {
		t.Errorf("settled launch flag = %+v, want 0", got)
	}
	var settledMeta string
	if err := db.QueryRow(`SELECT meta FROM items WHERE id = 'settled'`).Scan(&settledMeta); err != nil {
		t.Fatalf("read settled meta: %v", err)
	}
	if !strings.Contains(settledMeta, `"task_id":"x"`) {
		t.Errorf("backfill dropped the launch's other meta keys: %q", settledMeta)
	}
	if got := flag("live"); got.Valid {
		t.Errorf("live launch flag = %+v, want absent", got)
	}
	if got := flag("torn-down"); !got.Valid || got.String != "0" {
		t.Errorf("torn-down launch flag = %+v, want the teardown mark intact", got)
	}
	if got := flag("foreground"); got.Valid {
		t.Errorf("foreground launch flag = %+v, want absent", got)
	}

	// And the index the backfill exists to shrink now holds only the
	// live row.
	var live int
	if err := db.QueryRow(`SELECT COUNT(*) FROM items INDEXED BY idx_items_running_bg_tool_calls
	     WHERE kind = 'tool_call' AND status = 'running' AND is_background = 1
	       AND COALESCE(json_extract(meta, '$.live_background_active'), 1) != 0`).Scan(&live); err != nil {
		t.Fatalf("count live index entries: %v", err)
	}
	if live != 1 {
		t.Errorf("live index entries = %d, want 1", live)
	}
}
