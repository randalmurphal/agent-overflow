package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Agent-owned rows. A background agent owns every row written under its
// launch, whichever turn wrote it, down to any background launch inside
// it, which owns its own (docs/architecture/turn-lifecycle.md, §Agent-owned
// rows). A turn's end, a Stop and the crash sweep of a turn settle only
// the rows no agent owns; an agent's rows are settled by what ends the
// agent: its completion sibling, written with UpsertAgentEnd.
//
// An owner is a background tool call other than a Codex spawn card. A
// Codex child's rows are settled by the parent turn as before: its spawn
// card never changes after the spawn and no Codex completion ends a
// child's rows.

// agentOwnerSQL is the predicate of a row that owns the rows under it. `a`
// is the row reference with its trailing dot.
func agentOwnerSQL(a string) string {
	return a + "kind = 'tool_call' AND " + a + "is_background = 1 AND " + a + "tool_name <> 'collab_agent'"
}

// agentOwnerRowSQL reads what agentOwners asks about one row.
const agentOwnerRowSQL = `SELECT parent_id, kind = 'tool_call' AND is_background = 1 AND tool_name <> 'collab_agent'
  FROM items WHERE thread_id = ? AND id = ?`

// agentOwners reports, for each distinct non-empty parent id, the agent
// that owns the rows under it: the id itself when it is an owner, else its
// nearest owner ancestor, or "" when no agent owns them. It walks each
// chain up once, by primary key, sharing what earlier walks learned, so
// the cost is the distinct rows on the chains asked about. A parent id
// with no row owns nothing. A cycle is an error: the parent_id invariant
// forbids one.
func agentOwners(q sqlQueryer, threadID string, parentIDs []string) (map[string]string, error) {
	owners := make(map[string]string, len(parentIDs))
	for _, start := range parentIDs {
		if start == "" {
			continue
		}
		if _, known := owners[start]; known {
			continue
		}
		var chain []string
		seen := make(map[string]bool)
		result := ""
		for id := start; id != ""; {
			if known, ok := owners[id]; ok {
				result = known
				break
			}
			if seen[id] {
				return nil, fmt.Errorf("store: parent chain of %s/%s loops at %s", threadID, start, id)
			}
			seen[id] = true
			chain = append(chain, id)
			var parentID string
			var owner bool
			err := q.QueryRow(agentOwnerRowSQL, threadID, id).Scan(&parentID, &owner)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("store: read parent chain of %s/%s at %s: %w", threadID, start, id, err)
			}
			if owner {
				result = id
				break
			}
			id = parentID
		}
		for _, id := range chain {
			owners[id] = result
		}
	}
	return owners, nil
}

// withoutAgentOwned drops the rows an agent owns from rows, in order.
func withoutAgentOwned[T any](q sqlQueryer, threadID string, rows []T, parentOf func(T) string) ([]T, error) {
	var parents []string
	for _, row := range rows {
		if parent := parentOf(row); parent != "" {
			parents = append(parents, parent)
		}
	}
	if len(parents) == 0 {
		return rows, nil
	}
	owners, err := agentOwners(q, threadID, parents)
	if err != nil {
		return nil, err
	}
	kept := rows[:0]
	for _, row := range rows {
		if owners[parentOf(row)] == "" {
			kept = append(kept, row)
		}
	}
	return kept, nil
}

// AgentOwners reports the agent that owns the rows under each of the
// given parent ids, or "" for a parent id no agent owns (agentOwners).
// Triage asks it at a turn boundary about the scopes it holds open work
// in, and at an agent's end about the scopes of its open prompts.
func (s *Store) AgentOwners(threadID string, scopes []string) (map[string]string, error) {
	return agentOwners(s.reader(), threadID, scopes)
}

// AgentEndRule is what an agent's end makes of the rows it left open. A
// streaming row completes when StreamingCompletes is set and is errored
// otherwise; a running row is always errored. An errored row's summary is
// Summarise of its summary.
type AgentEndRule struct {
	StreamingCompletes bool
	Summarise          func(string) string
}

// SettledRow is one row an agent's end settled: the row as written, with
// the status and summary it had before.
type SettledRow struct {
	Item       Item
	WasStatus  string
	WasSummary string
}

// agentSubtreeSQL selects cols of the rows agent ?2 owns whose status is
// in statuses: the rows under it, through every row that is not an owner
// itself. The walk reads idx_items_parent once per row of the subtree.
func agentSubtreeSQL(cols, statuses string) string {
	return `WITH RECURSIVE owned(id) AS (
  SELECT id FROM items WHERE thread_id = ?1 AND parent_id = ?2 AND parent_id <> '' AND NOT (` + agentOwnerSQL("") + `)
  UNION ALL
  SELECT c.id FROM owned JOIN items c ON c.thread_id = ?1 AND c.parent_id = owned.id AND c.parent_id <> ''
   WHERE NOT (` + agentOwnerSQL("c.") + `)
)
SELECT ` + cols + ` FROM owned JOIN items i ON i.thread_id = ?1 AND i.id = owned.id
 WHERE i.status IN (` + statuses + `)
 ORDER BY i.turn_index, i.item_index`
}

