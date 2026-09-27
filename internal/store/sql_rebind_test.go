package store

import (
	"context"
	"database/sql/driver"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// A statement whose plan SQLite chose by reading a bound value recompiles
// on every run: the planner compares a bound `kind = ?` with the
// `kind = 'user_text'` of a partial index's WHERE, reads a bound LIMIT and
// the prefix of a bound LIKE pattern, and marks the parameter so that
// binding it expires the statement. The statement cache (stmt_cache.go)
// cannot help such a statement.
//
// The package's tests check every statement the cache runs, after each
// run (cachedStmtRan). modernc clears a cached statement's bindings after
// a run, and SQLite expires a statement when it clears or binds a
// parameter its plan read, so a statement expired after its run compiles
// again on its next. A schema change expires statements before a run,
// which recompiles them first, so it never shows here. TestMain fails the
// package on any statement of the package's source that recompiles, named
// with the call that ran it; a statement a test writes is not checked, nor
// is one prepared with Prepare, which bypasses the cache.
func init() {
	cachedStmtRan = checkCachedStmtRun
}

// recompilingStatement is a statement of the package's source whose plan
// reads a bound value, with the call site of its first such run.
type recompilingStatement struct {
	query, site string
}

var recompiling struct {
	mu    sync.Mutex
	found []recompilingStatement
	seen  map[string]bool
}

// cachedStmtRuns counts the statement runs checkCachedStmtRun checked.
var cachedStmtRuns atomic.Int64

// storePackagePrefix prefixes the name of every function of this package.
var storePackagePrefix = strings.TrimSuffix(runtime.FuncForPC(reflect.ValueOf(textParam).Pointer()).Name(), "textParam")

func checkCachedStmtRun(stmt driver.Stmt, query string) {
	cachedStmtRuns.Add(1)
	recompiles, err := cachedStmtRecompiles(stmt)
	if err != nil {
		recordRecompiling(query, err.Error())
		return
	}
	if !recompiles {
		return
	}
	if site, own := statementCallSite(); own {
		recordRecompiling(query, site)
	}
}

// cachedStmtRecompiles reports whether stmt, a cached statement that has
// just run, compiles again on its next run.
func cachedStmtRecompiles(stmt driver.Stmt) (bool, error) {
	tls, pstmt, err := sqliteStmtHandles(stmt)
	if err != nil || pstmt == 0 {
		// A statement of several statements keeps no handle; the driver
		// compiles it per run.
		return false, err
	}
	return sqlite3.Xsqlite3_expired(tls, pstmt) != 0, nil
}

func recordRecompiling(query, site string) {
	recompiling.mu.Lock()
	defer recompiling.mu.Unlock()
	if recompiling.seen == nil {
		recompiling.seen = map[string]bool{}
	}
	if recompiling.seen[query] {
		return
	}
	recompiling.seen[query] = true
	recompiling.found = append(recompiling.found, recompilingStatement{query: query, site: site})
}

// recompilingStatements returns the statements recorded so far.
func recompilingStatements() []recompilingStatement {
	recompiling.mu.Lock()
	defer recompiling.mu.Unlock()
	return slices.Clone(recompiling.found)
}

// recompilingReport describes found, or is empty.
func recompilingReport(found []recompilingStatement) string {
	if len(found) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("statements recompile on every run because a bound value decides their plan; " +
		"write a LIMIT or a code constant as a literal (sqlTextLiteral), and bind a LIKE pattern " +
		"or a value compared with a column a partial index pins through textParam:\n")
	for _, stmt := range found {
		fmt.Fprintf(&b, "%s:\n%s\n\n", stmt.site, strings.TrimSpace(stmt.query))
	}
	return b.String()
}

// statementCallSite is the innermost function of this package on the
// stack of a run, as file:line, and whether it is package source rather
// than a test.
func statementCallSite() (string, bool) {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(3, pcs)])
	for {
		frame, more := frames.Next()
		if strings.HasPrefix(frame.Function, storePackagePrefix) && !strings.HasSuffix(frame.File, "/stmt_cache.go") {
			return fmt.Sprintf("%s:%d", frame.File, frame.Line), !strings.HasSuffix(frame.File, "_test.go")
		}
		if !more {
			return "no function of the package on the stack", true
		}
	}
}

