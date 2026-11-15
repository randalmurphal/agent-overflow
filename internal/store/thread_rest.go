package store

import (
	"fmt"
	"math"
	"strconv"
)

// ThreadRestCursor continues ListThreadRestRequests after the last row of
// the page before. The zero value starts at the oldest row.
type ThreadRestCursor struct {
	CreatedAt int64
	Token     string
}

// After is the cursor that continues past row.
func (c ThreadRestCursor) After(row ThreadRequest) ThreadRestCursor {
	return ThreadRestCursor{CreatedAt: row.CreatedAt, Token: row.Token}
}

// ListThreadRestRequests returns, oldest first and at most limit at a time,
// the requests callerThreadID made that bringing it to rest acts on: every
// one still open, of any kind, and every spawn it made whatever its state,
// a lineage-only row included, since the thread a spawn made may still be
// running long after the spawn settled. A local spawn another spawn of the
// same thread superseded is not this caller's link to it.
func (s *Store) ListThreadRestRequests(callerThreadID string, after ThreadRestCursor, limit int) ([]ThreadRequest, error) {
	if limit < 1 {
		return nil, fmt.Errorf("store: list the requests a rest of %s stops: limit %d", callerThreadID, limit)
	}
	afterAt := after.CreatedAt
	if after == (ThreadRestCursor{}) {
		afterAt = math.MinInt64
	}
	rows, err := s.reader().Query(
		`SELECT `+threadRequestColumns+` FROM thread_requests
		  WHERE caller_thread_id = ?
		    AND ((lineage_only = 0 AND settled_at IS NULL)
		      OR (kind = 'spawn' AND target_thread_id <> '' AND (target_computer_id <> '' OR superseded = 0)))
		    AND (created_at > ? OR (created_at = ? AND token > ?))
		  ORDER BY created_at, token LIMIT `+strconv.Itoa(limit),
		callerThreadID, afterAt, afterAt, after.Token)
	if err != nil {
		return nil, fmt.Errorf("store: list the requests a rest of %s stops: %w", callerThreadID, err)
	}
	return collectThreadRequests(rows, "store: list the requests a rest of "+callerThreadID+" stops")
}
