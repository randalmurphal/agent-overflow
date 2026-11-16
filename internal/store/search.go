package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ThreadMessageHit is one hit from a message search. The result set is
// designed for an interactive UI: it pairs the thread's display title with the
// specific item's kind + summary so users can jump directly to the matching
// turn.
type ThreadMessageHit struct {
	OwnershipEpoch int64  `json:"ownershipEpoch"`
	ThreadID       string `json:"threadId"`
	ThreadTitle    string `json:"threadTitle"`
	Provider       string `json:"provider"`
	ItemID         string `json:"itemId"`
	TurnIndex      int    `json:"turnIndex"`
	// ItemIndex with TurnIndex is the item's timeline coordinate, so a
	// reader's position orders against a hit.
	ItemIndex int    `json:"itemIndex"`
	ItemKind  string `json:"itemKind"`
	ItemRole  string `json:"itemRole"`
	// Summary is the item's stored summary field. The frontend can highlight
	// the query within it.
	Summary string `json:"summary"`
	// MatchType reports whether the hit came from the thread title or from
	// a specific item's summary. Both are bundled into one result set so
	// callers can render a unified list.
	MatchType string `json:"matchType"` // "title" or "item"
}

// SearchThreadMessages returns hits that match the query across thread titles
// and item summaries. Matching is case-insensitive substring via SQLite's
// LOWER + LIKE — intentionally simple for now; a future migration can swap
// to an FTS5 virtual table without changing the return shape. The search
// modal highlights by the same rule (likePattern; frontend
// utils/highlight.ts searchHighlightSegments), held to it by
// searchVectors.json.
//
// limit caps the total number of hits returned. Zero or negative is treated
// as an unbounded search (callers should always pass a small positive int
// in production; tests may leave it at zero to validate exhaustive scans).
//
// Title hits and item hits are computed by two SEPARATE bounded queries and
// then merged title-first. This is deliberate: a single LEFT JOIN labels every
// item of a title-matching thread as a 'title' row, so a busy titled thread
// (more items than the limit) would fill the entire result window and starve
// out item-summary matches in other threads.
//
// Titles still rank first — a matching title is a stronger signal of intent
// than a stray word in a long message — but mergeTitleFirst reserves a share
// of the limit for item hits, so a query that ALSO matches many thread titles
// can't hide every message-body match (the same starvation, one level up).
// Within each kind, titles are most-recently-active first, items newest-first.
//
// Deleted threads are naturally excluded: the item query inner-joins threads,
// and the FK cascade removes a thread's items when it is deleted, so no orphan
// hits can surface.
//
// It reads every thread's history, so it takes a history read slot.
func (s *Store) SearchThreadMessages(query string, limit int) ([]ThreadMessageHit, error) {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return nil, nil
	}
	pattern := likePattern(trimmed)
	return historyReadSnapshot(context.Background(), s, "search threads", func(q sqlQueryer) ([]ThreadMessageHit, error) {
		titleHits, err := searchTitleHits(q, pattern, limit)
		if err != nil {
			return nil, err
		}
		itemHits, err := searchGlobalItemHits(q, pattern, limit)
		if err != nil {
			return nil, err
		}
		return mergeTitleFirst(titleHits, itemHits, limit), nil
	})
}

// ThreadFindSince is the read a thread find answer made and the state of
// the transcript it read. A refresh passes it back to re-read only what
// changed; the read is incremental only when it is the same read of a
// later state.
type ThreadFindSince struct {
	ThreadID string `json:"threadId"`
	// Needle is the query's loose form (findNeedle).
	Needle string `json:"needle"`
	// Selection is the transcript read, as resolved.
	Selection TimelineSelection `json:"selection"`
	Limit     int               `json:"limit"`
	// Capped is set when the answer stopped at its limit: the rows after
	// it are unknown, so a refresh reads in full.
	Capped bool `json:"capped"`
	// Generation is the store's replica generation, which a restore
	// re-mints.
	Generation string `json:"generation"`
	// Rev and Epoch are the thread's history_rev and history_epoch.
	Rev   int64 `json:"rev"`
	Epoch int64 `json:"epoch"`
	// ImportChunks and ImportOrder are the count and the last order of the
	// imported history chunks the thread reads.
	ImportChunks int64 `json:"importChunks"`
	ImportOrder  int64 `json:"importOrder"`
}

