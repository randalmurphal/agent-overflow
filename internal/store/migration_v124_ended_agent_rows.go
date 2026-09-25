package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Migration v124 settles the rows earlier builds left open under agents
// that had ended. Those builds settled an agent's rows with the turn that
// wrote them, keyed by that turn's index, so a row a later turn wrote under
// the agent stayed running or streaming after the agent's end. The live
// path now settles an agent's rows at its end (§Agent-owned rows in
// docs/architecture/turn-lifecycle.md); this settles what is left.
//
//   - agent_end_backfill lists the threads that hold an open row under a
//     parent when the migration runs, with that time. No session runs at
//     open, so every agent then was the previous process's.
//   - The deferred phase (settleEndedAgentRows) reads, per listed thread,
//     the agents owning its open rows, and settles each ended agent's open
//     rows in its own write, by the rule the live path gives its end
//     (DeferredHost.AgentEndRule). A thread leaves the list once each of
//     its agents is settled or found running, so a quit resumes from what
//     is left.
const endedAgentRowsV124SQL = `
CREATE TABLE agent_end_backfill (
    thread_id TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
    opened_at INTEGER NOT NULL
) WITHOUT ROWID;

INSERT INTO agent_end_backfill(thread_id, opened_at)
SELECT DISTINCT thread_id, CAST(unixepoch('subsec') * 1000 AS INTEGER)
  FROM items
 WHERE status IN ('running', 'streaming') AND parent_id <> '';
`

// The status and source of the sibling the boot sweep writes for an agent
// whose session died with the previous process
// (triage.RecoverOrphanedBackgroundTasks).
const (
	sessionDiedStatus = "killed"
	sessionDiedSource = "session_died"
)

