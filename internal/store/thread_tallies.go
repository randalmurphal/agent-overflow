package store

import (
	"encoding/json"
	"fmt"
)

// A thread's tallies are what an agent row counts of its whole life, as a
// subagent's row counts its run (docs/specs/standing-agents.md, "Agents
// segment"): its top-level tool calls and the tokens its turns spent.
// `thread_tallies` holds them per thread, kept by triggers in the
// transaction of every write that adds to them (migration v154):
//
//   - trg_items_tally_tool_calls counts each top-level `tool_call` row
//     written into the thread, a row a revert or a retention sweep later
//     removes included: the count is of what the thread did.
//   - trg_usage_ledger_tally_tokens adds each settled turn's tokens
//     (usageTotalTokensSQL) for a thread that exists; the ledger keeps rows
//     of deleted threads, which tally nothing.
//
// A turn still running has its tokens in usage_pending, which a read adds,
// so the count grows while the thread works. Each read is one probe of the
// tally and one of the pending rows per thread, whatever the thread's size.
// The row cascades with its thread.
const threadTalliesV154SQL = `
CREATE TABLE thread_tallies (
    thread_id  TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
    tool_calls INTEGER NOT NULL DEFAULT 0,
    tokens     INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;

` + threadTallyTriggersSQL + `
INSERT INTO thread_tallies (thread_id, tool_calls, tokens)
SELECT thread_id, SUM(tool_calls), SUM(tokens) FROM (
    SELECT thread_id, COUNT(*) AS tool_calls, 0 AS tokens FROM items
     WHERE kind = 'tool_call' AND parent_id = '' GROUP BY thread_id
    UNION ALL
    SELECT thread_id, 0, SUM(` + usageTotalTokensSQLPlain + `) FROM usage_ledger
     WHERE thread_id IN (SELECT id FROM threads) GROUP BY thread_id
) GROUP BY thread_id;
`

// threadTallyTriggersSQL keeps thread_tallies. A restore runs its copy
// without them, so the snapshot's tallies land as they are, and installs
// them again from here.
const threadTallyTriggersSQL = `
CREATE TRIGGER trg_items_tally_tool_calls AFTER INSERT ON items
WHEN NEW.kind = 'tool_call' AND NEW.parent_id = ''
BEGIN
    INSERT INTO thread_tallies (thread_id, tool_calls) VALUES (NEW.thread_id, 1)
    ON CONFLICT (thread_id) DO UPDATE SET tool_calls = tool_calls + 1;
END;

CREATE TRIGGER trg_usage_ledger_tally_tokens AFTER INSERT ON usage_ledger
WHEN EXISTS (SELECT 1 FROM threads WHERE id = NEW.thread_id)
BEGIN
    INSERT INTO thread_tallies (thread_id, tokens) VALUES (NEW.thread_id, ` + usageTotalTokensSQLNew + `)
    ON CONFLICT (thread_id) DO UPDATE SET tokens = tokens + excluded.tokens;
END;
`

// dropThreadTallyTriggersSQL removes the two triggers above.
const dropThreadTallyTriggersSQL = `DROP TRIGGER IF EXISTS trg_items_tally_tool_calls;
DROP TRIGGER IF EXISTS trg_usage_ledger_tally_tokens;`

// usageTotalTokensSQLPlain is a usage row's total tokens, the one sum every
// token total reads: what the thread read, cached or not, and wrote.
const usageTotalTokensSQLPlain = `input_tokens + output_tokens + cache_read_input_tokens + cache_creation_input_tokens`

// usageTotalTokensSQLNew is usageTotalTokensSQLPlain over a trigger's NEW row.
const usageTotalTokensSQLNew = `NEW.input_tokens + NEW.output_tokens + NEW.cache_read_input_tokens + NEW.cache_creation_input_tokens`

// ThreadTally is what one thread did over its life: its top-level tool
// calls and the tokens its turns spent, the running turn's included.
type ThreadTally struct {
	ToolCalls int64
	Tokens    int64
}

// ThreadTallies reads the tallies of threadIDs, keyed by thread; a thread
// that has done nothing yet is left out.
func (s *Store) ThreadTallies(threadIDs []string) (map[string]ThreadTally, error) {
	out := map[string]ThreadTally{}
	if len(threadIDs) == 0 {
		return out, nil
	}
	ids, err := json.Marshal(threadIDs)
	if err != nil {
		return nil, fmt.Errorf("store: read thread tallies: %w", err)
	}
	rows, err := s.reader().Query(`SELECT ids.value, COALESCE(t.tool_calls, 0),
		COALESCE(t.tokens, 0) + COALESCE((SELECT SUM(`+usageTotalTokensSQLPlain+`) FROM usage_pending p WHERE p.thread_id = ids.value), 0)
		  FROM (SELECT DISTINCT value FROM json_each(?)) AS ids
		  LEFT JOIN thread_tallies t ON t.thread_id = ids.value`, string(ids))
	if err != nil {
		return nil, fmt.Errorf("store: read thread tallies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var tally ThreadTally
		if err := rows.Scan(&id, &tally.ToolCalls, &tally.Tokens); err != nil {
			return nil, fmt.Errorf("store: scan thread tally: %w", err)
		}
		if tally != (ThreadTally{}) {
			out[id] = tally
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read thread tallies: %w", err)
	}
	return out, nil
}
