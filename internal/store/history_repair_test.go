package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// sealItemsForTest writes the representation the removed background sealing
// produced: the named private rows move into one "sealed:" chunk attached to
// the thread, payload append chunks folded into the chunk payload, highlight
// spans copied, search rows moved to the import arm with their rowids, and the
// thread stamps left alone.
func sealItemsForTest(t *testing.T, s *Store, threadID string, ids ...string) string {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	chunk := importHistoryChunk{id: sealedChunkLow + uuid.NewString()}
	for _, id := range ids {
		item, err := scanItemRow(tx.QueryRow(`SELECT `+itemHydrationColumns("items.thread_id", "''", "''", "''", "items.rev")+` FROM items WHERE thread_id=? AND id=?`, threadID, id))
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		row := ImportRow{Item: item}
		for _, p := range []struct {
			id  string
			dst **Payload
		}{{item.PayloadID, &row.Payload}, {item.InputPayloadID, &row.InputPayload}} {
			if p.id == "" {
				continue
			}
			var payload Payload
			if err := tx.QueryRow(`SELECT id,kind,meta,data,created_at FROM payloads WHERE thread_id=? AND id=?`, threadID, p.id).
				Scan(&payload.ID, &payload.Kind, &payload.Meta, &payload.Data, &payload.CreatedAt); err != nil {
				t.Fatalf("read payload %s: %v", p.id, err)
			}
			chunks, err := queryStringsTx(tx, `SELECT data FROM payload_chunks WHERE thread_id=? AND payload_id=? ORDER BY chunk_index`, threadID, p.id)
			if err != nil {
				t.Fatal(err)
			}
			payload.Data = append(payload.Data, strings.Join(chunks, "")...)
			*p.dst = &payload
		}
		chunk.rows = append(chunk.rows, row)
	}
	slices.SortFunc(chunk.rows, func(a, b ImportRow) int {
		if a.Item.TurnIndex != b.Item.TurnIndex {
			return a.Item.TurnIndex - b.Item.TurnIndex
		}
		return a.Item.ItemIndex - b.Item.ItemIndex
	})
	chunk.minTurn = chunk.rows[0].Item.TurnIndex
	chunk.maxTurn = chunk.rows[len(chunk.rows)-1].Item.TurnIndex
	steps := []func() error{
		func() error {
			_, err := tx.Exec(`INSERT INTO import_history_chunks(id,item_count,min_turn_index,max_turn_index) VALUES(?,?,?,?)`, chunk.id, len(chunk.rows), chunk.minTurn, chunk.maxTurn)
			return err
		},
		func() error { return insertImportHistoryChunkRowsTx(tx, chunk) },
		func() error {
			_, err := tx.Exec(`UPDATE import_history_payloads SET
 preview_spans=(SELECT preview_spans FROM payloads WHERE thread_id=? AND id=import_history_payloads.id),
 spans=(SELECT spans FROM payloads WHERE thread_id=? AND id=import_history_payloads.id)
 WHERE chunk_id=?`, threadID, threadID, chunk.id)
			return err
		},
		func() error { return setHistoryBulkLoadTx(tx, threadID, true, "seal") },
		func() error {
			clause, args := inClause("id", ids)
			_, err := tx.Exec(`DELETE FROM items WHERE thread_id=? AND `+clause, append([]any{threadID}, args...)...)
			return err
		},
		func() error {
			_, err := tx.Exec(`INSERT INTO thread_import_chunks(thread_id,chunk_order,chunk_id)
 SELECT ?,COALESCE(MAX(chunk_order)+1,0),? FROM thread_import_chunks WHERE thread_id=?`, threadID, chunk.id, threadID)
			return err
		},
		func() error {
			clause, args := inClause("item_id", ids)
			_, err := tx.Exec(`UPDATE thread_search_rows SET source='import' WHERE thread_id=? AND source='item' AND `+clause, append([]any{threadID}, args...)...)
			return err
		},
		func() error { return setHistoryBulkLoadTx(tx, threadID, false, "seal") },
	}
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("seal step %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return chunk.id
}

// legacyCopyForkForTest writes the copy fork the removed history clone made
// of src, whose history is all sealed, through throughTurn (every turn when
// negative): the source's chunks that end by the cut are attached, the rows
// of a chunk the cut splits are copied with their payload bytes, and every
// row is indexed for search in timeline order, shared rows on the import
// arm. Such forks predate pointer forks and still share sealed chunks.
func legacyCopyForkForTest(t *testing.T, s *Store, src, fork string, throughTurn int) {
	t.Helper()
	if err := s.CreateThread(makeThread(fork, "claude")); err != nil {
		t.Fatal(err)
	}
	if throughTurn < 0 {
		throughTurn = 1 << 30
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	const split = `FROM thread_import_chunks refs JOIN import_history_items items ON items.chunk_id = refs.chunk_id
 WHERE refs.thread_id = ? AND refs.max_turn_index > ? AND items.turn_index <= ?`
	if _, err := tx.Exec(`INSERT INTO thread_import_chunks(thread_id, chunk_order, chunk_id)
 SELECT ?, chunk_order, chunk_id FROM thread_import_chunks WHERE thread_id = ? AND max_turn_index <= ? ORDER BY chunk_order`,
		fork, src, throughTurn); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO payloads(thread_id, id, kind, meta, data, created_at, preview_spans, spans)
 SELECT ?, p.id, p.kind, p.meta, p.data, p.created_at, p.preview_spans, p.spans
   FROM import_history_payloads p
  WHERE EXISTS (SELECT 1 ` + split + ` AND items.chunk_id = p.chunk_id
                   AND p.id IN (items.payload_id, items.input_payload_id))`,
		`INSERT INTO items(id, thread_id, turn_index, item_index, kind, role, status, summary, payload_id, input_payload_id,
   parent_id, is_background, completion_of, tool_name, decision, meta, created_at, updated_at)
 SELECT items.id, ?, items.turn_index, items.item_index, items.kind, items.role, items.status, items.summary,
        items.payload_id, items.input_payload_id, items.parent_id, items.is_background, items.completion_of,
        items.tool_name, items.decision, items.meta, items.created_at, items.updated_at ` + split,
	} {
		if _, err := tx.Exec(statement, fork, src, throughTurn, throughTurn); err != nil {
			t.Fatalf("legacy fork %s: %v\n%s", fork, err, statement)
		}
	}
	rows, err := tx.Query(`SELECT id, kind, status, summary, shared FROM (
   SELECT items.id, items.kind, items.status, items.summary, items.turn_index, items.item_index, 1 AS shared
     FROM thread_import_chunks refs JOIN import_history_items items ON items.chunk_id = refs.chunk_id
    WHERE refs.thread_id = ?1
   UNION ALL
   SELECT id, kind, status, summary, turn_index, item_index, 0 FROM items WHERE thread_id = ?1)
 ORDER BY turn_index, item_index`, fork)
	if err != nil {
		t.Fatal(err)
	}
	var run []Item
	var runShared bool
	index := func() {
		t.Helper()
		source := ThreadSearchSourceItem
		if runShared {
			source = ThreadSearchSourceImport
		}
		if err := indexSettledItemsTx(tx, run, source); err != nil {
			t.Fatal(err)
		}
		run = nil
	}
	var all []struct {
		item   Item
		shared bool
	}
	for rows.Next() {
		var item Item
		var shared bool
		if err := rows.Scan(&item.ID, &item.Kind, &item.Status, &item.Summary, &shared); err != nil {
			t.Fatal(err)
		}
		item.ThreadID = fork
		all = append(all, struct {
			item   Item
			shared bool
		}{item, shared})
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	for _, row := range all {
		if len(run) > 0 && row.shared != runShared {
			index()
		}
		run, runShared = append(run, row.item), row.shared
	}
	if len(run) > 0 {
		index()
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// localHistoryFixture writes count settled assistant rows to a new thread,
// ten per turn. Every row has its own payload; every third payload has an
// append chunk, every fourth has highlight spans and every fifth row has an
// input payload.
func localHistoryFixture(t *testing.T, s *Store, thread string, count int) []string {
	t.Helper()
	if err := s.CreateThread(makeThread(thread, "claude")); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("row-%03d", i)
		item := Item{
			ID: id, ThreadID: thread, TurnIndex: i / 10, ItemIndex: i % 10,
			Kind: "assistant_text", Role: "assistant", Status: "completed",
			Summary: fmt.Sprintf("restorable history %d", i), Meta: fmt.Sprintf(`{"n":%d}`, i),
			CreatedAt: int64(1000 + i), UpdatedAt: int64(2000 + i),
		}
		payload := &Payload{ID: "p-" + id, Kind: "text", Meta: `{"m":1}`, Data: []byte("payload " + id), CreatedAt: int64(3000 + i)}
		var input *Payload
		if i%5 == 0 {
			input = &Payload{ID: "in-" + id, Kind: "tool_call_input", Meta: "{}", Data: []byte("input " + id), CreatedAt: 1}
		}
		if _, err := s.UpsertItemWithInputPayload(item, payload, input); err != nil {
			t.Fatal(err)
		}
		if i%3 == 0 {
			if err := s.AppendPayloadData(thread, payload.ID, []byte(" appended"), `{"m":2}`, int64(4000+i)); err != nil {
				t.Fatal(err)
			}
		}
		if i%4 == 0 {
			if err := s.UpdatePayloadSpans(thread, payload.ID, `{"preview":1}`, `{"full":1}`); err != nil {
				t.Fatal(err)
			}
		}
		ids = append(ids, id)
	}
	return ids
}

type repairPayloadView struct {
	Kind, Meta, Data, Preview, Spans string
	CreatedAt                        int64
}

type repairSearchRow struct {
	Rowid        int64
	Item, Source string
	Kind         string
}

// repairView is what a thread shows: its logical rows without revisions,
// every payload they name, its search mappings and its search hits.
type repairView struct {
	Items    []Item
	Payloads map[string]repairPayloadView
	Search   []repairSearchRow
	Hits     []string
}

func readRepairView(t *testing.T, s *Store, thread string) repairView {
	t.Helper()
	items, err := s.ListItems(thread)
	if err != nil {
		t.Fatal(err)
	}
	view := repairView{Payloads: map[string]repairPayloadView{}}
	for _, item := range items {
		item.Rev = 0
		view.Items = append(view.Items, item)
		for _, id := range []string{item.PayloadID, item.InputPayloadID} {
			if id == "" {
				continue
			}
			var p repairPayloadView
			if err := s.db.QueryRow(`SELECT kind, meta, created_at, preview_spans, spans FROM timeline_payloads WHERE thread_id=? AND id=?`, thread, id).
				Scan(&p.Kind, &p.Meta, &p.CreatedAt, &p.Preview, &p.Spans); err != nil {
				t.Fatalf("read payload %s/%s: %v", thread, id, err)
			}
			data, err := s.GetPayloadData(thread, id)
			if err != nil {
				t.Fatal(err)
			}
			p.Data = string(data)
			view.Payloads[id] = p
		}
	}
	rows, err := s.db.Query(`SELECT rowid, item_id, source, kind FROM thread_search_rows WHERE thread_id=? AND item_id<>'' ORDER BY rowid`, thread)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var r repairSearchRow
		if err := rows.Scan(&r.Rowid, &r.Item, &r.Source, &r.Kind); err != nil {
			t.Fatal(err)
		}
		view.Search = append(view.Search, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	hits, err := s.SearchThreads("restorable", ThreadSearchFilter{ThreadIDs: []string{thread}, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range hits {
		view.Hits = append(view.Hits, hit.ItemID)
	}
	slices.Sort(view.Hits)
	return view
}

// withSearchSource returns the view's search rows with every source replaced,
// which is what sealing changed.
func withSearchSource(rows []repairSearchRow, source string) []repairSearchRow {
	out := slices.Clone(rows)
	for i := range out {
		out[i].Source = source
	}
	return out
}

// requireSameLogicalView compares two views of a thread whose rows may sit in
// different representations: search rows keep their rowids and kinds, and
// only the arm they are filed under may differ.
func requireSameLogicalView(t *testing.T, label string, got, want repairView) {
	t.Helper()
	got.Search = withSearchSource(got.Search, "")
	want.Search = withSearchSource(want.Search, "")
	requireSameRepairView(t, label, got, want)
}

func requireSameRepairView(t *testing.T, label string, got, want repairView) {
	t.Helper()
	if !reflect.DeepEqual(got.Items, want.Items) {
		t.Fatalf("%s: items differ\n got %+v\nwant %+v", label, got.Items, want.Items)
	}
	if !reflect.DeepEqual(got.Payloads, want.Payloads) {
		t.Fatalf("%s: payloads differ\n got %+v\nwant %+v", label, got.Payloads, want.Payloads)
	}
	if !reflect.DeepEqual(got.Search, want.Search) {
		t.Fatalf("%s: search rows differ\n got %+v\nwant %+v", label, got.Search, want.Search)
	}
	if !reflect.DeepEqual(got.Hits, want.Hits) {
		t.Fatalf("%s: search hits differ\n got %v\nwant %v", label, got.Hits, want.Hits)
	}
}

func countRows(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// requireCheckpointed fails unless the database file alone, read without its
// WAL, shows the same history tables as the store: every committed frame has
// been checkpointed.
func requireCheckpointed(t *testing.T, s *Store, label string) {
	t.Helper()
	const counts = `SELECT (SELECT count(*) FROM items), (SELECT count(*) FROM payloads),
 (SELECT count(*) FROM thread_import_chunks), (SELECT count(*) FROM import_history_chunks)`
	read := func(db *sql.DB) [4]int {
		t.Helper()
		var n [4]int
		if err := db.QueryRow(counts).Scan(&n[0], &n[1], &n[2], &n[3]); err != nil {
			t.Fatalf("%s: count history rows: %v", label, err)
		}
		return n
	}
	file, err := sql.Open("sqlite", "file:"+s.path+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if got, want := read(file), read(s.db); got != want {
		t.Fatalf("%s: database file shows %v, store shows %v: a repair transaction was not checkpointed", label, got, want)
	}
}

func requireNoImportedHistory(t *testing.T, s *Store) {
	t.Helper()
	for _, table := range []string{"import_history_chunks", "import_history_items", "import_history_payloads", "thread_import_chunks", "thread_import_item_overrides"} {
		if n := countRows(t, s, `SELECT count(*) FROM `+table); n != 0 {
			t.Fatalf("%s kept %d rows after repair", table, n)
		}
	}
}

func historyStamp(t *testing.T, s *Store, thread string) HistoryStamp {
	t.Helper()
	stamp, found, err := s.ThreadHistoryStamp(thread)
	if err != nil || !found {
		t.Fatalf("stamp %s: found=%v err=%v", thread, found, err)
	}
	return stamp
}

func TestUnsealThreadHistoryRestoresPrivateRows(t *testing.T) {
	s := newTestStore(t)
	ids := localHistoryFixture(t, s, "t", 30)
	before := readRepairView(t, s, "t")
	if len(before.Search) != 30 || len(before.Hits) != 30 {
		t.Fatalf("fixture indexed %d rows with %d hits, want 30", len(before.Search), len(before.Hits))
	}
	// A row with no search mapping stands for a row an index build had not
	// reached; the repair indexes it.
	mustExec(t, s.db, `DELETE FROM thread_search WHERE rowid = (SELECT rowid FROM thread_search_rows WHERE thread_id='t' AND item_id='row-029')`)
	mustExec(t, s.db, `DELETE FROM thread_search_rows WHERE thread_id='t' AND item_id='row-029'`)

	sealItemsForTest(t, s, "t", ids[:10]...)
	sealItemsForTest(t, s, "t", ids[10:20]...)
	sealItemsForTest(t, s, "t", ids[20:]...)
	if n := countRows(t, s, `SELECT count(*) FROM items WHERE thread_id='t'`); n != 0 {
		t.Fatalf("sealing left %d private rows", n)
	}
	sealed := readRepairView(t, s, "t")
	if !reflect.DeepEqual(sealed.Items, before.Items) || !reflect.DeepEqual(sealed.Payloads, before.Payloads) {
		t.Fatal("sealed fixture does not read like the private rows")
	}
	if !reflect.DeepEqual(sealed.Search, withSearchSource(before.Search[:29], ThreadSearchSourceImport)) {
		t.Fatalf("sealed search rows = %+v", sealed.Search)
	}
	stamp := historyStamp(t, s, "t")

	stats, err := s.UnsealThreadHistory(context.Background(), "t", nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats != (UnsealStats{Chunks: 3, Rows: 30, Payloads: 36}) {
		t.Fatalf("stats = %+v, want 3 chunks, 30 rows, 36 payloads", stats)
	}
	after := readRepairView(t, s, "t")
	if len(after.Search) != 30 {
		t.Fatalf("%d search rows after repair, want 30", len(after.Search))
	}
	after.Search[29].Rowid = before.Search[29].Rowid
	requireSameRepairView(t, "unsealed", after, before)
	requireNoImportedHistory(t, s)
	if n := countRows(t, s, `SELECT count(*) FROM items WHERE thread_id='t'`); n != 30 {
		t.Fatalf("items holds %d rows, want 30", n)
	}
	// Append chunks were folded into the payload bytes by sealing.
	if n := countRows(t, s, `SELECT count(*) FROM payload_chunks WHERE thread_id='t'`); n != 0 {
		t.Fatalf("payload_chunks holds %d rows", n)
	}
	var data string
	if err := s.db.QueryRow(`SELECT data FROM payloads WHERE thread_id='t' AND id='p-row-000'`).Scan(&data); err != nil || data != "payload row-000 appended" {
		t.Fatalf("physical payload = %q, %v", data, err)
	}

	// Each transaction stamps the rows it moved at the thread stamp it started
	// from and advances the stamp by their count, however the time budget
	// split the move; the epoch holds.
	got := historyStamp(t, s, "t")
	if got.Rev != stamp.Rev+30 || got.Epoch != stamp.Epoch {
		t.Fatalf("stamp = %+v, want rev %d epoch %d", got, stamp.Rev+30, stamp.Epoch)
	}
	stamped := map[int64]int64{}
	for _, item := range mustListItems(t, s, "t") {
		stamped[item.Rev]++
	}
	for next := stamp.Rev; len(stamped) > 0; {
		moved, ok := stamped[next]
		if !ok {
			t.Fatalf("moved rows are stamped at %v, want runs starting at rev %d", stamped, stamp.Rev)
		}
		delete(stamped, next)
		next += moved
	}
	for _, item := range mustListItems(t, s, "t") {
		if item.Rev < 0 {
			t.Fatalf("row %s still reads as imported (rev %d)", item.ID, item.Rev)
		}
	}
	if n := countRows(t, s, `SELECT history_bulk_load FROM threads WHERE id='t'`); n != 0 {
		t.Fatal("history_bulk_load left set")
	}

	again, err := s.UnsealThreadHistory(context.Background(), "t", nil)
	if err != nil || again != (UnsealStats{}) {
		t.Fatalf("second repair = %+v, %v; want no work", again, err)
	}
	if historyStamp(t, s, "t") != got {
		t.Fatal("second repair moved the stamp")
	}
	threads, err := s.SealedHistoryThreads(context.Background())
	if err != nil || len(threads) != 0 {
		t.Fatalf("sealed threads after repair = %v, %v", threads, err)
	}
}

func mustListItems(t *testing.T, s *Store, thread string) []Item {
	t.Helper()
	items, err := s.ListItems(thread)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func TestUnsealThreadHistoryResumesAfterCancellationAndFailure(t *testing.T) {
	s := newTestStore(t)
	ids := localHistoryFixture(t, s, "t", 300)
	before := readRepairView(t, s, "t")
	for start := 0; start < 300; start += 60 {
		sealItemsForTest(t, s, "t", ids[start:start+60]...)
	}
	stamp := historyStamp(t, s, "t")
	remaining := func() int {
		t.Helper()
		return countRows(t, s, `SELECT count(*) FROM thread_import_chunks`)
	}

	// The first transaction moves pieces of chunks, latest turns first, and
	// pauses; a quit during the pause stops the loop without an error.
	ctx, cancel := context.WithCancel(context.Background())
	pauses := 0
	stats, err := s.UnsealThreadHistory(ctx, "t", func() {
		pauses++
		cancel()
		requireCheckpointed(t, s, "first pause")
	})
	if err != nil {
		t.Fatal(err)
	}
	inputs := countRows(t, s, `SELECT count(*) FROM items WHERE input_payload_id IS NOT NULL`)
	if pauses != 1 || stats.Rows < historyRepairPieceRows || stats.Rows > historyRepairRows || stats.Chunks > 4 || stats.Payloads != stats.Rows+inputs {
		t.Fatalf("cancelled repair: pauses=%d stats=%+v inputs=%d", pauses, stats, inputs)
	}
	if n := remaining(); n != 5-stats.Chunks {
		t.Fatalf("%d chunk references remain after releasing %d of 5", n, stats.Chunks)
	}
	if n := countRows(t, s, `SELECT count(*) FROM items WHERE id = 'row-240'`); n != 1 {
		t.Fatal("repair did not start from the latest turns")
	}
	requireSameLogicalView(t, "partial", readRepairView(t, s, "t"), before)
	partial := historyStamp(t, s, "t")
	if partial.Rev != stamp.Rev+int64(stats.Rows) || partial.Epoch != stamp.Epoch {
		t.Fatalf("partial stamp = %+v after %d rows, was %+v", partial, stats.Rows, stamp)
	}

	// The chunk holding the first turn fails. Transactions before it commit;
	// the failing one rolls back whole: flag clear, stamp and chunk intact.
	mustExec(t, s.db, `CREATE TRIGGER fail_repair BEFORE INSERT ON items WHEN NEW.id = 'row-059' BEGIN SELECT RAISE(ABORT, 'injected'); END`)
	failed, err := s.UnsealThreadHistory(context.Background(), "t", nil)
	if err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("failing repair error = %v", err)
	}
	if got := historyStamp(t, s, "t"); got.Rev != partial.Rev+int64(failed.Rows) || got.Epoch != stamp.Epoch {
		t.Fatalf("failed repair stamp = %+v after %d committed rows, was %+v", got, failed.Rows, partial)
	}
	if n := remaining(); n != 5-stats.Chunks-failed.Chunks || n < 1 {
		t.Fatalf("failed repair left %d references after releasing %d", n, stats.Chunks+failed.Chunks)
	}
	if n := countRows(t, s, `SELECT count(*) FROM import_history_items WHERE id = 'row-059'`); n != 1 {
		t.Fatal("the failing chunk was released")
	}
	if n := countRows(t, s, `SELECT history_bulk_load FROM threads WHERE id='t'`); n != 0 {
		t.Fatal("failed repair left history_bulk_load set")
	}
	requireSameLogicalView(t, "after failure", readRepairView(t, s, "t"), before)

	mustExec(t, s.db, `DROP TRIGGER fail_repair`)
	resumed, err := s.UnsealThreadHistory(context.Background(), "t", nil)
	if err != nil || stats.Chunks+failed.Chunks+resumed.Chunks != 5 || stats.Rows+failed.Rows+resumed.Rows != 300 {
		t.Fatalf("resumed repair = %+v, %v after %+v and %+v", resumed, err, stats, failed)
	}
	requireSameRepairView(t, "resumed", readRepairView(t, s, "t"), before)
	requireNoImportedHistory(t, s)
	requireCheckpointed(t, s, "resumed")
	if got := historyStamp(t, s, "t"); got.Rev != stamp.Rev+300 || got.Epoch != stamp.Epoch {
		t.Fatalf("final stamp = %+v", got)
	}
}

func TestUnsealThreadHistoryBatchHonorsBudgets(t *testing.T) {
	s := newTestStore(t)
	ids := localHistoryFixture(t, s, "t", 40)
	for start := 0; start < 40; start += 10 {
		sealItemsForTest(t, s, "t", ids[start:start+10]...)
	}
	// One piece per transaction when the next would pass the row, byte or
	// time budget, and always at least one.
	for _, budget := range []historyRepairBudget{
		{rows: 1, piece: 16, bytes: 1 << 20, time: time.Hour},
		{rows: 100, piece: 16, bytes: 1, time: time.Hour},
		{rows: 100, piece: 16, bytes: 1 << 20, time: 0},
	} {
		stats, more, err := s.unsealThreadHistoryBatch("t", budget)
		if err != nil || !more || stats.Chunks != 1 || stats.Rows != 10 {
			t.Fatalf("budget %+v: stats=%+v more=%v err=%v", budget, stats, more, err)
		}
	}
	stats, more, err := s.unsealThreadHistoryBatch("t", historyRepairBudget{rows: 10, piece: 16, bytes: 1 << 20, time: time.Hour})
	if err != nil || more || stats.Chunks != 1 {
		t.Fatalf("last batch: stats=%+v more=%v err=%v", stats, more, err)
	}
}

// An import-side search row the move cannot flip, because the row already
// has an item-side mapping, is dropped with the chunk.
func TestUnsealThreadHistoryDropsUnflippedImportSearchRows(t *testing.T) {
	s := newTestStore(t)
	ids := localHistoryFixture(t, s, "t", 10)
	sealItemsForTest(t, s, "t", ids...)
	mustExec(t, s.db, `INSERT INTO thread_search_rows(thread_id, item_id, source, kind)
 SELECT thread_id, item_id, 'item', kind FROM thread_search_rows WHERE thread_id = 't' AND item_id = 'row-003'`)
	if _, err := s.UnsealThreadHistory(context.Background(), "t", nil); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s, `SELECT count(*) FROM thread_search_rows WHERE source = 'import'`); n != 0 {
		t.Fatalf("%d import-side search rows survived the repair", n)
	}
	requireNoImportedHistory(t, s)
}

// A batch that works through every chunk it listed reports more: the list
// stops at the row budget, and a chunk whose rows the thread has all
// localized moves no rows.
func TestUnsealThreadHistoryBatchContinuesPastOverriddenChunks(t *testing.T) {
	s := newTestStore(t)
	localHistoryFixture(t, s, "t", 30)
	for _, id := range []string{"row-000", "row-010", "row-020"} {
		sealItemsForTest(t, s, "t", id)
	}
	for _, id := range []string{"row-010", "row-020"} {
		if err := s.UpdateItemMeta("t", id, `{"localized":true}`); err != nil {
			t.Fatal(err)
		}
	}
	budget := historyRepairBudget{rows: 1, piece: 16, bytes: 1 << 20, time: time.Hour}
	stats, more, err := s.unsealThreadHistoryBatch("t", budget)
	if err != nil || !more || stats != (UnsealStats{Chunks: 2}) {
		t.Fatalf("overridden chunks: stats=%+v more=%v err=%v", stats, more, err)
	}
	stats, more, err = s.unsealThreadHistoryBatch("t", budget)
	if err != nil || more || stats.Chunks != 1 || stats.Rows != 1 {
		t.Fatalf("last chunk: stats=%+v more=%v err=%v", stats, more, err)
	}
	requireNoImportedHistory(t, s)
}

// A chunk larger than a piece moves over several transactions. Between them
// the moved rows are overridden, as a localized row is, and the thread reads
// the same.
func TestUnsealThreadHistoryMovesLargeChunksInPieces(t *testing.T) {
	s := newTestStore(t)
	ids := localHistoryFixture(t, s, "t", 40)
	before := readRepairView(t, s, "t")
	sealItemsForTest(t, s, "t", ids...)
	stamp := historyStamp(t, s, "t")
	onePiece := historyRepairBudget{rows: 100, piece: 16, bytes: 1 << 20, time: 0}
	moved := 0
	for _, want := range []int{16, 16} {
		stats, more, err := s.unsealThreadHistoryBatch("t", onePiece)
		if err != nil || !more || stats != (UnsealStats{Rows: want, Payloads: stats.Payloads}) || stats.Payloads < want {
			t.Fatalf("piece: stats=%+v more=%v err=%v, want %d rows", stats, more, err, want)
		}
		moved += want
		if n := countRows(t, s, `SELECT count(*) FROM items WHERE thread_id = 't'`); n != moved {
			t.Fatalf("%d rows in items after moving %d", n, moved)
		}
		if n := countRows(t, s, `SELECT count(*) FROM thread_import_item_overrides WHERE thread_id = 't'`); n != moved {
			t.Fatalf("%d overrides after moving %d rows", n, moved)
		}
		requireSameLogicalView(t, fmt.Sprintf("after %d rows", moved), readRepairView(t, s, "t"), before)
		if got := historyStamp(t, s, "t"); got.Rev != stamp.Rev+int64(moved) || got.Epoch != stamp.Epoch {
			t.Fatalf("stamp = %+v after %d rows, was %+v", got, moved, stamp)
		}
	}
	stats, more, err := s.unsealThreadHistoryBatch("t", onePiece)
	if err != nil || more || stats.Rows != 8 || stats.Chunks != 1 {
		t.Fatalf("last piece: stats=%+v more=%v err=%v", stats, more, err)
	}
	requireSameRepairView(t, "released", readRepairView(t, s, "t"), before)
	requireNoImportedHistory(t, s)
	if got := historyStamp(t, s, "t"); got.Rev != stamp.Rev+40 || got.Epoch != stamp.Epoch {
		t.Fatalf("final stamp = %+v, was %+v", got, stamp)
	}
}

func TestUnsealThreadHistoryBatchSelectionReadsTurnIndex(t *testing.T) {
	s := newTestStore(t)
	args := []any{"t", sealedChunkLow, sealedChunkHigh, historyRepairRows + 1}
	assertPlanUses(t, s.db, "idx_thread_import_chunks_turns", `EXPLAIN QUERY PLAN `+sealedHistoryBatchSQL, args...)
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+sealedHistoryBatchSQL, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		// Sorting one turn's references is the contract; sorting all of
		// them is not.
		if strings.Contains(detail, "TEMP B-TREE") && detail != "USE TEMP B-TREE FOR LAST TERM OF ORDER BY" {
			t.Fatalf("batch selection sorts all of the thread's references: %s", detail)
		}
	}
}

func TestUnsealThreadHistoryReleasesSmallerChunksFirstWithinATurn(t *testing.T) {
	s := newTestStore(t)
	ids := localHistoryFixture(t, s, "t", 30)
	// Turn 1 in chunks of 3, 1, 4 and 2 rows; turn 2 in 4, 1, 3 and 2.
	for _, cut := range [][2]int{{10, 13}, {13, 14}, {14, 18}, {18, 20}, {20, 24}, {24, 25}, {25, 28}, {28, 30}} {
		sealItemsForTest(t, s, "t", ids[cut[0]:cut[1]]...)
	}
	var got []int
	for more := true; more; {
		var stats UnsealStats
		var err error
		stats, more, err = s.unsealThreadHistoryBatch("t", historyRepairBudget{rows: 1, piece: 16, bytes: 1 << 20, time: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, stats.Rows)
	}
	if want := []int{1, 2, 3, 4, 1, 2, 3, 4}; !slices.Equal(got, want) {
		t.Fatalf("released chunk sizes = %v, want %v", got, want)
	}
}

// Moving a launch's sealed children back is a representation change: the
// launch's read does not change, so its stamp stays where it was.
func TestUnsealThreadHistoryLeavesAnchorStampsAlone(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatal(err)
	}
	launch := Item{ID: "launch", ThreadID: "t", Kind: "tool_call", Role: "assistant", Status: "completed", ToolName: "Agent", Summary: "launch", Meta: "{}", CreatedAt: 1, UpdatedAt: 1}
	if _, err := s.UpsertItemWithInputPayload(launch, &Payload{ID: "p-launch", Kind: "text", Meta: "{}", Data: []byte("launch"), CreatedAt: 1}, nil); err != nil {
		t.Fatal(err)
	}
	var children []string
	for i := 1; i <= 8; i++ {
		id := fmt.Sprintf("child-%d", i)
		child := Item{ID: id, ThreadID: "t", TurnIndex: 1, ItemIndex: i, Kind: "assistant_text", Role: "assistant", Status: "completed", ParentID: "launch", Summary: id, Meta: "{}", CreatedAt: 1, UpdatedAt: 1}
		if _, err := s.UpsertItemWithInputPayload(child, &Payload{ID: "p-" + id, Kind: "text", Meta: "{}", Data: []byte(id), CreatedAt: 1}, nil); err != nil {
			t.Fatal(err)
		}
		children = append(children, id)
	}
	sealItemsForTest(t, s, "t", children...)
	// A later write moves the thread stamp past the launch's, so a re-stamp
	// would show.
	later := Item{ID: "later", ThreadID: "t", TurnIndex: 2, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "later", Meta: "{}", CreatedAt: 1, UpdatedAt: 1}
	if _, err := s.UpsertItemWithInputPayload(later, &Payload{ID: "p-later", Kind: "text", Meta: "{}", Data: []byte("later"), CreatedAt: 1}, nil); err != nil {
		t.Fatal(err)
	}
	before := readRepairView(t, s, "t")
	launchRev := itemRevisionOf(t, s, "t", "launch")
	stamp := historyStamp(t, s, "t")
	if launchRev >= stamp.Rev {
		t.Fatalf("launch rev %d is not below the thread stamp %d", launchRev, stamp.Rev)
	}

	stats, err := s.UnsealThreadHistory(context.Background(), "t", nil)
	if err != nil || stats.Rows != len(children) {
		t.Fatalf("repair = %+v, %v", stats, err)
	}
	if got := itemRevisionOf(t, s, "t", "launch"); got != launchRev {
		t.Fatalf("repair re-stamped the launch: rev %d -> %d", launchRev, got)
	}
	for _, id := range children {
		if got := itemRevisionOf(t, s, "t", id); got != stamp.Rev {
			t.Fatalf("moved row %s rev = %d, want the pre-repair stamp %d", id, got, stamp.Rev)
		}
	}
	if got := historyStamp(t, s, "t"); got.Rev != stamp.Rev+int64(len(children)) || got.Epoch != stamp.Epoch {
		t.Fatalf("stamp after repair = %+v, was %+v", got, stamp)
	}
	requireSameLogicalView(t, "repaired", readRepairView(t, s, "t"), before)
}

// TestUnsealThreadHistoryLocalizesEveryReference covers a sealed chunk that
// copy forks share: each referencing thread gets its own rows, overrides
// survive as the rows they chose, and a fork that copied part of a chunk
// keeps its bytes after the chunk is gone.
func TestUnsealThreadHistoryLocalizesEveryReference(t *testing.T) {
	s := newTestStore(t)
	ids := localHistoryFixture(t, s, "src", 40)
	sealItemsForTest(t, s, "src", ids[:20]...)
	sealItemsForTest(t, s, "src", ids[20:]...)
	legacyCopyForkForTest(t, s, "src", "whole", -1)
	legacyCopyForkForTest(t, s, "src", "cut", 2)
	if n := countRows(t, s, `SELECT count(*) FROM (SELECT chunk_id FROM thread_import_chunks GROUP BY chunk_id HAVING count(*) > 1)`); n != 2 {
		t.Fatalf("%d chunks shared by forks, want 2", n)
	}
	// Fork-private changes over shared rows: a localized row, a deleted row
	// and a payload overlay without a localized row.
	if err := s.UpdateItemMeta("whole", "row-005", `{"localized":true}`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteThreadItem("whole", "row-006"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplacePayloadData("whole", "p-row-007", []byte("overlay bytes"), `{"overlay":1}`, 9); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s, `SELECT count(*) FROM thread_import_item_overrides WHERE thread_id='whole'`); n != 2 {
		t.Fatalf("whole has %d overrides, want 2", n)
	}
	views := map[string]repairView{}
	stamps := map[string]HistoryStamp{}
	for _, thread := range []string{"src", "whole", "cut"} {
		views[thread] = readRepairView(t, s, thread)
		stamps[thread] = historyStamp(t, s, thread)
	}

	threads, err := s.SealedHistoryThreads(context.Background())
	if err != nil || !reflect.DeepEqual(threads, []string{"cut", "src", "whole"}) {
		t.Fatalf("sealed threads = %v, %v", threads, err)
	}
	moved := map[string]int{}
	for _, thread := range threads {
		stats, err := s.UnsealThreadHistory(context.Background(), thread, nil)
		if err != nil {
			t.Fatalf("repair %s: %v", thread, err)
		}
		moved[thread] = stats.Rows
	}
	if want := map[string]int{"src": 40, "whole": 38, "cut": 20}; !reflect.DeepEqual(moved, want) {
		t.Fatalf("moved rows = %v, want %v", moved, want)
	}
	for _, thread := range threads {
		got := readRepairView(t, s, thread)
		requireSameLogicalView(t, thread, got, views[thread])
		if want := withSearchSource(got.Search, ThreadSearchSourceItem); !reflect.DeepEqual(got.Search, want) {
			t.Fatalf("%s search rows still on the import arm: %+v", thread, got.Search)
		}
		stamp := historyStamp(t, s, thread)
		if stamp.Rev != stamps[thread].Rev+int64(moved[thread]) || stamp.Epoch != stamps[thread].Epoch {
			t.Fatalf("%s stamp = %+v, was %+v", thread, stamp, stamps[thread])
		}
	}
	requireNoImportedHistory(t, s)
	item, found, err := s.GetThreadItem("whole", "row-005")
	if err != nil || !found || item.Meta != `{"localized":true}` {
		t.Fatalf("localized row = %+v found=%v err=%v", item, found, err)
	}
	if _, found, err := s.GetThreadItem("whole", "row-006"); err != nil || found {
		t.Fatalf("deleted row reappeared: found=%v err=%v", found, err)
	}
	if data, err := s.GetPayloadData("whole", "p-row-007"); err != nil || string(data) != "overlay bytes" {
		t.Fatalf("overlay payload = %q, %v", data, err)
	}
	if data, err := s.GetPayloadData("cut", "p-row-021"); err != nil || string(data) != "payload row-021 appended" {
		t.Fatalf("cut fork payload = %q, %v", data, err)
	}
}

// TestReleaseDetachedSealedChunks: payload snapshots are retired (v120), so
// nothing keeps a sealed chunk after its last reference goes. A copy fork
// that copied part of the chunk holds its own bytes, and deleting the source
// leaves no chunk to release.
func TestReleaseDetachedSealedChunks(t *testing.T) {
	s := newTestStore(t)
	ids := localHistoryFixture(t, s, "src", 20)
	sealItemsForTest(t, s, "src", ids...)
	legacyCopyForkForTest(t, s, "src", "cut", 0)
	before := readRepairView(t, s, "cut")
	if err := s.DeleteThread("src"); err != nil {
		t.Fatal(err)
	}
	requireNoImportedHistory(t, s)

	released, err := s.ReleaseDetachedSealedChunks(context.Background(), nil, failOnSkip(t))
	if err != nil || released != 0 {
		t.Fatalf("released = %d, %v", released, err)
	}
	requireSameRepairView(t, "cut", readRepairView(t, s, "cut"), before)
	if data, err := s.GetPayloadData("cut", "p-row-003"); err != nil || string(data) != "payload row-003 appended" {
		t.Fatalf("cut payload = %q, %v", data, err)
	}
	if released, err := s.ReleaseDetachedSealedChunks(context.Background(), nil, failOnSkip(t)); err != nil || released != 0 {
		t.Fatalf("second release = %d, %v", released, err)
	}
}

// A chunk whose release fails stays, is reported once, and does not stop the
// release of the next chunk.
func TestReleaseDetachedSealedChunksSkipsAFailingChunk(t *testing.T) {
	s := newTestStore(t)
	// Payload snapshots kept a chunk after its last reference before v120.
	// v120 deletes such chunks and collects each chunk with its last
	// reference, so the test drops that collection to leave two behind.
	mustExec(t, s.db, `DROP TRIGGER trg_thread_import_chunks_gc`)
	var chunks []string
	for _, thread := range []string{"a", "b"} {
		ids := localHistoryFixture(t, s, thread, 20)
		chunks = append(chunks, sealItemsForTest(t, s, thread, ids...))
		if err := s.DeleteThread(thread); err != nil {
			t.Fatal(err)
		}
	}
	if n := countRows(t, s, `SELECT count(*) FROM import_history_chunks`); n != 2 {
		t.Fatalf("%d detached chunks, want 2", n)
	}
	failing, other := chunks[0], chunks[1]
	if other < failing {
		failing, other = other, failing
	}
	mustExec(t, s.db, `CREATE TRIGGER fail_release BEFORE DELETE ON import_history_chunks WHEN OLD.id = '`+failing+`' BEGIN SELECT RAISE(ABORT, 'injected'); END`)

	var skipped []error
	released, err := s.ReleaseDetachedSealedChunks(context.Background(), nil, func(err error) { skipped = append(skipped, err) })
	if err != nil || released != 1 {
		t.Fatalf("released = %d, %v; want the other chunk", released, err)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0].Error(), failing) || !strings.Contains(skipped[0].Error(), "injected") {
		t.Fatalf("skipped = %v; want the failing chunk once", skipped)
	}
	if n := countRows(t, s, `SELECT count(*) FROM import_history_chunks WHERE id = ?`, failing); n != 1 {
		t.Fatal("the failing chunk was not left in place")
	}
	if n := countRows(t, s, `SELECT count(*) FROM import_history_chunks WHERE id = ?`, other); n != 0 {
		t.Fatal("the failing chunk stopped the release of the next one")
	}
}

// failOnSkip is the skip callback of a repair that must leave nothing.
func failOnSkip(t *testing.T) func(error) {
	t.Helper()
	return func(err error) { t.Errorf("repair skipped: %v", err) }
}

// leakReplacedPayloads drops v119's replaced-payload collection, so
// repointing an item leaves the old payload behind as it did before v119.
func leakReplacedPayloads(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(`DROP TRIGGER trg_items_gc_replaced_payload; DROP TRIGGER trg_items_gc_replaced_input_payload`); err != nil {
		t.Fatal(err)
	}
}

func countOrphanPayloads(t *testing.T, s *Store) orphanPayloadStats {
	t.Helper()
	var stats orphanPayloadStats
	if err := s.scanOrphanPayloads(context.Background(), func(page []orphanPayload) error {
		for _, row := range page {
			stats.payloads++
			stats.bytes += row.bytes
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return stats
}

// TestPruneOrphanPayloadsDeletesOnlyUnreferenced keeps a payload for every
// kind of reference and deletes rows only when none applies.
func TestPruneOrphanPayloadsDeletesOnlyUnreferenced(t *testing.T) {
	s := newTestStore(t)
	leakReplacedPayloads(t, s)
	if err := s.CreateThread(makeThread("local", "claude")); err != nil {
		t.Fatal(err)
	}
	upsert := func(thread, id, payloadID, inputID string) {
		t.Helper()
		item := Item{ID: id, ThreadID: thread, TurnIndex: 0, Kind: "command_result", Role: "assistant", Status: "completed", Meta: "{}", CreatedAt: 1, UpdatedAt: 1}
		payload := &Payload{ID: payloadID, Kind: "command_output", Meta: "{}", Data: []byte("bytes of " + payloadID), CreatedAt: 1}
		var input *Payload
		if inputID != "" {
			input = &Payload{ID: inputID, Kind: "tool_call_input", Meta: "{}", Data: []byte("input " + inputID), CreatedAt: 1}
		}
		if _, err := s.UpsertItemWithInputPayload(item, payload, input); err != nil {
			t.Fatal(err)
		}
	}
	// Referenced by payload_id and input_payload_id, with append chunks and
	// an edit snapshot of its own.
	upsert("local", "a", "pa", "pa-in")
	if err := s.AppendPayloadData("local", "pa", []byte(" more"), "{}", 2); err != nil {
		t.Fatal(err)
	}
	if err := s.PutEditFileSnapshot("local", "pa", "a.go", "package a", 2); err != nil {
		t.Fatal(err)
	}
	upsert("local", "b", "pb1", "")
	upsert("local", "c", "pc", "")
	// Imported rows reference their chunk payloads and local overlays.
	importedHistoryFixture(t, s, "imported", 2)
	if err := s.ReplacePayloadData("imported", "item-000", []byte("overlay"), "{}", 3); err != nil {
		t.Fatal(err)
	}
	if err := s.CreatePointerFork(makeThread("fork", "claude"), "local", ForkCut{}, testInterruptedSummary, 1); err != nil {
		t.Fatal(err)
	}
	storedBytes := func(thread, id string) int64 {
		t.Helper()
		var n int64
		if err := s.db.QueryRow(`SELECT `+payloadStoredBytesSQL+` FROM payloads p WHERE thread_id=? AND id=?`, thread, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// The leak: a re-persisted row names a new payload id. pb1 carries an
	// append chunk and an edit snapshot that must go with it. The fork reads
	// row b, so the append first gives the fork row b with its own copy of
	// pb1.
	if err := s.AppendPayloadData("local", "pb1", []byte(" chunk"), "{}", 4); err != nil {
		t.Fatal(err)
	}
	if err := s.PutEditFileSnapshot("local", "pb1", "b.go", "package b", 4); err != nil {
		t.Fatal(err)
	}
	orphanBytes := storedBytes("local", "pb1") + storedBytes("local", "pa") + storedBytes("local", "pc") + storedBytes("fork", "pb1")
	upsert("local", "b", "pb2", "")
	// Rewriting a row the fork reads hands the row and its payload to the
	// fork first, so the source's pc is left unreferenced.
	upsert("local", "c", "pc2", "")
	// A fork row stops naming its copy of pb1.
	upsert("fork", "b", "pb3", "")
	// Both sides stop naming pa: the source's rewrite hands the fork row a
	// and a copy of pa, which the fork's rewrite then leaves unreferenced.
	upsert("local", "a", "pa2", "pa-in")
	orphanBytes += storedBytes("fork", "pa")
	upsert("fork", "a", "pa3", "pa-in")

	kept := map[[2]string]string{
		{"local", "pa-in"}: "input pa-in", {"local", "pb2"}: "bytes of pb2", {"local", "pc2"}: "bytes of pc2",
		{"local", "pa2"}: "bytes of pa2",
		{"fork", "pc"}:   "bytes of pc", {"fork", "pa-in"}: "input pa-in",
		{"fork", "pb3"}: "bytes of pb3", {"fork", "pa3"}: "bytes of pa3",
		{"imported", "item-000"}: "overlay", {"imported", "item-001"}: "original chunk",
	}
	orphans := countOrphanPayloads(t, s)
	if orphans != (orphanPayloadStats{payloads: 5, bytes: orphanBytes}) {
		t.Fatalf("orphans = %+v, want 5 payloads, %d bytes", orphans, orphanBytes)
	}

	pruned, err := s.pruneOrphanPayloads(context.Background(), nil, failOnSkip(t))
	if err != nil {
		t.Fatal(err)
	}
	if pruned != orphans {
		t.Fatalf("pruned = %+v, want %+v", pruned, orphans)
	}
	requireCheckpointed(t, s, "prune")
	for _, gone := range [][2]string{{"local", "pb1"}, {"local", "pa"}, {"local", "pc"}, {"fork", "pb1"}, {"fork", "pa"}} {
		if n := countRows(t, s, `SELECT count(*) FROM payloads WHERE thread_id=? AND id=?`, gone[0], gone[1]); n != 0 {
			t.Fatalf("orphan %v survived", gone)
		}
	}
	for key, want := range kept {
		data, err := s.GetPayloadData(key[0], key[1])
		if err != nil || string(data) != want {
			t.Fatalf("kept payload %v = %q, %v; want %q", key, data, err, want)
		}
	}
	for _, table := range []string{"payload_chunks", "edit_file_snapshots"} {
		if n := countRows(t, s, `SELECT count(*) FROM `+table+` WHERE thread_id IN ('local','fork') AND payload_id IN ('pb1','pa')`); n != 0 {
			t.Fatalf("%s kept %d rows of pruned payloads", table, n)
		}
	}
	if orphans := countOrphanPayloads(t, s); orphans != (orphanPayloadStats{}) {
		t.Fatalf("orphans after prune = %+v", orphans)
	}
	if pruned, err := s.pruneOrphanPayloads(context.Background(), nil, failOnSkip(t)); err != nil || pruned != (orphanPayloadStats{}) {
		t.Fatalf("second prune = %+v, %v", pruned, err)
	}
}

// TestItemWritesLeaveNoOrphanPayloads is the tripwire for payload leaks: the
// writers that repoint a row's payload or input payload, on a thread and on
// its fork, leave no payload row that nothing references.
func TestItemWritesLeaveNoOrphanPayloads(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("local", "claude")); err != nil {
		t.Fatal(err)
	}
	upsert := func(thread, id, payloadID, inputID string) Item {
		t.Helper()
		item := Item{ID: id, ThreadID: thread, TurnIndex: 0, Kind: "command_result", Role: "assistant", Status: "completed", Meta: "{}", CreatedAt: 1, UpdatedAt: 1}
		payload := &Payload{ID: payloadID, Kind: "command_output", Meta: "{}", Data: []byte("bytes of " + payloadID), CreatedAt: 1}
		var input *Payload
		if inputID != "" {
			input = &Payload{ID: inputID, Kind: "tool_call_input", Meta: "{}", Data: []byte("input " + inputID), CreatedAt: 1}
		}
		persisted, err := s.UpsertItemWithInputPayload(item, payload, input)
		if err != nil {
			t.Fatal(err)
		}
		return persisted
	}
	upsert("local", "a", "pa", "pa-in")
	if err := s.AppendPayloadData("local", "pa", []byte(" more"), "{}", 2); err != nil {
		t.Fatal(err)
	}
	if err := s.PutEditFileSnapshot("local", "pa", "a.go", "package a", 2); err != nil {
		t.Fatal(err)
	}
	upsert("local", "b", "pb", "pa-in")
	upsert("local", "c", "pc", "c-in")
	if err := s.CreatePointerFork(makeThread("fork", "claude"), "local", ForkCut{}, testInterruptedSummary, 1); err != nil {
		t.Fatal(err)
	}

	upsert("local", "a", "pa2", "pa-in2")
	upsert("fork", "a", "pa3", "")
	upsert("local", "c", "pc", "c-in2")
	b := upsert("local", "b", "pb2", "")
	b.PayloadID = "pa2"
	if _, updated, err := s.UpdateItemIfRevision(b); err != nil || !updated {
		t.Fatalf("conditional repoint = %v, %v", updated, err)
	}

	if orphans := countOrphanPayloads(t, s); orphans != (orphanPayloadStats{}) {
		t.Fatalf("item writes left orphans %+v", orphans)
	}
	for _, gone := range [][2]string{{"local", "pa"}, {"local", "pb"}, {"local", "pb2"}, {"local", "c-in"}, {"fork", "pa"}} {
		if n := countRows(t, s, `SELECT count(*) FROM payloads WHERE thread_id=? AND id=?`, gone[0], gone[1]); n != 0 {
			t.Errorf("replaced payload %v survived", gone)
		}
	}
	for key, want := range map[[2]string]string{
		{"local", "pa2"}: "bytes of pa2", {"local", "pa-in2"}: "input pa-in2", {"local", "pa-in"}: "input pa-in",
		{"fork", "pa3"}: "bytes of pa3", {"fork", "pa-in"}: "input pa-in", {"fork", "pb"}: "bytes of pb",
		{"local", "pc"}: "bytes of pc", {"local", "c-in2"}: "input c-in2", {"fork", "c-in"}: "input c-in",
	} {
		if data, err := s.GetPayloadData(key[0], key[1]); err != nil || string(data) != want {
			t.Errorf("payload %v = %q, %v; want %q", key, data, err, want)
		}
	}
}

// TestPruneOrphanPayloadBatchRechecksReferences keeps a payload that became
// referenced between the scan and the delete.
func TestPruneOrphanPayloadBatchRechecksReferences(t *testing.T) {
	s := newTestStore(t)
	leakReplacedPayloads(t, s)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatal(err)
	}
	item := Item{ID: "a", ThreadID: "t", Kind: "compaction", Role: "system", Status: "completed", Meta: "{}", CreatedAt: 1, UpdatedAt: 1}
	if _, err := s.UpsertItem(item, &Payload{ID: "old", Kind: "compaction", Meta: "{}", Data: []byte("old"), CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertItem(item, &Payload{ID: "new", Kind: "compaction", Meta: "{}", Data: []byte("new"), CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	var scanned []orphanPayload
	if err := s.scanOrphanPayloads(context.Background(), func(page []orphanPayload) error {
		scanned = append(scanned, page...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(scanned) != 1 || scanned[0].id != "old" {
		t.Fatalf("scan = %+v, want the old payload", scanned)
	}
	item.PayloadID = "old"
	if _, err := s.UpsertItem(item, nil); err != nil {
		t.Fatal(err)
	}
	stats, released, err := s.pruneOrphanPayloadBatch(scanned)
	if err != nil || stats != (orphanPayloadStats{}) || len(released) != 0 {
		t.Fatalf("prune of a re-referenced payload = %+v %v %v", stats, released, err)
	}
	if data, err := s.GetPayloadData("t", "old"); err != nil || string(data) != "old" {
		t.Fatalf("re-referenced payload = %q, %v", data, err)
	}
}

// A batch whose delete fails stays, is reported once, and does not stop the
// batches after it.
func TestPruneOrphanPayloadsSkipsAFailingBatch(t *testing.T) {
	s := newTestStore(t)
	leakReplacedPayloads(t, s)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatal(err)
	}
	item := Item{ID: "a", ThreadID: "t", Kind: "compaction", Role: "system", Status: "completed", Meta: "{}", CreatedAt: 1, UpdatedAt: 1}
	// The large payload exceeds the byte budget, so it is a batch of its own.
	for _, payload := range []Payload{
		{ID: "p1", Data: []byte("fails")},
		{ID: "p2", Data: make([]byte, historyRepairBytes+1)},
		{ID: "p3", Data: []byte("live")},
	} {
		payload.Kind, payload.Meta, payload.CreatedAt = "compaction", "{}", 1
		if _, err := s.UpsertItem(item, &payload); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(t, s.db, `CREATE TRIGGER fail_prune BEFORE DELETE ON payloads WHEN OLD.id = 'p1' BEGIN SELECT RAISE(ABORT, 'injected'); END`)

	var skipped []error
	pruned, err := s.pruneOrphanPayloads(context.Background(), nil, func(err error) { skipped = append(skipped, err) })
	if err != nil || pruned.payloads != 1 {
		t.Fatalf("pruned = %+v, %v; want the batch after the failing one", pruned, err)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0].Error(), "t/p1") || !strings.Contains(skipped[0].Error(), "injected") {
		t.Fatalf("skipped = %v; want the failing batch once", skipped)
	}
	for id, want := range map[string]int{"p1": 1, "p2": 0, "p3": 1} {
		if n := countRows(t, s, `SELECT count(*) FROM payloads WHERE thread_id='t' AND id=?`, id); n != want {
			t.Errorf("payload %s rows = %d, want %d", id, n, want)
		}
	}
}

func TestPruneOrphanPayloadsStopsOnCancel(t *testing.T) {
	s := newTestStore(t)
	leakReplacedPayloads(t, s)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatal(err)
	}
	item := Item{ID: "a", ThreadID: "t", Kind: "compaction", Role: "system", Status: "completed", Meta: "{}", CreatedAt: 1, UpdatedAt: 1}
	for i := 0; i <= historyRepairRows+1; i++ {
		if _, err := s.UpsertItem(item, &Payload{ID: fmt.Sprintf("p%03d", i), Kind: "compaction", Meta: "{}", Data: []byte("x"), CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	pruned, err := s.pruneOrphanPayloads(ctx, ChunkPause(cancel), failOnSkip(t))
	if err != nil || pruned.payloads != historyRepairRows {
		t.Fatalf("cancelled prune = %+v, %v; want one batch of %d", pruned, err, historyRepairRows)
	}
	if n := countRows(t, s, `SELECT count(*) FROM payloads WHERE thread_id='t'`); n != 2 {
		t.Fatalf("%d payloads left, want the live one and one orphan", n)
	}
}
