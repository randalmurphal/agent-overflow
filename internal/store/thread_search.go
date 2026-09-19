package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"agent-overflow/internal/threadmode"
)

// Index arms. `item` is a row of `items`; `import` is a row of shared
// imported history attached to the thread.
const (
	ThreadSearchSourceItem   = "item"
	ThreadSearchSourceImport = "import"
)

// Indexed kinds, as the tool's `kind` filter names them.
const (
	ThreadSearchKindUser      = "user"
	ThreadSearchKindAssistant = "assistant"
	ThreadSearchKindTool      = "tool"
	ThreadSearchKindTitle     = "title"
)

var threadSearchKinds = map[string]struct{}{
	ThreadSearchKindUser: {}, ThreadSearchKindAssistant: {},
	ThreadSearchKindTool: {}, ThreadSearchKindTitle: {},
}

// threadSearchItemKinds maps the item kinds that are indexed to the kind the
// index records. Thinking is deliberately absent: `items.summary` holds only
// its tail, and the full text is one thread_show away.
var threadSearchItemKinds = map[string]string{
	"user_text":      ThreadSearchKindUser,
	"assistant_text": ThreadSearchKindAssistant,
	"tool_call":      ThreadSearchKindTool,
}

// threadSearchBuildBatch is how many rows one build transaction claims before
// it commits and pauses. Small enough that a 38k-item thread never holds the
// writer for long, large enough that the walk is not all transaction overhead.
const threadSearchBuildBatch = 500

// threadSearchBuildPause is the gap between build batches. The build is
// background work behind every live write.
const threadSearchBuildPause = 20 * time.Millisecond

// threadSearchSnippetBudget is the default snippet width in runes.
const threadSearchSnippetBudget = 240

// ThreadSearchHit is one ranked match. Snippet is produced in Go from the
// source text, because a contentless FTS5 table cannot return one.
type ThreadSearchHit struct {
	ThreadID string `json:"threadId"`
	// ItemID is empty for a title hit.
	ItemID  string `json:"itemId,omitempty"`
	Source  string `json:"source"`
	Kind    string `json:"kind"`
	Snippet string `json:"snippet"`
	// Rank is bm25, lower is better. Ranks are comparable only within one
	// computer's database.
	Rank float64 `json:"rank"`
}

// ThreadSearchFilter narrows a search. The zero value searches every thread
// this computer owns except the hidden modes.
//
// ListThreadsByActivity takes the same filter for the query-less listing, so
// a row a search would refuse is a row the listing refuses. Kinds and
// SnippetBudget describe indexed item rows and mean nothing to the listing,
// which ignores them.
type ThreadSearchFilter struct {
	// ThreadIDs restricts the search to these threads.
	ThreadIDs []string
	ProjectID string
	// Provider restricts to threads running on one provider.
	Provider string
	// Archived is nil for "either"; a non-nil value restricts to archived
	// or to unarchived threads.
	Archived *bool
	// SinceUnixMs restricts to threads whose last activity is at or after
	// it. Last activity is the latest completed turn, or updated_at for a
	// thread that has never completed one.
	SinceUnixMs int64
	// SpawnedBy restricts to the threads one caller thread spawned: the
	// targets of its `spawn` rows in the request ledger.
	SpawnedBy string
	// Kinds restricts to user / assistant / tool / title rows.
	Kinds []string
	// ScratchThreadIDs are the scratch threads the caller may see. Scratch
	// is a hidden mode, so a scratch thread is invisible to a search unless
	// its id is named here.
	ScratchThreadIDs []string
	Limit            int
	Offset           int
	// SnippetBudget is the snippet width in runes; zero takes the default.
	SnippetBudget int
}

