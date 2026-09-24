package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
)

// RecoverCodexBackgroundRuntime retires every Codex-owned background runtime
// left by a prior app instance. Spawn events are immutable and excluded. A running tool
// call becomes errored/lost because its provider process no longer exists.
// It runs before any provider session starts, and stops agents in threads
// whose cards it does not lock (sweepItemWrites): any card left is
// flushed first.
func (s *Store) RecoverCodexBackgroundRuntime(summarise func(string) string, updatedAt int64) ([]Item, error) {
	if summarise == nil {
		return nil, errors.New("store: retire Codex background runtime: summariser required")
	}
	flushErr := s.FlushAllSubagentCards()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, errors.Join(flushErr, fmt.Errorf("store: begin Codex background runtime retirement: %w", err))
	}
	defer tx.Rollback()
	retired, err := retireCodexBackgroundRuntimeTx(tx, "", summarise, updatedAt, func(threadID string) *cardWrite {
		return s.sweepItemWrites(tx, threadID)
	})
	if err != nil {
		return nil, errors.Join(flushErr, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, errors.Join(flushErr, fmt.Errorf("store: commit Codex background runtime retirement: %w", err))
	}
	return retired, flushErr
}

// RetireCodexBackgroundRuntime is the thread-scoped form used when AO replaces
// a Codex session inside the same app process.
func (s *Store) RetireCodexBackgroundRuntime(threadID string, summarise func(string) string, updatedAt int64) ([]Item, error) {
	if threadID == "" {
		return nil, errors.New("store: retire Codex background runtime: thread id required")
	}
	if summarise == nil {
		return nil, errors.New("store: retire Codex background runtime: summariser required")
	}
	var retired []Item
	err := s.bulkWriteItems(threadID, "Codex background runtime retirement", func(tx *sql.Tx, w *cardWrite) error {
		var err error
		retired, err = retireCodexBackgroundRuntimeTx(tx, threadID, summarise, updatedAt, func(string) *cardWrite { return w })
		return err
	})
	if err != nil {
		return nil, err
	}
	return retired, nil
}

// retireCodexBackgroundRuntimeTx retires the running Codex background
// tool calls of threadID, or of every thread for "", recording each
// thread's writes in writes(threadID).
func retireCodexBackgroundRuntimeTx(tx *sql.Tx, threadID string, summarise func(string) string, updatedAt int64, writes func(threadID string) *cardWrite) ([]Item, error) {
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
			w = writes(item.ThreadID)
		}
		old := subagentRowOf(*item)
		item.Status = "errored"
		item.Summary = summarise(item.Summary)
		item.Decision = "lost"
		item.UpdatedAt = updatedAt
		row := old
		row.status, row.summary = item.Status, item.Summary
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
		var err error
		if item.Rev, err = readItemRevTx(tx, item.ThreadID, item.ID); err != nil {
			return nil, err
		}
	}
	return retired, nil
}
