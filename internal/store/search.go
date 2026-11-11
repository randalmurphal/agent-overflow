package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
// and item summaries. Matching is the find bar's literal substring rule
// (find_fold, stringsx.FoldForFind), so a hit is a match the bar paints.
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
	pattern := findPattern(trimmed)
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

// SearchThreadItems returns item-summary hits within a SINGLE thread, in
// document order (oldest turn/item first), so callers can step through matches
// top-to-bottom like an in-document "find". It never matches thread titles:
// the caller already knows which thread it is searching.
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
// limit caps the number of hits; zero or negative means unbounded. The
// read runs on ctx, so a cancelled call stops it, and it takes a history
// read slot.
func (s *Store) SearchThreadItems(ctx context.Context, threadID, query string, selection TimelineSelection, limit int) ([]ThreadMessageHit, error) {
	if err := s.CheckForkReady(threadID); err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return nil, nil
	}
	pattern := findPattern(trimmed)
	return historyReadSnapshot(ctx, s, "search thread items", func(q sqlQueryer) ([]ThreadMessageHit, error) {
		var thread ThreadMessageHit
		if err := q.QueryRow(
			`SELECT id, title, provider, ownership_epoch FROM owned_threads WHERE id = ?`, threadID,
		).Scan(&thread.ThreadID, &thread.ThreadTitle, &thread.Provider, &thread.OwnershipEpoch); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil
			}
			return nil, fmt.Errorf("store: search thread items: read thread: %w", err)
		}
		scope, err := s.resolveTimelineScope(q, threadID, selection)
		if err != nil {
			return nil, err
		}
		sel := threadFindSelection(scope, pattern, max(limit, 0))
		sel.Columns = func(string, string) string {
			return `items.id AS id, items.turn_index AS turn_index, items.item_index AS item_index,
			       items.kind AS kind, items.role AS role, items.summary AS summary`
		}
		arms, args, err := timelineArms(q, threadID, sel)
		if err != nil {
			return nil, fmt.Errorf("store: search thread items: %w", err)
		}
		rows, err := q.Query(arms, args...)
		if err != nil {
			return nil, fmt.Errorf("store: search thread items: %w", err)
		}
		defer rows.Close()
		var hits []ThreadMessageHit
		for rows.Next() {
			h := thread
			if err := rows.Scan(&h.ItemID, &h.TurnIndex, &h.ItemIndex, &h.ItemKind, &h.ItemRole, &h.Summary); err != nil {
				return nil, fmt.Errorf("store: scan thread item hit: %w", err)
			}
			h.MatchType = "item"
			hits = append(hits, h)
		}
		return hits, rows.Err()
	})
}

// threadFindSelection selects scope's rows whose summary matches pattern,
// in document order, cut to limit (zero for none).
func threadFindSelection(scope timelineScope, pattern string, limit int) timelineSelection {
	filter, args := scope.filter("items.")
	return timelineSelection{
		Where:     filter + ` AND ` + findMatchSQL("items.summary"),
		WhereArgs: append(args, pattern),
		OrderBy:   "turn_index ASC, item_index ASC",
		Limit:     limit,
	}
}

// searchTitleHits returns one hit per thread whose title matches — no per-item
// fan-out. Most-recently-active threads first.
func searchTitleHits(q sqlQueryer, pattern string, limit int) ([]ThreadMessageHit, error) {
	hiddenClause, hiddenArgs := hiddenThreadModesClause("mode")
	args := append([]any{pattern}, hiddenArgs...)
	rows, err := q.Query(`
		SELECT id, title, provider, ownership_epoch
		FROM owned_threads
		WHERE fork_preparing = 0 AND `+findMatchSQL("title")+` AND `+hiddenClause+`
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
		WHERE t.fork_preparing = 0 AND `+findMatchSQL("i.summary")+` AND `+hiddenClause+`
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

// limitSuffix renders an optional "LIMIT n" clause; non-positive means none.
func limitSuffix(limit int) string {
	if limit > 0 {
		return fmt.Sprintf("LIMIT %d", limit)
	}
	return ""
}