// settleEndedAgentRows is migration v124's deferred phase. For each thread
// agent_end_backfill lists, it settles the open rows of every agent that
// owns one and has ended (endedAgentEnd), one agent per paced write, and
// removes the thread from the list when all of them are done. An agent
// that has not ended is left to its own end. A failing agent is reported
// and keeps its thread listed for the next run; the agents and threads
// after it go on.
func settleEndedAgentRows(ctx context.Context, s *Store, run *deferredRun) error {
	after := ""
	writes := 0
	for {
		threadID, openedAt, err := s.nextAgentEndBackfillThread(ctx, after)
		if err != nil || threadID == "" {
			return err
		}
		after = threadID
		if run.host.AgentEndRule == nil {
			return errors.New("the host supplies no agent end rule")
		}
		owners, err := s.openRowOwners(ctx, threadID)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			run.fail(fmt.Errorf("read the agents of thread %s: %w", threadID, err))
			continue
		}
		settled := true
		for _, owner := range owners {
			if writes > 0 {
				run.pause()
			}
			if ctx.Err() != nil {
				return nil
			}
			writes++
			if err := s.settleEndedAgent(threadID, owner, openedAt, run.host.AgentEndRule); err != nil {
				run.fail(fmt.Errorf("settle the rows of agent %s/%s: %w", threadID, owner, err))
				settled = false
			}
		}
		if !settled {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM agent_end_backfill WHERE thread_id = ?`, threadID); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			run.fail(fmt.Errorf("finish thread %s: %w", threadID, err))
		}
	}
}

// nextAgentEndBackfillThread names the first listed thread after `after`,
// with the time the migration listed it, or "" when none follows it.
func (s *Store) nextAgentEndBackfillThread(ctx context.Context, after string) (string, int64, error) {
	var threadID string
	var openedAt int64
	err := s.reader().QueryRowContext(ctx,
		`SELECT thread_id, opened_at FROM agent_end_backfill WHERE thread_id > ? ORDER BY thread_id LIMIT 1`,
		after).Scan(&threadID, &openedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("store: next agent end backfill thread: %w", err)
	}
	return threadID, openedAt, nil
}

// openRowOwners lists, in id order, the agents that own an open row of
// the thread.
func (s *Store) openRowOwners(ctx context.Context, threadID string) ([]string, error) {
	rows, err := s.reader().QueryContext(ctx, `SELECT DISTINCT parent_id FROM items
 WHERE thread_id = ? AND status IN ('running', 'streaming') AND parent_id <> ''`, threadID)
	if err != nil {
		return nil, fmt.Errorf("store: select the parents of open rows of %s: %w", threadID, err)
	}
	var parents []string
	for rows.Next() {
		var parent string
		if err := rows.Scan(&parent); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan a parent of open rows of %s: %w", threadID, err)
		}
		parents = append(parents, parent)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: iterate the parents of open rows of %s: %w", threadID, err)
	}
	rows.Close()
	owners, err := agentOwners(s.reader(), threadID, parents)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, parent := range parents {
		if owner := owners[parent]; owner != "" && !slices.Contains(out, owner) {
			out = append(out, owner)
		}
	}
	slices.Sort(out)
	return out, nil
}

// settleEndedAgent settles the open rows of agent ownerID when it has
// ended, in one item write that decides whether it has.
func (s *Store) settleEndedAgent(threadID, ownerID string, openedAt int64, ruleFor func(status, source string) AgentEndRule) error {
	return s.bulkWriteItems(threadID, "ended agent settle", func(tx *sql.Tx, w *cardWrite) error {
		status, source, ended, err := endedAgentEnd(tx, threadID, ownerID, openedAt)
		if err != nil || !ended {
			return err
		}
		_, err = settleAgentSubtreeTx(tx, w, ownerID, ruleFor(status, source), time.Now().UnixMilli())
		return err
	})
}

// agentLatestLifecycleSQL reads agent ?2's latest lifecycle row: the
// owner, or the latest resume carrier rooted at it (claude-wire.md §E6).
const agentLatestLifecycleSQL = `SELECT id, created_at, turn_index FROM (
  SELECT id, created_at, turn_index, item_index FROM items WHERE thread_id = ?1 AND id = ?2
  UNION ALL
  SELECT id, created_at, turn_index, item_index FROM items
   WHERE thread_id = ?1 AND kind = 'tool_call'
     AND CASE WHEN json_valid(meta) THEN json_extract(meta, '$.transcript_root_id') END = ?2
) ORDER BY turn_index DESC, item_index DESC LIMIT 1`

// latestCompletionSQL reads the status and source of row ?2's latest
// completion sibling.
const latestCompletionSQL = `SELECT status,
       CASE WHEN json_valid(meta) THEN COALESCE(json_extract(meta, '$.status_source'), '') ELSE '' END
  FROM items WHERE thread_id = ? AND completion_of = ? AND completion_of <> ''
 ORDER BY turn_index DESC, item_index DESC LIMIT 1`

// endedAgentEnd reports whether agent ownerID has ended and, when it has,
// the status and source of its end. Its latest lifecycle row decides: a
// completion sibling of that row is the end; without one, a row older than
// openedAt belonged to the previous process, whose session died with it,
// and ends as the boot sweep ends such an agent. A newer row is a live
// session's, and its agent runs. A dead session's turn the crash sweep
// has not closed yet is an error, so the thread stays listed for the run
// after the sweep.
func endedAgentEnd(tx *sql.Tx, threadID, ownerID string, openedAt int64) (status, source string, ended bool, err error) {
	var lifecycleID string
	var createdAt int64
	var turnIndex int
	err = tx.QueryRow(agentLatestLifecycleSQL, threadID, ownerID).Scan(&lifecycleID, &createdAt, &turnIndex)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("store: read the lifecycle of agent %s/%s: %w", threadID, ownerID, err)
	}
	err = tx.QueryRow(latestCompletionSQL, threadID, lifecycleID).Scan(&status, &source)
	if err == nil {
		return status, source, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", "", false, fmt.Errorf("store: read the end of agent %s/%s: %w", threadID, lifecycleID, err)
	}
	if createdAt >= openedAt {
		return "", "", false, nil
	}
	var turnOpen bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM turns WHERE thread_id = ? AND turn_index = ? AND completed_at IS NULL)`,
		threadID, turnIndex).Scan(&turnOpen); err != nil {
		return "", "", false, fmt.Errorf("store: read the turn of agent %s/%s: %w", threadID, lifecycleID, err)
	}
	if turnOpen {
		return "", "", false, fmt.Errorf("store: turn %d of agent %s/%s is still open", turnIndex, threadID, lifecycleID)
	}
	return sessionDiedStatus, sessionDiedSource, true, nil
}
