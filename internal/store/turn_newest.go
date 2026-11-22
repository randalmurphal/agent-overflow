package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// A thread's newest turn row is the record of whether a turn runs there.
// Its open state (completed_at IS NULL) is the turn in flight, from the
// row's insert at the turn's start until the one UPDATE that settles it
// writes completed_at and stop_reason together (UpdateTurnCompleted, the
// boot sweep RecoverCrashedTurns, a fork settling its copies). A reader of
// this one row cannot see a turn between running and settled, and an
// interruption is what that write recorded (stop_reason 'interrupted'),
// never inferred from a live provider round having ended.
//
// An open row whose turn nothing in this process runs (its end write
// failed, a failed boot sweep left it, its start was refused after the row
// was written) has ended all the same. The store cannot tell such a row
// from a running turn's; triage can (Router.TurnEnded), so readers that
// decide running or rest ask it about a thread whose newest row is open.
// Such a turn reads as the next boot's sweep will record it:
// interrupted, ended at its last activity (turnLastActivitySQL), unread by
// the same rule as a recorded interruption (NewestOpenTurn). GetActiveTurn
// still reads its row as active.

// newestTurnSQL selects expr from thread's newest turn row, one probe of
// turns_thread_index; NULL when the thread has no turn. expr reads the row
// as nt.
func newestTurnSQL(thread, expr string) string {
	return `(SELECT ` + expr + ` FROM turns AS nt INDEXED BY turns_thread_index
	          WHERE nt.thread_id = ` + thread + ` ORDER BY nt.turn_index DESC LIMIT 1)`
}

// newestOpenTurnJoinSQL joins, as alias, thread's newest turn row while it
// is in flight, and no row once it settled: one probe of
// turns_thread_index and one of the primary key, however many columns read
// alias.
func newestOpenTurnJoinSQL(thread, alias string) string {
	return `LEFT JOIN turns AS ` + alias + ` ON ` + alias + `.turn_id = ` + newestTurnSQL(thread, openTurnIDSQL)
}

// turnOpenSQL is true for a newest turn row that is still in flight.
const turnOpenSQL = `nt.completed_at IS NULL`

// openTurnIDSQL is the turn id of a newest turn row still in flight, NULL
// for a settled one.
const openTurnIDSQL = `CASE WHEN ` + turnOpenSQL + ` THEN nt.turn_id END`

// unreadSinceSQL is true when the thread row alias names was not read since
// at: the rule that makes an interruption unread.
func unreadSinceSQL(alias, at string) string {
	return `(` + alias + `.last_read_at IS NULL OR ` + alias + `.last_read_at < ` + at + `)`
}

// newestTurnInterruptedUnreadSQL is true for a threads row whose newest
// turn settled as interrupted after the user last read the thread.
func newestTurnInterruptedUnreadSQL() string {
	return `COALESCE(` + newestTurnSQL("threads.id", `nt.stop_reason = 'interrupted' AND NOT (`+turnOpenSQL+`)
	      AND `+unreadSinceSQL("threads", "nt.completed_at")) + `, 0)`
}

// turnLastActivitySQL is when the turn row alias names ended if nothing
// recorded its end: the last time one of its items was written, no earlier
// than its start and no later than now (an SQL expression). The boot sweep
// records it as such a turn's end; a reader of an ended turn whose row is
// still open reads it the same way. It reads the turn's items through the
// (thread_id, turn_index) prefix of their unique index.
func turnLastActivitySQL(alias, now string) string {
	return `MIN(` + now + `, MAX(` + alias + `.started_at, COALESCE((SELECT MAX(MAX(i.created_at, i.updated_at)) FROM items i
	  WHERE i.thread_id = ` + alias + `.thread_id AND i.turn_index = ` + alias + `.turn_index), 0)))`
}

// EndedTurn is a thread's newest turn row while it is open, as it reads
// once its turn has ended unrecorded: when it ended (its last activity) and
// whether the thread was read since.
type EndedTurn struct {
	TurnID    string
	TurnIndex int
	StartedAt int64
	EndedAt   int64
	Unread    bool
}

// NewestOpenTurn reads threadID's newest turn row at now (milliseconds)
// when it is open, as EndedTurn reads it; false when the thread has no
// turn or its newest one settled.
func (s *Store) NewestOpenTurn(threadID string, now int64) (EndedTurn, bool, error) {
	var out EndedTurn
	err := s.reader().QueryRow(`SELECT turn_id, turn_index, started_at, ended_at, `+unreadSinceSQL("o", "o.ended_at")+`
		  FROM (SELECT ot.turn_id, ot.turn_index, ot.started_at, t.last_read_at, `+turnLastActivitySQL("ot", "?2")+` AS ended_at
		          FROM threads AS t JOIN turns AS ot ON ot.turn_id = `+newestTurnSQL("t.id", openTurnIDSQL)+`
		         WHERE t.id = ?1) AS o`, threadID, now).Scan(&out.TurnID, &out.TurnIndex, &out.StartedAt, &out.EndedAt, &out.Unread)
	if errors.Is(err, sql.ErrNoRows) {
		return EndedTurn{}, false, nil
	}
	if err != nil {
		return EndedTurn{}, false, fmt.Errorf("store: read the open newest turn of %s: %w", threadID, err)
	}
	return out, true, nil
}

// OpenNewestTurn is a thread whose newest turn row is open.
type OpenNewestTurn struct {
	ThreadID string
	TurnID   string
}

// ListOpenNewestTurns lists the threads whose newest turn row is open,
// through the partial index of open rows (idx_turns_inflight): its cost
// follows the open rows, one per running turn and per turn whose end was
// not recorded.
func (s *Store) ListOpenNewestTurns() (_ []OpenNewestTurn, err error) {
	rows, err := s.reader().Query(`SELECT o.thread_id, o.turn_id FROM turns AS o INDEXED BY idx_turns_inflight
		  WHERE o.completed_at IS NULL
		    AND NOT EXISTS (SELECT 1 FROM turns AS later INDEXED BY turns_thread_index
		                     WHERE later.thread_id = o.thread_id AND later.turn_index > o.turn_index)`)
	if err != nil {
		return nil, fmt.Errorf("store: list open newest turns: %w", err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	var out []OpenNewestTurn
	for rows.Next() {
		var turn OpenNewestTurn
		if err := rows.Scan(&turn.ThreadID, &turn.TurnID); err != nil {
			return nil, fmt.Errorf("store: scan open newest turn: %w", err)
		}
		out = append(out, turn)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list open newest turns: %w", err)
	}
	return out, nil
}
