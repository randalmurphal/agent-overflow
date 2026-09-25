package store

import (
	"strings"
	"testing"
)

// TestPointerForkQueriesStayIndexed pins the plans the fork design depends
// on: fork creation reads only the source's unsettled rows, a lineage arm
// walks the ancestor's index in page order, and the reads a write makes to
// learn whether a fork shows what it changes (the guards of fork_triggers.go
// and the split's reader lookups) are probes of the ancestor index, which
// find nothing for a row after every reader's cut.
func TestPointerForkQueriesStayIndexed(t *testing.T) {
	s := forkChainFixture(t)
	plan := explainPlan(t, s, forkUnsettledRowsSQL, "G", 9, 0, "G", 9, 0)
	searches := 0
	for _, row := range plan {
		if strings.HasPrefix(row.detail, "SCAN ") {
			t.Errorf("unsettled rows scan: %s\n%s", row.detail, planText(plan))
		}
		if strings.HasPrefix(row.detail, "SEARCH items ") {
			searches++
			if !strings.Contains(row.detail, "idx_items_unsettled") {
				t.Errorf("unsettled rows read without idx_items_unsettled: %s\n%s", row.detail, planText(plan))
			}
		}
	}
	if searches != 2 {
		t.Errorf("unsettled rows: %d item searches, want the source's and its ancestors'\n%s", searches, planText(plan))
	}

	query, args := mustTimelineIDSelection(t, s, "G", timelineSelection{OrderBy: "turn_index DESC, item_index DESC", Limit: 20})
	assertEveryItemsArmWalksAnIndex(t, s, "fork page", query, 3, args...)

	const ancestorIndex = "idx_thread_fork_lineage_ancestor"
	for _, probe := range []struct {
		name, query string
		args        []any
		want        []string
	}{
		{"readers past a row", readerLevelsSQL, []any{"S", 0, 0}, []string{ancestorIndex}},
		{"last reader cut", maxReaderCutSQL, []any{"S"}, []string{ancestorIndex}},
		{"shown item", "SELECT " + readerShowsItemSQL("?1", "?2", "?3", "?4"), []any{"S", "a0", 0, 1}, []string{ancestorIndex}},
		{"shown turn", "SELECT " + readerShowsTurnSQL("?1", "?2"), []any{"S", 0}, []string{ancestorIndex}},
		{"shown payload", "SELECT " + readerShowsPayloadSQL("?1", "?2"), []any{"S", "p"},
			[]string{ancestorIndex, "idx_items_payload_id", "idx_items_input_payload_id", "idx_import_history_payloads_id"}},
		{"empty holder level", emptyHolderLevelSQL, []any{"G", "F"}, []string{"idx_thread_fork_lineage_ancestor"}},
	} {
		plan := explainPlan(t, s, probe.query, probe.args...)
		text := planText(plan)
		for _, row := range plan {
			if strings.HasPrefix(row.detail, "SCAN ") && !strings.Contains(row.detail, "json_each") && row.detail != "SCAN CONSTANT ROW" {
				t.Errorf("%s scans: %s\n%s", probe.name, row.detail, text)
			}
			if strings.Contains(row.detail, "USE TEMP B-TREE") {
				t.Errorf("%s sorts: %s\n%s", probe.name, row.detail, text)
			}
		}
		for _, index := range probe.want {
			if !strings.Contains(text, index) {
				t.Errorf("%s does not probe %s\n%s", probe.name, index, text)
			}
		}
	}
}

// assertEveryItemsArmWalksAnIndex is assertLocalArmWalksAnIndex for every
// arm that reads the physical `items` table: the thread's own and one per
// lineage level. None may sort, scan, or read its lineage row other than by
// primary key. Imported arms sort their chunk rows as a thread's own
// imported arm does, bounded by the lineage cut.
func assertEveryItemsArmWalksAnIndex(t *testing.T, s *Store, what, query string, arms int, args ...any) {
	t.Helper()
	plan := explainPlan(t, s, query, args...)
	text := planText(plan)
	byID := make(map[int]planRow, len(plan))
	var searches []planRow
	for _, row := range plan {
		byID[row.id] = row
		switch {
		case strings.HasPrefix(row.detail, "SCAN items"), strings.HasPrefix(row.detail, "SCAN l"),
			strings.Contains(row.detail, "timeline_items"):
			t.Errorf("%s: %s\n%s", what, row.detail, text)
		case strings.HasPrefix(row.detail, "SEARCH l ") && !strings.Contains(row.detail, "PRIMARY KEY"):
			t.Errorf("%s: lineage read off its key: %s\n%s", what, row.detail, text)
		case strings.HasPrefix(row.detail, "SEARCH items USING INDEX idx_items_"), strings.HasPrefix(row.detail, "SEARCH items USING COVERING INDEX idx_items_"):
			searches = append(searches, row)
		}
	}
	if len(searches) < arms {
		t.Fatalf("%s: %d indexed item arms, want %d\n%s", what, len(searches), arms, text)
	}
	for _, search := range searches {
		for id := search.id; id != 0; id = byID[id].parent {
			row, ok := byID[id]
			if !ok {
				break
			}
			for _, sibling := range plan {
				if (sibling.id == row.id || sibling.parent == row.parent) && strings.Contains(sibling.detail, "USE TEMP B-TREE FOR ORDER BY") {
					t.Errorf("%s: a sorter covers an item arm (%q)\n%s", what, sibling.detail, text)
				}
			}
		}
	}
}

// TestLosingCompletionReadProbesTheCompletionIndex pins the one statement
// launchesCompletedTx runs for every candidate, for each caller's kept
// predicate: each arm is driven from the candidate list and probes
// its completion index per candidate. The stat-less planner would rather
// walk the thread's position range once per candidate, which a fork of a
// thread with hundreds of settled launches paid in seconds; the unary
// plus on the position columns is what keeps it off that index.
func TestLosingCompletionReadProbesTheCompletionIndex(t *testing.T) {
	s := forkChainFixture(t)
	split := forkSplit{fromTurn: 1, where: "id = ?", args: []any{"x"}}
	span, spanArgs := split.moveSel(timelineRow{turn: 9, item: 0})
	for _, c := range []struct {
		name string
		kept string
		args []any
	}{
		{"fork creation", "(+turn_index, +item_index) < (?, ?)", []any{9, 0}},
		{"fork creation beyond the cut", "(+turn_index, +item_index) >= (?, ?)", []any{9, 0}},
		{"split", "NOT (" + span + ")", spanArgs},
	} {
		query, args, err := launchesCompletedSQL(s.db, "G", c.kept, c.args)
		if err != nil {
			t.Fatal(err)
		}
		plan := explainPlan(t, s, query, append([]any{`["a","b"]`}, args...)...)
		text := planText(plan)
		for _, row := range plan {
			if strings.HasPrefix(row.detail, "SCAN ") && !strings.Contains(row.detail, "json_each") {
				t.Errorf("%s scans: %s\n%s", c.name, row.detail, text)
			}
			if strings.Contains(row.detail, "USE TEMP B-TREE") {
				t.Errorf("%s sorts: %s\n%s", c.name, row.detail, text)
			}
		}
		// G reads itself, F and S: three local arms and three imported.
		for index, want := range map[string]int{
			"SEARCH items USING INDEX idx_items_completion_of (thread_id=? AND completion_of=?)":    3,
			"SEARCH items USING INDEX idx_import_history_items_completion_lookup (completion_of=?)": 3,
		} {
			if n := strings.Count(text, index); n != want {
				t.Errorf("%s probes %s %d times, want %d\n%s", c.name, index, n, want, text)
			}
		}
	}
}
