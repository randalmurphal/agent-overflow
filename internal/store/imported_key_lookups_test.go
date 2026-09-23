package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"
)

// A lookup pinned by id, by another key or by turn must cost the same
// however many import chunks its thread references. These tests record the
// SQL that production calls run and read its plans: a thread's chunk
// references may be searched by chunk id or by a turn bound, never
// enumerated by thread id alone.

type recordedStatement struct {
	query string
	args  []any
}

type statementRecorder struct {
	mu    sync.Mutex
	on    bool
	stmts []recordedStatement
}

func (r *statementRecorder) record(query string, args []driver.NamedValue) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.on {
		return
	}
	values := make([]any, len(args))
	for i, arg := range args {
		values[i] = arg.Value
	}
	r.stmts = append(r.stmts, recordedStatement{query: query, args: values})
}

// capture returns the statements fn ran.
func (r *statementRecorder) capture(fn func()) []recordedStatement {
	r.mu.Lock()
	r.on, r.stmts = true, nil
	r.mu.Unlock()
	fn()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.on = false
	return r.stmts
}

type recordingConnector struct {
	driver.Connector
	rec *statementRecorder
}

func (c recordingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return recordingConn{Conn: conn, rec: c.rec}, nil
}

// recordingConn records each statement and forwards it to the modernc
// connection, which implements every interface forwarded here.
type recordingConn struct {
	driver.Conn
	rec *statementRecorder
}

func (c recordingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	c.rec.record(query, nil)
	return c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
}

func (c recordingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.rec.record(query, args)
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c recordingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.rec.record(query, args)
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c recordingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c recordingConn) ResetSession(ctx context.Context) error {
	return c.Conn.(driver.SessionResetter).ResetSession(ctx)
}

func (c recordingConn) IsValid() bool {
	return c.Conn.(driver.Validator).IsValid()
}

// recordStatements reopens both of s's pools through a recorder.
func recordStatements(t *testing.T, s *Store) *statementRecorder {
	t.Helper()
	rec := &statementRecorder{}
	open := func(pragmas []connPragma, conns int) *sql.DB {
		base, err := sqlite.NewConnector(poolDSN(s.path, pragmas))
		if err != nil {
			t.Fatal(err)
		}
		db := sql.OpenDB(recordingConnector{Connector: base, rec: rec})
		db.SetMaxOpenConns(conns)
		return db
	}
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	s.db = open(writerConnPragmas, 1)
	if s.read != nil {
		if err := s.read.Close(); err != nil {
			t.Fatal(err)
		}
		s.read = open(readerConnPragmas, readPoolConns)
	}
	return rec
}

var (
	chunkRefAliasPattern = regexp.MustCompile(`(?i)\bthread_import_chunks\s+(?:AS\s+)?([a-z_][a-z0-9_]*)`)
	planAccessPattern    = regexp.MustCompile(`^(SCAN|SEARCH) (\S+)`)
	planKeyPattern       = regexp.MustCompile(`\(([^()]*)\)$`)
)

var sqlWordsAfterTable = map[string]bool{
	"WHERE": true, "ON": true, "JOIN": true, "CROSS": true, "LEFT": true, "INNER": true,
	"NATURAL": true, "USING": true, "GROUP": true, "ORDER": true, "LIMIT": true, "SET": true,
	"UNION": true, "EXCEPT": true, "INTERSECT": true, "AND": true, "OR": true, "NOT": true,
	"INDEXED": true, "WINDOW": true, "RETURNING": true, "VALUES": true, "SELECT": true,
	"HAVING": true, "DEFAULT": true,
}

// chunkRefNames are the names a plan can give thread_import_chunks for a
// statement over sources: the table itself and every alias they assign it.
func chunkRefNames(sources ...string) map[string]bool {
	names := map[string]bool{"thread_import_chunks": true}
	for _, source := range sources {
		for _, m := range chunkRefAliasPattern.FindAllStringSubmatch(source, -1) {
			if !sqlWordsAfterTable[strings.ToUpper(m[1])] {
				names[m[1]] = true
			}
		}
	}
	return names
}

// chunkRefNodes returns the plan nodes that read chunk references, and
// those among them that read a thread's references without a chunk id or
// turn bound: a SCAN, or a SEARCH keyed by thread_id alone.
func chunkRefNodes(plan []planRow, names map[string]bool) (reads, enumerations []string) {
	for _, r := range plan {
		m := planAccessPattern.FindStringSubmatch(r.detail)
		if m == nil || (!names[m[2]] && !strings.Contains(r.detail, "thread_import_chunks")) {
			continue
		}
		reads = append(reads, r.detail)
		key := planKeyPattern.FindStringSubmatch(r.detail)
		if m[1] == "SCAN" || key == nil || key[1] == "thread_id=?" {
			enumerations = append(enumerations, r.detail)
		}
	}
	return reads, enumerations
}

