package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// A thread's tallies are what an agent row counts of its whole life, as a
// subagent's row counts its run (docs/specs/standing-agents.md, "Agents
// segment"): the tool calls it executed, the tokens it and its native
// subagents spent, and the time it spent in turns. `thread_tallies` holds
// them per thread, each kept where the work is first recorded as the
// thread's own, in that write's transaction (migration v154):
//
//   - trg_items_tally_tool_calls counts each top-level `tool_call` row the
//     thread's own writers insert. Every copy of history into a thread, a
//     pointer fork's snapshot of a running cut turn, a copy-on-write
//     localization, a history repair or an import, runs under
//     threads.history_bulk_load and counts nothing; a restore runs with the
//     triggers dropped. A row a revert or a retention sweep later removes
//     stays counted: the count is of what the thread did.
//   - trg_usage_ledger_tally_tokens adds each settled turn's spend
//     (usageSpendSQLPlain) for a thread that exists. A provider whose turn
//     usage includes its native subagents' (Claude's modelUsage) counts
//     them here.
//   - trg_items_tally_agent_tokens adds the spend of a native subagent whose
//     usage is outside its parent's turn usage (a Codex child), from the
//     final numbers its completion record carries
//     (provider.SubagentProgressMeta.OutsideParentUsage): what that record
//     adds over the launch's earlier completions, since a child's figure is
//     cumulative.
//   - turn_ms is the time the thread's turns ran, from start to end, added
//     by the turn owner's writes (UpdateTurnCompleted, ReopenTurn and the
//     late payload's completion advance) as each moves a turn's end
//     (tallyTurnSpanTx). Time between turns is not in it; a crashed turn the
//     boot settles, and a fork's copy of its source's turns, add nothing.
//
// A read adds what the running turn has done so far: its usage_pending
// tokens and the time since it started. Each read is a probe of the tally,
// the pending rows and the open turn per thread, whatever the thread's
// size. The row cascades with its thread.
var threadTalliesV154SQL = `
CREATE TABLE thread_tallies (
    thread_id  TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
    tool_calls INTEGER NOT NULL DEFAULT 0,
    tokens     INTEGER NOT NULL DEFAULT 0,
    turn_ms    INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;

` + threadTallyTriggersSQL + `
INSERT INTO thread_tallies (thread_id, tool_calls, tokens, turn_ms)
SELECT thread_id, SUM(tool_calls), SUM(tokens), SUM(turn_ms) FROM (
    SELECT i.thread_id, COUNT(*) AS tool_calls, 0 AS tokens, 0 AS turn_ms FROM items i
     WHERE i.kind = 'tool_call' AND i.parent_id = '' AND ` + threadOwnTurnSQL("i") + `
     GROUP BY i.thread_id
    UNION ALL
    SELECT thread_id, 0, SUM(` + usageSpendSQLPlain + `), 0 FROM usage_ledger
     WHERE thread_id IN (SELECT id FROM threads) GROUP BY thread_id
    UNION ALL
    SELECT i.thread_id, 0, 0, SUM(MAX(0, i.completed_at - i.started_at)) FROM turns i
     WHERE i.completed_at IS NOT NULL AND ` + threadOwnTurnSQL("i") + `
     GROUP BY i.thread_id
) GROUP BY thread_id;
`

// threadOwnTurnSQL is whether the row alias names, of a thread's items or
// turns, is one the thread ran rather than one a pointer fork inherited
// through its cut: the migration's reading of a thread's past, where the
// history_bulk_load the copies ran under is no longer visible.
func threadOwnTurnSQL(alias string) string {
	return `NOT EXISTS (SELECT 1 FROM threads f WHERE f.id = ` + alias + `.thread_id
	           AND f.fork_source_thread_id <> '' AND ` + alias + `.turn_index <= f.fork_cut_turn_index)`
}

