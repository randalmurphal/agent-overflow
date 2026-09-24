package store

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	sqlite "modernc.org/sqlite"
)

// seedStampLaunch writes a launch and its children through the store, so
// the launch ends stamped the way live writes stamp it.
func seedStampLaunch(t *testing.T, s *Store, threadID, launchID string, turn, children int) {
	t.Helper()
	if err := s.InsertItem(stampFixtureRow{id: launchID, kind: "tool_call", tool: "Agent",
		summary: "Agent: " + launchID, status: "running", turn: turn}.item(threadID)); err != nil {
		t.Fatalf("insert %s: %v", launchID, err)
	}
	for i := 1; i <= children; i++ {
		row := stampFixtureRow{id: fmt.Sprintf("%s-c%d", launchID, i), kind: "tool_call", tool: "Bash",
			summary: fmt.Sprintf("Bash: step %d", i), parent: launchID, turn: turn, index: i}
		if err := s.InsertItem(row.item(threadID)); err != nil {
			t.Fatalf("insert %s: %v", row.id, err)
		}
	}
}

func itemMetaForTest(t *testing.T, s *Store, threadID, id string) string {
	t.Helper()
	var meta string
	if err := s.db.QueryRow(`SELECT meta FROM items WHERE thread_id = ? AND id = ?`, threadID, id).Scan(&meta); err != nil {
		t.Fatalf("read meta %s/%s: %v", threadID, id, err)
	}
	return meta
}

// TestSubagentAggregateIgnoresWritesOutsideTheCard pins that a write
// changing nothing a card shows leaves every stamp byte-identical:
// status, updated_at, decision and payload writes to a child or to the
// launch, and a summary change on a row that is neither preview nor tray.
func TestSubagentAggregateIgnoresWritesOutsideTheCard(t *testing.T) {
	s := newTestStore(t)
	const thread = "t-quiet"
	mustCreateThread(t, s, thread)
	seedStampLaunch(t, s, thread, "L", 1, 2)
	if _, err := s.AppendItemWithPayload(stampFixtureRow{id: "L-out", kind: "tool_completion", tool: "Bash",
		summary: "output", parent: "L", turn: 1, index: 3}.item(thread),
		Payload{ID: "pay-1", Kind: "text", Data: []byte("x"), CreatedAt: 1}); err != nil {
		t.Fatalf("insert payload child: %v", err)
	}
	if err := s.InsertItem(stampFixtureRow{id: "L-note", kind: "notification", tool: "hook",
		summary: "note", parent: "L", turn: 1, index: 4}.item(thread)); err != nil {
		t.Fatalf("insert notification: %v", err)
	}
	if _, mode := subagentStampStateForTest(t, s, thread, "L"); mode != subagentStampClean {
		t.Fatalf("fixture launch is not stamped clean")
	}
	before := itemMetaForTest(t, s, thread, "L")

	errored, completed, approved, note := "errored", "completed", "approved", "note, edited"
	updatedAt := int64(99_000)
	writes := []struct {
		name  string
		write func() error
	}{
		{"child status", func() error {
			_, err := s.UpdateItemFields(thread, "L-c1", ItemPartialUpdate{Status: &errored})
			return err
		}},
		{"child updated_at", func() error {
			_, err := s.UpdateItemFields(thread, "L-c2", ItemPartialUpdate{UpdatedAt: &updatedAt})
			return err
		}},
		{"child decision", func() error {
			_, err := s.UpdateItemFields(thread, "L-c2", ItemPartialUpdate{Decision: &approved})
			return err
		}},
		{"child payload", func() error {
			return s.ReplacePayloadData(thread, "pay-1", []byte("replaced"), "", 2)
		}},
		{"child rev only", func() error {
			_, err := s.db.Exec(`UPDATE items SET rev = rev + 1000 WHERE thread_id = ? AND id = 'L-c1'`, thread)
			return err
		}},
		{"non-preview summary", func() error {
			_, err := s.UpdateItemFields(thread, "L-note", ItemPartialUpdate{Summary: &note})
			return err
		}},
		{"launch status", func() error {
			_, err := s.UpdateItemFields(thread, "L", ItemPartialUpdate{Status: &completed})
			return err
		}},
		{"launch updated_at", func() error {
			_, err := s.UpdateItemFields(thread, "L", ItemPartialUpdate{UpdatedAt: &updatedAt})
			return err
		}},
	}
	for _, w := range writes {
		if err := w.write(); err != nil {
			t.Fatalf("%s: %v", w.name, err)
		}
		if got := itemMetaForTest(t, s, thread, "L"); got != before {
			t.Errorf("%s rewrote the launch's stamp:\n got %s\nwant %s", w.name, got, before)
		}
	}
	assertSubagentStampParity(t, s, thread, "after quiet writes", true)
}

// pageAccessesForTest counts the page-cache lookups (hits plus misses) of
// one statement on the writer connection: every table and index page its
// statement and triggers touch.
func pageAccessesForTest(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatalf("writer conn: %v", err)
	}
	defer conn.Close()
	read := func(reset bool) int {
		total := 0
		if err := conn.Raw(func(dc any) error {
			if cached, ok := dc.(*stmtCacheConn); ok {
				dc = cached.sqliteConn
			}
			status, ok := dc.(sqlite.DBStatus)
			if !ok {
				return fmt.Errorf("driver connection %T has no page counters", dc)
			}
			for _, op := range []sqlite.DBStatusOp{sqlite.DBStatusCacheHit, sqlite.DBStatusCacheMiss} {
				current, _, err := status.Status(op, reset)
				if err != nil {
					return err
				}
				total += current
			}
			return nil
		}); err != nil {
			t.Fatalf("read page counters: %v", err)
		}
		return total
	}
	read(true)
	if _, err := conn.ExecContext(ctx, query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
	return read(false)
}