// chunkRefViewSQL is the definition of every view that reads chunk
// references, so a statement over one of them is checked under the
// aliases the view assigns.
func chunkRefViewSQL(t *testing.T, s *Store) string {
	t.Helper()
	rows, err := s.db.Query(`SELECT sql FROM sqlite_master WHERE type = 'view' AND sql LIKE '%thread_import_chunks%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var views strings.Builder
	for rows.Next() {
		var view string
		if err := rows.Scan(&view); err != nil {
			t.Fatal(err)
		}
		views.WriteString(view + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return views.String()
}

const keyedThreadID = "keyed-lookups"

// seedKeyedLookupThread imports turns 0-3 as one chunk each, every turn
// carrying each key a lookup pins, then adds local turn 4 and overrides
// one imported row.
func seedKeyedLookupThread(t *testing.T, s *Store) {
	t.Helper()
	newImportTargetThread(t, s, keyedThreadID)
	const base = int64(1_700_000_000_000)
	const diff = "--- a/f\n+++ b/f\n@@ -1 +1 @@\n-a\n+b\n"
	for turn := range 4 {
		at := base + int64(turn)*1000
		id := func(prefix string) string { return fmt.Sprintf("%s-%d", prefix, turn) }
		row := func(prefix string, index int, kind, role, summary string) Item {
			return Item{
				ID: id(prefix), TurnIndex: turn, ItemIndex: index, Kind: kind, Role: role,
				Status: "completed", Summary: summary, Meta: "{}",
				CreatedAt: at + int64(index), UpdatedAt: at + int64(index),
			}
		}
		user := row("user", 0, "user_text", "user", "ask")
		user.Meta = fmt.Sprintf(`{"sendId":"send-%d"}`, turn)
		launch := row("launch", 1, "tool_call", "assistant", "Task")
		launch.ToolName, launch.Meta = "Task", fmt.Sprintf(`{"task_id":"task-%d"}`, turn)
		child := row("child", 2, "tool_call", "assistant", "Bash")
		child.ToolName, child.ParentID = "Bash", launch.ID
		note := row("note", 3, "notification", "system", "note")
		note.Meta = fmt.Sprintf(`{"task_id":"task-%d"}`, turn)
		answer := row("answer", 4, "assistant_text", "assistant", "answer")
		answer.Meta, answer.PayloadID = fmt.Sprintf(`{"provider_item_id":"prov-%d"}`, turn), id("answer-payload")
		edit := row("edit", 5, "tool_call", "assistant", "Edit")
		edit.ToolName, edit.PayloadID = "Edit", id("diff-payload")
		done := row("done", 6, "tool_completion", "assistant", "done")
		done.CompletionOf = launch.ID
		batch := ImportBatch{
			Turns: []Turn{{TurnID: fmt.Sprintf("%s:%d", keyedThreadID, turn), ThreadID: keyedThreadID, TurnIndex: turn, StartedAt: at}},
			Rows: []ImportRow{
				{Item: user}, {Item: launch}, {Item: child}, {Item: note},
				{Item: answer, Payload: &Payload{ID: answer.PayloadID, Kind: "text", Meta: "{}", Data: []byte("answer text"), CreatedAt: at}},
				{Item: edit, Payload: &Payload{ID: edit.PayloadID, Kind: "tool_result", Meta: "{}", Data: []byte(diff), CreatedAt: at}},
				{Item: done},
			},
		}
		if err := s.ApplyImportBatch(keyedThreadID, batch); err != nil {
			t.Fatalf("import turn %d: %v", turn, err)
		}
	}
	if err := s.InsertTurn(Turn{TurnID: keyedThreadID + ":4", ThreadID: keyedThreadID, TurnIndex: 4, StartedAt: base + 4000}); err != nil {
		t.Fatal(err)
	}
	for index, id := range []string{"local-user-4", "local-answer-4"} {
		kind, role := "user_text", "user"
		if index == 1 {
			kind, role = "assistant_text", "assistant"
		}
		if err := s.InsertItem(Item{
			ID: id, ThreadID: keyedThreadID, TurnIndex: 4, ItemIndex: index, Kind: kind, Role: role,
			Status: "completed", Summary: id, CreatedAt: base + 4000 + int64(index),
		}); err != nil {
			t.Fatal(err)
		}
	}
	edited := "edited ask"
	if _, err := s.UpdateItemFields(keyedThreadID, "user-1", ItemPartialUpdate{Summary: &edited}); err != nil {
		t.Fatal(err)
	}
	var chunks int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM thread_import_chunks WHERE thread_id = ?`, keyedThreadID).Scan(&chunks); err != nil || chunks != 4 {
		t.Fatalf("fixture chunks = %d, %v; want one per imported turn", chunks, err)
	}
}