// ThreadFindAnswer is a thread find read: the rows of one transcript that
// may hold a match, as timeline pages serve them. The frontend decides
// which hold one, and where, by the find rule over each row's find text.
type ThreadFindAnswer struct {
	// Since is the state the answer read.
	Since ThreadFindSince `json:"since"`
	// Incremental is set when the answer holds only what changed after
	// the state the caller passed.
	Incremental bool `json:"incremental"`
	// Items are candidate rows in document order: every one of the
	// transcript's, or of an incremental answer the changed ones.
	Items []Item `json:"items"`
	// Changed are the ids of every row an incremental answer re-read,
	// whether it is a candidate now or not. A row among them that Items
	// does not hold is no longer a candidate.
	Changed []string `json:"changed"`
	// Capped is set when a full answer stopped at its limit with more
	// candidates after it.
	Capped bool `json:"capped"`
}

// SearchThreadItems returns the candidate rows of one thread transcript for
// its find bar, in document order (oldest turn/item first), so the bar can
// step through the matches top to bottom. A candidate's stored candidate
// text holds the query's loose form (find_candidate.go); a query with no
// letter or digit has no loose form, so every row is a candidate and the
// limit decides. It never matches thread titles: the caller already knows
// which thread it is searching.
//
// selection names the transcript searched, with the rows the timeline shows
// for it: the zero value is the main transcript's visible top-level rows, a
// scope root its agent's direct children (resolved to the canonical root as
// timeline pages resolve it).
//
// The rows are the timeline's physical arms, each walked in document order
// by its own index and merged, so a read stops at its limit instead of
// sorting every match (timeline_arms.go).
//
// since, when it is the state of an earlier uncapped answer to the same
// query, selection and limit, makes the read incremental: it re-reads the
// thread's own rows whose revision is at or after that answer's, by
// idx_items_thread_rev, so its cost follows what changed. A write stamps
// every row whose read it changes (history_rev_triggers.go), the contract
// timeline page refreshes read by too. Imported history and the rows a
// fork inherits never change. A delete or move (history_epoch), a restore,
// a change of imported history, or more changed rows than limit answers in
// full instead.
//
// limit caps the number of rows; zero or negative means unbounded. The
// read runs on ctx, so a cancelled call stops it, and it takes a history
// read slot.
func (s *Store) SearchThreadItems(ctx context.Context, threadID, query string, selection TimelineSelection, limit int, since *ThreadFindSince) (ThreadFindAnswer, error) {
	if err := s.CheckForkReady(threadID); err != nil {
		return ThreadFindAnswer{}, err
	}
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return ThreadFindAnswer{Items: []Item{}, Changed: []string{}}, nil
	}
	needle := findNeedle(trimmed)
	limit = max(limit, 0)
	return historyReadSnapshot(ctx, s, "search thread items", func(q sqlQueryer) (ThreadFindAnswer, error) {
		answer := ThreadFindAnswer{Items: []Item{}, Changed: []string{}}
		state, found, err := threadFindState(q, threadID)
		if err != nil || !found {
			return answer, err
		}
		scope, err := s.resolveTimelineScope(q, threadID, selection)
		if err != nil {
			return answer, err
		}
		state.ThreadID, state.Needle, state.Selection, state.Limit = threadID, needle, scope.selection, limit
		answer.Since = state
		filter, filterArgs := threadFindFilter(scope, needle)
		if since != nil && !since.Capped && since.ThreadID == state.ThreadID && since.Needle == state.Needle &&
			since.Selection == state.Selection && since.Limit == state.Limit &&
			since.Generation == state.Generation && since.Epoch == state.Epoch &&
			since.ImportChunks == state.ImportChunks && since.ImportOrder == state.ImportOrder && since.Rev <= state.Rev {
			changed, hits, ok, err := threadFindChanged(q, threadID, filter, filterArgs, since.Rev, limit)
			if err != nil {
				return answer, err
			}
			if ok {
				answer.Incremental = true
				answer.Changed = changed
				if len(hits) == 0 {
					return answer, nil
				}
				list, err := json.Marshal(hits)
				if err != nil {
					return answer, fmt.Errorf("store: search thread items: %w", err)
				}
				items, err := s.querySelectedPagedItems(q, threadID, `SELECT value AS id FROM json_each(?)`, string(list))
				if err != nil {
					return answer, fmt.Errorf("store: search thread items: %w", err)
				}
				answer.Items = items
				return answer, nil
			}
		}
		sel := threadFindSelection(filter, filterArgs, limit)
		ids, args, err := timelineIDSelection(q, threadID, sel)
		if err != nil {
			return answer, fmt.Errorf("store: search thread items: %w", err)
		}
		items, err := s.querySelectedPagedItems(q, threadID, ids, args...)
		if err != nil {
			return answer, fmt.Errorf("store: search thread items: %w", err)
		}
		if limit > 0 && len(items) > limit {
			items, answer.Capped = items[:limit], true
		}
		answer.Since.Capped = answer.Capped
		answer.Items = items
		return answer, nil
	})
}

