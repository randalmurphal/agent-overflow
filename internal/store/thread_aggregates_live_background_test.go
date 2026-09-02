package store

import (
	"encoding/json"
	"strings"
	"testing"
)

// preSettlementLiveBackgroundTasksSQL is the tray query as it stood
// before the settlement triggers (migration v74). It is kept verbatim as
// TEST-ONLY text so the rewrite has something to be equal to: the new
// query is a plan change, not a behavior change, and the only way to say
// that with evidence is to run both over the same history.
//
// The seed is the whole difference — "every background tool_call this
// thread ever had" versus two index reads — plus the one predicate that
// had to move with the trigger (see ListLiveBackgroundTasks' doc
// comment).
const preSettlementLiveBackgroundTasksSQL = `WITH RECURSIVE bg(id) AS (
		    SELECT id FROM items
		     WHERE thread_id = ?
		       AND kind = 'tool_call'
		       AND is_background = 1
		),
		` + preSettlementDescendantsCTE + `,
		anchors(id) AS (
		    SELECT id FROM bg
		    UNION
		    SELECT i.id
		      FROM rel
		      CROSS JOIN items i ON i.thread_id = ? AND i.id = rel.id
		     WHERE i.kind = 'tool_call'
		       AND EXISTS (
		         SELECT 1 FROM items child
		          WHERE child.thread_id = i.thread_id
		            AND child.parent_id = i.id
		            AND child.parent_id <> ''
		       )
		)
		SELECT ` + itemColumns + `
		   FROM items
		   LEFT JOIN payloads ON payloads.thread_id = items.thread_id AND payloads.id = items.payload_id
		  WHERE items.thread_id = ?
		    AND (
		      (
		        items.id IN (SELECT id FROM anchors)
		        AND items.kind = 'tool_call'
		        AND items.status = 'running'
		        AND COALESCE(json_extract(items.meta, '$.live_background_active'), 1) != 0
		        AND (
		          NOT EXISTS (
		            SELECT 1 FROM items c
		             WHERE c.thread_id = items.thread_id
		               AND c.completion_of = items.id
		               AND c.completion_of <> ''
		          )
		          OR EXISTS (
		            SELECT 1 FROM items c
		             WHERE c.thread_id = items.thread_id
		               AND c.completion_of = items.id
		               AND c.completion_of <> ''
		               AND c.created_at >= ?
		          )
		        )
		      )
		      OR (
		        items.completion_of <> ''
		        AND items.created_at >= ?
		        AND items.completion_of IN (SELECT id FROM anchors)
		      )
		    )
		  ORDER BY items.turn_index, items.item_index`

const preSettlementDescendantsCTE = `rel(root, id) AS (
		SELECT i.parent_id, i.id
		  FROM items i
		 WHERE i.thread_id = ?
		   AND i.parent_id IN (SELECT id FROM bg)
		   AND i.parent_id <> ''
		   AND NOT (i.kind = 'notification' AND i.tool_name = 'plan_update')
		UNION
		SELECT rel.root, i.id
		  FROM rel
		  CROSS JOIN items i ON i.parent_id = rel.id
		 WHERE i.thread_id = ?
		   AND i.parent_id <> ''
		   AND NOT (i.kind = 'notification' AND i.tool_name = 'plan_update')
	)`

func preSettlementLiveBackgroundTasks(t *testing.T, s *Store, threadID string, cutoff int64) []Item {
	t.Helper()
	rows, err := s.db.Query(preSettlementLiveBackgroundTasksSQL,
		threadID, threadID, threadID, threadID, threadID, cutoff, cutoff)
	if err != nil {
		t.Fatalf("pre-settlement tray query: %v", err)
	}
	defer rows.Close()
	out := []Item{}
	for rows.Next() {
		item, err := scanItemRow(rows)
		if err != nil {
			t.Fatalf("scan pre-settlement row: %v", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate pre-settlement rows: %v", err)
	}
	return out
}

// metaWithoutLiveFlag is what makes the comparison meaningful. The
// settlement flag is the ONE field v74 changes on a row, so comparing
// it would just restate the migration; comparing everything else pins
// that nothing ELSE moved.
func metaWithoutLiveFlag(t *testing.T, meta string) string {
	t.Helper()
	if strings.TrimSpace(meta) == "" {
		return ""
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(meta), &decoded); err != nil {
		t.Fatalf("decode meta %q: %v", meta, err)
	}
	delete(decoded, "live_background_active")
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("re-encode meta %q: %v", meta, err)
	}
	return string(encoded)
}

func assertTrayRowsEqual(t *testing.T, want, got []Item) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("row count = %d (%v), want %d (%v)",
			len(got), collectIDs(got), len(want), collectIDs(want))
	}
	for i := range want {
		wantRow, gotRow := want[i], got[i]
		wantMeta := metaWithoutLiveFlag(t, wantRow.Meta)
		gotMeta := metaWithoutLiveFlag(t, gotRow.Meta)
		wantRow.Meta, gotRow.Meta = "", ""
		if wantRow != gotRow {
			t.Errorf("row %d differs:\n old = %+v\n new = %+v", i, wantRow, gotRow)
		}
		if wantMeta != gotMeta {
			t.Errorf("row %d meta differs beyond the settlement flag:\n old = %s\n new = %s", i, wantMeta, gotMeta)
		}
	}
}

