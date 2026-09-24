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

// stampFixtureAnchor is the anchor a live writer names for a row under
// parent: the parent itself when it is a subagent launch in the thread's
// timeline, as triage names it (Router.subagentAnchorFor). A row whose
// parent has not arrived names none.
func stampFixtureAnchor(t *testing.T, s *Store, threadID, parent string) string {
	t.Helper()
	if parent == "" {
		return ""
	}
	row, found, err := s.GetThreadItem(threadID, parent)
	if err != nil {
		t.Fatalf("read parent %s/%s: %v", threadID, parent, err)
	}
	if !found || row.Kind != "tool_call" || row.ToolName == "collab_agent" {
		return ""
	}
	return parent
}

// subagentStampRowsForTest reads a thread's stamp rows without their
// generation, which only says which path wrote a row.
func subagentStampRowsForTest(t *testing.T, s *Store, threadID string) map[string]subagentStampValues {
	t.Helper()
	rows, err := s.reader().Query(`SELECT item_id, state, `+strings.Join(subagentAggregateValueColumns, ", ")+`
	  FROM subagent_aggregates WHERE thread_id = ?`, threadID)
	if err != nil {
		t.Fatalf("read stamp rows: %v", err)
	}
	defer rows.Close()
	out := make(map[string]subagentStampValues)
	for rows.Next() {
		var id string
		var values subagentStampValues
		if err := rows.Scan(append([]any{&id, &values.State}, values.scanTargets()...)...); err != nil {
			t.Fatalf("scan stamp row: %v", err)
		}
		out[id] = values
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate stamp rows: %v", err)
	}
	return out
}