// threadLastActivityExpr is the thread clock both the search filter and the
// listing order read: the newest completed turn, falling back to updated_at
// for a thread that has never completed one. `completed_at IS NOT NULL` is
// stated so SQLite can use the partial idx_turns_thread_completed, and
// NULLIF keeps the fallback identical to the Go projection, which treats a
// zero stamp as no completed turn.
//
// alias is the qualified thread-row prefix ("t." or "threads.").
func threadLastActivityExpr(alias string) string {
	return `COALESCE(NULLIF((SELECT MAX(completed_at) FROM turns
		             WHERE turns.thread_id = ` + alias + `id AND completed_at IS NOT NULL), 0),
		   ` + alias + `updated_at)`
}

// threadRowConditions renders the predicates that narrow a thread ROW, for
// the ranked search and the listing alike. Visibility comes first: hidden
// modes are out unless the caller named that scratch thread, which is the
// rule that keeps another agent's side chat out of both answers.
//
// It does not render the ThreadIDs restriction: the search applies that to
// the index row it already has in hand, the listing to the thread id.
func (f ThreadSearchFilter) threadRowConditions(alias string) ([]string, []any) {
	var conditions []string
	var args []any

	hiddenClause, hiddenArgs := hiddenThreadModesClause(alias + "mode")
	if len(f.ScratchThreadIDs) > 0 {
		scratchClause, scratchArgs := inClause(alias+"id", f.ScratchThreadIDs)
		conditions = append(conditions, "("+hiddenClause+" OR ("+alias+"mode = ? AND "+scratchClause+"))")
		args = append(args, hiddenArgs...)
		args = append(args, threadmode.ModeScratch)
		args = append(args, scratchArgs...)
	} else {
		conditions = append(conditions, hiddenClause)
		args = append(args, hiddenArgs...)
	}

	if f.ProjectID != "" {
		conditions = append(conditions, alias+"project_id = ?")
		args = append(args, f.ProjectID)
	}
	if f.Provider != "" {
		conditions = append(conditions, alias+"provider = ?")
		args = append(args, f.Provider)
	}
	if f.Archived != nil {
		archived := 0
		if *f.Archived {
			archived = 1
		}
		conditions = append(conditions, alias+"archived = ?")
		args = append(args, archived)
	}
	if f.SinceUnixMs > 0 {
		conditions = append(conditions, threadLastActivityExpr(alias)+" >= ?")
		args = append(args, f.SinceUnixMs)
	}
	if f.SpawnedBy != "" {
		conditions = append(conditions, `EXISTS (SELECT 1 FROM thread_requests spawns
		            WHERE spawns.caller_thread_id = ?
		              AND spawns.kind = ?
		              AND spawns.target_thread_id = `+alias+`id)`)
		args = append(args, f.SpawnedBy, ThreadRequestSpawn)
	}
	return conditions, args
}