// threadTallyTriggersSQL keeps thread_tallies. A restore runs its copy
// without them, so the snapshot's tallies land as they are, and installs
// them again from here.
const threadTallyTriggersSQL = `
CREATE TRIGGER trg_items_tally_tool_calls AFTER INSERT ON items
WHEN NEW.kind = 'tool_call' AND NEW.parent_id = ''
 AND (SELECT history_bulk_load FROM threads WHERE id = NEW.thread_id) = 0
BEGIN
    INSERT INTO thread_tallies (thread_id, tool_calls) VALUES (NEW.thread_id, 1)
    ON CONFLICT (thread_id) DO UPDATE SET tool_calls = tool_calls + 1;
END;

CREATE TRIGGER trg_items_tally_agent_tokens AFTER INSERT ON items
WHEN NEW.completion_of <> '' AND json_valid(NEW.meta)
 AND json_extract(NEW.meta, '$.subagentProgress.outsideParentUsage') = 1
 AND (SELECT history_bulk_load FROM threads WHERE id = NEW.thread_id) = 0
BEGIN
    INSERT INTO thread_tallies (thread_id, tokens)
    VALUES (NEW.thread_id, MAX(0, COALESCE(json_extract(NEW.meta, '$.subagentProgress.totalTokens'), 0)
        - COALESCE((SELECT MAX(json_extract(o.meta, '$.subagentProgress.totalTokens')) FROM items o
                     WHERE o.thread_id = NEW.thread_id AND o.completion_of = NEW.completion_of AND o.completion_of <> ''
                       AND o.id <> NEW.id AND json_valid(o.meta)), 0)))
    ON CONFLICT (thread_id) DO UPDATE SET tokens = tokens + excluded.tokens;
END;

CREATE TRIGGER trg_usage_ledger_tally_tokens AFTER INSERT ON usage_ledger
WHEN EXISTS (SELECT 1 FROM threads WHERE id = NEW.thread_id)
BEGIN
    INSERT INTO thread_tallies (thread_id, tokens) VALUES (NEW.thread_id, ` + usageSpendSQLNew + `)
    ON CONFLICT (thread_id) DO UPDATE SET tokens = tokens + excluded.tokens;
END;
`

// dropThreadTallyTriggersSQL removes the triggers above.
const dropThreadTallyTriggersSQL = `DROP TRIGGER IF EXISTS trg_items_tally_tool_calls;
DROP TRIGGER IF EXISTS trg_items_tally_agent_tokens;
DROP TRIGGER IF EXISTS trg_usage_ledger_tally_tokens;`

// usageSpendSQLPlain is a usage row's token spend, as a subagent's row
// counts its agent's (provider.SubagentProgressMeta.TotalTokens): fresh
// input, cache writes and output. A cached prompt read again is no spend;
// counting it would grow with each round rather than with the work.
const usageSpendSQLPlain = `input_tokens + cache_creation_input_tokens + output_tokens`

// usageSpendSQLNew is usageSpendSQLPlain over a trigger's NEW row.
const usageSpendSQLNew = `NEW.input_tokens + NEW.cache_creation_input_tokens + NEW.output_tokens`

// ThreadTally is what one thread did over its life: the tool calls it
// executed, the tokens it and its native subagents spent and the time its
// turns ran, the running turn's included as of the read. Running is
// whether a turn runs now, whose time grows from the read on.
type ThreadTally struct {
	ToolCalls int64
	Tokens    int64
	RuntimeMs int64
	Running   bool
}

// threadTallyColumnsSQL is the four columns of the tally of the thread id
// names, tool calls, tokens, runtime and whether a turn runs, with now in
// milliseconds as the parameter now names: one probe of thread_tallies, one
// of the running turn's usage_pending rows and one of its open turn.
func threadTallyColumnsSQL(id, now string) string {
	open := `(SELECT started_at FROM turns WHERE thread_id = ` + id + ` AND completed_at IS NULL ORDER BY turn_index DESC LIMIT 1)`
	return `COALESCE((SELECT tool_calls FROM thread_tallies WHERE thread_id = ` + id + `), 0),
		COALESCE((SELECT tokens FROM thread_tallies WHERE thread_id = ` + id + `), 0)
		+ COALESCE((SELECT SUM(` + usageSpendSQLPlain + `) FROM usage_pending WHERE thread_id = ` + id + `), 0),
		COALESCE((SELECT turn_ms FROM thread_tallies WHERE thread_id = ` + id + `), 0)
		+ COALESCE(MAX(0, ` + now + ` - ` + open + `), 0),
		` + open + ` IS NOT NULL`
}