// agentSubtreeUnsettledSQL selects the open rows agent ?2 owns.
var agentSubtreeUnsettledSQL = agentSubtreeSQL(subagentRowColumns("i."), "'running', 'streaming'")

// agentSubtreeStreamingSQL selects the streaming rows agent ?2 owns, with
// the provider item id their live stream is keyed by.
var agentSubtreeStreamingSQL = agentSubtreeSQL(
	"i.id, i.kind, i.turn_index, i.parent_id, "+
		"CASE WHEN json_valid(i.meta) THEN COALESCE(json_extract(i.meta, '$.provider_item_id'), '') ELSE '' END",
	"'streaming'",
)

// AgentStream is a streaming row an agent owns, with what triage keys its
// live stream by.
type AgentStream struct {
	ID             string
	Kind           string
	TurnIndex      int
	ParentID       string
	ProviderItemID string
}

// AgentStreams lists the streaming rows agent rootID owns, in timeline
// order. Triage settles the streams it still holds before the agent's end
// settles the rest (UpsertAgentEnd).
func (s *Store) AgentStreams(threadID, rootID string) ([]AgentStream, error) {
	rows, err := s.reader().Query(agentSubtreeStreamingSQL, threadID, rootID)
	if err != nil {
		return nil, fmt.Errorf("store: select streaming rows of agent %s/%s: %w", threadID, rootID, err)
	}
	defer rows.Close()
	var streams []AgentStream
	for rows.Next() {
		var st AgentStream
		if err := rows.Scan(&st.ID, &st.Kind, &st.TurnIndex, &st.ParentID, &st.ProviderItemID); err != nil {
			return nil, fmt.Errorf("store: scan streaming row of agent %s/%s: %w", threadID, rootID, err)
		}
		streams = append(streams, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate streaming rows of agent %s/%s: %w", threadID, rootID, err)
	}
	return streams, nil
}

// settleAgentSubtreeTx settles every open row agent rootID owns, by rule,
// inside the caller's item write.
func settleAgentSubtreeTx(tx *sql.Tx, w *cardWrite, rootID string, rule AgentEndRule, now int64) ([]SettledRow, error) {
	threadID := w.threadID
	rows, err := tx.Query(agentSubtreeUnsettledSQL, threadID, rootID)
	if err != nil {
		return nil, fmt.Errorf("store: select open rows of agent %s/%s: %w", threadID, rootID, err)
	}
	var open []subagentRow
	for rows.Next() {
		row, err := scanSubagentRow(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan open row of agent %s/%s: %w", threadID, rootID, err)
		}
		open = append(open, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: iterate open rows of agent %s/%s: %w", threadID, rootID, err)
	}
	rows.Close()
	if len(open) == 0 {
		return nil, nil
	}

	settled := make([]SettledRow, 0, len(open))
	for _, old := range open {
		row := old
		row.status, row.summary = "errored", rule.Summarise(old.summary)
		if old.status == "streaming" && rule.StreamingCompletes {
			row.status, row.summary = "completed", old.summary
		}
		if err := w.updated(old, row); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(
			`UPDATE items SET status = ?, summary = ?, updated_at = ?
			  WHERE thread_id = ? AND id = ?`,
			row.status, row.summary, now, threadID, old.id,
		); err != nil {
			return nil, fmt.Errorf("store: settle row %s/%s of agent %s: %w", threadID, old.id, rootID, err)
		}
		if err := indexSettledItemTx(tx, threadID, old.id, old.kind, row.status, row.summary); err != nil {
			return nil, err
		}
		settled = append(settled, SettledRow{WasStatus: old.status, WasSummary: old.summary})
	}
	if err := w.finish(); err != nil {
		return nil, err
	}
	for i, old := range open {
		item, err := readBackItemTx(tx, threadID, old.id)
		if err != nil {
			return nil, fmt.Errorf("store: read settled row %s/%s: %w", threadID, old.id, err)
		}
		settled[i].Item = item
	}
	return settled, nil
}

// UpsertAgentEnd writes an agent's completion sibling and settles the rows
// the agent left open in the same transaction, so no process death can
// leave an ended agent's rows running. rootID is the agent's transcript
// root, where every round's rows are parented. The sibling is written like
// UpsertItem; it carries no card, and the write recomputes the chains it
// touched.
func (s *Store) UpsertAgentEnd(item Item, payload *Payload, rootID string, rule AgentEndRule, now int64) (Item, []SettledRow, error) {
	applyItemDefaults(&item)
	item.SubagentCard = nil
	var persisted Item
	var settled []SettledRow
	err := s.bulkWriteItems(item.ThreadID, "agent end", func(tx *sql.Tx, w *cardWrite) error {
		if err := upsertPayload(tx, payload, &item); err != nil {
			return err
		}
		var err error
		if persisted, err = writeItemAndReadBack(tx, w, &item, nextItemIndexTx); err != nil {
			return err
		}
		settled, err = settleAgentSubtreeTx(tx, w, rootID, rule, now)
		return err
	})
	if err != nil {
		return Item{}, nil, err
	}
	return persisted, settled, nil
}