// SearchThreads runs one FTS5 query over settled message text, tool-call
// summaries and thread titles, ranked by bm25.
//
// `query` is FTS5 match syntax, so a malformed query comes back as an error
// for the caller to report rather than as an empty result. Hidden-mode threads
// are excluded by joining `owned_threads` at query time: a thread moved to
// another computer stops matching without a reindex, and so does a scratch
// thread the caller does not own.
//
// Every filter is applied in SQL, so LIMIT and OFFSET count the rows the
// caller receives. A caller that drops rows of its own can no longer page
// by the offset it passed in.
func (s *Store) SearchThreads(query string, filter ThreadSearchFilter) ([]ThreadSearchHit, error) {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return nil, errors.New("store: search threads: empty query")
	}
	if err := s.checkSearchQuery(trimmed); err != nil {
		return nil, err
	}
	limit := filter.Limit
	if limit < 1 {
		limit = 20
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}

	conditions := []string{"thread_search MATCH ?"}
	args := []any{trimmed}

	rowConditions, rowArgs := filter.threadRowConditions("t.")
	conditions = append(conditions, rowConditions...)
	args = append(args, rowArgs...)

	if len(filter.ThreadIDs) > 0 {
		clause, clauseArgs := inClause("r.thread_id", filter.ThreadIDs)
		conditions = append(conditions, clause)
		args = append(args, clauseArgs...)
	}
	if len(filter.Kinds) > 0 {
		for _, kind := range filter.Kinds {
			if _, ok := threadSearchKinds[kind]; !ok {
				return nil, fmt.Errorf("store: search threads: invalid kind %q", kind)
			}
		}
		clause, clauseArgs := inClause("r.kind", filter.Kinds)
		conditions = append(conditions, clause)
		args = append(args, clauseArgs...)
	}
	args = append(args, limit, offset)

	rows, err := s.reader().Query(
		`SELECT r.thread_id, r.item_id, r.source, r.kind, bm25(thread_search),
		        CASE WHEN r.kind = 'title' THEN t.title
		             ELSE COALESCE((SELECT logical.summary FROM timeline_items logical
		                             WHERE logical.thread_id = r.thread_id AND logical.id = r.item_id), '')
		        END
		   FROM thread_search
		   JOIN thread_search_rows r ON r.rowid = thread_search.rowid
		   JOIN owned_threads t ON t.id = r.thread_id
		  WHERE `+strings.Join(conditions, " AND ")+`
		  ORDER BY bm25(thread_search) ASC, r.rowid ASC
		  LIMIT ? OFFSET ?`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: search threads: %w", err)
	}
	defer rows.Close()

	budget := filter.SnippetBudget
	if budget < 1 {
		budget = threadSearchSnippetBudget
	}
	terms := searchQueryTerms(trimmed)
	hits := []ThreadSearchHit{}
	for rows.Next() {
		var hit ThreadSearchHit
		var text string
		if err := rows.Scan(&hit.ThreadID, &hit.ItemID, &hit.Source, &hit.Kind, &hit.Rank, &text); err != nil {
			return nil, fmt.Errorf("store: scan thread search hit: %w", err)
		}
		hit.Snippet = searchSnippet(text, terms, budget)
		hits = append(hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate thread search hits: %w", err)
	}
	return hits, nil
}

// checkSearchQuery reports a malformed match expression before the ranked
// query runs. fts5 raises a syntax error only when the expression actually
// reaches the index, and the ranked query joins two ordinary tables first, so
// a plan that finds no candidate rows would return "no matches" for a query
// the caller needs told is invalid. The probe stops at the first match.
func (s *Store) checkSearchQuery(match string) error {
	var probe int
	err := s.reader().QueryRow(
		`SELECT 1 FROM thread_search WHERE thread_search MATCH ? LIMIT 1`, match).Scan(&probe)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: search threads: %w", err)
	}
	return nil
}

// SearchIndexing reports whether the background build still has work. Callers
// flag a result as partial while it is true.
func (s *Store) SearchIndexing() (bool, error) {
	var building bool
	if err := s.reader().QueryRow(`SELECT EXISTS(SELECT 1 FROM thread_search_build WHERE id = 1)`).Scan(&building); err != nil {
		return false, fmt.Errorf("store: probe thread search build: %w", err)
	}
	return building, nil
}

// BuildSearchIndex walks the existing corpus into the index and deletes the
// progress row when it is done. It is safe to call on an index that is
// already complete (it returns at once) and safe to interrupt: the cursor
// advances only after a batch commits, so a restart resumes where the last
// committed batch ended.
//
// Rows that are still streaming are skipped — their settle hook indexes them
// later — and every insert is INSERT OR IGNORE on the unique key, so a row
// that settled during the build is indexed exactly once.
func (s *Store) BuildSearchIndex(ctx context.Context) error {
	for {
		progress, found, err := s.searchBuildProgress()
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		var done bool
		switch {
		case progress.itemsDone:
			done, err = s.buildSearchIndexTail(progress)
		default:
			done, err = s.buildSearchIndexItems(progress)
		}
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(threadSearchBuildPause):
		}
	}
}

type searchBuildProgress struct {
	cursorThreadID string
	cursorItemID   string
	itemsDone      bool
	importsDone    bool
	titlesDone     bool
}

// searchBuildProgress reads the progress row. The items pass is finished when
// the cursor has been parked at the sentinel below.
const searchBuildItemsDoneCursor = "￿"

