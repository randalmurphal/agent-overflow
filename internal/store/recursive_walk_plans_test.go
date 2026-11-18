package store

import (
	"context"
	"regexp"
	"strings"
	"testing"
)

// A recursive walk reads each level by the key of the queued row. Without
// sqlite_stat1, which no store database has (the app never runs ANALYZE),
// the planner may instead drive a step from the table through an index
// prefix it can search without the queue, such as thread_id alone, and scan
// the queue for every row that prefix selects: each step then reads every
// row of the thread. A fresh migrated store has no statistics either, so
// its plans are the ones every user database gets.

// walkPlan is what one walk's plan must hold: lines inside its recursive
// steps, lines anywhere, the indexes it may search by a thread or chunk
// prefix alone because their partial predicate bounds them to live rows,
// and the indexes a seed may read whole outside every step because their
// partial predicate holds only the walk's own seeds.
type walkPlan struct {
	step     []string
	want     []string
	prefixed []string
	seeds    []string
}

var (
	prefixOnlySearch = regexp.MustCompile(`^SEARCH \S+ USING (?:COVERING )?INDEX (\S+) \((?:thread_id|chunk_id)=\?\)$`)
	planSubqueryName = regexp.MustCompile(`^(?:CO-ROUTINE|MATERIALIZE) (\S+)$`)
)

// assertWalkProbesTheQueuedKey fails a plan that has no recursive step,
// scans anything but a CTE, view or json_each (whose own reads are nodes of
// the plan) or, outside its recursive steps, an index of w.seeds, builds an
// automatic index, searches an index by a thread or chunk prefix alone
// outside w.prefixed, or lacks a line of w.
func assertWalkProbesTheQueuedKey(t *testing.T, s *Store, name string, w walkPlan, query string, args ...any) {
	t.Helper()
	plan := explainPlan(t, s, query, args...)
	text := planText(plan)
	byID := make(map[int]planRow, len(plan))
	for _, r := range plan {
		byID[r.id] = r
	}
	inStep := func(r planRow) bool {
		for id := r.parent; id != 0; id = byID[id].parent {
			if byID[id].detail == "RECURSIVE STEP" {
				return true
			}
		}
		return false
	}
	scans := map[string]bool{"CONSTANT": true, "json_each": true}
	for _, r := range plan {
		if m := planSubqueryName.FindStringSubmatch(r.detail); m != nil {
			scans[m[1]] = true
		}
	}
	prefixed := map[string]bool{}
	for _, index := range w.prefixed {
		prefixed[index] = true
	}
	seeds := map[string]bool{}
	for _, index := range w.seeds {
		seeds[index] = true
	}
	seedScan := func(r planRow) bool {
		fields := strings.Fields(r.detail)
		return len(fields) == 5 && fields[2] == "USING" && fields[3] == "INDEX" && seeds[fields[4]] && !inStep(r)
	}
	steps := 0
	for _, r := range plan {
		switch {
		case r.detail == "RECURSIVE STEP":
			steps++
		case strings.HasPrefix(r.detail, "SCAN ") && !scans[strings.Fields(r.detail)[1]] && !seedScan(r):
			t.Errorf("%s scans a stored table: %q\n%s", name, r.detail, text)
		case strings.Contains(r.detail, "AUTOMATIC"):
			t.Errorf("%s builds an automatic index: %q\n%s", name, r.detail, text)
		}
		if m := prefixOnlySearch.FindStringSubmatch(r.detail); m != nil && !prefixed[m[1]] {
			t.Errorf("%s reads every row of a thread or chunk: %q\n%s", name, r.detail, text)
		}
	}
	if steps == 0 {
		t.Fatalf("%s has no recursive step; the check proves nothing\n%s", name, text)
	}
	for _, line := range w.step {
		found := false
		for _, r := range plan {
			found = found || (r.detail == line && inStep(r))
		}
		if !found {
			t.Errorf("%s: no recursive step reads %q\n%s", name, line, text)
		}
	}
	for _, line := range w.want {
		if !strings.Contains(text, line) {
			t.Errorf("%s does not read %q\n%s", name, line, text)
		}
	}
}