// assertStampsAreTheRecompute recomputes every stamped anchor of the
// thread and fails on each whose stored row differs from what the
// recompute derives, internal columns included. An unstamped row is left
// out: a carrier stays unstamped until its prompt arrives.
func assertStampsAreTheRecompute(t *testing.T, s *Store, threadID, stage string) {
	t.Helper()
	stored := subagentStampRowsForTest(t, s, threadID)
	ids := make([]string, 0, len(stored))
	for id := range stored {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	writes, err := computeSubagentStamps(s.reader(), threadID, ids)
	if err != nil {
		t.Fatalf("%s: recompute: %v", stage, err)
	}
	for _, write := range writes {
		if was, ok := stored[write.id]; ok {
			t.Errorf("%s: %s is stored as %+v, the recompute derives %+v", stage, write.id, was, write.values)
		}
	}
}

// stampParityStore is one of the two stores the parity test drives with
// the same writes: one names each row's anchor, so the keyed writes keep
// the stamps, the other names none, so every stamp is the recompute's.
type stampParityStore struct {
	name    string
	claimed bool
	s       *Store
}

// TestSubagentAggregateStampsMatchTheReadTimeAggregator drives the shapes
// the stamps must keep through every kind of write, on two stores: one
// whose writes name their anchors (the keyed writes) and one whose writes
// name none (the triggers' marks and the settle's recompute). After each
// write both must serve what the read-time aggregator computes from the
// rows, hold the same stamp rows, internal columns included, and hold
// exactly what a recompute of every stamped anchor derives. The shapes: a
// plain launch, a nested launch, a §E6 root with two carrier rounds and a
// wake prompt, a detached launch with its completion sibling, plan_update
// notifications, and an imported chunk with a local child under an
// imported launch. Stamps the keyed writes keep must also stay at their
// generation, which only a recompute moves, so a keyed rule that stopped
// working cannot hide behind the settle.
func TestSubagentAggregateStampsMatchTheReadTimeAggregator(t *testing.T) {
	const thread = "t-parity"
	stores := []*stampParityStore{{name: "keyed", claimed: true}, {name: "recomputed"}}
	for _, p := range stores {
		p.s = newTestStore(t)
		s := p.s
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
			t.Fatalf("%s: apply import batch: %v", p.name, err)
		}
		assertSubagentStampParity(t, s, thread, p.name+": imported chunk", true)
	}

	anchorOf := func(p *stampParityStore, parent string) string {
		t.Helper()
		if !p.claimed {
			return ""
		}
		return stampFixtureAnchor(t, p.s, thread, parent)
	}
	parentOf := func(p *stampParityStore, id string) string {
		t.Helper()
		row, found, err := p.s.GetThreadItem(thread, id)
		if err != nil || !found {
			t.Fatalf("%s: read %s: found=%v err=%v", p.name, id, found, err)
		}
		return row.ParentID
	}
	gens := func(s *Store, ids ...string) map[string]int64 {
		out := make(map[string]int64, len(ids))
		for _, id := range ids {
			out[id], _ = subagentStampStateForTest(t, s, thread, id)
		}
		return out
	}
	// incremental asserts the keyed writes kept these stamps without a
	// recompute: clean, and at the generation they had before the step.
	// A row unstamped before the step must now be at generation 0, which
	// only a keyed write leaves.
	incremental := func(p *stampParityStore, stage string, before map[string]int64) {
		t.Helper()
		for id, was := range before {
			gen, mode := subagentStampStateForTest(t, p.s, thread, id)
			want := max(was, 0)
			if mode != subagentStampClean || gen != want {
				t.Errorf("%s: %s: %s is mode %d at gen %d, want clean at gen %d (kept by the keyed writes)",
					p.name, stage, id, mode, gen, want)
			}
		}
	}
	// step runs one write on both stores. keep names the stamps the keyed
	// writes must keep on their own. A row whose served card a write
	// changed must be served at a new revision, or a client holding it
	// could prove a stale card fresh: the stamp's own row and every
	// completion sibling borrowing it.
	step := func(stage string, keep []string, write func(p *stampParityStore)) {
		t.Helper()
		for _, p := range stores {
			s := p.s
			before := gens(s, keep...)
			cardsBefore := subagentCardsForTest(t, s, s.reader(), thread)
			revsBefore := servedRevsForTest(t, s, thread)
			write(p)
			assertSubagentStampParity(t, s, thread, p.name+": "+stage, true)
			assertStampsAreTheRecompute(t, s, thread, p.name+": "+stage)
			if p.claimed {
				incremental(p, stage, before)
			}
			cardsAfter := subagentCardsForTest(t, s, s.reader(), thread)
			revsAfter := servedRevsForTest(t, s, thread)
			for id, card := range cardsAfter {
				was, existed := cardsBefore[id]
				if existed && !mapsEqual(was, card) && revsAfter[id] == revsBefore[id] {
					t.Errorf("%s: %s: %s's card changed %v -> %v at the same revision %d", p.name, stage, id, was, card, revsAfter[id])
				}
			}
		}
		keyed, recomputed := subagentStampRowsForTest(t, stores[0].s, thread), subagentStampRowsForTest(t, stores[1].s, thread)
		if !reflect.DeepEqual(keyed, recomputed) {
			ids := make([]string, 0, len(keyed)+len(recomputed))
			for id := range keyed {
				ids = append(ids, id)
			}
			for id := range recomputed {
				if _, ok := keyed[id]; !ok {
					ids = append(ids, id)
				}
			}
			slices.Sort(ids)
			for _, id := range ids {
				k, kok := keyed[id]
				r, rok := recomputed[id]
				if kok != rok || k != r {
					t.Errorf("%s: %s is %+v (stamped %v) by the keyed writes, %+v (stamped %v) by the recompute", stage, id, k, kok, r, rok)
				}
			}
		}
	}
	add := func(r stampFixtureRow) func(*stampParityStore) {
		return func(p *stampParityStore) {
			t.Helper()
			item := r.item(thread)
			item.SubagentAnchor = anchorOf(p, r.parent)
			if err := p.s.InsertItem(item); err != nil {
				t.Fatalf("%s: insert %s: %v", p.name, r.id, err)
			}
		}
	}
	update := func(id string, update ItemPartialUpdate) func(*stampParityStore) {
		return func(p *stampParityStore) {
			t.Helper()
			update.SubagentAnchor = anchorOf(p, parentOf(p, id))
			if _, err := p.s.UpdateItemFields(thread, id, update); err != nil {
				t.Fatalf("%s: update %s: %v", p.name, id, err)
			}
		}
	}
	summary := func(id, text string) func(*stampParityStore) { return update(id, ItemPartialUpdate{Summary: &text}) }
	status := func(id, value string) func(*stampParityStore) { return update(id, ItemPartialUpdate{Status: &value}) }
	// An append names no anchor: the streaming writers append only to
	// text and thinking rows, which no card previews.
	appendSummary := func(id, delta string) func(*stampParityStore) {
		return func(p *stampParityStore) {
			t.Helper()
			if _, err := p.s.AppendItemSummary(thread, id, delta, 5_000); err != nil {
				t.Fatalf("%s: append summary %s: %v", p.name, id, err)
			}
		}
	}
	remove := func(id string) func(*stampParityStore) {
		return func(p *stampParityStore) {
			t.Helper()
			if err := p.s.DeleteThreadItem(thread, id); err != nil {
				t.Fatalf("%s: delete %s: %v", p.name, id, err)
			}
		}
	}
	// upsert rewrites a whole row through UpsertItem, the path triage's
	// persist takes for a row that exists.
	upsert := func(id string, change func(*Item)) func(*stampParityStore) {
		return func(p *stampParityStore) {
			t.Helper()
			row, found, err := p.s.GetThreadItem(thread, id)
			if err != nil || !found {
				t.Fatalf("%s: read %s: found=%v err=%v", p.name, id, found, err)
			}
			change(&row)
			row.SubagentAnchor = anchorOf(p, row.ParentID)
			if _, err := p.s.UpsertItem(row, nil); err != nil {
				t.Fatalf("%s: upsert %s: %v", p.name, id, err)
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
	step("pick goes blank", nil, summary("L-b2", "  "))
	step("blank pick gains text again", []string{"L"}, summary("L-b2", "Bash: pwd -P"))
	step("child status flip", []string{"L"}, status("L-b1", "errored"))
	step("child rewritten whole", []string{"L"}, upsert("L-b1", func(row *Item) { row.Summary, row.Status = "Bash: ls -la /", "completed" }))
	step("launch status flip", []string{"L"}, status("L", "completed"))

	// A nested launch counts toward its parent's card.
	step("nested launch", []string{"L"}, add(stampFixtureRow{id: "N", kind: "tool_call", tool: "Agent", summary: "Agent: nested", parent: "L", status: "running", turn: 1, index: 5}))
	step("nested first child", []string{"L", "N"}, add(stampFixtureRow{id: "N-a1", kind: "assistant_text", summary: "nested work", parent: "N", turn: 1, index: 6}))
	step("nested tool", []string{"L", "N"}, add(stampFixtureRow{id: "N-b1", kind: "tool_call", tool: "Read", summary: "Read: file", parent: "N", status: "streaming", turn: 1, index: 7}))
	step("nested tool summary", []string{"L", "N"}, summary("N-b1", "Read: file.go"))
	step("nested tool appends", nil, appendSummary("N-b1", " more"))

	// A §E6 root resumed twice, then woken without a carrier.
	step("root", nil, add(stampFixtureRow{id: "R", kind: "tool_call", tool: "Agent", summary: "Agent: root", turn: 2}))
	step("root first child", []string{"R"}, add(stampFixtureRow{id: "R-a1", kind: "assistant_text", summary: "round one", parent: "R", turn: 2, index: 1}))
	step("carrier one", []string{"R"}, add(stampFixtureRow{id: "C1", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("R"), turn: 3}))
	step("prompt one", []string{"R", "C1"}, add(stampFixtureRow{id: "P1", kind: "user_text", summary: "again", parent: "R", meta: resumePromptMeta("C1"), turn: 3, index: 1}))
	step("round two child", []string{"R", "C1"}, add(stampFixtureRow{id: "R-a2", kind: "assistant_text", summary: "round two", parent: "R", turn: 3, index: 2}))
	step("round two tool", []string{"R", "C1"}, add(stampFixtureRow{id: "R-b2", kind: "tool_call", tool: "Bash", summary: "Bash: make", parent: "R", status: "streaming", turn: 3, index: 3}))
	step("round two pick changes", []string{"R", "C1"}, summary("R-b2", "Bash: make test"))
	step("carrier two", []string{"R", "C1"}, add(stampFixtureRow{id: "C2", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("R"), turn: 4}))
	step("prompt two", []string{"R", "C1", "C2"}, add(stampFixtureRow{id: "P2", kind: "user_text", summary: "once more", parent: "R", meta: resumePromptMeta("C2"), turn: 4, index: 1}))
	step("round three child", []string{"R", "C1", "C2"}, add(stampFixtureRow{id: "R-a3", kind: "tool_call", tool: "Grep", summary: "Grep: x", parent: "R", turn: 4, index: 2}))
	step("round three pick changes", []string{"R", "C1", "C2"}, summary("R-a3", "Grep: y"))
	step("wake prompt", []string{"R", "C1", "C2"}, add(stampFixtureRow{id: "W", kind: "user_text", summary: "wake", parent: "R", meta: resumePromptMeta(""), turn: 5}))
	step("woken child", []string{"R", "C1", "C2"}, add(stampFixtureRow{id: "R-a4", kind: "assistant_text", summary: "woken", parent: "R", turn: 5, index: 1}))
	// Rows under a carrier itself count toward no card.
	step("text under a carrier", []string{"R", "C1", "C2"}, add(stampFixtureRow{id: "C1-a", kind: "assistant_text", summary: "under the carrier", parent: "C1", turn: 5, index: 2}))
	step("tool under a carrier", []string{"R", "C1", "C2"}, add(stampFixtureRow{id: "C1-b", kind: "tool_call", tool: "Bash", summary: "Bash: under", parent: "C1", turn: 5, index: 3}))
	step("tool under a carrier changes", []string{"R", "C1", "C2"}, summary("C1-b", "Bash: under the carrier"))

	// A detached launch and its completion sibling, which carries the
	// launch's card.
	step("detached launch", nil, add(stampFixtureRow{id: "B", kind: "tool_call", tool: "Agent", summary: "Agent: detached", status: "running", background: true, turn: 6}))
	step("detached child", []string{"B"}, add(stampFixtureRow{id: "B-a1", kind: "assistant_text", summary: "detached work", parent: "B", turn: 6, index: 1}))
	step("detached tool", []string{"B"}, add(stampFixtureRow{id: "B-b1", kind: "tool_call", tool: "Bash", summary: "Bash: sleep", parent: "B", turn: 6, index: 2}))
	step("completion sibling", []string{"B"}, func(p *stampParityStore) {
		if _, err := p.s.AppendCompletionItem(Item{ID: "B", ThreadID: thread},
			stampFixtureRow{id: "B-done", kind: "tool_completion", tool: "Agent", summary: "done", turn: 7}.item(thread), nil); err != nil {
			t.Fatalf("%s: append completion: %v", p.name, err)
		}
	})
	step("late detached child", []string{"B"}, add(stampFixtureRow{id: "B-a2", kind: "error", summary: "late", parent: "B", turn: 7, index: 5}))
	step("detached blank tool", []string{"B"}, add(stampFixtureRow{id: "B-b2", kind: "tool_call", tool: "Bash", parent: "B", turn: 7, index: 6}))
	// B-a2 is B's pick and not its tray: its going blank is the preview
	// rule's alone.
	step("error pick goes blank", nil, summary("B-a2", "  "))
	step("error pick gains text again", []string{"B"}, summary("B-a2", "late again"))
	// B-b1 is B's tray and not its pick.
	step("tray goes blank", nil, summary("B-b1", " "))
	step("tray gains text again", []string{"B"}, summary("B-b1", "Bash: sleep 1"))

	// A local child under the imported launch shadows the launch into
	// the overlay, where it is stamped like a local one.
	step("child under imported launch", nil, add(stampFixtureRow{id: "imp-local", kind: "tool_call", tool: "Bash", summary: "Bash: local", parent: "imp-launch", turn: 8}))
	for _, p := range stores {
		if gen, mode := subagentStampStateForTest(t, p.s, thread, "imp-launch"); mode != subagentStampClean {
			t.Fatalf("%s: shadowed imported launch is mode %d gen %d, want clean", p.name, mode, gen)
		}
	}
	step("second child under shadowed launch", []string{"imp-launch"}, add(stampFixtureRow{id: "imp-local-2", kind: "assistant_text", summary: "local text", parent: "imp-launch", turn: 8, index: 1}))

	// Top-level rows no parent chain reaches: a launch inserted after its
	// children adopts them, and a launch whose meta later names a
	// transcript root becomes that root's carrier.
	step("orphan child", nil, add(stampFixtureRow{id: "O-a1", kind: "assistant_text", summary: "early", parent: "O", turn: 9, index: 1}))
	step("orphan tool", nil, add(stampFixtureRow{id: "O-b1", kind: "tool_call", tool: "Bash", summary: "Bash: early", parent: "O", turn: 9, index: 2}))
	step("launch adopts its children", nil, add(stampFixtureRow{id: "O", kind: "tool_call", tool: "Agent", summary: "Agent: late", turn: 9}))
	step("second root", nil, add(stampFixtureRow{id: "Q", kind: "tool_call", tool: "Agent", summary: "Agent: second root", turn: 10}))
	step("launch becomes a carrier", nil, update("O", ItemPartialUpdate{Meta: new(carrierMeta("Q"))}))
	step("top-level text", nil, add(stampFixtureRow{id: "T-top", kind: "assistant_text", summary: "top level", turn: 11}))

	// A root resumed before any child arrived: the prompt is its first
	// child, which opens its transcript and its carrier's round at once.
	step("childless root", nil, add(stampFixtureRow{id: "S", kind: "tool_call", tool: "Agent", summary: "Agent: quiet", turn: 12}))
	step("childless root's carrier", nil, add(stampFixtureRow{id: "SC", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("S"), turn: 13}))
	step("prompt is the first child", []string{"S", "SC"}, add(stampFixtureRow{id: "SP", kind: "user_text", summary: "go on", parent: "S", meta: resumePromptMeta("SC"), turn: 13, index: 1}))
	step("childless root's second round", []string{"S", "SC"}, add(stampFixtureRow{id: "S-b2", kind: "tool_call", tool: "Bash", summary: "Bash: go", parent: "S", turn: 13, index: 2}))

	// A prompt stored before a child already written cuts that child's
	// round: the rounds are recomputed.
	step("root with late rows", nil, add(stampFixtureRow{id: "U", kind: "tool_call", tool: "Agent", summary: "Agent: late rows", turn: 14}))
	step("late root child", []string{"U"}, add(stampFixtureRow{id: "U-a1", kind: "assistant_text", summary: "first", parent: "U", turn: 14, index: 1}))
	step("late root newer child", []string{"U"}, add(stampFixtureRow{id: "U-b2", kind: "tool_call", tool: "Bash", summary: "Bash: newer", parent: "U", turn: 14, index: 5}))
	step("late root carrier", nil, add(stampFixtureRow{id: "UC", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("U"), turn: 14, index: 6}))
	step("prompt before the newest child", nil, add(stampFixtureRow{id: "UP", kind: "user_text", summary: "resume", parent: "U", meta: resumePromptMeta("UC"), turn: 14, index: 3}))
	step("child after the out-of-order prompt", []string{"U", "UC"}, add(stampFixtureRow{id: "U-a3", kind: "assistant_text", summary: "after", parent: "U", turn: 14, index: 7}))

	// One carrier named by two prompts: the family is readTime.
	step("twice-resumed root", nil, add(stampFixtureRow{id: "V", kind: "tool_call", tool: "Agent", summary: "Agent: twice", turn: 15}))
	step("twice-resumed child", []string{"V"}, add(stampFixtureRow{id: "V-a1", kind: "assistant_text", summary: "v", parent: "V", turn: 15, index: 1}))
	step("twice-named carrier", nil, add(stampFixtureRow{id: "VC", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("V"), turn: 16}))
	step("first prompt naming it", []string{"V", "VC"}, add(stampFixtureRow{id: "VP1", kind: "user_text", summary: "again", parent: "V", meta: resumePromptMeta("VC"), turn: 16, index: 1}))
	step("round child", []string{"V", "VC"}, add(stampFixtureRow{id: "V-b2", kind: "tool_call", tool: "Bash", summary: "Bash: v", parent: "V", turn: 16, index: 2}))
	step("second prompt naming it", nil, add(stampFixtureRow{id: "VP2", kind: "user_text", summary: "again again", parent: "V", meta: resumePromptMeta("VC"), turn: 17, index: 1}))
	step("child of a readTime family", nil, add(stampFixtureRow{id: "V-a3", kind: "assistant_text", summary: "v3", parent: "V", turn: 17, index: 2}))

	// A prompt under one root naming another root's carrier: the carrier's
	// card is that root's round, so this root's rounds are recomputed.
	step("root naming a foreign carrier", nil, add(stampFixtureRow{id: "X", kind: "tool_call", tool: "Agent", summary: "Agent: x", turn: 18}))
	step("foreign-carrier root child", []string{"X"}, add(stampFixtureRow{id: "X-a1", kind: "assistant_text", summary: "x", parent: "X", turn: 18, index: 1}))
	step("carrier's own root", nil, add(stampFixtureRow{id: "Y", kind: "tool_call", tool: "Agent", summary: "Agent: y", turn: 18, index: 2}))
	step("carrier of the other root", nil, add(stampFixtureRow{id: "XC", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("Y"), turn: 19}))
	step("prompt naming the foreign carrier", nil, add(stampFixtureRow{id: "XP", kind: "user_text", summary: "resume x", parent: "X", meta: resumePromptMeta("XC"), turn: 19, index: 1}))
	step("child in the foreign carrier's round", nil, add(stampFixtureRow{id: "X-b2", kind: "tool_call", tool: "Bash", summary: "Bash: x", parent: "X", turn: 19, index: 2}))
	step("foreign round pick changes", nil, summary("X-b2", "Bash: x2"))
	step("later child in the foreign round", nil, add(stampFixtureRow{id: "X-b4", kind: "tool_call", tool: "Bash", summary: "Bash: x4", parent: "X", turn: 19, index: 4}))
	step("own carrier of the foreign-carrier root", nil, add(stampFixtureRow{id: "XC2", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("X"), turn: 19, index: 6}))
	step("prompt cuts the foreign carrier's round", nil, add(stampFixtureRow{id: "XP2", kind: "user_text", summary: "resume x again", parent: "X", meta: resumePromptMeta("XC2"), turn: 19, index: 3}))

	// A root woken without a carrier has a transcript and no carrier.
	step("wake-only root", nil, add(stampFixtureRow{id: "Z", kind: "tool_call", tool: "Agent", summary: "Agent: z", turn: 21}))
	step("wake-only root child", []string{"Z"}, add(stampFixtureRow{id: "Z-a1", kind: "assistant_text", summary: "z", parent: "Z", turn: 21, index: 1}))
	step("wake prompt as the only prompt", []string{"Z"}, add(stampFixtureRow{id: "ZW", kind: "user_text", summary: "wake", parent: "Z", meta: resumePromptMeta(""), turn: 22, index: 1}))
	step("woken root's child", []string{"Z"}, add(stampFixtureRow{id: "Z-b2", kind: "tool_call", tool: "Bash", summary: "Bash: z", parent: "Z", turn: 22, index: 2}))

	// A prompt stored inside a later round moves that round's later rows
	// to the round it opens.
	step("root with two rounds", nil, add(stampFixtureRow{id: "K", kind: "tool_call", tool: "Agent", summary: "Agent: k", turn: 24}))
	step("two-round root child", []string{"K"}, add(stampFixtureRow{id: "K-a1", kind: "assistant_text", summary: "k", parent: "K", turn: 24, index: 1}))
	step("first carrier of the two-round root", nil, add(stampFixtureRow{id: "KC1", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("K"), turn: 25}))
	step("first prompt of the two-round root", []string{"K", "KC1"}, add(stampFixtureRow{id: "KP1", kind: "user_text", summary: "k again", parent: "K", meta: resumePromptMeta("KC1"), turn: 25, index: 1}))
	step("second round child", []string{"K", "KC1"}, add(stampFixtureRow{id: "K-b2", kind: "tool_call", tool: "Bash", summary: "Bash: k2", parent: "K", turn: 25, index: 3}))
	step("second round later child", []string{"K", "KC1"}, add(stampFixtureRow{id: "K-b3", kind: "tool_call", tool: "Bash", summary: "Bash: k3", parent: "K", turn: 25, index: 5}))
	step("second carrier of the two-round root", nil, add(stampFixtureRow{id: "KC2", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("K"), turn: 25, index: 6}))
	step("prompt inside the second round", nil, add(stampFixtureRow{id: "KP2", kind: "user_text", summary: "k split", parent: "K", meta: resumePromptMeta("KC2"), turn: 25, index: 4}))

	// A prompt naming its own root: the family is readTime.
	step("self-resumed root", nil, add(stampFixtureRow{id: "H", kind: "tool_call", tool: "Agent", summary: "Agent: h", turn: 26}))
	step("self-resumed root child", []string{"H"}, add(stampFixtureRow{id: "H-a1", kind: "assistant_text", summary: "h", parent: "H", turn: 26, index: 1}))
	step("prompt naming its own root", nil, add(stampFixtureRow{id: "HP", kind: "user_text", summary: "h again", parent: "H", meta: resumePromptMeta("H"), turn: 27, index: 1}))
	step("self-resumed round child", nil, add(stampFixtureRow{id: "H-b2", kind: "tool_call", tool: "Bash", summary: "Bash: h", parent: "H", turn: 27, index: 2}))

	// A prompt stored before the carrier it names: the carrier arrives
	// unstamped and is stamped by the first write in its round.
	step("root resumed before its carrier", nil, add(stampFixtureRow{id: "E", kind: "tool_call", tool: "Agent", summary: "Agent: e", turn: 28}))
	step("early-prompt root child", []string{"E"}, add(stampFixtureRow{id: "E-a1", kind: "assistant_text", summary: "e", parent: "E", turn: 28, index: 1}))
	step("prompt before its carrier", []string{"E"}, add(stampFixtureRow{id: "EP", kind: "user_text", summary: "e again", parent: "E", meta: resumePromptMeta("EC"), turn: 29, index: 1}))
	step("late carrier", nil, add(stampFixtureRow{id: "EC", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("E"), turn: 29, index: 2}))
	step("child in the late carrier's round", []string{"E"}, add(stampFixtureRow{id: "E-b2", kind: "tool_call", tool: "Bash", summary: "Bash: e", parent: "E", turn: 29, index: 3}))
	step("next child in the late carrier's round", []string{"E", "EC"}, add(stampFixtureRow{id: "E-b3", kind: "tool_call", tool: "Bash", summary: "Bash: e3", parent: "E", turn: 29, index: 4}))

	// A launch written before the stamps existed carries none, and its
	// thread is listed for the backfill, whose reads walk it. A child
	// claimed under it before the backfill arrives recomputes it.
	step("unstamped launch with children", nil, func(p *stampParityStore) {
		for _, r := range []stampFixtureRow{
			{id: "G", kind: "tool_call", tool: "Agent", summary: "Agent: legacy", turn: 23},
			{id: "G-a1", kind: "assistant_text", summary: "g", parent: "G", turn: 23, index: 1},
			{id: "G-b2", kind: "tool_call", tool: "Bash", summary: "Bash: g", parent: "G", turn: 23, index: 2},
		} {
			add(r)(p)
		}
		stripSubagentStampsForTest(t, p.s, thread, "G")
		mustExec(t, p.s.db, `INSERT INTO subagent_aggregate_backfill(thread_id) VALUES (?)`, thread)
	})
	step("child under the unstamped launch", nil, add(stampFixtureRow{id: "G-b3", kind: "tool_call", tool: "Bash", summary: "Bash: g3", parent: "G", turn: 23, index: 3}))
	step("next child under the recomputed launch", []string{"G"}, add(stampFixtureRow{id: "G-a4", kind: "assistant_text", summary: "g4", parent: "G", turn: 23, index: 4}))
	// The backfill finds nothing left to stamp.
	for _, p := range stores {
		mustExec(t, p.s.db, `DELETE FROM subagent_aggregate_backfill WHERE thread_id = ?`, thread)
		assertSubagentStampParity(t, p.s, thread, p.name+": backfill done", true)
	}

	// A claimed rewrite that moves its row is left to the marks.
	step("child moves to another launch", nil, upsert("B-a1", func(row *Item) { row.ParentID = "L" }))

	// Deletes name no anchor; the recompute keeps them.
	step("delete an older child", nil, remove("L-a1"))
	step("delete the pick", nil, remove("N-b1"))
	// B's newest row is B-b2 (blank, so neither preview nor tray); B-b1
	// is only its tray row and B-a2 only its preview.
	step("delete the tray row", nil, remove("B-b1"))
	step("delete a preview that is not the newest", nil, remove("B-a2"))
	step("delete the transcript's newest", nil, remove("R-a4"))
	step("delete a round row", nil, remove("R-a2"))
	step("delete a prompt", nil, remove("P2"))
	step("delete a nested launch", nil, remove("N"))

	for _, p := range stores {
		s := p.s
		// A write the store did not settle (raw SQL moving a row between
		// launches) leaves anchors dirty; reads walk them until the
		// recompute.
		if _, err := s.db.Exec(`UPDATE items SET parent_id = 'B' WHERE thread_id = ? AND id = 'L-b1'`, thread); err != nil {
			t.Fatalf("re-parent: %v", err)
		}
		for _, id := range []string{"L", "B"} {
			if _, mode := subagentStampStateForTest(t, s, thread, id); mode != subagentStampWalk {
				t.Errorf("%s: re-parent left %s mode %d, want dirty", p.name, id, mode)
			}
		}
		assertSubagentStampParity(t, s, thread, p.name+": re-parented, unsettled", false)
		if _, err := s.RecomputeSubagentAggregates(t.Context(), thread, 16); err != nil {
			t.Fatalf("recompute: %v", err)
		}
		assertSubagentStampParity(t, s, thread, p.name+": re-parented, recomputed", true)

		// Moves across the top level change the card of a launch on one
		// side of the move only: a row leaving B, and a top-level row
		// joining L.
		for _, move := range [][2]string{{"B-b2", ""}, {"T-top", "L"}} {
			if _, err := s.db.Exec(`UPDATE items SET parent_id = ? WHERE thread_id = ? AND id = ?`, move[1], thread, move[0]); err != nil {
				t.Fatalf("move %s: %v", move[0], err)
			}
		}
		for _, id := range []string{"B", "L"} {
			if _, mode := subagentStampStateForTest(t, s, thread, id); mode != subagentStampWalk {
				t.Errorf("%s: move across the top level left %s mode %d, want dirty", p.name, id, mode)
			}
		}
		assertSubagentStampParity(t, s, thread, p.name+": moved across the top level, unsettled", false)
		if _, err := s.RecomputeSubagentAggregates(t.Context(), thread, 16); err != nil {
			t.Fatalf("recompute: %v", err)
		}
		assertSubagentStampParity(t, s, thread, p.name+": moved across the top level, recomputed", true)
		assertStampsAreTheRecompute(t, s, thread, p.name+": after the raw moves")

		// Every anchor the fixture names ends on a stamp a page serves as
		// stored: the parity above was not won by walking everything. C2
		// lost the prompt that named it, so it shows the whole transcript,
		// which only the walk computes.
		for _, id := range []string{"L", "R", "C1", "B", "imp-launch"} {
			if _, mode := subagentStampStateForTest(t, s, thread, id); mode != subagentStampClean {
				t.Errorf("%s: %s ends mode %d, want clean", p.name, id, mode)
			}
		}
		if _, mode := subagentStampStateForTest(t, s, thread, "C2"); mode != subagentStampWalk {
			t.Errorf("%s: unnamed carrier C2 ends mode %d, want readTime", p.name, mode)
		}
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
