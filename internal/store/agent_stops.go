package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Agent stops (claude-wire.md §E6b, docs/specs/agent-visibility.md
// §Agent runs and stops). Every stop of a Claude background agent's run
// is a completion-shaped sibling of the row that started the run, the
// launch or a §E6 resume carrier: the ending one settles that row, and a
// parked one (ItemStatusParked) records a pause the agent wakes from. A
// parked sibling settles nothing (background_settle_triggers.go,
// noCompletionSiblingSQL), ends no agent (agent_rows.go is never reached
// for it) and borrows no card (aggBorrowsCardSQL): it is history of one
// run and reads the same after the runs that follow it.

// ItemStatusParked is the status of a parked stop's sibling.
const ItemStatusParked = "parked"

// wakePromptMetaKey marks the row the parser writes for an agent's wake
// (provider.MetaSubagentWakePromptKey; triage persistWakePromptRow).
const wakePromptMetaKey = "subagent_wake_prompt"

// newestStopSQL is a launch's newest completion-shaped sibling, parked or
// ending, served as a page reads it.
var newestStopSQL = `SELECT ` + itemColumns + `
	   FROM items
	   LEFT JOIN payloads ON payloads.thread_id = items.thread_id AND payloads.id = items.payload_id` + servedItemJoin + `
	  WHERE items.thread_id = ? AND items.completion_of = ? AND items.completion_of <> ''
	  ORDER BY items.created_at DESC, items.turn_index DESC, items.item_index DESC
	  LIMIT 1`

// latestWakeSQL is the creation time of the newest wake row under a
// transcript root at or after a time. Served by the partial user_text
// parent index.
const latestWakeSQL = `SELECT created_at FROM items
	  WHERE thread_id = ? AND parent_id = ? AND parent_id <> '' AND kind = 'user_text'
	    AND created_at >= ?
	    AND CASE WHEN json_valid(meta) THEN json_extract(meta, '$.` + wakePromptMetaKey + `') END = 1
	  ORDER BY created_at DESC
	  LIMIT 1`

// CurrentParkedStop returns the parked sibling launchID is paused at: its
// newest stop, when that stop is parked and no wake under the agent's
// transcript root rootID has followed it. A wake starts the next run, so
// from then on the agent runs again until that run's own stop. Triage
// writes each after the row it follows, in a later millisecond (triage
// writeWakePromptRow, writeParkedStop).
func (s *Store) CurrentParkedStop(threadID, launchID, rootID string) (Item, bool, error) {
	type result struct {
		item  Item
		found bool
	}
	got, err := readSnapshot(s.reader(), "current parked stop", func(q sqlQueryer) (result, error) {
		stop, found, err := newestAgentStop(q, threadID, launchID)
		if err != nil || !found || stop.Status != ItemStatusParked {
			return result{}, err
		}
		_, woke, err := latestAgentWake(q, threadID, rootID, stop.CreatedAt)
		if err != nil || woke {
			return result{}, err
		}
		return result{stop, true}, nil
	})
	return got.item, got.found, err
}

// NewestAgentStop returns launchID's newest completion-shaped sibling,
// parked or ending.
func (s *Store) NewestAgentStop(threadID, launchID string) (Item, bool, error) {
	return newestAgentStop(s.reader(), threadID, launchID)
}

func newestAgentStop(q sqlQueryer, threadID, launchID string) (Item, bool, error) {
	stop, err := scanItemRow(q.QueryRow(newestStopSQL, threadID, launchID))
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("store: read the newest stop of %s/%s: %w", threadID, launchID, err)
	}
	return stop, true, nil
}

