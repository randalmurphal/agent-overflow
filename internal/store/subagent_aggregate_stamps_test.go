package store

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// writeSubagentStampForTest writes an anchor's meta the way the stamp's
// owner does: the generation moves by one, so the update trigger takes it
// as a stamp write rather than a stale whole-meta write to undo.
func writeSubagentStampForTest(t *testing.T, s *Store, threadID, id, metaExpr string) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE items SET meta = json_set(`+metaExpr+`, '`+aggGenPath+`',
	        COALESCE(`+aggJX("meta", aggGenPath)+`, 0) + 1)
	  WHERE thread_id = ? AND id = ?`, threadID, id); err != nil {
		t.Fatalf("write stamp %s/%s: %v", threadID, id, err)
	}
}

// stripSubagentStampsForTest leaves rows as a store before v121 held
// them: no stamp keys. It writes under the bulk-load flag, which suspends
// the aggregate triggers. No ids strips the whole thread.
func stripSubagentStampsForTest(t *testing.T, s *Store, threadID string, ids ...string) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin strip: %v", err)
	}
	defer tx.Rollback()
	if err := setHistoryBulkLoadTx(tx, threadID, true, "test strip"); err != nil {
		t.Fatal(err)
	}
	where, args := "thread_id = ? AND "+aggHasKeysSQL("meta"), []any{threadID}
	if len(ids) > 0 {
		clause, idArgs := inClause("id", ids)
		where += " AND " + clause
		args = append(args, idArgs...)
	}
	if _, err := tx.Exec(`UPDATE items SET meta = `+aggStripMetaSQL("meta")+` WHERE `+where, args...); err != nil {
		t.Fatalf("strip stamps: %v", err)
	}
	if err := setHistoryBulkLoadTx(tx, threadID, false, "test strip"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit strip: %v", err)
	}
}

// subagentStampCardKeys are the keys a stamp owns and a read serves.
var subagentStampCardKeys = []string{
	metaKeySubagentDescendantCount, metaKeySubagentLatestChildSummary,
	metaKeySubagentTranscriptDescendantCount, metaKeySubagentLatestToolSummary,
	metaKeySubagentLatestToolTurn, metaKeySubagentLatestToolItem,
}

type subagentCard map[string]any

func subagentCardOf(t *testing.T, meta string) subagentCard {
	t.Helper()
	card := subagentCard{}
	if strings.TrimSpace(meta) == "" {
		return card
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(meta), &decoded); err != nil {
		t.Fatalf("decode meta %q: %v", meta, err)
	}
	for _, key := range subagentStampCardKeys {
		if value, ok := decoded[key]; ok {
			card[key] = value
		}
	}
	return card
}

func threadTimelineIDsForTest(t *testing.T, q sqlQueryer, threadID string) []string {
	t.Helper()
	ids, err := subagentAnchorIDs(q, `SELECT id FROM timeline_items WHERE thread_id = ?`, threadID)
	if err != nil {
		t.Fatalf("list timeline ids: %v", err)
	}
	slices.Sort(ids)
	return ids
}

// subagentCardsForTest reads every row as the page (ListWireItems) and the
// tray (decorateLatestDirectSubagentTools) serve it.
func subagentCardsForTest(t *testing.T, s *Store, q sqlQueryer, threadID string) map[string]subagentCard {
	t.Helper()
	rows, err := s.listWireItemsTx(q, threadID, threadTimelineIDsForTest(t, q, threadID))
	if err != nil {
		t.Fatalf("read wire items: %v", err)
	}
	rows, err = s.decorateLatestDirectSubagentTools(q, threadID, rows)
	if err != nil {
		t.Fatalf("decorate tray: %v", err)
	}
	out := make(map[string]subagentCard, len(rows))
	for _, row := range rows {
		out[row.ID] = subagentCardOf(t, row.Meta)
	}
	return out
}

// walkedSubagentCardsForTest is the reference read: every stamp removed
// and the thread listed for the backfill, so every anchor goes through the
// read-time aggregator. The transaction is rolled back.
func walkedSubagentCardsForTest(t *testing.T, s *Store, threadID string) map[string]subagentCard {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin walked read: %v", err)
	}
	defer tx.Rollback()
	if err := setHistoryBulkLoadTx(tx, threadID, true, "test walk"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE items SET meta = `+aggStripMetaSQL("meta")+`
	 WHERE thread_id = ? AND `+aggHasKeysSQL("meta"), threadID); err != nil {
		t.Fatalf("strip stamps: %v", err)
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO subagent_aggregate_backfill(thread_id) VALUES (?)`, threadID); err != nil {
		t.Fatalf("list thread: %v", err)
	}
	return subagentCardsForTest(t, s, tx, threadID)
}

// assertSubagentStampParity compares every row's served card with the
// read-time aggregator's. settled adds the post-write invariants: nothing
// is left dirty, and outside the backfill every anchor a read decorates
// carries a stamp.
func assertSubagentStampParity(t *testing.T, s *Store, threadID, stage string, settled bool) {
	t.Helper()
	got := subagentCardsForTest(t, s, s.reader(), threadID)
	want := walkedSubagentCardsForTest(t, s, threadID)
	ids := make([]string, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if !reflect.DeepEqual(got[id], want[id]) {
			t.Errorf("%s: %s serves %v, read-time aggregator says %v", stage, id, got[id], want[id])
		}
	}
	if len(got) != len(want) {
		t.Errorf("%s: served %d rows, reference read %d", stage, len(got), len(want))
	}
	if !settled {
		return
	}
	dirty, err := subagentAnchorIDs(s.reader(), subagentDirtyAnchorsSQL, threadID, 100)
	if err != nil {
		t.Fatalf("%s: dirty probe: %v", stage, err)
	}
	if len(dirty) > 0 {
		t.Errorf("%s: anchors left dirty after the write: %v", stage, dirty)
	}
	listed, err := subagentBackfillListed(s.reader(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	if !listed {
		// A carrier stays unstamped, and walked, until the prompt that
		// opens its round arrives.
		unstamped, err := subagentAnchorIDs(s.reader(), `SELECT l.id FROM (`+subagentLegacyAnchorsSQL+`) AS l
		  CROSS JOIN items a ON a.thread_id = ?1 AND a.id = l.id
		 WHERE NOT `+aggCarrierSQL("a."), threadID, 100)
		if err != nil {
			t.Fatalf("%s: legacy probe: %v", stage, err)
		}
		if len(unstamped) > 0 {
			t.Errorf("%s: anchors a read decorates carry no stamp: %v", stage, unstamped)
		}
	}
}

// subagentStampStateForTest reads a row's generation and mode; gen is -1
// for an unstamped row.
func subagentStampStateForTest(t *testing.T, s *Store, threadID, id string) (int64, subagentStampMode) {
	t.Helper()
	var meta string
	if err := s.db.QueryRow(`SELECT meta FROM items WHERE thread_id = ? AND id = ?`, threadID, id).Scan(&meta); err != nil {
		t.Fatalf("read %s/%s: %v", threadID, id, err)
	}
	mode := subagentStampModeOf(meta)
	if mode == subagentUnstamped {
		return -1, mode
	}
	var gen int64
	if err := s.db.QueryRow(`SELECT COALESCE(`+aggJX("meta", aggGenPath)+`, 0) FROM items WHERE thread_id = ? AND id = ?`,
		threadID, id).Scan(&gen); err != nil {
		t.Fatalf("read gen %s/%s: %v", threadID, id, err)
	}
	return gen, mode
}

type stampFixtureRow struct {
	id, kind, tool, summary, parent, meta, status string
	turn, index                                   int
	background                                    bool
	completionOf                                  string
}

func (r stampFixtureRow) item(threadID string) Item {
	status := r.status
	if status == "" {
		status = "completed"
	}
	role := "assistant"
	if r.kind == "user_text" {
		role = "user"
	}
	return Item{
		ID: r.id, ThreadID: threadID, TurnIndex: r.turn, ItemIndex: r.index,
		Kind: r.kind, Role: role, ToolName: r.tool, Status: status, Summary: r.summary,
		ParentID: r.parent, Meta: r.meta, IsBackground: r.background, CompletionOf: r.completionOf,
		CreatedAt: int64(1_000 + r.turn*1_000 + r.index), UpdatedAt: int64(1_000 + r.turn*1_000 + r.index),
	}
}

func resumePromptMeta(carrierID string) string {
	if carrierID == "" {
		return fmt.Sprintf(`{"wire_only":true,%q:true}`, metaKeySubagentResumePrompt)
	}
	return fmt.Sprintf(`{"wire_only":true,%q:true,%q:%q}`, metaKeySubagentResumePrompt, metaKeyResumeCarrierID, carrierID)
}

func carrierMeta(rootID string) string {
	return fmt.Sprintf(`{%q:%q}`, metaKeyTranscriptRootID, rootID)
}

// TestSubagentAggregateStampsMatchTheReadTimeAggregator drives the
// shapes the stamps must keep through every kind of write, and after each
// one compares what every row serves with what the read-time aggregator
// computes from the rows: a plain launch, a nested launch, a §E6 root
// with two carrier rounds and a wake prompt, a detached launch with its
// completion sibling, plan_update notifications, and an imported chunk
// with a local child under an imported launch. The steps the triggers
// keep incrementally must also leave the stamp's generation where it was:
// a Go recompute would move it, and would hide a trigger arm that
// stopped working behind a correct result.
func TestSubagentAggregateStampsMatchTheReadTimeAggregator(t *testing.T) {
	s := newTestStore(t)
	const thread = "t-parity"
	newImportTargetThread(t, s, thread)
	imported := func(r stampFixtureRow) ImportRow { return ImportRow{Item: r.item(thread)} }
	if err := s.ApplyImportBatch(thread, ImportBatch{
		Turns: []Turn{{TurnID: thread + ":0", ThreadID: thread, TurnIndex: 0, StartedAt: 1_000}},
		Rows: []ImportRow{
			imported(stampFixtureRow{id: "imp-user", kind: "user_text", summary: "imported ask"}),
			imported(stampFixtureRow{id: "imp-launch", kind: "tool_call", tool: "Agent", summary: "Agent: imported", index: 1}),
			imported(stampFixtureRow{id: "imp-c1", kind: "assistant_text", summary: "imported child", parent: "imp-launch", index: 2}),
			imported(stampFixtureRow{id: "imp-c2", kind: "tool_call", tool: "Grep", summary: "grep x", parent: "imp-launch", index: 3}),
			imported(stampFixtureRow{id: "imp-plan", kind: "notification", tool: "plan_update", summary: "plan", parent: "imp-launch", index: 4}),
		},
	}); err != nil {
		t.Fatalf("apply import batch: %v", err)
	}
	assertSubagentStampParity(t, s, thread, "imported chunk", true)

	insert := func(r stampFixtureRow) {
		t.Helper()
		if err := s.InsertItem(r.item(thread)); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
	gens := func(ids ...string) map[string]int64 {
		out := make(map[string]int64, len(ids))
		for _, id := range ids {
			out[id], _ = subagentStampStateForTest(t, s, thread, id)
		}
		return out
	}
	// incremental asserts the trigger kept these stamps without a
	// recompute: clean, and at the generation they had before the step.
	// An unstamped row before the step must now be at generation 0, which
	// only the trigger's init writes.
	incremental := func(stage string, before map[string]int64) {
		t.Helper()
		for id, was := range before {
			gen, mode := subagentStampStateForTest(t, s, thread, id)
			want := was
			if was < 0 {
				want = 0
			}
			if mode != subagentStampClean || gen != want {
				t.Errorf("%s: %s is mode %d at gen %d, want clean at gen %d (kept by the trigger)", stage, id, mode, gen, want)
			}
		}
	}
	step := func(stage string, keep []string, write func()) {
		t.Helper()
		before := gens(keep...)
		write()
		assertSubagentStampParity(t, s, thread, stage, true)
		incremental(stage, before)
	}
	summary := func(id, text string) func() {
		return func() {
			t.Helper()
			if _, err := s.UpdateItemFields(thread, id, ItemPartialUpdate{Summary: &text}); err != nil {
				t.Fatalf("update summary %s: %v", id, err)
			}
		}
	}
	appendSummary := func(id, delta string) func() {
		return func() {
			t.Helper()
			if _, err := s.AppendItemSummary(thread, id, delta, 5_000); err != nil {
				t.Fatalf("append summary %s: %v", id, err)
			}
		}
	}
	status := func(id, value string) func() {
		return func() {
			t.Helper()
			if _, err := s.UpdateItemFields(thread, id, ItemPartialUpdate{Status: &value}); err != nil {
				t.Fatalf("update status %s: %v", id, err)
			}
		}
	}
	add := func(r stampFixtureRow) func() { return func() { t.Helper(); insert(r) } }
	remove := func(id string) func() {
		return func() {
			t.Helper()
			if err := s.DeleteThreadItem(thread, id); err != nil {
				t.Fatalf("delete %s: %v", id, err)
			}
		}
	}

	// A plain launch.
	step("launch", nil, add(stampFixtureRow{id: "L", kind: "tool_call", tool: "Agent", summary: "Agent: plain", status: "running", turn: 1}))
	step("launch first child", []string{"L"}, add(stampFixtureRow{id: "L-a1", kind: "assistant_text", summary: "thinking", parent: "L", status: "streaming", turn: 1, index: 1}))
	step("launch tool child", []string{"L"}, add(stampFixtureRow{id: "L-b1", kind: "tool_call", tool: "Bash", summary: "Bash: ls", parent: "L", turn: 1, index: 2}))
	step("launch blank tool", []string{"L"}, add(stampFixtureRow{id: "L-b2", kind: "tool_call", tool: "Bash", summary: " \t", parent: "L", turn: 1, index: 3}))
	step("launch plan_update", []string{"L"}, add(stampFixtureRow{id: "L-plan", kind: "notification", tool: "plan_update", summary: "plan", parent: "L", turn: 1, index: 4}))
	step("older summary grows", []string{"L"}, appendSummary("L-a1", " harder"))
	step("pick summary changes", []string{"L"}, summary("L-b1", "Bash: ls -la"))
	step("blank tool gains text", []string{"L"}, summary("L-b2", "Bash: pwd"))
	step("child status flip", []string{"L"}, status("L-b1", "errored"))
	step("launch status flip", []string{"L"}, status("L", "completed"))

	// A nested launch counts toward its parent's card.
	step("nested launch", []string{"L"}, add(stampFixtureRow{id: "N", kind: "tool_call", tool: "Agent", summary: "Agent: nested", parent: "L", status: "running", turn: 1, index: 5}))
	step("nested first child", []string{"L", "N"}, add(stampFixtureRow{id: "N-a1", kind: "assistant_text", summary: "nested work", parent: "N", turn: 1, index: 6}))
	step("nested tool", []string{"L", "N"}, add(stampFixtureRow{id: "N-b1", kind: "tool_call", tool: "Read", summary: "Read: file", parent: "N", status: "streaming", turn: 1, index: 7}))
	step("nested tool streams", []string{"L", "N"}, appendSummary("N-b1", " more"))

	// A §E6 root resumed twice, then woken without a carrier.
	step("root", nil, add(stampFixtureRow{id: "R", kind: "tool_call", tool: "Agent", summary: "Agent: root", turn: 2}))
	step("root first child", []string{"R"}, add(stampFixtureRow{id: "R-a1", kind: "assistant_text", summary: "round one", parent: "R", turn: 2, index: 1}))
	step("carrier one", []string{"R"}, add(stampFixtureRow{id: "C1", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("R"), turn: 3}))
	step("prompt one", []string{"R"}, add(stampFixtureRow{id: "P1", kind: "user_text", summary: "again", parent: "R", meta: resumePromptMeta("C1"), turn: 3, index: 1}))
	step("round two child", []string{"R", "C1"}, add(stampFixtureRow{id: "R-a2", kind: "assistant_text", summary: "round two", parent: "R", turn: 3, index: 2}))
	step("round two tool", []string{"R", "C1"}, add(stampFixtureRow{id: "R-b2", kind: "tool_call", tool: "Bash", summary: "Bash: make", parent: "R", status: "streaming", turn: 3, index: 3}))
	step("round two pick streams", []string{"R", "C1"}, appendSummary("R-b2", " test"))
	step("carrier two", []string{"R", "C1"}, add(stampFixtureRow{id: "C2", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("R"), turn: 4}))
	step("prompt two", []string{"R", "C1"}, add(stampFixtureRow{id: "P2", kind: "user_text", summary: "once more", parent: "R", meta: resumePromptMeta("C2"), turn: 4, index: 1}))
	step("round three child", []string{"R", "C1", "C2"}, add(stampFixtureRow{id: "R-a3", kind: "assistant_text", summary: "round three", parent: "R", turn: 4, index: 2}))
	step("wake prompt", []string{"R", "C1", "C2"}, add(stampFixtureRow{id: "W", kind: "user_text", summary: "wake", parent: "R", meta: resumePromptMeta(""), turn: 5}))
	step("woken child", []string{"R", "C1", "C2"}, add(stampFixtureRow{id: "R-a4", kind: "assistant_text", summary: "woken", parent: "R", turn: 5, index: 1}))

	// A detached launch and its completion sibling, which carries the
	// launch's card.
	step("detached launch", nil, add(stampFixtureRow{id: "B", kind: "tool_call", tool: "Agent", summary: "Agent: detached", status: "running", background: true, turn: 6}))
	step("detached child", []string{"B"}, add(stampFixtureRow{id: "B-a1", kind: "assistant_text", summary: "detached work", parent: "B", turn: 6, index: 1}))
	step("detached tool", []string{"B"}, add(stampFixtureRow{id: "B-b1", kind: "tool_call", tool: "Bash", summary: "Bash: sleep", parent: "B", turn: 6, index: 2}))
	step("completion sibling", []string{"B"}, func() {
		if _, err := s.AppendCompletionItem(Item{ID: "B", ThreadID: thread},
			stampFixtureRow{id: "B-done", kind: "tool_completion", tool: "Agent", summary: "done", turn: 7}.item(thread), nil); err != nil {
			t.Fatalf("append completion: %v", err)
		}
	})
	step("late detached child", []string{"B"}, add(stampFixtureRow{id: "B-a2", kind: "error", summary: "late", parent: "B", turn: 7, index: 5}))
	step("detached blank tool", []string{"B"}, add(stampFixtureRow{id: "B-b2", kind: "tool_call", tool: "Bash", parent: "B", turn: 7, index: 6}))

	// A local child under the imported launch shadows the launch into
	// the overlay, where it is stamped like a local one.
	step("child under imported launch", nil, add(stampFixtureRow{id: "imp-local", kind: "tool_call", tool: "Bash", summary: "Bash: local", parent: "imp-launch", turn: 8}))
	if gen, mode := subagentStampStateForTest(t, s, thread, "imp-launch"); mode != subagentStampClean {
		t.Fatalf("shadowed imported launch is mode %d gen %d, want clean", mode, gen)
	}
	step("second child under shadowed launch", []string{"imp-launch"}, add(stampFixtureRow{id: "imp-local-2", kind: "assistant_text", summary: "local text", parent: "imp-launch", turn: 8, index: 1}))

	// Deletes that a trigger can apply, then ones that need a recompute.
	step("delete an older child", []string{"L"}, remove("L-a1"))
	step("delete the pick", nil, remove("N-b1"))
	// B's newest row is B-b2 (blank, so neither preview nor tray); B-b1
	// is only its tray row and B-a2 only its preview.
	step("delete the tray row", nil, remove("B-b1"))
	step("delete a preview that is not the newest", nil, remove("B-a2"))
	step("delete the transcript's newest", nil, remove("R-a4"))
	step("delete a round row", nil, remove("R-a2"))
	step("delete a prompt", nil, remove("P2"))
	step("delete a nested launch", nil, remove("N"))

	// A write the store did not settle (raw SQL moving a row between
	// launches) leaves anchors dirty; reads walk them until the recompute.
	if _, err := s.db.Exec(`UPDATE items SET parent_id = 'B' WHERE thread_id = ? AND id = 'L-b1'`, thread); err != nil {
		t.Fatalf("re-parent: %v", err)
	}
	for _, id := range []string{"L", "B"} {
		if _, mode := subagentStampStateForTest(t, s, thread, id); mode != subagentStampWalk {
			t.Errorf("re-parent left %s mode %d, want dirty", id, mode)
		}
	}
	assertSubagentStampParity(t, s, thread, "re-parented, unsettled", false)
	if _, err := s.RecomputeSubagentAggregates(t.Context(), thread, 16); err != nil {
		t.Fatalf("recompute: %v", err)
	}
	assertSubagentStampParity(t, s, thread, "re-parented, recomputed", true)

	// Every anchor the fixture names ends on a stamp a page serves as
	// stored: the parity above was not won by walking everything. C2 lost
	// the prompt that named it, so it shows the whole transcript, which
	// only the walk computes.
	for _, id := range []string{"L", "R", "C1", "B", "imp-launch"} {
		if _, mode := subagentStampStateForTest(t, s, thread, id); mode != subagentStampClean {
			t.Errorf("%s ends mode %d, want clean", id, mode)
		}
	}
	if _, mode := subagentStampStateForTest(t, s, thread, "C2"); mode != subagentStampWalk {
		t.Errorf("unnamed carrier C2 ends mode %d, want readTime", mode)
	}
}