type keyedLookup struct {
	name string
	// turn marks a lookup pinned to a turn: its plan must range over
	// idx_thread_import_chunks_turns.
	turn bool
	run  func(t *testing.T, s *Store)
}

func must[T any](t *testing.T, what string) func(T, error) {
	return func(_ T, err error) {
		t.Helper()
		if err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
}

func mustFind[T any](t *testing.T, what string) func(T, bool, error) {
	return func(_ T, found bool, err error) {
		t.Helper()
		if err != nil || !found {
			t.Errorf("%s: found=%v err=%v", what, found, err)
		}
	}
}

func keyedLookups() []keyedLookup {
	const th = keyedThreadID
	return []keyedLookup{
		{name: "GetThreadItem imported", run: func(t *testing.T, s *Store) {
			mustFind[Item](t, "imported")(s.GetThreadItem(th, "answer-1"))
		}},
		{name: "GetThreadItem local", run: func(t *testing.T, s *Store) {
			mustFind[Item](t, "local")(s.GetThreadItem(th, "local-answer-4"))
		}},
		{name: "GetThreadItem missing", run: func(t *testing.T, s *Store) {
			if _, found, err := s.GetThreadItem(th, "missing"); err != nil || found {
				t.Errorf("missing: found=%v err=%v", found, err)
			}
		}},
		{name: "GetThreadItemByPayloadID", run: func(t *testing.T, s *Store) {
			mustFind[Item](t, "by payload")(s.GetThreadItemByPayloadID(th, "answer-payload-1"))
		}},
		{name: "FindStreamItemByProviderItemID", turn: true, run: func(t *testing.T, s *Store) {
			mustFind[Item](t, "stream item")(s.FindStreamItemByProviderItemID(th, 1, "assistant_text", "", "prov-1"))
		}},
		{name: "ListItemsForTurn", turn: true, run: func(t *testing.T, s *Store) {
			must[[]Item](t, "list for turn")(s.ListItemsForTurn(th, 1))
		}},
		{name: "ListTurnItemsSansPayload", turn: true, run: func(t *testing.T, s *Store) {
			must[[]Item](t, "sans payload")(s.ListTurnItemsSansPayload(th, 1))
		}},
		{name: "FindTurnItem", turn: true, run: func(t *testing.T, s *Store) {
			mustFind[Item](t, "turn item")(s.FindTurnItem(th, 1, "notification"))
		}},
		{name: "FindToolCallItemByTaskID", run: func(t *testing.T, s *Store) {
			mustFind[Item](t, "task")(s.FindToolCallItemByTaskID(th, "task-1"))
		}},
		{name: "FindOriginalAgentLaunchByTaskID", run: func(t *testing.T, s *Store) {
			mustFind[Item](t, "original launch")(s.FindOriginalAgentLaunchByTaskID(th, "task-1", "note-1"))
		}},
		{name: "FindNotificationItemByTaskID", run: func(t *testing.T, s *Store) {
			mustFind[Item](t, "notification")(s.FindNotificationItemByTaskID(th, "task-1"))
		}},
		{name: "FindUserTextItemBySendID", run: func(t *testing.T, s *Store) {
			mustFind[Item](t, "send id")(s.FindUserTextItemBySendID(th, "send-2"))
		}},
		{name: "HasMatchingSystemItem", turn: true, run: func(t *testing.T, s *Store) {
			if found, err := s.HasMatchingSystemItem(th, 1, "notification", "", "note"); err != nil || !found {
				t.Errorf("system item: found=%v err=%v", found, err)
			}
		}},
		{name: "LatestToolCallByName", turn: true, run: func(t *testing.T, s *Store) {
			mustFind[Item](t, "tool call")(s.LatestToolCallByName(th, 1, []string{"task"}))
		}},
		{name: "MaxItemIndexForTurn", turn: true, run: func(t *testing.T, s *Store) {
			mustFind[int](t, "max index")(s.MaxItemIndexForTurn(th, 1))
		}},
		{name: "ItemReadNeedsDecoration", run: func(t *testing.T, s *Store) {
			launch, _, err := s.GetThreadItem(th, "launch-1")
			if err != nil {
				t.Fatal(err)
			}
			must[bool](t, "needs decoration")(s.ItemReadNeedsDecoration(launch))
		}},
		{name: "ListWireItems", run: func(t *testing.T, s *Store) {
			must[[]Item](t, "wire items")(s.ListWireItems(th, []string{"launch-1", "answer-1"}))
		}},
		{name: "ListWireItemsBehind", run: func(t *testing.T, s *Store) {
			must[[]Item](t, "behind")(s.ListWireItemsBehind(th, map[string]int64{"child-1": 0, "done-1": 0}))
		}},
		{name: "subagent reads", run: func(t *testing.T, s *Store) {
			q := s.reader()
			must[[]subagentRound](t, "resume rounds")(s.subagentResumeRounds(q, th, []string{"launch-1"}))
			must[map[string]Item](t, "launch rows")(s.subagentLaunchRowsByID(q, th, []string{"launch-1"}))
			must[map[string]subagentAnchorAggregate](t, "aggregates")(s.subagentAggregatesByRoot(q, th, []string{"launch-1"}))
			must[int](t, "completed child index")(s.SubagentCompletedChildIndex(th, "launch-1"))
		}},
		{name: "ThreadTurnPreview", run: func(t *testing.T, s *Store) {
			mustFind[TurnPreview](t, "preview")(s.ThreadTurnPreview(th, "user-2"))
		}},
		{name: "resolveTimelineScope", run: func(t *testing.T, s *Store) {
			scope, err := s.resolveTimelineScope(s.reader(), th, TimelineSelection{ScopeRootID: "launch-1"})
			if err != nil {
				t.Fatal(err)
			}
			if scope.context.Completion == nil || scope.context.Completion.ID != "done-1" {
				t.Errorf("scope completion = %+v", scope.context.Completion)
			}
			mustFind[TimelineCursor](t, "edge cursor")(windowEdgeCursorTx(s.reader(), th, "child-1", scope))
		}},
		{name: "resolveTimelineScope digest", run: func(t *testing.T, s *Store) {
			must[timelineScope](t, "digest scope")(s.resolveTimelineScope(s.reader(), th, TimelineSelection{ScopeRootID: "launch-1", DigestItemID: "done-1"}))
		}},
		{name: "has newer items", run: func(t *testing.T, s *Store) {
			must[bool](t, "newer")(hasNewerItems(s.reader(), th, TimelineCursor{TurnIndex: 4, ItemIndex: 0}, timelineScope{}))
			must[bool](t, "after cursor")(s.HasItemsAfterCursor(th, 3, 6))
		}},
		{name: "CaptureUserPlacementBoundary", turn: true, run: func(t *testing.T, s *Store) {
			must[string](t, "boundary")(s.CaptureUserPlacementBoundary(th, 1, nil))
		}},
		{name: "payload reads", run: func(t *testing.T, s *Store) {
			must[PayloadMeta](t, "meta")(s.GetPayloadMeta(th, "answer-payload-1"))
			must[[]byte](t, "data")(s.GetPayloadData(th, "answer-payload-1"))
			must[string](t, "spans")(s.GetPayloadSpans(th, "answer-payload-1"))
			if _, _, _, err := s.GetPayloadChunk(th, "answer-payload-1", 0, 4); err != nil {
				t.Errorf("chunk: %v", err)
			}
		}},
		{name: "turn edit reads", turn: true, run: func(t *testing.T, s *Store) {
			must[[]TurnEditDiffPatch](t, "patches")(s.ListTurnEditDiffPatches(th, 1))
			if _, _, err := s.GetLatestTurnEditFileSnapshot(th, 1, "f"); err != nil {
				t.Errorf("latest snapshot: %v", err)
			}
		}},
		{name: "thread projection probes", turn: true, run: func(t *testing.T, s *Store) {
			var failed, planned bool
			if err := s.reader().QueryRow(`SELECT EXISTS(`+hasUnreadNewestTurnErrorSQL()+`) FROM threads WHERE id = ?`, th).Scan(&failed); err != nil {
				t.Error(err)
			}
			if err := s.reader().QueryRow(`SELECT EXISTS(SELECT 1 FROM proposed_plans WHERE proposed_plans.thread_id = ? AND EXISTS(`+proposedPlanItemSQL()+`))`, th).Scan(&planned); err != nil {
				t.Error(err)
			}
			query, args, err := threadReadStateQuery(s.reader(), th)
			if err != nil {
				t.Fatal(err)
			}
			var completed, started, errorAt, readAt sql.NullInt64
			if err := s.reader().QueryRow(query, args...).Scan(&completed, &started, &errorAt, &readAt); err != nil {
				t.Error(err)
			}
		}},
		{name: "search summaries", run: func(t *testing.T, s *Store) {
			must[[]ThreadSearchHit](t, "search")(s.SearchThreads("answer", ThreadSearchFilter{}))
			tx, err := s.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if err := sweepThreadSearchOrphansTx(tx); err != nil {
				t.Error(err)
			}
		}},
		// Writes run last, in an order that leaves each one its target.
		{name: "AppendItem", turn: true, run: func(t *testing.T, s *Store) {
			must[int](t, "append")(s.AppendItem(Item{ID: "appended-1", ThreadID: th, TurnIndex: 1, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "late"}))
		}},
		{name: "UpsertItemAtTurnHead", turn: true, run: func(t *testing.T, s *Store) {
			must[Item](t, "head")(s.UpsertItemAtTurnHead(Item{ID: "head-1", ThreadID: th, TurnIndex: 1, Kind: "user_text", Role: "user", Status: "completed", Summary: "head"}))
		}},
		{name: "PlaceUserItemsAfterBoundary", turn: true, run: func(t *testing.T, s *Store) {
			keep := func(meta string, _ int) (string, error) { return meta, nil }
			placed := Item{ID: "placed-2", ThreadID: th, TurnIndex: 2, Kind: "user_text", Role: "user", Status: "completed", Summary: "placed", Meta: "{}", CreatedAt: 1}
			must[[]Item](t, "place")(s.PlaceUserItemsAfterBoundary(th, 2, "launch-2", []Item{placed}, keep, time.Now().UnixMilli()))
		}},
		{name: "UpdateItemFields imported", run: func(t *testing.T, s *Store) {
			summary := "localized"
			must[int64](t, "localize")(s.UpdateItemFields(th, "answer-0", ItemPartialUpdate{Summary: &summary}))
		}},
		{name: "imported payload writes", run: func(t *testing.T, s *Store) {
			if err := s.AppendPayloadData(th, "answer-payload-3", []byte(" more"), "{}", 1); err != nil {
				t.Error(err)
			}
			if err := s.PutEditFileSnapshot(th, "diff-payload-3", "f", "b\n", 1); err != nil {
				t.Error(err)
			}
		}},
		{name: "streaming update of an imported row", run: func(t *testing.T, s *Store) {
			if _, err := s.AppendItemSummary(th, "note-3", " more", 1); err == nil {
				t.Error("appended to an immutable imported row")
			}
		}},
		{name: "DeleteThreadItem imported", run: func(t *testing.T, s *Store) {
			if err := s.DeleteThreadItem(th, "note-0"); err != nil {
				t.Error(err)
			}
		}},
		{name: "DeleteConversationFromItem", turn: true, run: func(t *testing.T, s *Store) {
			if _, _, err := s.DeleteConversationFromItem(th, "note-3"); err != nil {
				t.Error(err)
			}
		}},
		{name: "DeleteConversationFromTurn", turn: true, run: func(t *testing.T, s *Store) {
			if _, _, err := s.DeleteConversationFromTurn(th, 2); err != nil {
				t.Error(err)
			}
		}},
	}
}

func TestImportedLookupsDoNotEnumerateChunks(t *testing.T) {
	s := newTestStore(t)
	seedKeyedLookupThread(t, s)
	views := chunkRefViewSQL(t, s)
	rec := recordStatements(t, s)
	for _, lookup := range keyedLookups() {
		stmts := rec.capture(func() { lookup.run(t, s) })
		if len(stmts) == 0 {
			t.Errorf("%s: recorded no statements", lookup.name)
			continue
		}
		var reads int
		turnRanged := false
		for _, stmt := range stmts {
			plan := explainPlan(t, s, stmt.query, stmt.args...)
			read, enumerations := chunkRefNodes(plan, chunkRefNames(stmt.query, views))
			reads += len(read)
			for _, node := range read {
				turnRanged = turnRanged || strings.Contains(node, "idx_thread_import_chunks_turns")
			}
			for _, node := range enumerations {
				t.Errorf("%s: %q enumerates the thread's chunks\n%s\n%s", lookup.name, node, stmt.query, planText(plan))
			}
		}
		if reads == 0 {
			t.Errorf("%s: no statement read the imported arm; the check proved nothing", lookup.name)
		}
		if lookup.turn && !turnRanged {
			t.Errorf("%s: no chunk reference read ranged over idx_thread_import_chunks_turns", lookup.name)
		}
	}
}

// The local arm of a keyed or turn read must use its key's index, not walk
// the thread: the latest turn edit snapshot is found from the turn's rows,
// and a digest's previous completion through the completion key.
func TestKeyedReadsUseTheirLocalKeyIndex(t *testing.T) {
	s := newTestStore(t)
	seedKeyedLookupThread(t, s)
	if err := s.PutEditFileSnapshot(keyedThreadID, "diff-payload-1", "f", "b\n", 1); err != nil {
		t.Fatal(err)
	}
	rec := recordStatements(t, s)
	for _, tc := range []struct {
		name      string
		run       func()
		want      string
		forbidden []string
	}{
		{
			name: "latest turn edit snapshot",
			run: func() {
				content, found, err := s.GetLatestTurnEditFileSnapshot(keyedThreadID, 1, "f")
				if err != nil || !found || content != "b\n" {
					t.Errorf("latest snapshot = %q, %v, %v", content, found, err)
				}
			},
			want:      "idx_items_thread_turn_item_unique (thread_id=? AND turn_index=?)",
			forbidden: []string{"MATERIALIZE", "AUTOMATIC", "idx_items_payload_id"},
		},
		{
			name: "digest previous completion",
			run: func() {
				if _, err := s.resolveTimelineScope(s.reader(), keyedThreadID, TimelineSelection{ScopeRootID: "launch-1", DigestItemID: "done-1"}); err != nil {
					t.Error(err)
				}
			},
			want:      "idx_items_completion_of (thread_id=? AND completion_of=?)",
			forbidden: []string{"(turn_index,item_index)<"},
		},
	} {
		found := false
		for _, stmt := range rec.capture(tc.run) {
			plan := explainPlan(t, s, stmt.query, stmt.args...)
			for _, r := range plan {
				found = found || strings.Contains(r.detail, tc.want)
				for _, bad := range tc.forbidden {
					if strings.Contains(r.detail, bad) {
						t.Errorf("%s: %q\n%s", tc.name, r.detail, planText(plan))
					}
				}
			}
		}
		if !found {
			t.Errorf("%s: no plan uses %s", tc.name, tc.want)
		}
	}
}

var (
	triggerPattern      = regexp.MustCompile(`(?is)^CREATE TRIGGER\s+\S+\s+.*?\bON\s+\S+\s+(?:WHEN\s+(.*?)\s+)?BEGIN\b(.*)\bEND\s*$`)
	triggerRowRef       = regexp.MustCompile(`(?i)\b(?:NEW|OLD)\.[a-z_]+`)
	triggerRaise        = regexp.MustCompile(`(?i)RAISE\s*\([^)]*\)`)
	chunkTriggerChecked = []string{
		"trg_items_reject_import_position_collision",
		"trg_items_reject_import_position_update",
		"trg_items_require_import_override",
		"trg_thread_import_chunks_payload_overlap",
		"trg_thread_import_chunks_turn_overlap",
	}
)

// Trigger programs run inside the statement that fires them, so their
// plans never appear in that statement's EXPLAIN QUERY PLAN. Each WHEN
// clause and body statement that reads chunk references is explained on
// its own, with the row references bound as parameters.
func TestImportTriggersProbeKnownChunks(t *testing.T) {
	s := newTestStore(t)
	rows, err := s.db.Query(`SELECT name, sql FROM sqlite_master WHERE type = 'trigger' AND sql LIKE '%thread_import_chunks%' ORDER BY name`)
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
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for _, name := range chunkTriggerChecked {
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
		for _, body := range strings.Split(m[2], ";") {
			if body = strings.TrimSpace(body); body != "" {
				statements = append(statements, body)
			}
		}
		for _, statement := range statements {
			statement = triggerRaise.ReplaceAllString(triggerRowRef.ReplaceAllString(statement, "?"), "NULL")
			plan := explainPlan(t, s, statement, make([]any, strings.Count(statement, "?"))...)
			_, enumerations := chunkRefNodes(plan, chunkRefNames(statement))
			if name == "trg_thread_import_chunks_order" {
				// MAX over the primary key's trailing column is one seek
				// at the end of the thread's key range.
				continue
			}
			for _, node := range enumerations {
				t.Errorf("%s: %q enumerates the thread's chunks\n%s\n%s", name, node, statement, planText(plan))
			}
		}
	}
}

// A by-id read costs one index probe on each arm plus one membership check,
// however many chunks the thread references.
func TestByIDReadCostIsIndependentOfChunkCount(t *testing.T) {
	s := newTestStore(t)
	const thread = "many-chunks"
	const chunks = 2000
	newImportTargetThread(t, s, thread)
	for turn := range chunks {
		if err := s.ApplyImportBatch(thread, ImportBatch{
			Turns: []Turn{{TurnID: fmt.Sprintf("%s:%d", thread, turn), ThreadID: thread, TurnIndex: turn, StartedAt: int64(turn) + 1}},
			Rows: []ImportRow{{Item: Item{
				ID: fmt.Sprintf("row-%d", turn), TurnIndex: turn, Kind: "assistant_text", Role: "assistant",
				Status: "completed", Summary: "tiny", CreatedAt: int64(turn) + 1, UpdatedAt: int64(turn) + 1,
			}}},
		}); err != nil {
			t.Fatalf("import chunk %d: %v", turn, err)
		}
	}
	var refs int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM thread_import_chunks WHERE thread_id = ?`, thread).Scan(&refs); err != nil || refs != chunks {
		t.Fatalf("chunk references = %d, %v; want %d", refs, err, chunks)
	}

	start := time.Now()
	for i := range 1000 {
		id := fmt.Sprintf("row-%d", i*7919%chunks)
		item, found, err := s.GetThreadItem(thread, id)
		if err != nil || !found || item.ID != id {
			t.Fatalf("GetThreadItem(%s) = %s, %v, %v", id, item.ID, found, err)
		}
	}
	elapsed := time.Since(start)
	t.Logf("1,000 by-id reads over %d chunks took %v", chunks, elapsed)
	if elapsed > 250*time.Millisecond {
		t.Errorf("1,000 by-id reads over %d chunks took %v", chunks, elapsed)
	}

	rec := recordStatements(t, s)
	stmts := rec.capture(func() {
		if _, found, err := s.GetThreadItem(thread, "row-1234"); err != nil || !found {
			t.Fatalf("GetThreadItem: %v %v", found, err)
		}
	})
	byID := false
	for _, stmt := range stmts {
		plan := explainPlan(t, s, stmt.query, stmt.args...)
		_, enumerations := chunkRefNodes(plan, chunkRefNames(stmt.query))
		for _, node := range enumerations {
			t.Errorf("by-id read enumerates chunks: %q\n%s", node, planText(plan))
		}
		for _, r := range plan {
			byID = byID || strings.Contains(r.detail, "idx_import_history_items_id (id=?)")
		}
	}
	if !byID {
		t.Error("by-id read does not probe idx_import_history_items_id by id")
	}
}

// Rows need not arrive in turn order; a chunk's range still bounds every
// row, so a turn read finds each of them.
func TestImportChunkTurnRangeBoundsUnorderedRows(t *testing.T) {
	s := newTestStore(t)
	const thread = "unordered"
	newImportTargetThread(t, s, thread)
	var rows []ImportRow
	for _, turn := range []int{0, 2, 1} {
		rows = append(rows, ImportRow{Item: Item{
			ID: fmt.Sprintf("row-%d", turn), TurnIndex: turn, Kind: "assistant_text", Role: "assistant",
			Status: "completed", Summary: "row", CreatedAt: 1, UpdatedAt: 1,
		}})
	}
	if err := s.ApplyImportBatch(thread, ImportBatch{Rows: rows}); err != nil {
		t.Fatal(err)
	}
	for turn := range 3 {
		items, err := s.ListTurnItems(thread, turn)
		if err != nil || len(items) != 1 || items[0].ID != fmt.Sprintf("row-%d", turn) {
			t.Errorf("turn %d = %v, %v", turn, itemIDs(items), err)
		}
	}
}

func TestMigrationV116ImportedKeyLookups(t *testing.T) {
	db := migrateThrough(t, 115)
	mustExec(t, db, `INSERT INTO projects(id,path,name,slug,created_at,updated_at) VALUES('p','/p','p','p',1,1)`)
	mustExec(t, db, `INSERT INTO threads(id,project_id,title,provider,workspace_path,created_at,updated_at) VALUES('t','p','t','claude','/p',1,1)`)
	item := func(chunk, id string, turn int) {
		t.Helper()
		mustExec(t, db, `INSERT INTO import_history_items(chunk_id,id,turn_index,item_index,kind,role,created_at,updated_at) VALUES(?,?,?,0,'assistant_text','assistant',1,1)`, chunk, id, turn)
	}
	// A chunk whose recorded range is wider than its rows: v116 narrows it
	// to the rows before copying it to the reference.
	mustExec(t, db, `INSERT INTO import_history_chunks(id,item_count,min_turn_index,max_turn_index) VALUES('wide',2,0,9)`)
	item("wide", "a", 2)
	item("wide", "b", 3)
	mustExec(t, db, `INSERT INTO thread_import_chunks(thread_id,chunk_order,chunk_id) VALUES('t',0,'wide')`)
	migrateFrom(t, db, 115)

	ranges := func(query string, args ...any) [2]int {
		t.Helper()
		var r [2]int
		if err := db.QueryRow(query, args...).Scan(&r[0], &r[1]); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if got := ranges(`SELECT min_turn_index,max_turn_index FROM import_history_chunks WHERE id='wide'`); got != [2]int{2, 3} {
		t.Errorf("recomputed chunk range = %v", got)
	}
	if got := ranges(`SELECT min_turn_index,max_turn_index FROM thread_import_chunks WHERE chunk_id='wide'`); got != [2]int{2, 3} {
		t.Errorf("backfilled reference range = %v", got)
	}

	mustExec(t, db, `INSERT INTO import_history_chunks(id,item_count,min_turn_index,max_turn_index) VALUES('later',1,5,6)`)
	if _, err := db.Exec(`INSERT INTO import_history_items(chunk_id,id,turn_index,item_index,kind,role,created_at,updated_at) VALUES('later','outside',7,0,'assistant_text','assistant',1,1)`); err == nil || !strings.Contains(err.Error(), "outside its chunk turn range") {
		t.Errorf("row outside its chunk range: %v", err)
	}
	item("later", "c", 5)
	mustExec(t, db, `INSERT INTO thread_import_chunks(thread_id,chunk_order,chunk_id) VALUES('t',1,'later')`)
	if got := ranges(`SELECT min_turn_index,max_turn_index FROM thread_import_chunks WHERE chunk_id='later'`); got != [2]int{5, 6} {
		t.Errorf("attached reference range = %v", got)
	}

	assertPlanUses(t, db, "idx_thread_import_chunks_turns", `EXPLAIN QUERY PLAN SELECT chunk_id FROM thread_import_chunks WHERE thread_id=? AND max_turn_index>=? AND min_turn_index<=?`, "t", 5, 5)
	assertPlanUses(t, db, "idx_import_history_items_completion_lookup", `EXPLAIN QUERY PLAN SELECT chunk_id FROM import_history_items WHERE completion_of<>'' AND completion_of=?`, "launch")
	assertPlanUses(t, db, "idx_import_history_items_task_lookup", `EXPLAIN QUERY PLAN SELECT chunk_id FROM import_history_items WHERE json_extract(meta,'$.task_id')=?`, "task")
	assertPlanUses(t, db, "idx_import_history_items_scope_root_lookup", `EXPLAIN QUERY PLAN SELECT chunk_id FROM import_history_items WHERE kind='tool_call' AND CASE WHEN json_valid(meta) THEN json_extract(meta, '$.transcript_root_id') END=?`, "root")
}

// Restore copies reference ranges verbatim and reinstalls the admission
// triggers in their index-driven form.
func TestRestoreKeepsImportChunkTurnRanges(t *testing.T) {
	s := newTestStore(t)
	seedKeyedLookupThread(t, s)
	readRanges := func() string {
		t.Helper()
		rows, err := s.db.Query(`SELECT chunk_order, min_turn_index, max_turn_index FROM thread_import_chunks WHERE thread_id = ? ORDER BY chunk_order`, keyedThreadID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out strings.Builder
		for rows.Next() {
			var order int
			var lo, hi sql.NullInt64
			if err := rows.Scan(&order, &lo, &hi); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&out, "%d:%v-%v ", order, lo, hi)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	want := readRanges()
	if want != "0:{0 true}-{0 true} 1:{1 true}-{1 true} 2:{2 true}-{2 true} 3:{3 true}-{3 true} " {
		t.Fatalf("fixture ranges = %s", want)
	}
	path := filepath.Join(t.TempDir(), "snapshot.sqlite")
	if err := s.SnapshotTo(path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreFrom(path); err != nil {
		t.Fatal(err)
	}
	if got := readRanges(); got != want {
		t.Errorf("restored ranges = %s, want %s", got, want)
	}
	var admission string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'trg_thread_import_chunks_turn_overlap'`).Scan(&admission); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(admission, "refs.max_turn_index>=incoming.min_turn_index") {
		t.Errorf("restore reinstalled another admission trigger:\n%s", admission)
	}
	if err := s.ApplyImportBatch(keyedThreadID, ImportBatch{Rows: []ImportRow{{Item: Item{
		ID: "after-restore", TurnIndex: 5, Kind: "assistant_text", Role: "assistant", Status: "completed", CreatedAt: 1, UpdatedAt: 1,
	}}}}); err != nil {
		t.Fatal(err)
	}
	if items, err := s.ListTurnItems(keyedThreadID, 5); err != nil || len(items) != 1 {
		t.Errorf("turn read after restore = %v, %v", itemIDs(items), err)
	}
}
