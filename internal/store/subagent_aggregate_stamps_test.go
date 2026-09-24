package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// setSubagentStampStateForTest moves an anchor's stamp to state, keeping
// its values: the state a trigger or a recompute leaves, written directly.
func setSubagentStampStateForTest(t *testing.T, s *Store, threadID, id string, state int) {
	t.Helper()
	result, err := s.db.Exec(`UPDATE subagent_aggregates SET state = ? WHERE thread_id = ? AND item_id = ?`, state, threadID, id)
	if err != nil {
		t.Fatalf("set stamp state %s/%s: %v", threadID, id, err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("set stamp state %s/%s: %d rows (%v)", threadID, id, n, err)
	}
}

// stripSubagentStampsForTest leaves rows as a store before v121 held
// them: no stamp. No ids strips the whole thread.
func stripSubagentStampsForTest(t *testing.T, s *Store, threadID string, ids ...string) {
	t.Helper()
	where, args := "thread_id = ?", []any{threadID}
	if len(ids) > 0 {
		clause, idArgs := inClause("item_id", ids)
		where += " AND " + clause
		args = append(args, idArgs...)
	}
	if _, err := s.db.Exec(`DELETE FROM subagent_aggregates WHERE `+where, args...); err != nil {
		t.Fatalf("strip stamps: %v", err)
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

// servedRevsForTest reads every row's revision as the page serves it.
func servedRevsForTest(t *testing.T, s *Store, threadID string) map[string]int64 {
	t.Helper()
	rows, err := s.listWireItemsTx(s.reader(), threadID, threadTimelineIDsForTest(t, s.reader(), threadID))
	if err != nil {
		t.Fatalf("read wire items: %v", err)
	}
	out := make(map[string]int64, len(rows))
	for _, row := range rows {
		out[row.ID] = row.Rev
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
	if _, err := tx.Exec(`DELETE FROM subagent_aggregates WHERE thread_id = ?`, threadID); err != nil {
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

// subagentStampMode is how a read treats a local anchorable row.
type subagentStampMode int

const (
	// subagentUnstamped: no stamp row.
	subagentUnstamped subagentStampMode = iota
	// subagentStampClean: the stamp is the read.
	subagentStampClean
	// subagentStampWalk: dirty or readTime; the read-time aggregator
	// answers.
	subagentStampWalk
)

// subagentStampStateForTest reads a row's generation and mode; gen is -1
// for an unstamped row.
func subagentStampStateForTest(t *testing.T, s *Store, threadID, id string) (int64, subagentStampMode) {
	t.Helper()
	var gen, state int64
	err := s.db.QueryRow(`SELECT gen, state FROM subagent_aggregates WHERE thread_id = ? AND item_id = ?`,
		threadID, id).Scan(&gen, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return -1, subagentUnstamped
	}
	if err != nil {
		t.Fatalf("read stamp %s/%s: %v", threadID, id, err)
	}
	if state == aggStateClean {
		return gen, subagentStampClean
	}
	return gen, subagentStampWalk
}

// subagentStampRowForTest reads a row's whole stamp and generation.
func subagentStampRowForTest(t *testing.T, s *Store, threadID, id string) (subagentStampValues, int64) {
	t.Helper()
	targets, err := subagentStampTargets(s.db, threadID, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	target, ok := targets[id]
	if !ok || !target.stamped {
		t.Fatalf("%s/%s carries no stamp", threadID, id)
	}
	gen, _ := subagentStampStateForTest(t, s, threadID, id)
	return target.stored, gen
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
	// A row whose served card a write changed must be served at a new
	// revision, or a client holding it could prove a stale card fresh:
	// the stamp's own row and every completion sibling borrowing it.
	step := func(stage string, keep []string, write func()) {
		t.Helper()
		before := gens(keep...)
		cardsBefore := subagentCardsForTest(t, s, s.reader(), thread)
		revsBefore := servedRevsForTest(t, s, thread)
		write()
		assertSubagentStampParity(t, s, thread, stage, true)
		incremental(stage, before)
		cardsAfter := subagentCardsForTest(t, s, s.reader(), thread)
		revsAfter := servedRevsForTest(t, s, thread)
		for id, card := range cardsAfter {
			was, existed := cardsBefore[id]
			if existed && !mapsEqual(was, card) && revsAfter[id] == revsBefore[id] {
				t.Errorf("%s: %s's card changed %v -> %v at the same revision %d", stage, id, was, card, revsAfter[id])
			}
		}
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

	// Top-level rows no parent chain reaches: a launch inserted after its
	// children adopts them, and a launch whose meta later names a
	// transcript root becomes that root's carrier.
	step("orphan child", nil, add(stampFixtureRow{id: "O-a1", kind: "assistant_text", summary: "early", parent: "O", turn: 9, index: 1}))
	step("orphan tool", nil, add(stampFixtureRow{id: "O-b1", kind: "tool_call", tool: "Bash", summary: "Bash: early", parent: "O", turn: 9, index: 2}))
	step("launch adopts its children", nil, add(stampFixtureRow{id: "O", kind: "tool_call", tool: "Agent", summary: "Agent: late", turn: 9}))
	step("second root", nil, add(stampFixtureRow{id: "Q", kind: "tool_call", tool: "Agent", summary: "Agent: second root", turn: 10}))
	step("launch becomes a carrier", nil, func() {
		meta := carrierMeta("Q")
		if _, err := s.UpdateItemFields(thread, "O", ItemPartialUpdate{Meta: &meta}); err != nil {
			t.Fatalf("update meta O: %v", err)
		}
	})
	step("top-level text", nil, add(stampFixtureRow{id: "T-top", kind: "assistant_text", summary: "top level", turn: 11}))

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

	// Moves across the top level change the card of a launch on one side
	// of the move only: a child leaving B, and a top-level row joining L.
	for _, move := range [][2]string{{"B-a1", ""}, {"T-top", "L"}} {
		if _, err := s.db.Exec(`UPDATE items SET parent_id = ? WHERE thread_id = ? AND id = ?`, move[1], thread, move[0]); err != nil {
			t.Fatalf("move %s: %v", move[0], err)
		}
	}
	for _, id := range []string{"B", "L"} {
		if _, mode := subagentStampStateForTest(t, s, thread, id); mode != subagentStampWalk {
			t.Errorf("move across the top level left %s mode %d, want dirty", id, mode)
		}
	}
	assertSubagentStampParity(t, s, thread, "moved across the top level, unsettled", false)
	if _, err := s.RecomputeSubagentAggregates(t.Context(), thread, 16); err != nil {
		t.Fatalf("recompute: %v", err)
	}
	assertSubagentStampParity(t, s, thread, "moved across the top level, recomputed", true)

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

// TestSubagentAggregateChainedCarrierJoinsItsRootsFamily pins the
// recompute of a carrier whose transcript root is itself a carrier: a
// round resumed from a resumed round. Seeded alone, as a backfill batch
// can seed it, it brings in the carrier it names and that carrier's own
// root, so the named carrier is stamped from its root's rounds instead of
// being written readTime as a root, which would put every read of it and
// of its completion sibling on a walk of the root's whole transcript.
func TestSubagentAggregateChainedCarrierJoinsItsRootsFamily(t *testing.T) {
	s := newTestStore(t)
	const thread = "t-chained"
	mustCreateThread(t, s, thread)
	for _, r := range []stampFixtureRow{
		{id: "R", kind: "tool_call", tool: "Agent", summary: "Agent: root", turn: 1},
		{id: "R-a1", kind: "assistant_text", summary: "round one", parent: "R", turn: 1, index: 1},
		{id: "C1", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("R"), turn: 2},
		{id: "P1", kind: "user_text", summary: "again", parent: "R", meta: resumePromptMeta("C1"), turn: 2, index: 1},
		{id: "R-a2", kind: "assistant_text", summary: "round two", parent: "R", turn: 2, index: 2},
		{id: "C2", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("C1"), turn: 3},
		{id: "P2", kind: "user_text", summary: "once more", parent: "C1", meta: resumePromptMeta("C2"), turn: 3, index: 1},
		{id: "C1-a1", kind: "assistant_text", summary: "under the carrier", parent: "C1", turn: 3, index: 2},
	} {
		if err := s.InsertItem(r.item(thread)); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
	if _, err := s.AppendCompletionItem(Item{ID: "C1", ThreadID: thread},
		stampFixtureRow{id: "C1-done", kind: "tool_completion", tool: "SendMessage", summary: "done", turn: 4}.item(thread), nil); err != nil {
		t.Fatalf("append completion: %v", err)
	}
	stripSubagentStampsForTest(t, s, thread)
	mustExec(t, s.db, `INSERT INTO subagent_aggregate_backfill(thread_id) VALUES (?)`, thread)

	writes, err := computeSubagentStamps(s.reader(), thread, []string{"C2"})
	if err != nil {
		t.Fatal(err)
	}
	states := make(map[string]int64, len(writes))
	for _, write := range writes {
		states[write.id] = write.values.State
	}
	want := map[string]int64{"R": aggStateClean, "C1": aggStateClean, "C2": aggStateReadTime}
	if !reflect.DeepEqual(states, want) {
		t.Fatalf("a batch seeded with the chained carrier writes %v, want %v", states, want)
	}

	for calls := 0; ; calls++ {
		if calls > 10 {
			t.Fatal("backfill does not finish")
		}
		result, err := s.RecomputeSubagentAggregates(t.Context(), thread, 1)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Remaining {
			break
		}
	}
	assertSubagentStampParity(t, s, thread, "backfilled", true)
	for id, mode := range map[string]subagentStampMode{"R": subagentStampClean, "C1": subagentStampClean, "C2": subagentStampWalk} {
		if _, got := subagentStampStateForTest(t, s, thread, id); got != mode {
			t.Errorf("%s ends mode %d, want %d", id, got, mode)
		}
	}
	completion, found, err := s.GetThreadItem(thread, "C1-done")
	if err != nil || !found {
		t.Fatalf("read completion: found=%v err=%v", found, err)
	}
	if needs, err := s.ItemReadNeedsDecoration(completion); err != nil || !needs {
		t.Fatalf("completion needs decoration = %v (%v); completions always go through the page read", needs, err)
	}
	if card := subagentCardOf(t, completion.Meta); !mapsEqual(card, subagentCard{"subagentDescendantCount": float64(2)}) {
		t.Errorf("the completion's read-back is %v, want C1's round under R", card)
	}

	// The walk gives C1 its round under R in every window: alone, with
	// the round resumed from it, and with that round's carrier ahead of it.
	walkWindows := func(stage string) {
		t.Helper()
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.Exec(`DELETE FROM subagent_aggregates WHERE thread_id = ?`, thread); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO subagent_aggregate_backfill(thread_id) VALUES (?)`, thread); err != nil {
			t.Fatal(err)
		}
		full := subagentCardsForTest(t, s, tx, thread)
		for _, window := range [][]string{{"C1"}, {"C1-done"}, {"C1", "C2"}, {"C2", "C1-done"}, {"C2", "C1"}, {"R", "C2", "C1-done"}} {
			rows, err := s.listWireItemsTx(tx, thread, window)
			if err != nil {
				t.Fatal(err)
			}
			byID := make(map[string]Item, len(rows))
			for _, row := range rows {
				byID[row.ID] = row
			}
			ordered := make([]Item, 0, len(window))
			for _, id := range window {
				ordered = append(ordered, byID[id])
			}
			decorated, err := s.decorateSubagentAnchors(tx, thread, ordered)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range decorated {
				if row.ID == "C1" || row.ID == "C1-done" {
					if got := subagentCardOf(t, row.Meta); !mapsEqual(got, full[row.ID]) {
						t.Errorf("%s: window %v walks %s as %v, the whole thread as %v", stage, window, row.ID, got, full[row.ID])
					}
				}
			}
		}
	}
	walkWindows("backfilled")

	// Writes after the backfill: a row under C1 is in no card C1 shows,
	// and a row under R extends C1's round.
	for _, r := range []stampFixtureRow{
		{id: "C1-a2", kind: "assistant_text", summary: "under the carrier again", parent: "C1", turn: 5, index: 1},
		{id: "R-a3", kind: "assistant_text", summary: "round two goes on", parent: "R", turn: 5, index: 2},
	} {
		if err := s.InsertItem(r.item(thread)); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
		assertSubagentStampParity(t, s, thread, "after "+r.id, true)
	}
	if _, mode := subagentStampStateForTest(t, s, thread, "C1"); mode != subagentStampClean {
		t.Errorf("C1 ends mode %d after the writes, want clean", mode)
	}
	walkWindows("after writes")
}