// sqliteStmtHandles reads the thread state and the SQLite statement handle
// of a modernc statement, or of the test wrapper around one, which modernc
// keeps unexported.
func sqliteStmtHandles(stmt driver.Stmt) (*libc.TLS, uintptr, error) {
	if observed, ok := stmt.(*observedStmt); ok {
		stmt = observed.Stmt
	}
	v := reflect.ValueOf(stmt)
	if v.Kind() != reflect.Pointer || v.Elem().Kind() != reflect.Struct {
		return nil, 0, fmt.Errorf("statement %T is not a modernc statement", stmt)
	}
	pstmt, err := unexportedField(v.Elem(), "pstmt")
	if err != nil {
		return nil, 0, err
	}
	conn, err := unexportedField(v.Elem(), "c")
	if err != nil {
		return nil, 0, err
	}
	if conn.Kind() != reflect.Pointer || conn.IsNil() {
		return nil, 0, fmt.Errorf("statement %T has no connection", stmt)
	}
	tls, err := unexportedField(conn.Elem(), "tls")
	if err != nil {
		return nil, 0, err
	}
	handle, handleOK := pstmt.Interface().(uintptr)
	state, stateOK := tls.Interface().(*libc.TLS)
	if !handleOK || !stateOK {
		return nil, 0, fmt.Errorf("statement %T holds %s and %s, not a handle and a thread state", stmt, pstmt.Type(), tls.Type())
	}
	return state, handle, nil
}

func unexportedField(v reflect.Value, name string) (reflect.Value, error) {
	f := v.FieldByName(name)
	if !f.IsValid() {
		return reflect.Value{}, fmt.Errorf("%s has no field %s", v.Type(), name)
	}
	return reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem(), nil
}

