package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// A thread's tallies are what an agent row counts of its whole life, as a
// subagent's row counts its run (docs/specs/standing-agents.md, "Agents
// segment"): the tool calls it executed, the tokens it spent itself, and the
// time it spent in turns. Its native subagents are rows of their own with
// counts of their own, so none of theirs is in it. `thread_tallies` holds
// them per thread, each kept where the work is first recorded as the
// thread's own, in that write's transaction (migration v155):
//
//   - trg_items_tally_tool_calls counts each top-level `tool_call` row the
//     thread's own writers insert. Every copy of history into a thread, a
//     pointer fork's snapshot of a running cut turn, a copy-on-write
//     localization, a history repair, a session import or a transfer's
//     import, runs under threads.history_bulk_load and counts nothing; a
//     restore runs with the triggers dropped. A moved thread's archive
//     carries its tallies, which its import installs in place of the
//     destination's (installTransferTalliesTx); a copy starts at zero. A
//     row a revert or a retention sweep later removes stays counted: the
//     count is of what the thread did.
//   - tokens is the thread's own spend (provider.TokenUsage.Spend), added
//     by the usage settlement that accounts it (AppendUsageAndReconcile,
//     OwnSpend), from the figure the provider reports as the thread's own
//     (provider.WireTurnCompleteMeta.OwnUsage): Claude's flat result.usage,
//     not its subagent-inclusive modelUsage, which the billing ledger keeps;
//     a Codex turn's usage, which never carries its children's.
//   - turn_ms is the time the thread's turns ran, from start to end, added
//     by the writes that move a turn's end (tallyTurnSpanTx):
//     UpdateTurnCompleted, ReopenTurn, the late payload's completion
//     advance, and the boot's settle of a turn a crash left open
//     (RecoverCrashedTurns), which ends it at its last recorded activity,
//     so the time the app was down is not in it. Each adds what it moved
//     the span by, so a crashed turn later reopened and ended again counts
//     once. Time between turns is not in it, nor is a fork's copy of its
//     source's turns.
//
// A read adds what the running turn has done so far: its usage_pending
// tokens, which are the thread's own as well (a subagent's live usage is
// not reported there), and the time since it started. Each read is a probe of the tally,
// the pending rows and the open turn per thread, whatever the thread's
// size. The row cascades with its thread.
//
// The migration's count reads items through two covering index scans and
// no table row: the tool calls from idx_items_top_level, whose predicate
// the arm states so the planner can use it, and the turns that ran a
// nested row from idx_items_parent (ledgerOwnSpendSQL). Without either the
// planner reads every row from the table
// (TestThreadTalliesMigrationReadsCoveringIndexes).
var threadTalliesV155SQL = `
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
     WHERE i.kind = 'tool_call' AND i.parent_id = '' AND NOT (i.kind = 'notification' AND i.tool_name = 'plan_update')
       AND ` + threadOwnTurnSQL("i") + `
     GROUP BY i.thread_id
    UNION ALL
    SELECT l.thread_id, 0, SUM(` + usageSpendSQLPlain + `), 0 FROM usage_ledger l
     WHERE l.thread_id IN (SELECT id FROM threads) AND ` + ledgerOwnSpendSQL("l") + `
     GROUP BY l.thread_id
    UNION ALL
    SELECT t.thread_id, 0, 0, SUM(MAX(0, t.completed_at - t.started_at)) FROM turns t
     WHERE t.completed_at IS NOT NULL AND ` + threadOwnTurnSQL("t") + `
     GROUP BY t.thread_id
) GROUP BY thread_id;
`