func (s *Store) searchBuildProgress() (searchBuildProgress, bool, error) {
	var p searchBuildProgress
	var imports, titles int
	err := s.reader().QueryRow(
		`SELECT cursor_thread_id, cursor_item_id, imports_done, titles_done
		   FROM thread_search_build WHERE id = 1`,
	).Scan(&p.cursorThreadID, &p.cursorItemID, &imports, &titles)
	if errors.Is(err, sql.ErrNoRows) {
		return searchBuildProgress{}, false, nil
	}
	if err != nil {
		return searchBuildProgress{}, false, fmt.Errorf("store: read thread search build progress: %w", err)
	}
	p.importsDone = imports != 0
	p.titlesDone = titles != 0
	p.itemsDone = p.cursorThreadID == searchBuildItemsDoneCursor
	return p, true, nil
}

// buildSearchIndexItems indexes one batch of `items` in (thread_id, id) order
// and advances the cursor in the same transaction. It reports done=false while
// the items pass has more to do.
func (s *Store) buildSearchIndexItems(progress searchBuildProgress) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("store: begin thread search build batch: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(
		`SELECT thread_id, id, kind, status, summary
		   FROM items
		  WHERE (thread_id, id) > (?, ?)
		  ORDER BY thread_id ASC, id ASC
		  LIMIT ?`,
		progress.cursorThreadID, progress.cursorItemID, threadSearchBuildBatch,
	)
	if err != nil {
		return false, fmt.Errorf("store: read thread search build batch: %w", err)
	}
	type indexable struct{ threadID, itemID, kind, status, summary string }
	batch := make([]indexable, 0, threadSearchBuildBatch)
	for rows.Next() {
		var row indexable
		if err := rows.Scan(&row.threadID, &row.itemID, &row.kind, &row.status, &row.summary); err != nil {
			rows.Close()
			return false, fmt.Errorf("store: scan thread search build batch: %w", err)
		}
		batch = append(batch, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, fmt.Errorf("store: iterate thread search build batch: %w", err)
	}
	rows.Close()

	if len(batch) == 0 {
		if err := advanceSearchBuildCursorTx(tx, searchBuildItemsDoneCursor, ""); err != nil {
			return false, err
		}
		return false, commitSearchBuildBatch(tx)
	}

	for _, row := range batch {
		kind, indexed := threadSearchItemKinds[row.kind]
		if !indexed || !settledItemStatus(row.status) {
			continue
		}
		if _, err := buildThreadSearchRowTx(tx, row.threadID, row.itemID, ThreadSearchSourceItem, kind, row.summary); err != nil {
			return false, err
		}
	}
	last := batch[len(batch)-1]
	if err := advanceSearchBuildCursorTx(tx, last.threadID, last.itemID); err != nil {
		return false, err
	}
	return false, commitSearchBuildBatch(tx)
}

// buildSearchIndexTail runs the two passes after items — imported history,
// then titles — and finishes by sweeping mapping rows whose thread or item is
// gone and deleting the progress row. Each pass is one transaction: both are
// bounded by the number of attached chunks and threads, not by history size.
func (s *Store) buildSearchIndexTail(progress searchBuildProgress) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("store: begin thread search build pass: %w", err)
	}
	defer tx.Rollback()

	switch {
	case !progress.importsDone:
		if err := indexImportedHistoryTx(tx, "", ""); err != nil {
			return false, err
		}
		if _, err := tx.Exec(`UPDATE thread_search_build SET imports_done = 1 WHERE id = 1`); err != nil {
			return false, fmt.Errorf("store: mark thread search imports done: %w", err)
		}
		return false, commitSearchBuildBatch(tx)
	case !progress.titlesDone:
		if err := indexThreadTitlesTx(tx); err != nil {
			return false, err
		}
		if _, err := tx.Exec(`UPDATE thread_search_build SET titles_done = 1 WHERE id = 1`); err != nil {
			return false, fmt.Errorf("store: mark thread search titles done: %w", err)
		}
		return false, commitSearchBuildBatch(tx)
	}

	if err := sweepThreadSearchOrphansTx(tx); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`DELETE FROM thread_search_build WHERE id = 1`); err != nil {
		return false, fmt.Errorf("store: clear thread search build progress: %w", err)
	}
	return true, commitSearchBuildBatch(tx)
}