// threadFindState reads the state a find answer describes, in the read's
// snapshot. found is false for a thread this store does not own.
func threadFindState(q sqlQueryer, threadID string) (ThreadFindSince, bool, error) {
	var state ThreadFindSince
	if err := q.QueryRow(
		`SELECT history_rev, history_epoch FROM owned_threads WHERE id = ?`, threadID,
	).Scan(&state.Rev, &state.Epoch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return state, false, nil
		}
		return state, false, fmt.Errorf("store: search thread items: read thread: %w", err)
	}
	if err := q.QueryRow(
		`SELECT COUNT(*), COALESCE(MAX(chunk_order), -1) FROM thread_import_chunks WHERE thread_id = ?`, threadID,
	).Scan(&state.ImportChunks, &state.ImportOrder); err != nil {
		return state, false, fmt.Errorf("store: search thread items: read imported history: %w", err)
	}
	identity, err := identityFrom(q)
	if err != nil {
		return state, false, err
	}
	state.Generation = identity.ReplicaGeneration
	return state, true, nil
}

// threadFindFilter is the condition that a row of alias `items` is a
// candidate in scope: in the transcript, and its candidate text holds the
// loose needle (findMatchSQL), or any row for an empty needle.
func threadFindFilter(scope timelineScope, needle string) (string, []any) {
	filter, args := scope.filter("items.")
	if needle == "" {
		return filter, args
	}
	return filter + ` AND ` + findMatchSQL("items.", boundText), append(args, needle, needle)
}

// threadFindSelection selects the candidates filter admits, in document
// order, cut one past limit (zero for none) so the read knows whether it
// was capped. The scope narrows to one thread's rows by index before the
// candidate text is read.
func threadFindSelection(filter string, args []any, limit int) timelineSelection {
	sel := timelineSelection{
		Where:     filter,
		WhereArgs: args,
		OrderBy:   "turn_index ASC, item_index ASC",
	}
	if limit > 0 {
		sel.Limit = limit + 1
	}
	return sel
}

// threadFindChangedSQL lists the thread's own rows at or after a revision,
// and which of them filter admits. A row written under a bulk load carries
// the revision its load started at, which is never before the revision a
// read saw before the load, so `>=` holds it.
func threadFindChangedSQL(filter string, limit int) string {
	sql := `SELECT items.id, CASE WHEN ` + filter + ` THEN 1 ELSE 0 END
	  FROM items INDEXED BY idx_items_thread_rev
	 WHERE items.thread_id = ? AND items.rev >= ?`
	if limit > 0 {
		sql += ` LIMIT ` + strconv.Itoa(limit+1)
	}
	return sql
}