// seedTrayFixture writes the shapes the tray has to get right, with the
// settlement triggers OFF so the rows land exactly as a pre-v74 store
// held them. Returns the retention cutoff the assertions use.
func seedTrayFixture(t *testing.T, s *Store) int64 {
	t.Helper()
	const cutoff = 5000

	insert := func(item Item) {
		t.Helper()
		if err := s.InsertItem(item); err != nil {
			t.Fatalf("seed %s: %v", item.ID, err)
		}
	}
	launch := func(id string, index int, background bool, parent, meta string) {
		t.Helper()
		insert(Item{
			ID: id, ThreadID: "t", TurnIndex: 0, ItemIndex: index,
			Kind: "tool_call", Role: "assistant", Status: "running", Summary: id,
			ParentID: parent, IsBackground: background, Meta: meta, CreatedAt: 1000,
		})
	}
	completion := func(id, launchID string, index int, createdAt int64) {
		t.Helper()
		insert(Item{
			ID: id, ThreadID: "t", TurnIndex: 0, ItemIndex: index,
			Kind: "tool_completion", Role: "assistant", Status: "completed", Summary: id,
			IsBackground: true, CompletionOf: launchID, CreatedAt: createdAt,
		})
	}

	// 0-1: a live launch, and a launch a session teardown marked
	// inactive (no completion sibling — that is the shape the teardown
	// writer can produce).
	launch("live", 0, true, "", `{}`)
	launch("closed", 1, true, "", `{"live_background_active":false}`)

	// 2-5: a settled launch whose completion is inside the window, and
	// one whose completion has aged out. The pair must age out together.
	launch("recent", 2, true, "", `{}`)
	completion("complete:recent", "recent", 3, 6000)
	launch("stale", 4, true, "", `{}`)
	completion("complete:stale", "stale", 5, 1000)

	// 6-9: the invariant-24 nesting. A live background root, a nested
	// BACKGROUND launch under it, and a nested FOREGROUND agent launch
	// (which qualifies only because a child is attributed to it).
	launch("bg-root", 6, true, "", `{}`)
	launch("nested-bg", 7, true, "bg-root", `{}`)
	launch("nested-agent", 8, false, "bg-root", `{}`)
	insert(Item{
		ID: "nested-agent-child", ThreadID: "t", TurnIndex: 0, ItemIndex: 9,
		Kind: "assistant_text", Role: "assistant", Summary: "child",
		ParentID: "nested-agent", CreatedAt: 1000,
	})

	// 10: the Codex spawn card — completed on the wire while its child
	// thread keeps running. Never a row of this query (App adds it from
	// ListLiveCodexSubagentLaunches) and never touched by the triggers.
	insert(Item{
		ID: "spawn", ThreadID: "t", TurnIndex: 0, ItemIndex: 10,
		Kind: "tool_call", Role: "assistant", Status: "completed", Summary: "spawn",
		IsBackground: true, ToolName: "collab_agent",
		Meta:      `{"live_background_active":true,"input":{"tool":"spawn_agent"}}`,
		CreatedAt: 1000,
	})

	// 11: a recent completion naming a launch that does not exist. The
	// new seed reads launch ids OUT of completion rows, so this is the
	// row that would leak in without its existence guard.
	completion("complete:ghost", "ghost", 11, 6000)

	return cutoff
}

// The rewrite is a plan change. This is the evidence: the same history,
// read by the old query before v74 and the new one after it, gives the
// same rows in the same order.
func TestListLiveBackgroundTasksMatchesThePreSettlementQuery(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if _, err := s.db.Exec(dropBackgroundSettleTriggersSQL); err != nil {
		t.Fatalf("drop settle triggers: %v", err)
	}
	cutoff := seedTrayFixture(t, s)

	want := preSettlementLiveBackgroundTasks(t, s, "t", cutoff)
	if ids := collectIDs(want); !equalStringSlice(ids,
		[]string{"live", "recent", "complete:recent", "bg-root", "nested-bg", "nested-agent"}) {
		t.Fatalf("pre-settlement rows = %v, fixture no longer covers the shapes it names", ids)
	}

	// Now become a v74 store: backfill history, re-install the triggers.
	if _, err := s.db.Exec(backfillSettledBackgroundLaunchesSQL); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if _, err := s.db.Exec(backgroundSettleTriggersSQL); err != nil {
		t.Fatalf("install settle triggers: %v", err)
	}

	got, err := s.ListLiveBackgroundTasks("t", cutoff)
	if err != nil {
		t.Fatalf("list live background tasks: %v", err)
	}
	assertTrayRowsEqual(t, want, got)
}

// The seed is the point of the rewrite, so its plan is a test. Both
// halves must ride their partial index, and nothing in the statement may
// fall back to walking the thread through the timeline index — the 75k
// page reads this change exists to remove.
func TestListLiveBackgroundTasksSeedUsesPartialIndexes(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	seedTrayFixture(t, s)

	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+liveBackgroundTasksSQL,
		"t", "t", int64(5000), "t", "t", "t", "t", int64(5000), "t", int64(5000), int64(5000))
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int64
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(detail)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan: %v", err)
	}
	for _, want := range []string{
		"idx_items_running_bg_tool_calls",
		"idx_items_completion_created",
	} {
		if !strings.Contains(plan.String(), want) {
			t.Errorf("seed no longer uses %s:\n%s", want, plan.String())
		}
	}
	if strings.Contains(plan.String(), "idx_items_thread_turn_item_unique") {
		t.Errorf("the tray query walks the thread's timeline again:\n%s", plan.String())
	}
}

func TestItemsCompletionCreatedIndexIsPartial(t *testing.T) {
	s := newTestStore(t)
	sqlText := readIndexSQL(t, s.db, "idx_items_completion_created")
	for _, want := range []string{"thread_id", "created_at", "completion_of <> ''"} {
		if !strings.Contains(sqlText, want) {
			t.Errorf("idx_items_completion_created missing %q in SQL: %s", want, sqlText)
		}
	}
}