func commitSearchBuildBatch(tx *sql.Tx) error {
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit thread search build batch: %w", err)
	}
	return nil
}

func advanceSearchBuildCursorTx(tx *sql.Tx, threadID, itemID string) error {
	result, err := tx.Exec(
		`UPDATE thread_search_build SET cursor_thread_id = ?, cursor_item_id = ? WHERE id = 1`,
		threadID, itemID,
	)
	if err != nil {
		return fmt.Errorf("store: advance thread search build cursor: %w", err)
	}
	return requireRowsAffected(result, "store: advance thread search build cursor")
}

// indexThreadTitlesTx indexes every thread title that is not indexed yet.
func indexThreadTitlesTx(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT id, title FROM threads`)
	if err != nil {
		return fmt.Errorf("store: read thread titles for search build: %w", err)
	}
	type titleRow struct{ id, title string }
	titles := []titleRow{}
	for rows.Next() {
		var row titleRow
		if err := rows.Scan(&row.id, &row.title); err != nil {
			rows.Close()
			return fmt.Errorf("store: scan thread title for search build: %w", err)
		}
		titles = append(titles, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("store: iterate thread titles for search build: %w", err)
	}
	rows.Close()
	for _, row := range titles {
		if _, err := buildThreadSearchRowTx(tx, row.id, "", ThreadSearchSourceItem, ThreadSearchKindTitle, row.title); err != nil {
			return err
		}
	}
	return nil
}

// sweepThreadSearchOrphansTx removes mapping rows whose item is gone, with the
// FTS rows they name. The FTS table is contentless and cannot be enumerated,
// so the mapping table is the side that is swept; an FTS row whose mapping is
// already gone can never be returned by a search, because every hit joins the
// mapping row.
func sweepThreadSearchOrphansTx(tx *sql.Tx) error {
	rows, err := tx.Query(
		`SELECT r.rowid FROM thread_search_rows r
		  WHERE r.item_id <> ''
		    AND NOT EXISTS (SELECT 1 FROM timeline_items logical
		                     WHERE logical.thread_id = r.thread_id AND logical.id = r.item_id)`)
	if err != nil {
		return fmt.Errorf("store: read thread search orphans: %w", err)
	}
	var rowids []int64
	for rows.Next() {
		var rowid int64
		if err := rows.Scan(&rowid); err != nil {
			rows.Close()
			return fmt.Errorf("store: scan thread search orphan: %w", err)
		}
		rowids = append(rowids, rowid)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("store: iterate thread search orphans: %w", err)
	}
	rows.Close()
	for _, rowid := range rowids {
		if err := deleteThreadSearchRowidTx(tx, rowid); err != nil {
			return err
		}
	}
	return nil
}

// settledItemStatus reports whether an item's text is final. Streaming and
// running rows are indexed by their settle hook, never per append: an update
// per chunk would re-tokenize the whole row at streaming rate.
func settledItemStatus(status string) bool {
	return status != "streaming" && status != "running"
}

// indexSettledItemTx is the single settle-time hook. It indexes one item's
// text when the row is settled and its kind is indexed, and does nothing
// otherwise, so every item write path can call it without deciding anything.
func indexSettledItemTx(tx *sql.Tx, threadID, itemID, itemKind, status, summary string) error {
	kind, indexed := threadSearchItemKinds[itemKind]
	if !indexed || !settledItemStatus(status) {
		return nil
	}
	return indexThreadSearchRowTx(tx, threadID, itemID, ThreadSearchSourceItem, kind, summary)
}

// indexItemByIDTx re-reads one row and indexes it through the settle hook.
// Partial updates that can settle a row use it, since they never carry the
// whole row.
func indexItemByIDTx(tx *sql.Tx, threadID, itemID string) error {
	var kind, status, summary string
	if err := tx.QueryRow(
		`SELECT kind, status, summary FROM items WHERE thread_id = ? AND id = ?`, threadID, itemID,
	).Scan(&kind, &status, &summary); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("store: read item %s/%s for search index: %w", threadID, itemID, err)
	}
	return indexSettledItemTx(tx, threadID, itemID, kind, status, summary)
}

// indexThreadSearchRowTx replaces the indexed text for one (thread, item,
// source). It is the write side of every settle hook: the mapping row is
// upserted for its rowid, the old FTS row for that rowid is deleted, and the
// new text is inserted under the same rowid.
func indexThreadSearchRowTx(tx *sql.Tx, threadID, itemID, source, kind, text string) error {
	var rowid int64
	if err := tx.QueryRow(
		`INSERT INTO thread_search_rows (thread_id, item_id, source, kind)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(thread_id, item_id, source) DO UPDATE SET kind = excluded.kind
		 RETURNING rowid`,
		threadID, itemID, source, kind,
	).Scan(&rowid); err != nil {
		return fmt.Errorf("store: index search row %s/%s: %w", threadID, itemID, err)
	}
	return writeThreadSearchTextTx(tx, rowid, text, threadID, itemID)
}

// buildThreadSearchRowTx is the background build's insert: a row already
// indexed is left alone, so a row that settled during the build keeps the text
// its settle hook wrote. It reports whether it indexed anything.
func buildThreadSearchRowTx(tx *sql.Tx, threadID, itemID, source, kind, text string) (bool, error) {
	result, err := tx.Exec(
		`INSERT OR IGNORE INTO thread_search_rows (thread_id, item_id, source, kind) VALUES (?, ?, ?, ?)`,
		threadID, itemID, source, kind,
	)
	if err != nil {
		return false, fmt.Errorf("store: build search row %s/%s: %w", threadID, itemID, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: count built search row %s/%s: %w", threadID, itemID, err)
	}
	if inserted == 0 {
		return false, nil
	}
	rowid, err := result.LastInsertId()
	if err != nil {
		return false, fmt.Errorf("store: read built search rowid %s/%s: %w", threadID, itemID, err)
	}
	return true, writeThreadSearchTextTx(tx, rowid, text, threadID, itemID)
}

// writeThreadSearchTextTx writes one FTS row, deleting whatever sat at that
// rowid first. The delete is not optional even for a freshly inserted mapping
// row: mapping rowids are reused after a delete, and a stale FTS row under a
// reused rowid would answer for a message it never held.
func writeThreadSearchTextTx(tx *sql.Tx, rowid int64, text, threadID, itemID string) error {
	if _, err := tx.Exec(`DELETE FROM thread_search WHERE rowid = ?`, rowid); err != nil {
		return fmt.Errorf("store: clear search text %s/%s: %w", threadID, itemID, err)
	}
	if _, err := tx.Exec(`INSERT INTO thread_search (rowid, text) VALUES (?, ?)`, rowid, text); err != nil {
		return fmt.Errorf("store: write search text %s/%s: %w", threadID, itemID, err)
	}
	return nil
}

func deleteThreadSearchRowidTx(tx *sql.Tx, rowid int64) error {
	if _, err := tx.Exec(`DELETE FROM thread_search WHERE rowid = ?`, rowid); err != nil {
		return fmt.Errorf("store: delete search text %d: %w", rowid, err)
	}
	if _, err := tx.Exec(`DELETE FROM thread_search_rows WHERE rowid = ?`, rowid); err != nil {
		return fmt.Errorf("store: delete search row %d: %w", rowid, err)
	}
	return nil
}

// deleteThreadSearchItemsTx removes every index row for the named items, on
// both arms. Item deletion, conversation truncation and the paced thread
// delete all call it with the ids they removed.
func deleteThreadSearchItemsTx(tx *sql.Tx, threadID string, itemIDs []string) error {
	if len(itemIDs) == 0 {
		return nil
	}
	clause, args := inClause("item_id", itemIDs)
	return deleteThreadSearchWhereTx(tx, "thread_id = ? AND "+clause, append([]any{threadID}, args...))
}

// deleteItemsAndSearchRowsTx runs an item DELETE that ends in `RETURNING id`
// and removes the index rows of exactly what it deleted, returning the row
// count. Naming the deleted ids keeps the index delete from having to restate
// the delete's predicate, which on the item-granular revert is not a
// predicate anything else should own a second copy of.
func deleteItemsAndSearchRowsTx(tx *sql.Tx, threadID, query string, args []any, action string) (int64, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", action, err)
	}
	var deleted []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("%s: scan deleted id: %w", action, err)
		}
		deleted = append(deleted, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("%s: iterate deleted ids: %w", action, err)
	}
	rows.Close()
	if err := deleteThreadSearchItemsTx(tx, threadID, deleted); err != nil {
		return 0, err
	}
	return int64(len(deleted)), nil
}

// deleteThreadSearchSourceTx removes one thread's index rows for one arm.
// Detaching imported history uses it.
func deleteThreadSearchSourceTx(tx *sql.Tx, threadID, source string) error {
	return deleteThreadSearchWhereTx(tx, "thread_id = ? AND source = ?", []any{threadID, source})
}

// deleteThreadSearchThreadTx removes every index row for one thread,
// title included. The paced thread delete calls it before the thread row goes,
// so the FTS side is emptied rather than left to the mapping table's cascade.
func deleteThreadSearchThreadTx(tx *sql.Tx, threadID string) error {
	return deleteThreadSearchWhereTx(tx, "thread_id = ?", []any{threadID})
}

func deleteThreadSearchWhereTx(tx *sql.Tx, where string, args []any) error {
	if _, err := tx.Exec(
		`DELETE FROM thread_search WHERE rowid IN (SELECT rowid FROM thread_search_rows WHERE `+where+`)`,
		args...,
	); err != nil {
		return fmt.Errorf("store: delete search text: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM thread_search_rows WHERE `+where, args...); err != nil {
		return fmt.Errorf("store: delete search rows: %w", err)
	}
	return nil
}

