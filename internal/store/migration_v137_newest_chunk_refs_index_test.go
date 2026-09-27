package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	sqlite3 "modernc.org/sqlite/lib"
)

// v137 indexes a thread's chunk references by their newest turn,
// descending.
func TestNewestChunkRefsIndexMigration(t *testing.T) {
	db := migrateThrough(t, newestChunkRefsIndexMigrationVersion-1)
	seedMigrationThread(t, db, "t")
	for order, turns := range [][2]int{{4, 5}, {0, 1}, {2, 3}} {
		chunk := fmt.Sprintf("c%d-%d", turns[0], turns[1])
		mustExec(t, db, `INSERT INTO import_history_chunks(id,item_count,min_turn_index,max_turn_index) VALUES(?,2,?,?)`, chunk, turns[0], turns[1])
		for i, turn := range turns {
			mustExec(t, db, `INSERT INTO import_history_items(chunk_id,id,turn_index,item_index,kind,role,created_at,updated_at)
				VALUES(?,?,?,0,'assistant_text','assistant',1,1)`, chunk, fmt.Sprintf("%s-%d", chunk, i), turn)
		}
		mustExec(t, db, `INSERT INTO thread_import_chunks(thread_id,chunk_order,chunk_id) VALUES('t',?,?)`, order, chunk)
	}
	if err := applyMigration(db, migrationByVersion(t, newestChunkRefsIndexMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", newestChunkRefsIndexMigrationVersion, err)
	}
	if got := readIndexSQL(t, db, "idx_thread_import_chunks_newest"); normalizeSQLText(got) != normalizeSQLText(newestChunkRefsIndexV137SQL) {
		t.Errorf("idx_thread_import_chunks_newest:\n got  %s\n want %s", got, newestChunkRefsIndexV137SQL)
	}
	rows, err := db.Query(`SELECT chunk_id FROM thread_import_chunks INDEXED BY idx_thread_import_chunks_newest WHERE thread_id = 't'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var chunks []string
	for rows.Next() {
		var chunk string
		if err := rows.Scan(&chunk); err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, chunk)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(chunks, ","); got != "c4-5,c2-3,c0-1" {
		t.Errorf("newest index walks %s, want c4-5,c2-3,c0-1", got)
	}
}

// seedImportedChunks imports chunks chunks of eight 32-row turns into a
// new thread, which importHistoryTargetRows puts one to a chunk.
func seedImportedChunks(t *testing.T, s *Store, threadID string, chunks int) {
	t.Helper()
	newImportTargetThread(t, s, threadID)
	var batch ImportBatch
	for turn := range chunks * 8 {
		batch.Turns = append(batch.Turns, Turn{TurnID: fmt.Sprintf("%s:%d", threadID, turn), ThreadID: threadID, TurnIndex: turn, StartedAt: int64(turn)})
		for item := range 32 {
			kind, role := "tool_call", "assistant"
			if item == 0 {
				kind, role = "user_text", "user"
			}
			batch.Rows = append(batch.Rows, ImportRow{Item: Item{
				ID: fmt.Sprintf("%s-%d-%d", threadID, turn, item), TurnIndex: turn, ItemIndex: item, Kind: kind, Role: role,
				Status: "completed", Summary: "x", Meta: "{}", CreatedAt: 1, UpdatedAt: 1,
			}})
		}
	}
	if err := s.ApplyImportBatch(threadID, batch); err != nil {
		t.Fatal(err)
	}
	var got int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM thread_import_chunks WHERE thread_id = ?`, threadID).Scan(&got); err != nil || got != chunks {
		t.Fatalf("%s holds %d chunks, %v; want %d", threadID, got, err, chunks)
	}
}

// statementVMSteps reads every row of query twice on one connection and
// returns the virtual machine steps of the second read, which runs the
// statement the first compiled.
func statementVMSteps(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	read := func() {
		t.Helper()
		rows, err := conn.QueryContext(ctx, query, args...)
		if err != nil {
			t.Fatalf("query: %v\n%s", err, query)
		}
		for rows.Next() {
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatalf("read rows: %v", err)
		}
	}
	read()
	cache := cacheOf(t, conn)
	cache.mu.Lock()
	el, ok := cache.entries[query]
	cache.mu.Unlock()
	if !ok {
		t.Fatalf("statement is not cached:\n%s", query)
	}
	tls, pstmt, err := sqliteStmtHandles(el.Value.(*cachedStmt).stmt)
	if err != nil || pstmt == 0 {
		t.Fatalf("statement handle: %v", err)
	}
	sqlite3.Xsqlite3_stmt_status(tls, pstmt, sqlite3.SQLITE_STMTSTATUS_VM_STEP, 1)
	read()
	return int(sqlite3.Xsqlite3_stmt_status(tls, pstmt, sqlite3.SQLITE_STMTSTATUS_VM_STEP, 0))
}

// A page of imported history reads the chunks its rows come from and
// probes each other chunk of the thread once, whichever way it reads:
// the arm visits the chunks in the page's order (chunkRefsIndex), so its
// sorter, once it holds the page, leaves a chunk after its first row. A
// probe costs under 128 steps; reading a chunk's 256 rows costs over a
// thousand. A pointer fork reads its source's chunks the same way.
func TestImportedPagesReadTheirOwnChunks(t *testing.T) {
	const few, many = 3, 30
	s := newTestStore(t)
	seedImportedChunks(t, s, "few", few)
	seedImportedChunks(t, s, "many", many)
	for _, source := range []string{"few", "many"} {
		if err := s.CreatePointerFork(makeThread(source+"-fork", "claude"), source, ForkCut{}, testInterruptedSummary, 1); err != nil {
			t.Fatal(err)
		}
	}
	steps := func(threadID string, cursor TimelineCursor, newer bool) int {
		t.Helper()
		scope, err := s.resolveTimelineScope(s.db, threadID, TimelineSelection{})
		if err != nil {
			t.Fatal(err)
		}
		sel := beyondCursor(scope, cursor, newer)
		sel.OrderBy, sel.Limit = "turn_index DESC, item_index DESC", 50
		if newer {
			sel.OrderBy = "turn_index ASC, item_index ASC"
		}
		query, args := mustTimelineIDSelection(t, s, threadID, sel)
		index := "idx_thread_import_chunks_newest"
		if newer {
			index = "idx_thread_import_chunks_turns"
		}
		if plan := planText(explainPlan(t, s, query, args...)); !strings.Contains(plan, "SEARCH refs USING COVERING INDEX "+index) {
			t.Errorf("newer=%v page of %s does not walk %s:\n%s", newer, threadID, index, plan)
		}
		return statementVMSteps(t, s, query, args...)
	}
	middle := func(chunks int) TimelineCursor { return TimelineCursor{TurnIndex: chunks * 4, ItemIndex: 16} }
	for _, tc := range []struct {
		name   string
		cursor func(chunks int) TimelineCursor
		newer  bool
	}{
		{"tail", func(int) TimelineCursor { return timelineTailBound() }, false},
		{"older from the middle", middle, false},
		{"newer from the middle", middle, true},
		{"newer from the start", func(int) TimelineCursor { return TimelineCursor{TurnIndex: 0, ItemIndex: 0} }, true},
	} {
		for _, suffix := range []string{"", "-fork"} {
			small, big := steps("few"+suffix, tc.cursor(few), tc.newer), steps("many"+suffix, tc.cursor(many), tc.newer)
			t.Logf("%s%s: %d steps over %d chunks, %d over %d", tc.name, suffix, small, few, big, many)
			if big-small > (many-few)*128 {
				t.Errorf("%s%s: %d steps over %d chunks and %d over %d: the page reads other chunks' rows", tc.name, suffix, small, few, big, many)
			}
		}
	}
}
