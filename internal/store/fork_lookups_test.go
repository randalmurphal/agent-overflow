package store

import (
	"errors"
	"regexp"
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
// range such as (turn_index,item_index)<(?,?).
var planSearchKey = regexp.MustCompile(`USING (?:COVERING INDEX \S+|INDEX \S+|INTEGER PRIMARY KEY|PRIMARY KEY) \((.*)\)$`)

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
			// A thread's own rows: the key must pin more than the thread.
			if key == nil || key[1] == "thread_id=?" {
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
					must[int](t, "append")(s.AppendItem(Item{ID: "fork-appended", ThreadID: fork, TurnIndex: 4, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "late"}))
				}},
				keyedLookup{name: forkChildRowWrite, run: func(t *testing.T, s *Store) {
					launch := Item{ID: "fork-launch", ThreadID: fork, TurnIndex: 4, ItemIndex: 10, Kind: "tool_call", Role: "assistant", Status: "running", ToolName: "Task", Summary: "Task", Meta: "{}"}
					child := Item{ID: "fork-child", ThreadID: fork, TurnIndex: 4, ItemIndex: 11, Kind: "tool_call", Role: "assistant", Status: "completed", ToolName: "Bash", ParentID: launch.ID, Summary: "Bash", Meta: "{}"}
					for _, item := range []Item{launch, child} {
						if err := s.InsertItem(item); err != nil {
							t.Error(err)
						}
					}
					summary := "Bash done"
					must[int64](t, "child rev bump")(s.UpdateItemFields(fork, child.ID, ItemPartialUpdate{Summary: &summary}))
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
		"trg_items_fork_snapshot", "trg_items_fork_snapshot_move", "trg_threads_fork_history") {
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