// ThreadTallies reads the tallies of threadIDs at now, keyed by thread; a
// thread that has done nothing yet is left out.
func (s *Store) ThreadTallies(threadIDs []string, now time.Time) (map[string]ThreadTally, error) {
	out := map[string]ThreadTally{}
	if len(threadIDs) == 0 {
		return out, nil
	}
	ids, err := json.Marshal(threadIDs)
	if err != nil {
		return nil, fmt.Errorf("store: read thread tallies: %w", err)
	}
	rows, err := s.reader().Query(`SELECT ids.value, `+threadTallyColumnsSQL("ids.value", "?2")+`
		  FROM (SELECT DISTINCT value FROM json_each(?1)) AS ids`, string(ids), now.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("store: read thread tallies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var tally ThreadTally
		if err := rows.Scan(&id, &tally.ToolCalls, &tally.Tokens, &tally.RuntimeMs, &tally.Running); err != nil {
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

// AgentCompletionSpend reads, of threadID's native subagent launches,
// the largest spend each one's completion records carry whose usage is
// outside the thread's own (trg_items_tally_agent_tokens): what the tally
// already counts of a launch that runs again. A launch with none is left
// out.
func (s *Store) AgentCompletionSpend(threadID string, launches []string) (map[string]int64, error) {
	out := map[string]int64{}
	if len(launches) == 0 {
		return out, nil
	}
	ids, err := json.Marshal(launches)
	if err != nil {
		return nil, fmt.Errorf("store: read the agents' counted spend: %w", err)
	}
	rows, err := s.reader().Query(`SELECT o.completion_of, MAX(json_extract(o.meta, '$.subagentProgress.totalTokens'))
		  FROM json_each(?2) j CROSS JOIN items o
		 WHERE o.thread_id = ?1 AND o.completion_of = j.value AND o.completion_of <> '' AND json_valid(o.meta)
		   AND json_extract(o.meta, '$.subagentProgress.outsideParentUsage') = 1
		 GROUP BY o.completion_of`, threadID, string(ids))
	if err != nil {
		return nil, fmt.Errorf("store: read the agents' counted spend: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var launch string
		var spend sql.NullInt64
		if err := rows.Scan(&launch, &spend); err != nil {
			return nil, fmt.Errorf("store: scan an agent's counted spend: %w", err)
		}
		if spend.Valid {
			out[launch] = spend.Int64
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read the agents' counted spend: %w", err)
	}
	return out, nil
}

// tallyTurnSpanTx runs write, a turn owner's write of turnID's end, and
// adds what it moved the turn's span by to its thread's turn_ms: a turn
// that ends adds its span, one reopened takes it back until it ends again,
// and an end that advances adds the difference.
func tallyTurnSpanTx(tx *sql.Tx, turnID string, write func() error) error {
	span := func() (string, int64, error) {
		var thread string
		var ms int64
		err := tx.QueryRow(`SELECT thread_id, CASE WHEN completed_at IS NULL THEN 0 ELSE MAX(0, completed_at - started_at) END
			  FROM turns WHERE turn_id = ?`, turnID).Scan(&thread, &ms)
		if err == sql.ErrNoRows {
			return "", 0, nil
		}
		return thread, ms, err
	}
	thread, before, err := span()
	if err != nil {
		return fmt.Errorf("store: read the span of turn %s: %w", turnID, err)
	}
	if err := write(); err != nil {
		return err
	}
	_, after, err := span()
	if err != nil {
		return fmt.Errorf("store: read the span of turn %s: %w", turnID, err)
	}
	if thread == "" || after == before {
		return nil
	}
	if _, err := tx.Exec(`INSERT INTO thread_tallies (thread_id, turn_ms) VALUES (?1, MAX(0, ?2))
		ON CONFLICT (thread_id) DO UPDATE SET turn_ms = MAX(0, turn_ms + ?2)`, thread, after-before); err != nil {
		return fmt.Errorf("store: count the span of turn %s: %w", turnID, err)
	}
	return nil
}
