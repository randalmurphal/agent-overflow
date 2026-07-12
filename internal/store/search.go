package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// ThreadMessageHit is one hit from a message search. The result set is
// designed for an interactive UI: it pairs the thread's display title with the
// specific item's kind + summary so users can jump directly to the matching
// turn.
type ThreadMessageHit struct {
	ThreadID    string `json:"threadId"`
	ThreadTitle string `json:"threadTitle"`
	Provider    string `json:"provider"`
	ItemID      string `json:"itemId"`
	TurnIndex   int    `json:"turnIndex"`
	ItemKind    string `json:"itemKind"`
	ItemRole    string `json:"itemRole"`
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
// to an FTS5 virtual table without changing the return shape.
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
func (s *Store) SearchThreadMessages(query string, limit int) ([]ThreadMessageHit, error) {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return nil, nil
	}
	pattern := likePattern(trimmed)

	titleHits, err := s.searchTitleHits(pattern, limit)
	if err != nil {
		return nil, err
	}
	itemHits, err := s.searchGlobalItemHits(pattern, limit)
	if err != nil {
		return nil, err
	}
	return mergeTitleFirst(titleHits, itemHits, limit), nil
}

// SearchThreadItems returns item-summary hits within a SINGLE thread, in
// document order (oldest turn/item first), so callers can step through matches
// top-to-bottom like an in-document "find". It never matches thread titles —
// the caller already knows which thread it is searching.
//
// Because it is scoped to one thread via the thread_id index it stays fast
// even on a cold cache (a thread is hundreds of items, not the whole history),
// which is why in-thread find does not need the FTS5 work the global search
// will eventually want.
//
// limit caps the number of hits; zero or negative means unbounded.
func (s *Store) SearchThreadItems(threadID, query string, limit int) ([]ThreadMessageHit, error) {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return nil, nil
	}
	pattern := likePattern(trimmed)

	rows, err := s.db.Query(`
		SELECT t.id, t.title, t.provider,
			i.id, i.turn_index, i.kind, i.role, i.summary
		FROM items i
		JOIN threads t ON t.id = i.thread_id
		WHERE i.thread_id = ?
			AND LOWER(i.summary) LIKE ? ESCAPE '\'
		ORDER BY i.turn_index ASC, i.item_index ASC
		`+limitSuffix(limit),
		threadID, pattern,
	)
	if err != nil {
		return nil, fmt.Errorf("store: search thread items: %w", err)
	}
	defer rows.Close()
	return scanItemHits(rows)
}

// searchTitleHits returns one hit per thread whose title matches — no per-item
// fan-out. Most-recently-active threads first.
func (s *Store) searchTitleHits(pattern string, limit int) ([]ThreadMessageHit, error) {
	hiddenClause, hiddenArgs := hiddenThreadModesClause("mode")
	args := append([]any{pattern}, hiddenArgs...)
	rows, err := s.db.Query(`
		SELECT id, title, provider
		FROM threads
		WHERE LOWER(title) LIKE ? ESCAPE '\' AND `+hiddenClause+`
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
		if err := rows.Scan(&h.ThreadID, &h.ThreadTitle, &h.Provider); err != nil {
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
func (s *Store) searchGlobalItemHits(pattern string, limit int) ([]ThreadMessageHit, error) {
	hiddenClause, hiddenArgs := hiddenThreadModesClause("t.mode")
	args := append([]any{pattern}, hiddenArgs...)
	rows, err := s.db.Query(`
		SELECT t.id, t.title, t.provider,
			i.id, i.turn_index, i.kind, i.role, i.summary
		FROM items i
		JOIN threads t ON t.id = i.thread_id
		WHERE LOWER(i.summary) LIKE ? ESCAPE '\' AND `+hiddenClause+`
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
// thread-scoped item queries project these eight columns in this order).
func scanItemHits(rows *sql.Rows) ([]ThreadMessageHit, error) {
	var hits []ThreadMessageHit
	for rows.Next() {
		var h ThreadMessageHit
		if err := rows.Scan(
			&h.ThreadID, &h.ThreadTitle, &h.Provider,
			&h.ItemID, &h.TurnIndex, &h.ItemKind, &h.ItemRole, &h.Summary,
		); err != nil {
			return nil, fmt.Errorf("store: scan item hit: %w", err)
		}
		h.MatchType = "item"
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// likePattern lowercases the query and wraps it in substring wildcards, with
// the user's own %, _, and \ escaped so they are matched literally (paired
// with the ESCAPE '\' clause in every query above).
func likePattern(query string) string {
	return "%" + escapeLike(strings.ToLower(query)) + "%"
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
