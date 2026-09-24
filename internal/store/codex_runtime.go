package store

import (
	"errors"
	"fmt"
	"sort"
)

// RecoverCodexBackgroundRuntime retires every Codex-owned background runtime
// left by a prior app instance. Spawn events are immutable and excluded. A running tool
// call becomes errored/lost because its provider process no longer exists.
func (s *Store) RecoverCodexBackgroundRuntime(summarise func(string) string, updatedAt int64) ([]Item, error) {
	return s.retireCodexBackgroundRuntime("", summarise, updatedAt)
}

// RetireCodexBackgroundRuntime is the thread-scoped form used when AO replaces
// a Codex session inside the same app process.
func (s *Store) RetireCodexBackgroundRuntime(threadID string, summarise func(string) string, updatedAt int64) ([]Item, error) {
	if threadID == "" {
		return nil, errors.New("store: retire Codex background runtime: thread id required")
	}
	return s.retireCodexBackgroundRuntime(threadID, summarise, updatedAt)
}

func (s *Store) retireCodexBackgroundRuntime(threadID string, summarise func(string) string, updatedAt int64) ([]Item, error) {
	if summarise == nil {
		return nil, errors.New("store: retire Codex background runtime: summariser required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: begin Codex background runtime retirement: %w", err)
	}
	defer tx.Rollback()

	var retired []Item
	collect := func(query string, args ...any) error {
		rows, err := tx.Query(query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			item, scanErr := scanItemRowSansPayload(rows)
			if scanErr != nil {
				return scanErr
			}
			retired = append(retired, item)
		}
		return rows.Err()
	}
	scopeSQL := ""
	args := []any{}
	if threadID != "" {
		scopeSQL = " AND items.thread_id = ?"
		args = append(args, threadID)
	}
	// Keep these as two indexed walks. Combining running tools and completed
	// spawn cards behind an OR disqualifies both partial indexes and turns app
	// startup into a full history scan.
	if err := collect(
		`SELECT `+itemColumnsSansPayload+`
		   FROM items INDEXED BY idx_items_running_bg_tool_calls
		   JOIN threads ON threads.id = items.thread_id`+servedItemJoin+`
		  WHERE threads.provider = 'codex'
		    AND items.kind = 'tool_call'
		    AND items.status = 'running'
 AND items.tool_name <> 'collab_agent'
		    AND items.is_background = 1
		    AND COALESCE(json_extract(items.meta, '$.live_background_active'), 1) != 0`+scopeSQL,
		args...,
	); err != nil {
		return nil, fmt.Errorf("store: select running Codex background runtime: %w", err)
	}

	sort.Slice(retired, func(i, j int) bool {
		if retired[i].ThreadID != retired[j].ThreadID {
			return retired[i].ThreadID < retired[j].ThreadID
		}
		if retired[i].TurnIndex != retired[j].TurnIndex {
			return retired[i].TurnIndex < retired[j].TurnIndex
		}
		return retired[i].ItemIndex < retired[j].ItemIndex
	})

	// A retired child's summary can move its launch's card; the retirement
	// carries no card and recomputes those chains, one thread at a time
	// (retired is sorted by thread).
	var w *cardWrite
	for i := range retired {
		item := &retired[i]
		if w == nil || w.threadID != item.ThreadID {
			if w != nil {
				if err := w.finish(); err != nil {
					return nil, err
				}
			}
			w = s.bulkItemWrites(tx, item.ThreadID, false)
		}
		old := subagentRowOf(*item)
		item.Status = "errored"
		item.Summary = summarise(item.Summary)
		item.Decision = "lost"
		item.UpdatedAt = updatedAt
		row := old
		row.summary = item.Summary
		if err := w.updated(old, row); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(
			`UPDATE items SET status = ?, summary = ?, decision = ?, updated_at = ? WHERE thread_id = ? AND id = ?`,
			item.Status, item.Summary, item.Decision, item.UpdatedAt, item.ThreadID, item.ID,
		); err != nil {
			return nil, fmt.Errorf("store: retire Codex background item %s: %w", item.ID, err)
		}
	}
	if w != nil {
		if err := w.finish(); err != nil {
			return nil, err
		}
	}
	for i := range retired {
		item := &retired[i]
		// The rows were selected before the UPDATE, so their revision is
		// the pre-write one. These structs are emitted to clients.
		if item.Rev, err = readItemRevTx(tx, item.ThreadID, item.ID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit Codex background runtime retirement: %w", err)
	}
	return retired, nil
}