// ledgerOwnSpendSQL is whether the ledger row alias names is the thread's
// own spend, as far as the ledger can say: a Codex row is, and a Claude
// row is when its turn ran no native subagent, whose usage Claude's
// modelUsage includes and which the turn's items show as rows nested under
// its launch. A Claude turn that ran one recorded no parent-only figure, so
// the migration counts none of it. The turns that ran one are listed once,
// from the distinct turns of the nested rows, and each ledger row probes
// that list by its turn id; a turn without an id stays out of it, as a
// NULL would make NOT IN unknown for every row. The list names
// idx_items_parent: without sqlite_stat1 the planner takes the distinct
// turns in the order idx_items_thread_turn_item_unique gives and reads each
// row from the table.
func ledgerOwnSpendSQL(alias string) string {
	return `(` + alias + `.provider <> 'claude' OR ` + alias + `.turn_id NOT IN (SELECT t.turn_id
	           FROM (SELECT DISTINCT n.thread_id, n.turn_index FROM items n INDEXED BY idx_items_parent WHERE n.parent_id <> '') AS nested
	          CROSS JOIN turns t ON t.thread_id = nested.thread_id AND t.turn_index = nested.turn_index
	          WHERE t.turn_id IS NOT NULL))`
}

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
`

// dropThreadTallyTriggersSQL removes the trigger above.
const dropThreadTallyTriggersSQL = `DROP TRIGGER IF EXISTS trg_items_tally_tool_calls;`

// usageSpendSQLPlain is a usage row's token spend, as
// provider.TokenUsage.Spend counts it: fresh input, cache writes and output.
const usageSpendSQLPlain = `input_tokens + cache_creation_input_tokens + output_tokens`

// OwnSpend is a usage settlement's spend of the thread's own: what it adds
// to the thread's tally (AppendUsageAndReconcile).
type OwnSpend struct {
	ThreadID string
	Tokens   int64
}

// addOwnSpendTx adds spend to its thread's tally, in the settlement's
// transaction; a thread already gone keeps none.
func addOwnSpendTx(tx *sql.Tx, spend OwnSpend) error {
	if spend.ThreadID == "" || spend.Tokens <= 0 {
		return nil
	}
	if _, err := tx.Exec(`INSERT INTO thread_tallies (thread_id, tokens)
		SELECT ?1, ?2 WHERE EXISTS (SELECT 1 FROM threads WHERE id = ?1)
		ON CONFLICT (thread_id) DO UPDATE SET tokens = tokens + excluded.tokens`, spend.ThreadID, spend.Tokens); err != nil {
		return fmt.Errorf("store: count the own spend of %s: %w", spend.ThreadID, err)
	}
	return nil
}

// ThreadTally is what one thread did over its life: the tool calls it
// executed, the tokens it spent itself and the time its turns ran, the
// running turn's included as of the read. Running is
// whether a turn runs now, whose time grows from the read on, and
// TurnStartedAt that turn's start as this computer recorded it: which turn
// it is, so a reader can tell a later read of the same open turn from one
// of the next.
type ThreadTally struct {
	ToolCalls     int64
	Tokens        int64
	RuntimeMs     int64
	Running       bool
	TurnStartedAt int64
}

// threadTallyColumnsSQL is the five columns of the tally of the thread id
// names, tool calls, tokens, runtime, whether a turn runs and when it
// started, with now in
// milliseconds as the parameter now names: one probe of thread_tallies, one
// of the running turn's usage_pending rows and one of its open turn.
func threadTallyColumnsSQL(id, now string) string {
	open := `(SELECT started_at FROM turns WHERE thread_id = ` + id + ` AND completed_at IS NULL ORDER BY turn_index DESC LIMIT 1)`
	return `COALESCE((SELECT tool_calls FROM thread_tallies WHERE thread_id = ` + id + `), 0),
		COALESCE((SELECT tokens FROM thread_tallies WHERE thread_id = ` + id + `), 0)
		+ COALESCE((SELECT SUM(` + usageSpendSQLPlain + `) FROM usage_pending WHERE thread_id = ` + id + `), 0),
		COALESCE((SELECT turn_ms FROM thread_tallies WHERE thread_id = ` + id + `), 0)
		+ COALESCE(MAX(0, ` + now + ` - ` + open + `), 0),
		` + open + ` IS NOT NULL,
		COALESCE(` + open + `, 0)`
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
		if err := rows.Scan(&id, &tally.ToolCalls, &tally.Tokens, &tally.RuntimeMs, &tally.Running, &tally.TurnStartedAt); err != nil {
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