// TestSubagentAggregateTriggersDoNotScanASubtree measures the pages each
// trigger path touches under a launch with 2000 children against the same
// write under a launch with 20. The stamp keeps a child write at a few
// probes per nesting level, so the two must cost the same up to B-tree
// depth; any statement that walked the launch's children would read the
// 2000-row subtree.
func TestSubagentAggregateTriggersDoNotScanASubtree(t *testing.T) {
	s := newTestStore(t)
	const thread = "t-cost"
	mustCreateThread(t, s, thread)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := setHistoryBulkLoadTx(tx, thread, true, "test seed"); err != nil {
		t.Fatal(err)
	}
	insert, err := tx.Prepare(`INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
	    summary, parent_id, tool_name, meta, created_at, updated_at)
	  VALUES (?, ?, ?, ?, ?, 'assistant', 'completed', ?, ?, ?, ?, 1, 1)`)
	if err != nil {
		t.Fatal(err)
	}
	seed := func(launch string, turn, children int) {
		if _, err := insert.Exec(launch, thread, turn, 0, "tool_call", "Agent: "+launch, "", "Agent", "{}"); err != nil {
			t.Fatal(err)
		}
		for i := 1; i <= children; i++ {
			kind, tool := "tool_call", "Bash"
			if i%3 == 0 {
				kind, tool = "assistant_text", ""
			}
			if _, err := insert.Exec(fmt.Sprintf("%s-%d", launch, i), thread, turn, i, kind,
				fmt.Sprintf("step %d", i), launch, tool, "{}"); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Long ids spread the launches' index entries over many pages, so any
	// statement that walked a launch's children would read dozens of
	// pages more under the big one.
	big, small := strings.Repeat("p", 200)+"-big", strings.Repeat("p", 200)+"-sml"
	seed(big, 1, 2000)
	seed(small, 2, 20)
	if err := insert.Close(); err != nil {
		t.Fatal(err)
	}
	if err := restampSubagentAggregatesTx(tx, thread); err != nil {
		t.Fatal(err)
	}
	if err := setHistoryBulkLoadTx(tx, thread, false, "test seed"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, launch := range []string{big, small} {
		if _, mode := subagentStampStateForTest(t, s, thread, launch); mode != subagentStampClean {
			t.Fatalf("%s is not stamped clean", launch)
		}
	}

	type write struct {
		name  string
		query func(launch string, turn int) (string, []any)
	}
	writes := []write{
		{"insert a child", func(launch string, turn int) (string, []any) {
			return `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status, summary, parent_id, tool_name, created_at, updated_at)
			  VALUES (?, ?, ?, 2000000, 'tool_call', 'assistant', 'running', 'Bash: new', ?, 'Bash', 1, 1)`,
				[]any{launch + "-new", thread, turn, launch}
		}},
		{"stream the newest child", func(launch string, turn int) (string, []any) {
			return `UPDATE items SET summary = summary || ' more' WHERE thread_id = ? AND id = ?`, []any{thread, launch + "-new"}
		}},
		{"settle the newest child", func(launch string, turn int) (string, []any) {
			return `UPDATE items SET status = 'completed' WHERE thread_id = ? AND id = ?`, []any{thread, launch + "-new"}
		}},
		{"edit an old child", func(launch string, turn int) (string, []any) {
			return `UPDATE items SET summary = 'edited' WHERE thread_id = ? AND id = ?`, []any{thread, launch + "-2"}
		}},
		{"delete an old child", func(launch string, turn int) (string, []any) {
			return `DELETE FROM items WHERE thread_id = ? AND id = ?`, []any{thread, launch + "-4"}
		}},
		{"insert a carrier", func(launch string, turn int) (string, []any) {
			return `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status, summary, tool_name, meta, created_at, updated_at)
			  VALUES (?, ?, ?, 2500000, 'tool_call', 'assistant', 'running', 'Agent: continue', 'SendMessage', ?, 1, 1)`,
				[]any{launch + "-carrier", thread, turn, carrierMeta(launch)}
		}},
		{"insert the resume prompt", func(launch string, turn int) (string, []any) {
			return `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status, summary, parent_id, meta, created_at, updated_at)
			  VALUES (?, ?, ?, 3000000, 'user_text', 'user', 'completed', 'again', ?, ?, 1, 1)`,
				[]any{launch + "-prompt", thread, turn, launch, resumePromptMeta(launch + "-carrier")}
		}},
		{"insert a child in the second round", func(launch string, turn int) (string, []any) {
			return `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status, summary, parent_id, tool_name, created_at, updated_at)
			  VALUES (?, ?, ?, 4000000, 'tool_call', 'assistant', 'running', 'Bash: later', ?, 'Bash', 1, 1)`,
				[]any{launch + "-later", thread, turn, launch}
		}},
	}
	for _, w := range writes {
		smallQuery, smallArgs := w.query(small, 2)
		bigQuery, bigArgs := w.query(big, 1)
		smallPages := pageAccessesForTest(t, s, smallQuery, smallArgs...)
		bigPages := pageAccessesForTest(t, s, bigQuery, bigArgs...)
		t.Logf("%s: %d pages under 20 children, %d under 2000", w.name, smallPages, bigPages)
		if bigPages > smallPages+8 {
			t.Errorf("%s touches %d pages under a 2000-child launch and %d under a 20-child one: a trigger reads the subtree",
				w.name, bigPages, smallPages)
		}
	}
	for _, launch := range []string{big, small} {
		if _, mode := subagentStampStateForTest(t, s, thread, launch); mode != subagentStampClean {
			t.Errorf("%s left mode %d after the writes, want clean (kept by the triggers)", launch, mode)
		}
	}
	assertSubagentStampParity(t, s, thread, "after the measured writes", true)
}

// triggerStatementForTest rewrites a trigger statement's NEW./OLD. column
// references as bound parameters, which the planner treats as the
// constants a trigger's row values are, so EXPLAIN can show the plan the
// trigger runs.
func triggerStatementForTest(statement string, rows map[string]Item) (string, []any) {
	columns := []string{"thread_id", "turn_index", "item_index", "tool_name", "parent_id", "summary", "kind", "meta", "id"}
	var args []any
	pairs := make([]string, 0, 2*len(columns)*len(rows))
	for _, ref := range []string{"NEW", "OLD"} {
		row, ok := rows[ref]
		if !ok {
			continue
		}
		values := map[string]any{
			"thread_id": row.ThreadID, "turn_index": row.TurnIndex, "item_index": row.ItemIndex,
			"tool_name": row.ToolName, "parent_id": row.ParentID, "summary": row.Summary,
			"kind": row.Kind, "meta": row.Meta, "id": row.ID,
		}
		for _, column := range columns {
			args = append(args, values[column])
			pairs = append(pairs, ref+"."+column, fmt.Sprintf("?%d", len(args)))
		}
	}
	return strings.NewReplacer(pairs...).Replace(strings.TrimSuffix(strings.TrimSpace(statement), ";")), args
}

// aggregatePlanCTEs are the names the trigger statements give their CTEs
// and derived tables. A plan may SCAN those; the statements alias stored
// tables only as items, c, p, x, threads and agg_hc*, never as one of
// these.
var aggregatePlanCTEs = map[string]bool{
	"agg_chain": true, "agg_chain_new": true, "agg_chain_old": true, "agg_rounds": true, "agg_rounds_prompt": true,
	"agg_roles": true, "agg_ops": true, "agg_orphan": true, "agg_change": true, "agg": true, "ch": true, "r": true, "a": true, "b": true,
}

var planScanTarget = regexp.MustCompile(`^SCAN (\S+)`)

// boundedPlan is what a statement's plan may contain beyond keyed
// searches: scans of the named CTEs or derived tables, and, with sorts,
// a temporary B-tree the statement's shape requires.
type boundedPlan struct {
	scans map[string]bool
	sorts bool
}

func jsonListForTest(t *testing.T, values ...string) string {
	t.Helper()
	list, err := jsonList(values)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// assertBoundedPlan fails on a SCAN of a stored table and on an ORDER BY
// the index does not deliver (a sort would read the whole range).
func assertBoundedPlan(t *testing.T, s *Store, name string, allowed boundedPlan, query string, args ...any) string {
	t.Helper()
	text := planText(explainPlan(t, s, query, args...))
	for _, line := range strings.Split(text, "\n") {
		detail := strings.TrimSpace(line)
		if i := strings.Index(detail, " "); i >= 0 {
			detail = strings.TrimSpace(detail[i:])
		}
		if detail == "SCAN CONSTANT ROW" {
			continue
		}
		if m := planScanTarget.FindStringSubmatch(detail); m != nil && !allowed.scans[m[1]] {
			t.Errorf("%s scans %s:\n%s", name, m[1], text)
		}
		if strings.Contains(detail, "TEMP B-TREE FOR ORDER BY") && !allowed.sorts {
			t.Errorf("%s sorts a range the index should order:\n%s", name, text)
		}
	}
	return text
}

// TestSubagentAggregateStatementPlans is the EXPLAIN tripwire for every
// statement the stamps added: the three trigger statements, the recompute
// selections, and the reads that serve stamped rows.
func TestSubagentAggregateStatementPlans(t *testing.T) {
	s := newTestStore(t)
	const thread = "t-plan"
	mustCreateThread(t, s, thread)
	seedStampLaunch(t, s, thread, "L", 1, 3)
	child := stampFixtureRow{id: "L-c3", kind: "tool_call", tool: "Bash", summary: "Bash: step 3", parent: "L", turn: 1, index: 3}.item(thread)
	moved := child
	moved.ParentID, moved.Summary = "M", "Bash: moved"

	// probes is the number of resume-prompt probes each statement is
	// written with: the round probe, and the prompts under each chain it
	// dirties. A plan with more has inlined a CTE at each of its mentions,
	// so the item triggers compile, and run, the probe that many times.
	for _, tc := range []struct {
		name, statement string
		rows            map[string]Item
		probes          int
	}{
		{"insert trigger", subagentAggregateInsertStmt, map[string]Item{"NEW": child}, 2},
		{"delete trigger", subagentAggregateDeleteStmt, map[string]Item{"OLD": child}, 2},
		{"update trigger", subagentAggregateUpdateStmt, map[string]Item{"NEW": moved, "OLD": child}, 3},
	} {
		query, args := triggerStatementForTest(tc.statement, tc.rows)
		if strings.Contains(query, "NEW.") || strings.Contains(query, "OLD.") {
			t.Fatalf("%s: a row reference was left unbound", tc.name)
		}
		text := assertBoundedPlan(t, s, tc.name, boundedPlan{scans: aggregatePlanCTEs}, query, args...)
		if probes := strings.Count(text, "idx_items_subagent_resume_prompt"); probes != tc.probes {
			t.Errorf("%s plans %d resume-prompt probes, want %d:\n%s", tc.name, probes, tc.probes, text)
		}
		if !strings.Contains(text, "sqlite_autoindex_items_1") {
			t.Errorf("%s does not walk the chain by primary key:\n%s", tc.name, text)
		}
	}

	rounds, roundArgs := subagentResumeRoundsQuery(thread, jsonListForTest(t, "L"))
	// The legacy selection reads the union of the thread's parent ids: its
	// own derived table p, and the merge sort of the import arm's distinct
	// parent ids, which span chunks.
	legacy := boundedPlan{scans: map[string]bool{"p": true}, sorts: true}
	// An id list bound as one JSON array is read by scanning json_each.
	listed := boundedPlan{scans: map[string]bool{"json_each": true}}
	ids := jsonListForTest(t, "L", "M")
	for _, tc := range []struct {
		name, query, index string
		args               []any
		allowed            boundedPlan
	}{
		{"dirty selection", subagentDirtyAnchorsSQL, "idx_items_subagent_aggregate_dirty", []any{thread, 16}, boundedPlan{}},
		{"legacy selection", subagentLegacyAnchorsSQL, "COVERING INDEX idx_items_parent", []any{thread, 16}, legacy},
		{"resume rounds", rounds, "idx_items_subagent_resume_prompt (thread_id=? AND parent_id=?)", roundArgs, listed},
		{"first child anchors", firstChildAnchorsSQL, "sqlite_autoindex_items_1", []any{thread, "L", "L-c3", 1, 3}, boundedPlan{}},
		{"latest direct tool", latestDirectSubagentToolSQL, "idx_items_parent", []any{thread, "L"}, boundedPlan{}},
		{"carriers of roots", subagentCarriersSQL, "idx_items_transcript_root (thread_id=? AND <expr>=?)", []any{thread, ids}, listed},
		{"stamp targets", subagentStampTargetsSQL, "sqlite_autoindex_items_1 (thread_id=? AND id=?)", []any{thread, ids}, listed},
		{"chain marked dirty", markSubagentAnchorsDirtySQL, "sqlite_autoindex_items_1 (thread_id=? AND id=?)", []any{thread, ids}, listed},
		{"stamp write", writeSubagentStampSQL, "sqlite_autoindex_items_1", []any{"{}", thread, "L", 3}, boundedPlan{}},
	} {
		text := assertBoundedPlan(t, s, tc.name, tc.allowed, tc.query, tc.args...)
		if !strings.Contains(text, tc.index) {
			t.Errorf("%s does not use %s:\n%s", tc.name, tc.index, text)
		}
	}
}

// TestSubagentAggregateImportedPromptIsReadTime pins the shape the
// triggers leave to the read-time path: a round prompt in the immutable
// history arm, which the round probe cannot see. The shadowed launch is
// stamped readTime and walked.
func TestSubagentAggregateImportedPromptIsReadTime(t *testing.T) {
	s := newTestStore(t)
	const thread = "t-imported-prompt"
	newImportTargetThread(t, s, thread)
	imported := func(r stampFixtureRow) ImportRow { return ImportRow{Item: r.item(thread)} }
	if err := s.ApplyImportBatch(thread, ImportBatch{
		Turns: []Turn{{TurnID: thread + ":0", ThreadID: thread, TurnIndex: 0, StartedAt: 1_000}},
		Rows: []ImportRow{
			imported(stampFixtureRow{id: "A", kind: "tool_call", tool: "Agent", summary: "Agent: imported"}),
			imported(stampFixtureRow{id: "A-c1", kind: "tool_call", tool: "Bash", summary: "Bash: one", parent: "A", index: 1}),
			imported(stampFixtureRow{id: "A-carrier", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("A"), index: 2}),
			imported(stampFixtureRow{id: "A-prompt", kind: "user_text", summary: "again", parent: "A", meta: resumePromptMeta("A-carrier"), index: 3}),
			imported(stampFixtureRow{id: "A-c2", kind: "tool_call", tool: "Bash", summary: "Bash: two", parent: "A", index: 4}),
		},
	}); err != nil {
		t.Fatalf("apply import batch: %v", err)
	}
	if err := s.InsertItem(stampFixtureRow{id: "A-local", kind: "tool_call", tool: "Bash", summary: "Bash: local",
		parent: "A", turn: 1}.item(thread)); err != nil {
		t.Fatalf("insert local child: %v", err)
	}
	if _, mode := subagentStampStateForTest(t, s, thread, "A"); mode != subagentStampWalk {
		t.Errorf("shadowed launch with an imported prompt is mode %d, want readTime", mode)
	}
	assertSubagentStampParity(t, s, thread, "imported prompt", true)
}

// TestSubagentAggregateRollbackRecomputesSurvivors pins that a history
// cut settles the anchors it leaves behind in the same call: the
// surviving launch loses its preview, newest and tray rows and is stamped
// again before the cut returns.
func TestSubagentAggregateRollbackRecomputesSurvivors(t *testing.T) {
	s := newTestStore(t)
	const thread = "t-rollback"
	mustCreateThread(t, s, thread)
	seedStampLaunch(t, s, thread, "L", 1, 2)
	for turn := 2; turn <= 4; turn++ {
		if err := s.InsertItem(stampFixtureRow{id: fmt.Sprintf("L-t%d", turn), kind: "tool_call", tool: "Bash",
			summary: fmt.Sprintf("Bash: turn %d", turn), parent: "L", turn: turn}.item(thread)); err != nil {
			t.Fatal(err)
		}
	}
	genBefore, _ := subagentStampStateForTest(t, s, thread, "L")

	if _, _, err := s.DeleteConversationFromTurn(thread, 4); err != nil {
		t.Fatalf("delete from turn: %v", err)
	}
	assertSubagentStampParity(t, s, thread, "cut from turn", true)
	if _, _, err := s.DeleteConversationFromItem(thread, "L-t3"); err != nil {
		t.Fatalf("delete from item: %v", err)
	}
	assertSubagentStampParity(t, s, thread, "cut from item", true)
	gen, mode := subagentStampStateForTest(t, s, thread, "L")
	if mode != subagentStampClean || gen <= genBefore {
		t.Errorf("launch after the cuts is mode %d at gen %d, want clean and recomputed past gen %d", mode, gen, genBefore)
	}
}

// TestSubagentAggregateStampSurvivesAStaleMetaWrite pins the update
// trigger's protection: a writer that replaces a launch's meta with a copy
// read before the stamp moved gets the stamp back, and UpdateItemFields
// returns the row as stored so an emitter pushes what a read returns.
func TestSubagentAggregateStampSurvivesAStaleMetaWrite(t *testing.T) {
	s := newTestStore(t)
	const thread = "t-stale"
	mustCreateThread(t, s, thread)
	seedStampLaunch(t, s, thread, "L", 1, 2)
	stamped := subagentCardOf(t, itemMetaForTest(t, s, thread, "L"))
	if len(stamped) == 0 {
		t.Fatal("fixture launch carries no card")
	}

	if err := s.UpdateItemMeta(thread, "L", `{"input":{"prompt":"go"}}`); err != nil {
		t.Fatalf("update meta: %v", err)
	}
	if got := subagentCardOf(t, itemMetaForTest(t, s, thread, "L")); !mapsEqual(got, stamped) {
		t.Errorf("whole-meta write erased the stamp: %v, want %v", got, stamped)
	}

	meta := `{"input":{"prompt":"go"},"extra":1}`
	stored, err := s.UpdateItemFields(thread, "L", ItemPartialUpdate{Meta: &meta})
	if err != nil {
		t.Fatalf("update fields: %v", err)
	}
	if got := subagentCardOf(t, stored.Meta); !mapsEqual(got, stamped) {
		t.Errorf("UpdateItemFields returned %v, want the stored stamp %v", got, stamped)
	}
	if !strings.Contains(stored.Meta, `"extra":1`) {
		t.Errorf("UpdateItemFields lost the written meta: %s", stored.Meta)
	}
	if stored.Meta != itemMetaForTest(t, s, thread, "L") {
		t.Errorf("UpdateItemFields returned meta that is not the stored row")
	}
	assertSubagentStampParity(t, s, thread, "after stale writes", true)
}

func mapsEqual(a, b subagentCard) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if fmt.Sprint(b[key]) != fmt.Sprint(value) {
			return false
		}
	}
	return true
}

// TestSubagentAggregateBackfillIsRestartable drives migration v121's
// deferred phase over a thread whose anchors predate the stamps: paced
// calls, a store closed and reopened between them, the thread leaving the
// list only with its last anchor stamped.
func TestSubagentAggregateBackfillIsRestartable(t *testing.T) {
	path := newTestStorePath(t)
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	const thread = "t-legacy"
	mustCreateThread(t, s, thread)
	for i := range 5 {
		seedStampLaunch(t, s, thread, fmt.Sprintf("L%d", i), i+1, 3)
	}
	if err := s.InsertItem(stampFixtureRow{id: "N", kind: "tool_call", tool: "Agent", summary: "Agent: nested",
		parent: "L0", turn: 1, index: 9}.item(thread)); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertItem(stampFixtureRow{id: "N-c1", kind: "tool_call", tool: "Bash", summary: "Bash: deep",
		parent: "N", turn: 1, index: 10}.item(thread)); err != nil {
		t.Fatal(err)
	}
	stripSubagentStampsForTest(t, s, thread)
	if _, err := s.db.Exec(`INSERT INTO subagent_aggregate_backfill(thread_id) VALUES (?)`, thread); err != nil {
		t.Fatal(err)
	}
	assertSubagentStampParity(t, s, thread, "legacy, listed", false)

	ctx := context.Background()
	first, err := s.RecomputeSubagentAggregates(ctx, thread, 2)
	if err != nil {
		t.Fatalf("first batch: %v", err)
	}
	if first.Stamped == 0 || !first.Remaining {
		t.Fatalf("first batch = %+v, want some stamped and more remaining", first)
	}
	assertSubagentStampParity(t, s, thread, "partly backfilled", false)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	next, err := s.nextSubagentBackfillThread(ctx, "")
	if err != nil || next != thread {
		t.Fatalf("after reopen the next listed thread is %q (%v), want %q", next, err, thread)
	}
	if after, err := s.nextSubagentBackfillThread(ctx, thread); err != nil || after != "" {
		t.Fatalf("no thread follows %q, got %q (%v)", thread, after, err)
	}
	for calls := 0; ; calls++ {
		if calls > 10 {
			t.Fatal("backfill does not finish")
		}
		result, err := s.RecomputeSubagentAggregates(ctx, thread, 2)
		if err != nil {
			t.Fatalf("batch %d: %v", calls, err)
		}
		if !result.Remaining {
			break
		}
	}
	if listed, err := subagentBackfillListed(s.reader(), thread); err != nil || listed {
		t.Fatalf("thread still listed after the last batch (%v)", err)
	}
	if next, err := s.nextSubagentBackfillThread(ctx, ""); err != nil || next != "" {
		t.Fatalf("backfill list not empty: %q (%v)", next, err)
	}
	assertSubagentStampParity(t, s, thread, "backfilled", true)
	for i := range 5 {
		if _, mode := subagentStampStateForTest(t, s, thread, fmt.Sprintf("L%d", i)); mode != subagentStampClean {
			t.Errorf("L%d ends mode %d, want clean", i, mode)
		}
	}
	// Idle once applied: an unlisted thread with nothing dirty writes
	// nothing.
	if result, err := s.RecomputeSubagentAggregates(ctx, thread, 2); err != nil || result.Stamped != 0 || result.Remaining {
		t.Fatalf("recompute on a finished thread = %+v (%v)", result, err)
	}
}

// legacyStampThreadsForTest leaves each thread holding a launch with two
// children as v121's SQL leaves an existing thread: no stamp on any row,
// and the thread listed for the deferred phase. The watermark is v119's,
// so v121's phase is the one pending.
func legacyStampThreadsForTest(t *testing.T, s *Store, threads ...string) {
	t.Helper()
	for _, thread := range threads {
		mustCreateThread(t, s, thread)
		seedStampLaunch(t, s, thread, "L", 1, 2)
		stripSubagentStampsForTest(t, s, thread)
		mustExec(t, s.db, `INSERT INTO subagent_aggregate_backfill(thread_id) VALUES (?)`, thread)
	}
	mustExec(t, s.db, `PRAGMA user_version = 119`)
}

func backfillListForTest(t *testing.T, s *Store) []string {
	t.Helper()
	listed, err := subagentAnchorIDs(s.db, `SELECT thread_id FROM subagent_aggregate_backfill ORDER BY thread_id`)
	if err != nil {
		t.Fatal(err)
	}
	return listed
}

// TestSubagentAggregatePhaseStampsListedThreads runs migration v121's
// deferred phase over threads whose anchors predate the stamps. It stamps
// every listed thread. A thread whose stamps never land (its rows move
// under every batch) is reported without holding up the threads after it,
// the run records the failure and leaves the watermark, and the next open
// finishes the thread and clears the record.
func TestSubagentAggregatePhaseStampsListedThreads(t *testing.T) {
	s := openStoreAt(t)
	threads := []string{"a-moving", "b-legacy", "c-legacy"}
	legacyStampThreadsForTest(t, s, threads...)
	s = reopenStore(t, s)
	if !deferredPending(t, s) {
		t.Fatal("v121's phase is not pending")
	}
	for _, thread := range threads {
		assertSubagentStampParity(t, s, thread, thread+" listed", false)
	}
	mustExec(t, s.db, `CREATE TRIGGER test_rows_move BEFORE UPDATE ON items WHEN OLD.thread_id = 'a-moving'
	  BEGIN SELECT RAISE(IGNORE); END`)

	if err := s.RunDeferredMigrations(context.Background(), DeferredHost{}); err != nil {
		t.Fatal(err)
	}
	if got := backfillListForTest(t, s); !slices.Equal(got, []string{"a-moving"}) {
		t.Fatalf("after the first run the list is %v, want the moving thread alone", got)
	}
	failure := deferredFailureOf(t, s)
	if failure == nil || failure.Version != 121 || failure.Title != "Agent card update" || failure.Failures != 1 ||
		!strings.Contains(failure.FirstError, "a-moving") {
		t.Fatalf("recorded failure = %+v, want the moving thread's", failure)
	}
	if got := deferredWatermarkOf(t, s); got != 119 {
		t.Fatalf("a run that left a thread moved the watermark to %d", got)
	}
	for _, thread := range threads[1:] {
		assertSubagentStampParity(t, s, thread, thread+" stamped", true)
		if _, mode := subagentStampStateForTest(t, s, thread, "L"); mode != subagentStampClean {
			t.Errorf("%s's launch ends mode %d, want clean", thread, mode)
		}
	}
	assertSubagentStampParity(t, s, "a-moving", "moving thread, still listed", false)

	mustExec(t, s.db, `DROP TRIGGER test_rows_move`)
	s = reopenStore(t, s)
	if err := s.RunDeferredMigrations(context.Background(), DeferredHost{}); err != nil {
		t.Fatal(err)
	}
	if deferredPending(t, s) || deferredWatermarkOf(t, s) != latestDeferredVersion {
		t.Fatalf("the finishing run left watermark %d", deferredWatermarkOf(t, s))
	}
	if failure := deferredFailureOf(t, s); failure != nil {
		t.Fatalf("a finished run kept the failure record: %+v", failure)
	}
	if got := backfillListForTest(t, s); len(got) != 0 {
		t.Fatalf("a finished run left %v listed", got)
	}
	assertSubagentStampParity(t, s, "a-moving", "moving thread, finished", true)
}

// TestSubagentAggregatePhaseQuitRecordsNothing stops the phase at its first
// pause: the stamped thread stays done, the rest stay listed, nothing is
// recorded, and the next run finishes from the list.
func TestSubagentAggregatePhaseQuitRecordsNothing(t *testing.T) {
	s := openStoreAt(t)
	legacyStampThreadsForTest(t, s, "a", "b", "c")
	s = reopenStore(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.RunDeferredMigrations(ctx, DeferredHost{Pause: ChunkPause(cancel)}); err != nil {
		t.Fatal(err)
	}
	if got := deferredWatermarkOf(t, s); got != 119 {
		t.Fatalf("a quit moved the watermark to %d", got)
	}
	if failure := deferredFailureOf(t, s); failure != nil {
		t.Fatalf("a quit recorded a failure: %+v", failure)
	}
	if got := backfillListForTest(t, s); !slices.Equal(got, []string{"b", "c"}) {
		t.Fatalf("after the quit the list is %v, want the threads after the first", got)
	}
	for _, thread := range []string{"a", "b", "c"} {
		assertSubagentStampParity(t, s, thread, thread+" after the quit", false)
	}
	if err := s.RunDeferredMigrations(context.Background(), DeferredHost{}); err != nil {
		t.Fatal(err)
	}
	if deferredPending(t, s) || len(backfillListForTest(t, s)) != 0 {
		t.Fatalf("the resumed run left watermark %d and list %v", deferredWatermarkOf(t, s), backfillListForTest(t, s))
	}
	for _, thread := range []string{"a", "b", "c"} {
		assertSubagentStampParity(t, s, thread, thread+" resumed", true)
	}
}

// TestDeferredPhasesStampAFoldedSubtree upgrades from before v119: a
// launch's children are sealed and the launch predates the stamps. The
// phases run in version order, v119 folding the children back into the
// thread's rows and v121 stamping the launch, and the card matches the
// walk before, between and after.
func TestDeferredPhasesStampAFoldedSubtree(t *testing.T) {
	s := openStoreAt(t)
	const thread = "t-folded"
	mustCreateThread(t, s, thread)
	if err := s.InsertItem(stampFixtureRow{id: "L", kind: "tool_call", tool: "Agent", summary: "Agent: sealed children", turn: 1}.item(thread)); err != nil {
		t.Fatal(err)
	}
	var children []string
	for i := 1; i <= 6; i++ {
		row := stampFixtureRow{id: fmt.Sprintf("L-c%d", i), kind: "assistant_text", summary: fmt.Sprintf("child %d", i),
			parent: "L", turn: 1, index: i}
		if err := s.InsertItem(row.item(thread)); err != nil {
			t.Fatal(err)
		}
		children = append(children, row.id)
	}
	sealItemsForTest(t, s, thread, children[:3]...)
	sealItemsForTest(t, s, thread, children[3:]...)
	stripSubagentStampsForTest(t, s, thread)
	mustExec(t, s.db, `INSERT INTO subagent_aggregate_backfill(thread_id) VALUES (?)`, thread)
	mustExec(t, s.db, `PRAGMA user_version = 118`)
	s = reopenStore(t, s)
	assertSubagentStampParity(t, s, thread, "sealed and listed", false)

	var throughV119 []Migration
	for _, m := range migrations {
		if m.Version <= 119 {
			throughV119 = append(throughV119, m)
		}
	}
	if err := s.runDeferredMigrations(context.Background(), DeferredHost{}, throughV119); err != nil {
		t.Fatal(err)
	}
	if got := deferredWatermarkOf(t, s); got != 119 {
		t.Fatalf("watermark after v119's phase = %d, want 119", got)
	}
	if n := countRows(t, s, `SELECT count(*) FROM thread_import_chunks WHERE thread_id = ?`, thread); n != 0 {
		t.Fatalf("v119's phase left %d sealed chunks", n)
	}
	assertSubagentStampParity(t, s, thread, "folded, still listed", false)

	if err := s.RunDeferredMigrations(context.Background(), DeferredHost{}); err != nil {
		t.Fatal(err)
	}
	if deferredPending(t, s) || deferredFailureOf(t, s) != nil {
		t.Fatalf("phases left watermark %d, failure %+v", deferredWatermarkOf(t, s), deferredFailureOf(t, s))
	}
	if _, mode := subagentStampStateForTest(t, s, thread, "L"); mode != subagentStampClean {
		t.Fatalf("the launch ends mode %d, want clean", mode)
	}
	if meta := itemMetaForTest(t, s, thread, "L"); !strings.Contains(meta, `"subagentDescendantCount":6`) {
		t.Fatalf("launch meta %s, want a card counting the six folded children", meta)
	}
	assertSubagentStampParity(t, s, thread, "stamped", true)
}

// TestSubagentAggregateRecomputeSkipsARowWrittenSinceItsRead pins the
// optimistic write: values computed on a snapshot do not land on a row
// that moved after the snapshot, and the call reports work remaining.
func TestSubagentAggregateRecomputeSkipsARowWrittenSinceItsRead(t *testing.T) {
	s := newTestStore(t)
	const thread = "t-optimistic"
	mustCreateThread(t, s, thread)
	seedStampLaunch(t, s, thread, "L", 1, 2)
	stripSubagentStampsForTest(t, s, thread)
	writes, err := computeSubagentStamps(s.reader(), thread, []string{"L"})
	if err != nil || len(writes) != 1 {
		t.Fatalf("compute: %d writes (%v)", len(writes), err)
	}
	// The row moves after the read: a child lands and the trigger marks
	// the unstamped launch dirty.
	if _, err := s.db.Exec(`INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status, summary, parent_id, created_at, updated_at)
	  VALUES ('L-late', ?, 1, 50, 'tool_call', 'assistant', 'running', 'Bash: late', 'L', 1, 1)`, thread); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	landed, err := writeSubagentStampsTx(tx, thread, writes, true)
	if err != nil {
		t.Fatal(err)
	}
	if landed != 0 {
		t.Fatalf("a stamp computed before the row moved landed on it")
	}
}

// TestSubagentAggregateBulkLoadRestamps pins the loaders that write with
// the triggers' aggregate work suspended: rows that arrive carrying
// another store's stamps are rebuilt from this thread's rows, and an
// imported tail under a local launch re-stamps the launch.
func TestSubagentAggregateBulkLoadRestamps(t *testing.T) {
	s := newTestStore(t)
	const thread = "t-bulk"
	mustCreateThread(t, s, thread)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := setHistoryBulkLoadTx(tx, thread, true, "test load"); err != nil {
		t.Fatal(err)
	}
	foreign := `{"subagentDescendantCount":40,"subagentLatestChildSummary":"elsewhere","subagentAggregateState":{"gen":7}}`
	for _, r := range []stampFixtureRow{
		{id: "L", kind: "tool_call", tool: "Agent", summary: "Agent: moved", meta: foreign, turn: 1},
		{id: "L-c1", kind: "tool_call", tool: "Bash", summary: "Bash: here", parent: "L", turn: 1, index: 1},
		{id: "plain", kind: "tool_call", tool: "Bash", summary: "Bash: alone", meta: foreign, turn: 2},
	} {
		item := r.item(thread)
		applyItemDefaults(&item)
		if err := insertItemTx(tx, item, "test load"); err != nil {
			t.Fatalf("load %s: %v", r.id, err)
		}
	}
	if err := restampSubagentAggregatesTx(tx, thread); err != nil {
		t.Fatal(err)
	}
	if err := setHistoryBulkLoadTx(tx, thread, false, "test load"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	assertSubagentStampParity(t, s, thread, "loaded", true)
	if meta := itemMetaForTest(t, s, thread, "plain"); strings.Contains(meta, "elsewhere") || strings.Contains(meta, metaKeySubagentAggregateState) {
		t.Errorf("a childless row kept a loaded stamp: %s", meta)
	}

	// An imported tail with a row under the local launch.
	if err := s.ApplyImportBatch(thread, ImportBatch{
		Turns: []Turn{{TurnID: thread + ":3", ThreadID: thread, TurnIndex: 3, StartedAt: 3_000}},
		Rows: []ImportRow{{Item: stampFixtureRow{id: "L-imported", kind: "tool_call", tool: "Bash",
			summary: "Bash: imported", parent: "L", turn: 3}.item(thread)}},
	}); err != nil {
		t.Fatalf("apply import tail: %v", err)
	}
	assertSubagentStampParity(t, s, thread, "imported tail", true)
}

// TestMigrationV121SubagentAggregateStamps drives the migration over a
// database with history: the indexes and the backfill list are created,
// existing anchors keep no stamp until the deferred phase, the live
// trigger generation is installed, and the list follows thread deletion.
func TestMigrationV121SubagentAggregateStamps(t *testing.T) {
	db := migrateThrough(t, 115)
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	for _, id := range []string{"t-agents", "t-chat"} {
		mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode) VALUES ('`+id+`', 'p', 'T', 'claude', '/tmp', '', 1, 1, 0, 'chat')`)
	}
	insertRow := func(thread, id, kind, parent string, index int) {
		mustExec(t, db, `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
			summary, parent_id, tool_name, meta, created_at, updated_at)
			VALUES ('`+id+`', '`+thread+`', 0, `+fmt.Sprint(index)+`, '`+kind+`', 'assistant', 'completed',
			'`+id+`', '`+parent+`', '', '{}', 1, 1)`)
	}
	insertRow("t-agents", "L", "tool_call", "", 0)
	insertRow("t-agents", "L-c1", "tool_call", "L", 1)
	insertRow("t-chat", "text", "assistant_text", "", 0)

	migrateFrom(t, db, 115)

	for _, index := range []string{"idx_items_subagent_resume_prompt", "idx_items_subagent_aggregate_dirty", "idx_items_running_nested_fg_tool_calls"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, index).Scan(&n); err != nil || n != 1 {
			t.Errorf("index %s missing (%v)", index, err)
		}
	}
	listed, err := subagentAnchorIDs(db, `SELECT thread_id FROM subagent_aggregate_backfill ORDER BY thread_id`)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(listed, []string{"t-agents"}) {
		t.Errorf("backfill lists %v, want the thread with tool calls only", listed)
	}
	var state sql.NullString
	if err := db.QueryRow(`SELECT json_type(meta, '` + aggStatePath + `') FROM items WHERE thread_id = 't-agents' AND id = 'L'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state.Valid {
		t.Errorf("the migration stamped an existing anchor inline; that is the deferred phase's work")
	}
	var triggerSQL string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = 'trg_items_rev_insert'`).Scan(&triggerSQL); err != nil {
		t.Fatal(err)
	}
	if want := historyRevTriggerStatements(t)[0]; normalizeSQLText(triggerSQL) != normalizeSQLText(want) {
		t.Errorf("v121 did not install the live insert trigger")
	}
	// A new child under the legacy anchor cannot be applied to a stamp
	// that does not exist: the anchor goes dirty for the settle.
	insertRow("t-agents", "L-c2", "tool_call", "L", 2)
	var dirty int
	if err := db.QueryRow(`SELECT count(*) FROM items WHERE thread_id = 't-agents' AND ` + subagentAggregateDirtyPredicate).Scan(&dirty); err != nil || dirty != 1 {
		t.Errorf("legacy anchor dirty rows = %d (%v), want 1", dirty, err)
	}
	mustExec(t, db, `DELETE FROM threads WHERE id = 't-agents'`)
	if listed, err := subagentAnchorIDs(db, `SELECT thread_id FROM subagent_aggregate_backfill`); err != nil || len(listed) != 0 {
		t.Errorf("deleted thread still listed: %v (%v)", listed, err)
	}
}

// TestSubagentChildProbeKeepsTheCallersAliases pins that the child probe
// answers for the row the caller names even when the caller's own aliases
// match the probe's table names: an outer items c, and an outer
// thread_import_chunks refs whose thread scopes the import arm.
func TestSubagentChildProbeKeepsTheCallersAliases(t *testing.T) {
	s := newTestStore(t)
	for _, thread := range []string{"t-parent", "t-other"} {
		newImportTargetThread(t, s, thread)
	}
	importRows := func(thread string, rows ...stampFixtureRow) {
		t.Helper()
		batch := ImportBatch{Turns: []Turn{{TurnID: thread + ":0", ThreadID: thread, TurnIndex: 0, StartedAt: 1_000}}}
		for _, r := range rows {
			batch.Rows = append(batch.Rows, ImportRow{Item: r.item(thread)})
		}
		if err := s.ApplyImportBatch(thread, batch); err != nil {
			t.Fatalf("import into %s: %v", thread, err)
		}
	}
	importRows("t-parent", stampFixtureRow{id: "P", kind: "tool_call", tool: "Agent", summary: "Agent: imported"})
	importRows("t-other", stampFixtureRow{id: "P-c1", kind: "tool_call", tool: "Bash", summary: "Bash: elsewhere", parent: "P", index: 1})

	imported := func(thread string) bool {
		t.Helper()
		var has bool
		if err := s.db.QueryRow(`SELECT `+aggHasChildSQL("refs.thread_id", "'P'", "")+`
		    FROM thread_import_chunks refs WHERE refs.thread_id = ? LIMIT 1`, thread).Scan(&has); err != nil {
			t.Fatalf("probe %s: %v", thread, err)
		}
		return has
	}
	if imported("t-parent") {
		t.Error("P's child in another thread's chunk counts as P's child in t-parent")
	}
	if !imported("t-other") {
		t.Error("the import arm misses a child in the thread's own chunk")
	}

	const local = "t-local"
	mustCreateThread(t, s, local)
	seedStampLaunch(t, s, local, "K", 1, 1)
	for id, want := range map[string]bool{"K": true, "K-c1": false} {
		var has bool
		if err := s.db.QueryRow(`SELECT `+aggHasLocalChildSQL("c.thread_id", "c.id", "")+`
		    FROM items c WHERE c.thread_id = ? AND c.id = ?`, local, id).Scan(&has); err != nil {
			t.Fatalf("probe %s: %v", id, err)
		}
		if has != want {
			t.Errorf("local child probe for %s = %v, want %v", id, has, want)
		}
	}
}