// TestRecursiveWalksProbeTheQueuedKey pins every recursive walk in the
// package to a keyed probe per queued row.
func TestRecursiveWalksProbeTheQueuedKey(t *testing.T) {
	const byItemKey = "SEARCH items USING INDEX sqlite_autoindex_items_1 (thread_id=? AND id=?)"

	t.Run("agent subtree", func(t *testing.T) {
		s := agentRowsFixture(t)
		for name, query := range map[string]string{
			"agentSubtreeUnsettledSQL": agentSubtreeUnsettledSQL,
			"agentSubtreeStreamingSQL": agentSubtreeStreamingSQL,
		} {
			assertWalkProbesTheQueuedKey(t, s, name, walkPlan{
				step: []string{"SEARCH c USING INDEX idx_items_parent (thread_id=? AND parent_id=?)"},
				want: []string{"SEARCH i USING INDEX sqlite_autoindex_items_1 (thread_id=? AND id=?)"},
			}, query, "T", "A")
		}
	})

	t.Run("owned workers", func(t *testing.T) {
		s := newTestStore(t)
		for _, id := range []string{"lead", "child", "grand"} {
			mustCreateThread(t, s, id)
		}
		spawnLocal(t, s, "l-c", "lead", "child", true)
		spawnLocal(t, s, "c-g", "child", "grand", true)
		// Both walks down the owned lineage take the same step
		// (ownedStepSQL), through the index of spawn links by caller, so
		// neither reads an owner's sends and asks; owned_threads and the
		// delete's walk read a thread's transfer by the thread's own key.
		step := []string{
			"SEARCH link USING INDEX idx_thread_requests_spawn_caller (caller_thread_id=?)",
			"SEARCH threads USING INDEX sqlite_autoindex_threads_1 (id=?)",
		}
		assertWalkProbesTheQueuedKey(t, s, "deleteFamilySQL", walkPlan{
			step: step,
			want: []string{
				"SEARCH past USING COVERING INDEX idx_agent_past_threads_agent (agent_id=?)",
				"SEARCH helper USING INDEX idx_agent_helpers_agent (agent_id=?)",
			},
			// An assistant's record is one row a thread names (UNIQUE), and
			// a thread is one assistant's past thread or helper at most
			// (their primary keys).
			prefixed: []string{"idx_thread_transfers_owner", "sqlite_autoindex_agents_2",
				"sqlite_autoindex_agent_past_threads_1", "sqlite_autoindex_agent_helpers_1"},
		}, deleteFamilySQL, `["lead"]`)
		assertWalkProbesTheQueuedKey(t, s, "ownedWorkersToArchiveSQL", walkPlan{
			step:     step,
			prefixed: []string{"idx_thread_transfers_owner"},
		}, ownedWorkersToArchiveSQL, "lead")
		// Whether a thread works for an assistant walks every spawn link up,
		// whatever the placement, by target, and an ask copy's ask by its
		// token.
		assertWalkProbesTheQueuedKey(t, s, "worksForAssistantSQL", walkPlan{
			step: []string{
				"SEARCH link USING INDEX idx_thread_requests_spawn_link (target_thread_id=?)",
				"SEARCH copy USING INDEX sqlite_autoindex_scratch_threads_1 (thread_id=?)",
				"SEARCH ask USING INDEX sqlite_autoindex_thread_requests_1 (token=?)",
			},
			// A thread's scratch record is one row, its primary key.
			prefixed: []string{"sqlite_autoindex_scratch_threads_1"},
		}, worksForAssistantSQL, "grand")
		// A decision's read of rest walks every spawn link down, whatever
		// the placement, by caller through the index of links alone.
		assertWalkProbesTheQueuedKey(t, s, "subtreeReachedSQL", walkPlan{
			step: []string{"SEARCH link USING INDEX idx_thread_requests_spawn_caller (caller_thread_id=?)"},
			want: []string{"SEARCH threads USING COVERING INDEX sqlite_autoindex_threads_1 (id=?)"},
		}, subtreeReachedSQL, `["lead"]`)
	})

	t.Run("agent requests", func(t *testing.T) {
		s := newTestStore(t)
		for _, id := range []string{"lead", "child", "grand"} {
			mustCreateThread(t, s, id)
		}
		seedAgentRequest(t, s, "l-c", "lead", ThreadRequestSpawn, "child", false, 100)
		seedAgentRequest(t, s, "c-g", "child", ThreadRequestSpawn, "grand", false, 110)
		assertWalkProbesTheQueuedKey(t, s, "threadAgentTreeSQL", walkPlan{
			step: []string{
				"SEARCH r USING INDEX sqlite_autoindex_thread_requests_1 (token=?)",
				"SEARCH a USING INDEX idx_thread_requests_open_created (caller_thread_id=?)",
				"SEARCH a USING INDEX idx_thread_requests_caller_settled (caller_thread_id=? AND settled_at>?)",
				"SEARCH a USING INDEX idx_thread_requests_open_below (caller_thread_id=?)",
				"SEARCH a USING INDEX sqlite_autoindex_thread_requests_1 (token=?)",
				"SEARCH a USING INDEX idx_thread_requests_nested_caller (caller_thread_id=?)",
				"SEARCH ps USING INDEX idx_thread_requests_spawn_link (target_thread_id=?)",
			},
			seeds: []string{"idx_thread_requests_open_deep", "idx_thread_requests_answer_due"},
		}, threadAgentTreeSQL(129), "lead", `{"waiting":["g-x"]}`, int64(0), 4, 160)
		assertWalkProbesTheQueuedKey(t, s, "threadAgentNamedRowsSQL", walkPlan{
			step: []string{
				"SEARCH sp USING INDEX idx_thread_requests_spawn_link (target_thread_id=?)",
				"SEARCH ps USING INDEX idx_thread_requests_spawn_link (target_thread_id=?)",
			},
			seeds: []string{"idx_thread_requests_open_deep", "idx_thread_requests_answer_due"},
		}, threadAgentNamedRowsSQL, "lead", `{"waiting":["g-x"]}`, int64(0), 4, 160, `["c-g"]`)
		assertWalkProbesTheQueuedKey(t, s, "threadAgentRowFramesSQL", walkPlan{
			step: []string{
				"SEARCH link USING INDEX idx_thread_requests_spawn_link (target_thread_id=?)",
				"SEARCH sp USING INDEX idx_thread_requests_spawn_link (target_thread_id=?)",
			},
		}, threadAgentRowFramesSQL, `["c-g"]`, "[]", "local", `["grand"]`, 4)
	})

	t.Run("background tray", func(t *testing.T) {
		s := newTestStore(t)
		if err := s.CreateThread(makeThread("t", "claude")); err != nil {
			t.Fatal(err)
		}
		seedTrayFixture(t, s)
		step := []string{"SEARCH p USING INDEX sqlite_autoindex_items_1 (thread_id=? AND id=?)"}
		assertWalkProbesTheQueuedKey(t, s, "liveBackgroundTasksSQL", walkPlan{
			step:     step,
			prefixed: []string{"idx_items_running_bg_tool_calls", "idx_items_running_nested_fg_tool_calls"},
		}, liveBackgroundTasksSQL, "t", int64(5000))
		assertWalkProbesTheQueuedKey(t, s, "backgroundTrayRowsSQL", walkPlan{
			step: step,
		}, backgroundTrayRowsSQL, "t", int64(5000), `["live","nested-agent"]`)
	})

	t.Run("descendants", func(t *testing.T) {
		s := forkChainFixture(t)
		for thread, step := range map[string][]string{
			"S": {
				"SEARCH items USING INDEX idx_items_parent (thread_id=? AND parent_id=?)",
				"SEARCH items USING INDEX idx_import_history_items_parent_lookup (parent_id=?)",
			},
			"G": {
				"SEARCH items USING INDEX idx_items_parent (thread_id=? AND parent_id=?)",
				"SEARCH items USING INDEX idx_import_history_items_parent_lookup (parent_id=?)",
				"SEARCH items USING INDEX idx_items_parent (thread_id=? AND parent_id=? AND turn_index<?)",
			},
		} {
			walk, args, err := descendantsWalk(s.reader(), thread, []string{"tool"}, visibleItemsFilterFor)
			if err != nil {
				t.Fatal(err)
			}
			assertWalkProbesTheQueuedKey(t, s, "descendantsWalk in "+thread, walkPlan{
				step: step,
			}, walk+` SELECT id FROM rel`, args...)
		}
	})

	// The row stamp every item write runs from the rev triggers walks the
	// written row's parent chain. EXPLAIN cannot see inside a trigger, so
	// each installed statement that walks is explained with its row
	// references bound as parameters, beside the Go form of the same walk.
	t.Run("stamped rows", func(t *testing.T) {
		s := newTestStore(t)
		ancestors := walkPlan{step: []string{byItemKey}}
		assertWalkProbesTheQueuedKey(t, s, "stampedRowIDsByParamsSQL", ancestors, stampedRowIDsByParamsSQL, "t", "child", "parent")
		rows, err := s.db.Query(`SELECT name, sql FROM sqlite_master WHERE type = 'trigger' AND sql LIKE '%WITH RECURSIVE%' ORDER BY name`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		triggers := map[string]string{}
		for rows.Next() {
			var name, text string
			if err := rows.Scan(&name, &text); err != nil {
				t.Fatal(err)
			}
			triggers[name] = text
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		for _, name := range []string{"trg_items_rev_insert", "trg_items_rev_update", "trg_items_rev_delete"} {
			if triggers[name] == "" {
				t.Fatalf("trigger %s does not walk the parent chain", name)
			}
		}
		for name, text := range triggers {
			m := triggerPattern.FindStringSubmatch(text)
			if m == nil {
				t.Fatalf("cannot parse trigger %s:\n%s", name, text)
			}
			walks := 0
			for _, statement := range strings.Split(m[2], ";") {
				if !strings.Contains(statement, "WITH RECURSIVE") {
					continue
				}
				walks++
				statement = triggerRaise.ReplaceAllString(triggerRowRef.ReplaceAllString(statement, "?"), "NULL")
				args := make([]any, strings.Count(statement, "?"))
				for i := range args {
					args[i] = "t"
				}
				assertWalkProbesTheQueuedKey(t, s, name, ancestors, statement, args...)
			}
			if walks == 0 {
				t.Errorf("%s: no body statement walks; the check proves nothing", name)
			}
		}
	})

	// The run tree's statements are recorded from the store calls that run
	// them: the downward walk under every whole-tree read, and the upward
	// walk to the root.
	t.Run("work item tree", func(t *testing.T) {
		s := newTestStore(t)
		seedRunMapTree(t, s)
		rec := recordStatements(t, s)
		stmts := rec.capture(func() {
			if _, err := s.WorkItemTreeRoot("wave", 8); err != nil {
				t.Errorf("tree root: %v", err)
			}
			if _, err := s.ReadWorkItemTree(context.Background(), "root", 8, 100, func(WorkItemTreeRun) error { return nil }); err != nil {
				t.Errorf("read tree: %v", err)
			}
		})
		down := walkPlan{step: []string{"SEARCH child USING INDEX idx_work_items_parent (parent_item_id=?)"}}
		up := walkPlan{step: []string{"SEARCH parent USING INDEX sqlite_autoindex_work_items_1 (id=?)"}}
		downs, ups := 0, 0
		for _, stmt := range stmts {
			switch {
			case strings.Contains(stmt.query, "WITH RECURSIVE tree("):
				downs++
				assertWalkProbesTheQueuedKey(t, s, "run tree read", down, stmt.query, stmt.args...)
			case strings.Contains(stmt.query, "WITH RECURSIVE ancestors("):
				ups++
				assertWalkProbesTheQueuedKey(t, s, "run tree root", up, stmt.query, stmt.args...)
			}
		}
		// The run scan, auto resumes, phases, units, usage and usage detail.
		if downs != 6 || ups != 1 {
			t.Errorf("recorded %d downward and %d upward walks, want 6 and 1", downs, ups)
		}
	})
}
