package store

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// A pointer fork resolves its inherited rows through its lineage: a
// lookup by id, by key or by turn probes thread_fork_lineage by the fork's
// id and then the ancestor's key or turn index. It never scans a history
// table or the lineage, and never walks an ancestor's whole thread.

const keyedForkID = "keyed-fork"

// historyTables are the physical tables a lookup reads history from.
var historyTables = []string{
	"items", "import_history_items", "thread_import_chunks", "thread_import_item_overrides",
	"thread_fork_lineage", "thread_fork_hidden", "turns",
	"payloads", "payload_chunks", "edit_file_snapshots", "import_history_payloads",
}

// localItemsIndex matches a plan step over an index of the thread-owned
// items table; `items` also aliases import_history_items, whose indexes
// are named for it.
var localItemsIndex = regexp.MustCompile(`\b(?:idx_items_[a-z_]+|sqlite_autoindex_items_\d+)\b`)

// planSearchKey is a SEARCH step's whole key, which may hold a row-value
// range such as (turn_index,item_index)<(?,?); an outer join's step ends
// in LEFT-JOIN.
var planSearchKey = regexp.MustCompile(`USING (?:COVERING INDEX \S+|INDEX \S+|INTEGER PRIMARY KEY|PRIMARY KEY) \((.*)\)(?: LEFT-JOIN)?$`)

// cutOnlyKey is a SEARCH key that pins the thread and bounds the rows by a
// lineage cut alone: the step walks the thread's history below the cut.
var cutOnlyKey = regexp.MustCompile(`^thread_id=\? AND (?:turn_index<\?|\(turn_index,item_index\)<\(\?,\?\))$`)

var tableAliasPattern = regexp.MustCompile(`(?i)\b(` + strings.Join(historyTables, "|") + `)\s+(?:AS\s+)?([a-z_][a-z0-9_]*)`)

// historyTableNames maps every name a plan can give a history table in
// sources (the table itself or an alias) to the table.
func historyTableNames(sources ...string) map[string]string {
	names := make(map[string]string, len(historyTables))
	for _, table := range historyTables {
		names[table] = table
	}
	for _, source := range sources {
		for _, m := range tableAliasPattern.FindAllStringSubmatch(source, -1) {
			if !sqlWordsAfterTable[strings.ToUpper(m[2])] {
				names[m[2]] = strings.ToLower(m[1])
			}
		}
	}
	return names
}

// lineagePlanViolations checks one plan against the lineage contract. It
// returns the offending nodes, and whether the plan read the lineage by
// the fork's id.
func lineagePlanViolations(plan []planRow, names map[string]string) (violations []string, lineageByThread bool) {
	for _, r := range plan {
		m := planAccessPattern.FindStringSubmatch(r.detail)
		if m == nil {
			continue
		}
		table, ok := names[m[2]]
		if !ok {
			continue
		}
		if m[1] == "SCAN" {
			violations = append(violations, r.detail)
			continue
		}
		key := planSearchKey.FindStringSubmatch(r.detail)
		switch {
		case table == "thread_fork_lineage":
			switch {
			case key != nil && strings.HasPrefix(key[1], "thread_id=?"):
				lineageByThread = true
			case key != nil && strings.HasPrefix(key[1], "ancestor_id=?"):
				// Which forks read a row: the reverse probe.
			default:
				violations = append(violations, r.detail)
			}
		case localItemsIndex.MatchString(r.detail):
			// A thread's own rows: the key must pin more than the thread,
			// and more than an ancestor's rows below a lineage cut.
			if key == nil || key[1] == "thread_id=?" || cutOnlyKey.MatchString(key[1]) {
				violations = append(violations, r.detail)
			}
		}
	}
	return violations, lineageByThread
}

// forkChildRowWrite writes the fork's own rows only; the lineage probes it
// makes are its triggers', which TestPointerForkRowTriggersProbeTheLineage
// explains.
const forkChildRowWrite = "fork child row write"

