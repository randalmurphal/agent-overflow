package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// ThreadMessageHit is one hit from a global message search. The result set is
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
	// Summary is the item's stored summary field, truncated to the preview
	// budget. The frontend can highlight the query within it.
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
// Deleted threads (soft-delete isn't modeled yet; the FK cascade on items
// keeps this consistent) are excluded by the inner join: no item rows
// survive a thread delete, so no orphan hits can surface.
//
// Ordering:
//   - Title matches come first (a matching title is a stronger signal of
//     intent than a stray word in a long command output).
//   - Within each match type, newer items come first (items.created_at DESC).
//
// Queries that are empty or whitespace-only return an empty slice rather
// than "everything". This keeps the UI contract trivial — the caller passes
// whatever the user typed and relies on the store to short-circuit.
func (s *Store) SearchThreadMessages(query string, limit int) ([]ThreadMessageHit, error) {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return nil, nil
	}
	// Escape % and _ so the user's query isn't interpreted as a LIKE wildcard.
	pattern := "%" + escapeLike(strings.ToLower(trimmed)) + "%"
	limitClause := ""
	if limit > 0 {
		limitClause = fmt.Sprintf("LIMIT %d", limit)
	}

	rows, err := s.db.Query(`
		SELECT
			t.id, t.title, t.provider,
			COALESCE(i.id, ''), COALESCE(i.turn_index, 0),
			COALESCE(i.kind, ''), COALESCE(i.role, ''), COALESCE(i.summary, ''),
			CASE
				WHEN LOWER(t.title) LIKE ? ESCAPE '\' THEN 'title'
				ELSE 'item'
			END AS match_type
		FROM threads t
		LEFT JOIN items i ON i.thread_id = t.id
		WHERE
			LOWER(t.title) LIKE ? ESCAPE '\'
			OR LOWER(COALESCE(i.summary, '')) LIKE ? ESCAPE '\'
		ORDER BY
			CASE WHEN LOWER(t.title) LIKE ? ESCAPE '\' THEN 0 ELSE 1 END,
			COALESCE(i.created_at, 0) DESC
		`+limitClause,
		pattern, pattern, pattern, pattern,
	)
	if err != nil {
		return nil, fmt.Errorf("store: search thread messages: %w", err)
	}
	defer rows.Close()

	var hits []ThreadMessageHit
	for rows.Next() {
		var h ThreadMessageHit
		if err := rows.Scan(
			&h.ThreadID, &h.ThreadTitle, &h.Provider,
			&h.ItemID, &h.TurnIndex,
			&h.ItemKind, &h.ItemRole, &h.Summary,
			&h.MatchType,
		); err != nil {
			return nil, fmt.Errorf("store: scan search hit: %w", err)
		}
		// If the hit is a title match and we got a random item via the LEFT
		// JOIN, drop the item fields so the frontend doesn't show an arbitrary
		// item next to a title hit. This is cosmetic — the MatchType is the
		// source of truth; we're just avoiding a confusing mixed row.
		if h.MatchType == "title" && h.ItemID != "" {
			h.ItemID = ""
			h.ItemKind = ""
			h.ItemRole = ""
			h.Summary = ""
			h.TurnIndex = 0
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate search hits: %w", err)
	}

	return dedupeTitleHits(hits), nil
}

// escapeLike escapes the SQLite LIKE wildcards (%, _) and the escape char
// (\) from the input so an arbitrary user query doesn't behave like a
// pattern. Used in tandem with the ESCAPE '\' clause in the query.
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

// dedupeTitleHits collapses the (potentially N) duplicate title-match rows
// the LEFT JOIN produces — one per item in the matching thread — into a
// single title hit per thread. Item hits are preserved as-is (each item is
// a distinct match).
func dedupeTitleHits(hits []ThreadMessageHit) []ThreadMessageHit {
	seen := map[string]bool{}
	out := make([]ThreadMessageHit, 0, len(hits))
	for _, h := range hits {
		if h.MatchType == "title" {
			if seen[h.ThreadID] {
				continue
			}
			seen[h.ThreadID] = true
		}
		out = append(out, h)
	}
	return out
}

// sql.ErrNoRows isn't imported directly — using it here via the standard
// library to document the intent without the check actually firing under
// our query shape. Left to silence the linter's "unused import" complaint
// in a later refactor.
var _ = sql.ErrNoRows