// The check finds a statement whose plan reads a bound value, a bound
// LIMIT, a LIKE pattern or a bare parameter compared with a column a
// partial index pins, after its first run and after a later one, and
// passes the same statements written with a literal LIMIT and textParam.
func TestCachedStatementCheckFindsBoundPlans(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, tc := range []struct {
		query      string
		args       []any
		recompiles bool
	}{
		{`SELECT id FROM items WHERE thread_id = ? AND kind = ? AND parent_id = ''`, []any{"t", "user_text"}, true},
		{`SELECT id FROM items WHERE thread_id = ? AND kind = ` + boundText + ` AND parent_id = ''`, []any{"t", "user_text"}, false},
		{`SELECT id FROM items WHERE thread_id = ? LIMIT ?`, []any{"t", 5}, true},
		{`SELECT id FROM items WHERE thread_id = ? LIMIT 5`, []any{"t"}, false},
		{`SELECT id FROM items WHERE thread_id = ? AND LOWER(summary) LIKE ?`, []any{"t", "%a%"}, true},
		{`SELECT id FROM items WHERE thread_id = ? AND LOWER(summary) LIKE ` + boundText, []any{"t", "%a%"}, false},
	} {
		for run := range 2 {
			rows, err := conn.QueryContext(ctx, tc.query, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
			var entry *cachedStmt
			cache := cacheOf(t, conn)
			cache.mu.Lock()
			if el, ok := cache.entries[tc.query]; ok {
				entry = el.Value.(*cachedStmt)
			}
			cache.mu.Unlock()
			if entry == nil {
				t.Fatalf("%s is not cached", tc.query)
			}
			got, err := cachedStmtRecompiles(entry.stmt)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.recompiles {
				t.Errorf("run %d of %s: recompiles = %v, want %v", run+1, tc.query, got, tc.recompiles)
			}
		}
	}
}

// The keyed lookups, the writes a running turn makes, on a thread and on a
// pointer fork of it, the searches and the workflow run lookup compile
// once.
func TestStatementsCompileOnce(t *testing.T) {
	s := newTestStore(t)
	seedKeyedLookupThread(t, s)
	if err := s.CreatePointerFork(makeThread(keyedForkID, "claude"), keyedThreadID, ForkCut{}, testInterruptedSummary, 1); err != nil {
		t.Fatal(err)
	}
	before, runs := len(recompilingStatements()), cachedStmtRuns.Load()
	for _, lookup := range append(keyedLookups(keyedThreadID), forkLookups(keyedForkID)...) {
		lookup.run(t, s)
	}
	runTurnWrites(t, s, keyedThreadID)
	if _, err := s.resolveTimelineScope(s.reader(), keyedThreadID, TimelineSelection{ScopeRootID: "launch-1", DigestItemID: "done-1"}); err != nil {
		t.Errorf("digest scope: %v", err)
	}
	if _, err := s.SearchThreads("answer", ThreadSearchFilter{SpawnedBy: keyedThreadID}); err != nil {
		t.Errorf("search spawned threads: %v", err)
	}
	if _, err := s.SearchThreadMessages("answer", 10); err != nil {
		t.Errorf("search messages: %v", err)
	}
	if _, err := s.SearchThreadItems(keyedThreadID, "answer", 10); err != nil {
		t.Errorf("search thread items: %v", err)
	}
	if _, _, err := s.GetWorkItemBySourceRef("agent", "phase-ref"); err != nil {
		t.Errorf("work item by source ref: %v", err)
	}
	// The store has no owner; the read's statement runs all the same.
	_, _ = s.ownerUser()
	if checked := cachedStmtRuns.Load() - runs; checked < 20 {
		t.Fatalf("checked %d statement runs; the check proved nothing", checked)
	}
	if report := recompilingReport(recompilingStatements()[before:]); report != "" {
		t.Error(report)
	}
}

// runTurnWrites makes the writes of a turn with a background agent at
// turn 5 of threadID: the launch, the agent's rows as they start, stream
// and settle, a top-level streaming answer, and the agent's completion.
func runTurnWrites(t *testing.T, s *Store, threadID string) {
	t.Helper()
	const at = int64(1_700_000_005_000)
	if err := s.InsertTurn(Turn{TurnID: threadID + ":5", ThreadID: threadID, TurnIndex: 5, StartedAt: at}); err != nil {
		t.Fatal(err)
	}
	row := func(id, kind, status, parent string) Item {
		return Item{ID: id, ThreadID: threadID, TurnIndex: 5, Kind: kind, Role: "assistant", Status: status, ParentID: parent, Meta: "{}", CreatedAt: at, UpdatedAt: at}
	}
	launch := row("w-launch", "tool_call", "running", "")
	launch.ToolName, launch.IsBackground, launch.Meta = "Agent", true, `{"task_id":"w-task"}`
	for _, item := range []Item{
		launch,
		row("w-thought", "thinking", "completed", launch.ID),
		row("w-call", "tool_call", "running", launch.ID),
		row("w-answer", "assistant_text", "streaming", ""),
	} {
		if item.ID == "w-thought" {
			item.Meta = `{"provider_item_id":"w-prov"}`
		}
		if _, err := upsertCarded(s, item, nil); err != nil {
			t.Fatalf("upsert %s: %v", item.ID, err)
		}
	}
	if _, found, err := s.FindStreamItemByProviderItemID(threadID, 5, "thinking", launch.ID, "w-prov"); err != nil || !found {
		t.Errorf("find settled thought: %v %v", found, err)
	}
	done := row("w-call", "tool_call", "completed", launch.ID)
	done.ToolName, done.Summary = "Bash", "ran"
	if _, err := upsertCarded(s, done, nil); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := s.AppendItemSummary(threadID, "w-answer", "more ", at+1); err != nil {
			t.Fatal(err)
		}
	}
	summary := "settled"
	if _, err := s.UpdateItemFields(threadID, "w-answer", ItemPartialUpdate{Summary: &summary}); err != nil {
		t.Fatal(err)
	}
	completion := row("w-done", "tool_completion", "completed", "")
	completion.ToolName, completion.CompletionOf, completion.IsBackground = "Agent", launch.ID, true
	if _, err := s.AppendCompletionItem(launch, completion, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NextTurnIndex(threadID); err != nil {
		t.Fatal(err)
	}
}