// threadFindChanged reads the changed rows of an incremental answer: every
// own row at or after rev, and the candidates among them. ok is false when
// more than limit changed.
func threadFindChanged(q sqlQueryer, threadID, filter string, filterArgs []any, rev int64, limit int) ([]string, []string, bool, error) {
	args := append(append([]any{}, filterArgs...), threadID, rev)
	rows, err := q.Query(threadFindChangedSQL(filter, limit), args...)
	if err != nil {
		return nil, nil, false, fmt.Errorf("store: search thread items: read changed rows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	changed, hits := []string{}, []string{}
	for rows.Next() {
		var id string
		var hit int
		if err := rows.Scan(&id, &hit); err != nil {
			return nil, nil, false, fmt.Errorf("store: search thread items: scan changed row: %w", err)
		}
		changed = append(changed, id)
		if hit == 1 {
			hits = append(hits, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, false, fmt.Errorf("store: search thread items: read changed rows: %w", err)
	}
	if limit > 0 && len(changed) > limit {
		return nil, nil, false, nil
	}
	return changed, hits, true, nil
}

// searchTitleHits returns one hit per thread whose title matches — no per-item
// fan-out. Most-recently-active threads first.
func searchTitleHits(q sqlQueryer, pattern string, limit int) ([]ThreadMessageHit, error) {
	hiddenClause, hiddenArgs := hiddenThreadModesClause("mode")
	args := append([]any{pattern}, hiddenArgs...)
	rows, err := q.Query(`
		SELECT id, title, provider, ownership_epoch
		FROM owned_threads
		WHERE fork_preparing = 0 AND LOWER(title) LIKE `+boundText+` ESCAPE '\' AND `+hiddenClause+`
		ORDER BY updated_at DESC
		`+limitSuffix(limit),
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: search thread titles: %w", err)
	}
	defer rows.Close()

	var hits []ThreadMessageHit
	for rows.Next() {
		var h ThreadMessageHit
		if err := rows.Scan(&h.ThreadID, &h.ThreadTitle, &h.Provider, &h.OwnershipEpoch); err != nil {
			return nil, fmt.Errorf("store: scan title hit: %w", err)
		}
		h.MatchType = "title"
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// mergeTitleFirst combines title and item hits into one list capped at limit.
// Titles lead, but a share of the limit (up to half) is reserved for item hits
// whenever both kinds are present, so a query that matches many thread titles
// still surfaces message-body matches instead of filling the whole window with
// titles. When one kind is sparse the other reclaims the slack. A non-positive
// limit returns everything.
func mergeTitleFirst(titleHits, itemHits []ThreadMessageHit, limit int) []ThreadMessageHit {
	if limit > 0 && len(titleHits)+len(itemHits) > limit {
		itemReserve := min(len(itemHits), limit/2)
		titleBudget := limit - itemReserve
		if len(titleHits) > titleBudget {
			titleHits = titleHits[:titleBudget]
		}
		itemBudget := limit - len(titleHits)
		if len(itemHits) > itemBudget {
			itemHits = itemHits[:itemBudget]
		}
	}
	merged := make([]ThreadMessageHit, 0, len(titleHits)+len(itemHits))
	merged = append(merged, titleHits...)
	merged = append(merged, itemHits...)
	return merged
}

// searchGlobalItemHits returns one hit per item (any thread) whose summary
// matches, newest-first. Item summaries only — title matches come from
// searchTitleHits and are merged in by SearchThreadMessages.
func searchGlobalItemHits(q sqlQueryer, pattern string, limit int) ([]ThreadMessageHit, error) {
	hiddenClause, hiddenArgs := hiddenThreadModesClause("t.mode")
	args := append([]any{pattern}, hiddenArgs...)
	rows, err := q.Query(`
		SELECT t.id, t.title, t.provider, t.ownership_epoch,
			i.id, i.turn_index, i.item_index, i.kind, i.role, i.summary
		FROM timeline_items i
		JOIN owned_threads t ON t.id = i.thread_id
		WHERE t.fork_preparing = 0 AND LOWER(i.summary) LIKE `+boundText+` ESCAPE '\' AND `+hiddenClause+`
		ORDER BY i.created_at DESC
		`+limitSuffix(limit),
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: search item summaries: %w", err)
	}
	defer rows.Close()
	return scanItemHits(rows)
}

// scanItemHits scans the shared item-hit column tuple (both the global and the
// thread-scoped item queries project these ten columns in this order).
func scanItemHits(rows *sql.Rows) ([]ThreadMessageHit, error) {
	var hits []ThreadMessageHit
	for rows.Next() {
		var h ThreadMessageHit
		if err := rows.Scan(
			&h.ThreadID, &h.ThreadTitle, &h.Provider, &h.OwnershipEpoch,
			&h.ItemID, &h.TurnIndex, &h.ItemIndex, &h.ItemKind, &h.ItemRole, &h.Summary,
		); err != nil {
			return nil, fmt.Errorf("store: scan item hit: %w", err)
		}
		h.MatchType = "item"
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// likePattern folds the query as SQLite LOWER folds the text and wraps it
// in substring wildcards, with the user's own %, _, and \ escaped so they
// are matched literally (paired with the ESCAPE '\' clause in every query
// above).
func likePattern(query string) string {
	// Matching is case-insensitive for ASCII letters and exact for every other character, on both sides.
	return "%" + escapeLike(asciiLower(query)) + "%"
}

// asciiLower lowers ASCII capitals only, as SQLite's built-in LOWER does.
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
}

// limitSuffix renders an optional "LIMIT n" clause; non-positive means none.
func limitSuffix(limit int) string {
	if limit > 0 {
		return fmt.Sprintf("LIMIT %d", limit)
	}
	return ""
}

// escapeLike escapes the SQLite LIKE wildcards (%, _) and the escape char
// (\) from the input so an arbitrary user query doesn't behave like a
// pattern. Used in tandem with the ESCAPE '\' clause in the queries.
func escapeLike(in string) string {
	var b strings.Builder
	b.Grow(len(in))
	for _, r := range in {
		switch r {
		case '\\', '%', '_':
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