// NewestTaskStop returns the newest completion-shaped sibling carrying
// taskID, parked or ending: the latest stop of the agent, whether it
// completes the launch or a §E6 carrier the task was rebound to.
func (s *Store) NewestTaskStop(threadID, taskID string) (Item, bool, error) {
	if taskID == "" {
		return Item{}, false, nil
	}
	selection, args, err := timelineKeyedIDSelection(s.reader(), threadID,
		"items.created_at AS created_at",
		"json_extract(items.meta, '$.task_id') = ? AND items.kind = 'tool_completion' AND items.completion_of <> ''", []any{taskID},
		"created_at DESC", 1)
	if err != nil {
		return Item{}, false, err
	}
	stop, found, err := queryOneHydratedTimelineItem(s.reader(), threadID, selection, args...)
	if err != nil {
		return Item{}, false, fmt.Errorf("store: read the newest stop of task %s/%s: %w", threadID, taskID, err)
	}
	return stop, found, nil
}

// LatestAgentWake returns the creation time of the newest wake row under
// transcript root rootID created at or after since.
func (s *Store) LatestAgentWake(threadID, rootID string, since int64) (int64, bool, error) {
	return latestAgentWake(s.reader(), threadID, rootID, since)
}

func latestAgentWake(q sqlQueryer, threadID, rootID string, since int64) (int64, bool, error) {
	rootID = strings.TrimSpace(rootID)
	if rootID == "" {
		return 0, false, nil
	}
	var createdAt int64
	err := q.QueryRow(latestWakeSQL, threadID, rootID, since).Scan(&createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: read the latest wake under %s/%s: %w", threadID, rootID, err)
	}
	return createdAt, true, nil
}

// retireParkedLaunchesSQL marks inactive every live background tool call
// bound to task ?2 other than row ?3 that has a parked stop and no ending
// one. The task_id term is served by idx_items_meta_task_id.
var retireParkedLaunchesSQL = `UPDATE items
	    SET meta = json_set(
	          CASE WHEN json_valid(meta) THEN meta ELSE '{}' END,
	          '$.live_background_active',
	          json('false')
	        )
	  WHERE thread_id = ?
	    AND json_extract(meta, '$.task_id') = ?
	    AND id <> ?
	    AND kind = 'tool_call'
	    AND status = 'running'
	    AND is_background = 1
	    AND COALESCE(json_extract(meta, '$.live_background_active'), 1) != 0
	    AND ` + noCompletionSiblingSQL + `
	    AND EXISTS (
	      SELECT 1 FROM items c
	       WHERE c.thread_id = items.thread_id
	         AND c.completion_of = items.id
	         AND c.completion_of <> ''
	         AND c.status = '` + ItemStatusParked + `'
	    )
	RETURNING ` + subagentRowColumns("")

// RetireParkedAgentLaunches takes the rows a parked agent's task was bound
// to out of the live set when a §E6 rebind moves the task to carrierID:
// the run each started ended at its parked stop, which records it, so
// nothing else is written. It returns the ids it retired; a task with no
// parked row changes nothing.
func (s *Store) RetireParkedAgentLaunches(threadID, taskID, carrierID string) ([]string, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, nil
	}
	var retired []string
	err := s.bulkWriteItems(threadID, "retire parked agent launches", func(tx *sql.Tx, w *cardWrite) error {
		rows, err := tx.Query(retireParkedLaunchesSQL, threadID, taskID, carrierID)
		if err != nil {
			return fmt.Errorf("store: retire the parked launches of %s/%s: %w", threadID, taskID, err)
		}
		var marked []subagentRow
		for rows.Next() {
			row, err := scanSubagentRow(rows)
			if err != nil {
				return errors.Join(fmt.Errorf("store: scan a retired launch of %s/%s: %w", threadID, taskID, err), rows.Close())
			}
			marked = append(marked, row)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return fmt.Errorf("store: iterate the retired launches of %s/%s: %w", threadID, taskID, err)
		}
		// Each launch ran until this write, as a teardown's does
		// (MarkLiveBackgroundToolCallsInactive).
		for _, row := range marked {
			old := row
			old.inactive = false
			if err := w.updated(old, row); err != nil {
				return err
			}
			retired = append(retired, row.id)
		}
		return w.finish()
	})
	if err != nil {
		return nil, err
	}
	return retired, nil
}