// indexImportedHistoryTx indexes imported rows on the `import` arm. An empty
// threadID and chunkID index everything attached anywhere, which is the
// background build's pass; naming both indexes exactly the chunk an import
// just attached, so a long import does not rescan what it already indexed.
// Rows an override shadows are skipped: the local item carries the text and is
// indexed on the `item` arm.
func indexImportedHistoryTx(tx *sql.Tx, threadID, chunkID string) error {
	where := ""
	args := []any{}
	if threadID != "" {
		where += " AND refs.thread_id = ?"
		args = append(args, threadID)
	}
	if chunkID != "" {
		where += " AND refs.chunk_id = ?"
		args = append(args, chunkID)
	}
	rows, err := tx.Query(
		`SELECT refs.thread_id, imported.id, imported.kind, imported.status, imported.summary
		   FROM thread_import_chunks refs
		   JOIN import_history_items imported ON imported.chunk_id = refs.chunk_id
		   LEFT JOIN thread_import_item_overrides overrides
		     ON overrides.thread_id = refs.thread_id AND overrides.item_id = imported.id
		  WHERE overrides.item_id IS NULL`+where, args...)
	if err != nil {
		return fmt.Errorf("store: read imported history for search: %w", err)
	}
	type importedRow struct{ threadID, itemID, kind, status, summary string }
	imported := []importedRow{}
	for rows.Next() {
		var row importedRow
		if err := rows.Scan(&row.threadID, &row.itemID, &row.kind, &row.status, &row.summary); err != nil {
			rows.Close()
			return fmt.Errorf("store: scan imported history for search: %w", err)
		}
		imported = append(imported, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("store: iterate imported history for search: %w", err)
	}
	rows.Close()
	for _, row := range imported {
		kind, indexed := threadSearchItemKinds[row.kind]
		if !indexed || !settledItemStatus(row.status) {
			continue
		}
		if _, err := buildThreadSearchRowTx(tx, row.threadID, row.itemID, ThreadSearchSourceImport, kind, row.summary); err != nil {
			return err
		}
	}
	return nil
}

// indexThreadItemsTx indexes every settled local item of one thread, leaving
// rows already indexed alone. Materializing shared history calls it: the rows
// it just copied into `items` carry the text the detached import arm held.
func indexThreadItemsTx(tx *sql.Tx, threadID string) error {
	rows, err := tx.Query(
		`SELECT id, kind, status, summary FROM items WHERE thread_id = ?`, threadID)
	if err != nil {
		return fmt.Errorf("store: read thread %s items for search: %w", threadID, err)
	}
	type localRow struct{ itemID, kind, status, summary string }
	local := []localRow{}
	for rows.Next() {
		var row localRow
		if err := rows.Scan(&row.itemID, &row.kind, &row.status, &row.summary); err != nil {
			rows.Close()
			return fmt.Errorf("store: scan thread %s item for search: %w", threadID, err)
		}
		local = append(local, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("store: iterate thread %s items for search: %w", threadID, err)
	}
	rows.Close()
	for _, row := range local {
		kind, indexed := threadSearchItemKinds[row.kind]
		if !indexed || !settledItemStatus(row.status) {
			continue
		}
		if _, err := buildThreadSearchRowTx(tx, threadID, row.itemID, ThreadSearchSourceItem, kind, row.summary); err != nil {
			return err
		}
	}
	return nil
}

// indexThreadTitleTx indexes one thread's title. A title row has an empty item
// id, so it never collides with a message.
func indexThreadTitleTx(tx *sql.Tx, threadID, title string) error {
	return indexThreadSearchRowTx(tx, threadID, "", ThreadSearchSourceItem, ThreadSearchKindTitle, title)
}

// inClause renders `column IN (?, ?, ...)` with its bind arguments.
func inClause(column string, values []string) (string, []any) {
	args := make([]any, len(values))
	for i, value := range values {
		args[i] = value
	}
	return column + " IN (" + strings.TrimRight(strings.Repeat("?,", len(values)), ",") + ")", args
}

// searchQueryTerms pulls the literal words out of an FTS5 match expression so
// a snippet can be anchored on one. Operators, column filters and quoting are
// dropped; a trailing `*` is kept off the term so the prefix still matches.
func searchQueryTerms(query string) []string {
	var terms []string
	var current strings.Builder
	flush := func() {
		if current.Len() == 0 {
			return
		}
		term := strings.ToLower(current.String())
		current.Reset()
		switch term {
		case "and", "or", "not", "near":
			return
		}
		terms = append(terms, term)
	}
	for _, r := range query {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
			current.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return terms
}

// searchSnippet returns a window of text around the first matched term, with
// an ellipsis on each side that was cut. A text with no term in it (a hit on
// a different arm of an OR, a term the tokenizer folded) falls back to the
// head of the text, which is still the most useful thing to show.
func searchSnippet(text string, terms []string, budget int) string {
	runes := []rune(text)
	if len(runes) <= budget {
		return text
	}
	lowered := strings.ToLower(text)
	match := -1
	for _, term := range terms {
		if term == "" {
			continue
		}
		if at := strings.Index(lowered, term); at >= 0 && (match < 0 || at < match) {
			match = at
		}
	}
	if match < 0 {
		return string(runes[:budget]) + "…"
	}
	// Convert the byte offset to a rune offset and centre the window on it.
	matchRune := len([]rune(text[:match]))
	start := matchRune - budget/3
	if start < 0 {
		start = 0
	}
	end := start + budget
	if end > len(runes) {
		end = len(runes)
		start = end - budget
		if start < 0 {
			start = 0
		}
	}
	snippet := string(runes[start:end])
	if start > 0 {
		snippet = "…" + snippet
	}
	if end < len(runes) {
		snippet += "…"
	}
	return snippet
}