// forkLookups are the keyed lookups run on a fork of the seeded thread cut
// at its tail. The seeded writes at turns 1 and 2 are left out: a fork
// writes no row before its cut. The fork writes at its cut turn instead,
// including a child row whose insert and revision bump run the row
// stamping and fork triggers, before the reverts lower its cut.
func forkLookups(fork string) []keyedLookup {
	var lookups []keyedLookup
	for _, lookup := range keyedLookups(fork) {
		switch lookup.name {
		case "AppendItem", "UpsertItemAtTurnHead", "PlaceUserItemsAfterBoundary":
			continue
		case "DeleteConversationFromItem":
			lookups = append(lookups,
				keyedLookup{name: "fork AppendItem", turn: true, run: func(t *testing.T, s *Store) {
					must[int](t, "append")(appendCarded(s, Item{ID: "fork-appended", ThreadID: fork, TurnIndex: 4, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "late"}))
				}},
				keyedLookup{name: forkChildRowWrite, run: func(t *testing.T, s *Store) {
					launch := Item{ID: "fork-launch", ThreadID: fork, TurnIndex: 4, ItemIndex: 10, Kind: "tool_call", Role: "assistant", Status: "running", ToolName: "Task", Summary: "Task", Meta: "{}"}
					child := Item{ID: "fork-child", ThreadID: fork, TurnIndex: 4, ItemIndex: 11, Kind: "tool_call", Role: "assistant", Status: "completed", ToolName: "Bash", ParentID: launch.ID, Summary: "Bash", Meta: "{}"}
					for _, item := range []Item{launch, child} {
						if err := insertCarded(s, item); err != nil {
							t.Error(err)
						}
					}
					summary := "Bash done"
					if err := updateFieldsCarded(s, fork, launch.ID, child.ID, ItemPartialUpdate{Summary: &summary}); err != nil {
						t.Errorf("child rev bump: %v", err)
					}
				}},
			)
		}
		lookups = append(lookups, lookup)
	}
	return lookups
}

func TestPointerForkLookupsProbeTheLineage(t *testing.T) {
	s := newTestStore(t)
	seedKeyedLookupThread(t, s)
	if err := s.CreatePointerFork(makeThread(keyedForkID, "claude"), keyedThreadID, ForkCut{}, testInterruptedSummary, 1); err != nil {
		t.Fatal(err)
	}
	if got := forkLineage(t, s, keyedForkID); len(got) != 1 {
		t.Fatalf("fork lineage = %v, want one level", got)
	}
	views := chunkRefViewSQL(t, s)
	rec := recordStatements(t, s)
	for _, lookup := range forkLookups(keyedForkID) {
		stmts := rec.capture(func() { lookup.run(t, s) })
		if len(stmts) == 0 {
			t.Errorf("%s: recorded no statements", lookup.name)
			continue
		}
		readLineage := false
		for _, stmt := range stmts {
			plan := explainPlan(t, s, stmt.query, stmt.args...)
			violations, byThread := lineagePlanViolations(plan, historyTableNames(stmt.query, views))
			readLineage = readLineage || byThread
			for _, node := range violations {
				t.Errorf("%s: %q leaves the lineage's keys\n%s\n%s", lookup.name, node, stmt.query, planText(plan))
			}
			_, enumerations := chunkRefNodes(plan, chunkRefNames(stmt.query, views))
			for _, node := range enumerations {
				t.Errorf("%s: %q enumerates a thread's chunks\n%s\n%s", lookup.name, node, stmt.query, planText(plan))
			}
		}
		if !readLineage && lookup.name != forkChildRowWrite {
			t.Errorf("%s: no statement read the fork's lineage; the check proved nothing", lookup.name)
		}
	}
}

