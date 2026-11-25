package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	sqlite3 "modernc.org/sqlite/lib"
)

// referencesCTE matches a statement that names a common table expression.
var referencesCTE = regexp.MustCompile(`\bWITH\b`)

// A fork's reads and writes open at every lineage depth up to the cap.
// SQLite 3.54 refuses a CTE reference that it expands once the statement
// holds 1000 cursors (select.c withExpand), so a statement that references
// a CTE keeps one text at every depth; only an ordered read's pair of arms
// per level grows, and it holds no CTE (renderTimelineArms). The fixture is
// imported history with an agent whose children span two turns, under a
// chain of forks whose first cut excludes a child the source's card counts,
// so the reads walk the agent instead of reading its stamp.
func TestForkReadsKeepTheirShapeAtEveryDepth(t *testing.T) {
	s := newTestStore(t)
	seedKeyedLookupThread(t, s)
	if err := s.InsertTurn(Turn{TurnID: keyedThreadID + ":5", ThreadID: keyedThreadID, TurnIndex: 5, StartedAt: 1_700_000_005_000}); err != nil {
		t.Fatal(err)
	}
	for _, row := range []stampFixtureRow{
		{id: "A", kind: "tool_call", tool: "Agent", summary: "Agent: A", turn: 4, index: 2},
		{id: "A-c1", kind: "tool_call", tool: "Bash", summary: "Bash: one", parent: "A", turn: 4, index: 3},
		{id: "A-c2", kind: "assistant_text", summary: "two", parent: "A", turn: 4, index: 4},
		{id: "A-c3", kind: "tool_call", tool: "Read", summary: "Read: three", parent: "A", turn: 5},
	} {
		if err := insertCarded(s, row.item(keyedThreadID)); err != nil {
			t.Fatalf("insert %s: %v", row.id, err)
		}
	}
	prev := keyedThreadID
	for level := 1; level <= forkLineageMaxDepth; level++ {
		cut := ForkCut{}
		if level == 1 {
			cut = throughTurn(4)
		}
		id := "depth-" + strconv.Itoa(level)
		mustPointerFork(t, s, prev, id, cut)
		prev = id
	}
	shallow, deep := "depth-1", "depth-"+strconv.Itoa(forkLineageMaxDepth)

	var mu sync.Mutex
	ran := map[string]map[string]bool{shallow: {}, deep: {}}
	current := ""
	prevHook := cachedStmtRan
	cachedStmtRan = func(stmt driver.Stmt, query string) {
		prevHook(stmt, query)
		mu.Lock()
		defer mu.Unlock()
		if current != "" {
			ran[current][query] = true
		}
	}
	t.Cleanup(func() { cachedStmtRan = prevHook })
	at := func(threadID string, run func()) {
		mu.Lock()
		current = threadID
		mu.Unlock()
		run()
		mu.Lock()
		current = ""
		mu.Unlock()
	}

	ctx := context.Background()
	reads := func(threadID string) {
		for _, read := range []struct {
			name string
			run  func() error
		}{
			{"window", func() error {
				_, err := s.ListThreadSliceAround(ctx, threadID, "", 100, testRunWindowRows, TimelineSelection{}, AgentMessagesAreMembers)
				return err
			}},
			{"sync", func() error {
				_, err := s.SyncThreadWindow(ctx, threadID, "", 100, testRunWindowRows, HistoryStamp{}, nil, TimelineSelection{}, AgentMessagesAreMembers)
				return err
			}},
			{"agent scope", func() error {
				_, err := s.ListThreadSliceAround(ctx, threadID, "", 100, testRunWindowRows, TimelineSelection{ScopeRootID: "A"}, AgentMessagesAreMembers)
				return err
			}},
			{"search", func() error {
				_, err := s.SearchThreadItems(ctx, threadID, "answer", TimelineSelection{}, 50, nil)
				return err
			}},
			{"ListItems", func() error { _, err := s.ListItems(threadID); return err }},
			{"SnapshotSubagentExecutionMeta", func() error {
				_, _, err := s.SnapshotSubagentExecutionMeta(threadID, "A", "", "{}", 4, 2, 10)
				return err
			}},
		} {
			if err := read.run(); err != nil {
				t.Errorf("%s at %s: %v", read.name, threadID, err)
			}
		}
		t.Run("cards at "+threadID, func(t *testing.T) { subagentCardsForTest(t, s, s.reader(), threadID) })
	}
	at(shallow, func() { reads(shallow) })
	at(deep, func() { reads(deep) })
	// The writes come last, the deepest fork's first: depth-1's writes
	// change rows every deeper fork reads.
	for _, threadID := range []string{deep, shallow} {
		at(threadID, func() {
			for _, lookup := range forkLookups(threadID) {
				t.Run(threadID+"/"+lookup.name, func(t *testing.T) { lookup.run(t, s) })
			}
		})
	}

	mu.Lock()
	defer mu.Unlock()
	var grown []string
	shared := 0
	for query := range ran[deep] {
		if !referencesCTE.MatchString(query) {
			continue
		}
		if ran[shallow][query] {
			shared++
			continue
		}
		grown = append(grown, query)
	}
	sort.Strings(grown)
	for _, query := range grown {
		t.Errorf("a statement that references a CTE ran at depth %d with a text that never ran at depth 1 (%s cursors):\n%s",
			forkLineageMaxDepth, cursorCountForTest(t, s, query), strings.TrimSpace(query))
	}
	if shared < 10 {
		t.Errorf("%d statements that reference a CTE ran at both depths; the check proved nothing", shared)
	}
}

// cursorCountForTest is one more than the highest cursor query's program
// opens: the least the statement holds.
func cursorCountForTest(t *testing.T, s *Store, query string) string {
	t.Helper()
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	}()
	most := -1
	err = conn.Raw(func(dc any) error {
		st, err := dc.(*stmtCacheConn).sqliteConn.PrepareContext(context.Background(), "EXPLAIN "+query)
		if err != nil {
			return err
		}
		tls, pstmt, err := sqliteStmtHandles(st)
		if err != nil {
			return errors.Join(err, st.Close())
		}
		args := make([]driver.NamedValue, sqlite3.Xsqlite3_bind_parameter_count(tls, pstmt))
		for i := range args {
			args[i].Ordinal = i + 1
		}
		rows, err := st.(driver.StmtQueryContext).QueryContext(context.Background(), args)
		if err != nil {
			return errors.Join(err, st.Close())
		}
		vals := make([]driver.Value, len(rows.Columns()))
		for rows.Next(vals) == nil {
			op, _ := vals[1].(string)
			opens := strings.HasPrefix(op, "Open") || op == "SorterOpen" || op == "ReopenIdx"
			if p1, ok := vals[2].(int64); ok && opens && int(p1) > most {
				most = int(p1)
			}
		}
		return errors.Join(rows.Close(), st.Close())
	})
	if err != nil {
		return "EXPLAIN failed: " + err.Error()
	}
	return fmt.Sprint(most + 1)
}