// triggerPrograms lists the triggers whose programs a statement codes, one
// entry per coded program, nested programs included. SQLite codes a row
// trigger only into a statement that can fire it (an UPDATE OF trigger only
// into an UPDATE that sets one of its columns), so a trigger absent here
// does no work, not even its WHEN, when the statement runs.
func triggerPrograms(t *testing.T, s *Store, query string, args ...any) []string {
	t.Helper()
	if len(args) == 0 {
		args = make([]any, strings.Count(query, "?"))
	}
	rows, err := s.db.Query("EXPLAIN "+query, args...)
	if err != nil {
		t.Fatalf("explain: %v\n%s", err, query)
	}
	var programs []string
	for rows.Next() {
		var addr, p1, p2, p3, p5, comment any
		var opcode string
		var p4 sql.NullString
		if err := rows.Scan(&addr, &opcode, &p1, &p2, &p3, &p4, &p5, &comment); err != nil {
			t.Fatal(err)
		}
		if name, ok := strings.CutPrefix(p4.String, "-- TRIGGER "); ok {
			programs = append(programs, name)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	return programs
}

// The fork copy triggers guard row positions: two on insert, two on an
// UPDATE OF turn_index, item_index. A write that keeps every row where it
// is (a revision stamp, a content update, or an insert, including one under
// history_bulk_load, whose own stamp is a revision-only update) must not
// code the position pair at all, and only the insert itself codes the
// insert pair. The writes run on a source that a pointer fork reads.
func TestForkCopyTriggersSkipRevisionOnlyWrites(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 2)
	sealItemsForTest(t, s, "S", "u0", "a0")
	launch := Item{ID: "launch", ThreadID: "S", TurnIndex: 1, ItemIndex: 5, Kind: "tool_call", Role: "assistant", Status: "running", ToolName: "Task", Summary: "Task", Meta: "{}"}
	if err := insertCarded(s, launch); err != nil {
		t.Fatal(err)
	}
	if err := insertWithPayloadCarded(s,
		Item{ID: "tool", ThreadID: "S", TurnIndex: 1, ItemIndex: 6, Kind: "tool_call", Role: "assistant", Status: "running", ToolName: "Bash", ParentID: launch.ID, PayloadID: "pt", Meta: "{}"},
		Payload{ID: "pt", Kind: "text", Meta: "{}", Data: []byte("out")},
	); err != nil {
		t.Fatal(err)
	}
	mustPointerFork(t, s, "S", "F", throughTurn(0))

	const (
		positionInsert = "trg_items_fork_position"
		snapshotInsert = "trg_items_fork_snapshot"
		positionUpdate = "trg_items_fork_position_update"
		snapshotMove   = "trg_items_fork_snapshot_move"
	)
	moved := triggerPrograms(t, s, `UPDATE items SET turn_index = ?, item_index = ? WHERE thread_id = ? AND id = ?`)
	if !slices.Contains(moved, positionUpdate) || !slices.Contains(moved, snapshotMove) {
		t.Fatalf("a position update codes %v; the check cannot see the position triggers", moved)
	}

	summary := "edited"
	writes := []struct {
		name string
		run  func()
	}{
		{"child insert", func() {
			if err := insertCarded(s, Item{ID: "child", ThreadID: "S", TurnIndex: 1, ItemIndex: 7, Kind: "tool_call", Role: "assistant", Status: "completed", ToolName: "Read", ParentID: launch.ID, Summary: "Read", Meta: "{}"}); err != nil {
				t.Fatal(err)
			}
		}},
		{"child content update", func() {
			if err := updateFieldsCarded(s, "S", launch.ID, "child", ItemPartialUpdate{Summary: &summary}); err != nil {
				t.Fatal(err)
			}
		}},
		{"payload append revision touch", func() {
			if err := s.AppendPayloadData("S", "pt", []byte(" more"), "{}", 2); err != nil {
				t.Fatal(err)
			}
		}},
		{"imported row the fork reads, localized under history_bulk_load", func() {
			if _, err := s.UpdateItemFields("S", "a0", ItemPartialUpdate{Summary: &summary}); err != nil {
				t.Fatal(err)
			}
		}},
	}
	rec := recordStatements(t, s)
	var revisionTouches, bulkLoadInserts int
	for _, write := range writes {
		stmts := rec.capture(write.run)
		bulkLoad := false
		for _, stmt := range stmts {
			query := strings.TrimSpace(stmt.query)
			insert := strings.HasPrefix(query, "INSERT INTO items")
			if strings.Contains(query, "SET updated_at = updated_at") {
				revisionTouches++
			}
			if strings.HasPrefix(query, "UPDATE threads SET history_bulk_load") {
				bulkLoad = fmt.Sprint(stmt.args[0]) == "1"
			} else if bulkLoad && insert {
				bulkLoadInserts++
			}
			counts := map[string]int{}
			for _, name := range triggerPrograms(t, s, stmt.query, stmt.args...) {
				counts[name]++
			}
			for _, name := range []string{positionUpdate, snapshotMove} {
				if counts[name] > 0 {
					t.Errorf("%s: the statement codes %s\n%s", write.name, name, query)
				}
			}
			for _, name := range []string{positionInsert, snapshotInsert} {
				if want := map[bool]int{true: 1}[insert]; counts[name] != want {
					t.Errorf("%s: %d programs of %s, want %d\n%s", write.name, counts[name], name, want, query)
				}
			}
		}
	}
	if revisionTouches == 0 || bulkLoadInserts == 0 {
		t.Fatalf("recorded %d revision touches and %d bulk-load inserts; the writes no longer reach both", revisionTouches, bulkLoadInserts)
	}
}

// forkReaderRowsRead matches the hand-off's read of the rows the readers
// at one depth show (readersInheritedRowsByID).
var forkReaderRowsRead = regexp.MustCompile(`\bthread_fork_lineage r\s`)

// sourceWriteCost is what one write on a source costs the store: the
// statements it runs and the rows they and their triggers change.
type sourceWriteCost struct {
	statements  int
	rowsChanged int64
}

// TestSourceWritesCostTheSameForAnyForkCount: a source keeps running after
// forks are made from it mid-turn (a side chat, a thread_ask copy). Each
// fork took interrupted copies of the running rows and hides the source's,
// and reads nothing after its cut, so no fork reads what the source writes
// next. Those writes must cost the same with ten forks as with one and
// change the rows they change with none: a fork's stamps are its own and
// move only for a row it shows, so no write touches a fork's thread row,
// and the hand-off asks every reader at a depth in one statement
// (handOffReadIDsTx), and none for a row after every reader's cut. Every
// statement also keeps to the lineage keys.
func TestSourceWritesCostTheSameForAnyForkCount(t *testing.T) {
	writes := []struct {
		name string
		// readerReads is how many reader-row reads the write makes with
		// forks: one per depth for a row below their cuts, none after.
		readerReads int
		run         func(*testing.T, *Store)
	}{
		{"child insert", 0, func(t *testing.T, s *Store) {
			if err := insertCarded(s, Item{ID: "child", ThreadID: "S", TurnIndex: 3, ItemIndex: 7, Kind: "tool_call", Role: "assistant", Status: "running", ToolName: "Read", ParentID: "launch", Summary: "Read", Meta: "{}"}); err != nil {
				t.Fatal(err)
			}
		}},
		{"child content update", 0, func(t *testing.T, s *Store) {
			summary := "Read done"
			if err := updateFieldsCarded(s, "S", "launch", "child", ItemPartialUpdate{Summary: &summary}); err != nil {
				t.Fatal(err)
			}
		}},
		{"running row content update", 1, func(t *testing.T, s *Store) {
			summary := "Bash still running"
			if err := updateFieldsCarded(s, "S", "launch", "tool", ItemPartialUpdate{Summary: &summary}); err != nil {
				t.Fatal(err)
			}
		}},
		{"running row payload append and revision touch", 1, func(t *testing.T, s *Store) {
			if err := s.AppendPayloadData("S", "pt", []byte(" more"), "{}", 2); err != nil {
				t.Fatal(err)
			}
		}},
	}
	costs := map[int][]sourceWriteCost{}
	for _, forks := range []int{0, 1, 10} {
		s := newTestStore(t)
		seedLinearSource(t, s, "S", 3)
		if err := insertCarded(s, Item{ID: "launch", ThreadID: "S", TurnIndex: 3, ItemIndex: 5, Kind: "tool_call", Role: "assistant", Status: "running", ToolName: "Task", Summary: "Task", Meta: "{}"}); err != nil {
			t.Fatal(err)
		}
		if err := insertWithPayloadCarded(s,
			Item{ID: "tool", ThreadID: "S", TurnIndex: 3, ItemIndex: 6, Kind: "tool_call", Role: "assistant", Status: "running", ToolName: "Bash", ParentID: "launch", PayloadID: "pt", Meta: "{}"},
			Payload{ID: "pt", Kind: "text", Meta: "{}", Data: []byte("out")},
		); err != nil {
			t.Fatal(err)
		}
		for i := range forks {
			mustPointerFork(t, s, "S", fmt.Sprintf("F%d", i), ForkCut{})
		}
		rec := recordStatements(t, s)
		totalChanges := func() int64 {
			t.Helper()
			var n int64
			if err := s.db.QueryRow(`SELECT total_changes()`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		for _, write := range writes {
			before := totalChanges()
			stmts := rec.capture(func() { write.run(t, s) })
			costs[forks] = append(costs[forks], sourceWriteCost{statements: len(stmts), rowsChanged: totalChanges() - before})
			readerReads := 0
			for _, stmt := range stmts {
				if forkReaderRowsRead.MatchString(stmt.query) {
					readerReads++
				}
			}
			if want := min(forks, write.readerReads); readerReads != want {
				t.Errorf("%d forks, %s: %d reader-row reads, want %d", forks, write.name, readerReads, want)
			}
			for _, stmt := range stmts {
				plan := explainPlan(t, s, stmt.query, stmt.args...)
				violations, _ := lineagePlanViolations(plan, historyTableNames(stmt.query))
				for _, node := range violations {
					t.Errorf("%d forks, %s: %q leaves the lineage's keys\n%s\n%s", forks, write.name, node, stmt.query, planText(plan))
				}
			}
		}
		if forks > 0 {
			if got := forkRows(t, s, "F0"); len(got) != 8 || got[7].ID != "tool" || got[7].Status == "running" {
				t.Fatalf("fork rows = %+v, want the source's rows with the running ones interrupted", got)
			}
		}
	}
	for i, write := range writes {
		none, one, ten := costs[0][i], costs[1][i], costs[10][i]
		if ten != one {
			t.Errorf("%s with 10 forks costs %+v, with 1 %+v", write.name, ten, one)
		}
		if one.rowsChanged != none.rowsChanged {
			t.Errorf("%s with a fork changes %d rows, with none %d", write.name, one.rowsChanged, none.rowsChanged)
		}
	}
}

// Trigger programs do not appear in their statement's plan. Every
// statement of the triggers a row write fires (the row stamping, fork
// position and fork snapshot triggers on items, and the history stamp
// triggers on threads they update) is explained on its own, with the row
// references bound as parameters, against the same contract.
func TestPointerForkRowTriggersProbeTheLineage(t *testing.T) {
	s := newTestStore(t)
	rows, err := s.db.Query(`SELECT name, sql FROM sqlite_master WHERE type = 'trigger' AND tbl_name IN ('items', 'threads') ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	triggers := map[string]string{}
	for rows.Next() {
		var name, text string
		if err := rows.Scan(&name, &text); err != nil {
			t.Fatal(err)
		}
		triggers[name] = text
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	for _, name := range append(append([]string{}, revTriggerNames...),
		"trg_items_fork_position", "trg_items_fork_position_update",
		"trg_items_fork_snapshot", "trg_items_fork_snapshot_move", "trg_items_fork_reader_stamp", "trg_threads_fork_source_delete") {
		if triggers[name] == "" {
			t.Fatalf("trigger %s is missing", name)
		}
	}
	for name, text := range triggers {
		m := triggerPattern.FindStringSubmatch(text)
		if m == nil {
			t.Fatalf("cannot parse trigger %s:\n%s", name, text)
		}
		var statements []string
		if m[1] != "" {
			statements = append(statements, "SELECT "+m[1])
		}
		// A RAISE message may hold a semicolon.
		for _, body := range strings.Split(triggerRaise.ReplaceAllString(m[2], "NULL"), ";") {
			if body = strings.TrimSpace(body); body != "" {
				statements = append(statements, body)
			}
		}
		for _, statement := range statements {
			statement = triggerRowRef.ReplaceAllString(statement, "?")
			plan := explainPlan(t, s, statement, make([]any, strings.Count(statement, "?"))...)
			violations, _ := lineagePlanViolations(plan, historyTableNames(statement))
			for _, node := range violations {
				t.Errorf("%s: %q\n%s\n%s", name, node, statement, planText(plan))
			}
		}
	}
}
