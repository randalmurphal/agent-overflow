package store

import "strconv"

// A thread's newest turn row is the record of whether a turn runs there.
// Its open state (completed_at IS NULL) is the turn in flight, from the
// row's insert at the turn's start until the one UPDATE that settles it
// writes completed_at and stop_reason together (UpdateTurnCompleted, the
// boot sweep RecoverCrashedTurns, a fork settling its copies). A reader of
// this one row cannot see a turn between running and settled, and an
// interruption is what that write recorded (stop_reason 'interrupted'),
// never inferred from a live provider round having ended.
//
// The row stays open when the write of a turn's end fails, or the boot
// sweep fails to settle it. Triage then holds the end it could not record
// and writes it again until the store takes it
// (triage/unrecorded_turn_ends.go); until then the app does not read the
// open row as a running turn (App.turnRuns), which is why readers that
// decide that take the open row's turn id (openTurnIDSQL). GetActiveTurn
// still reads it as active, and send placement writes the end again before
// it asks.

// newestTurnSQL selects expr from thread's newest turn row, one probe of
// turns_thread_index; NULL when the thread has no turn. expr reads the row
// as nt.
func newestTurnSQL(thread, expr string) string {
	return `(SELECT ` + expr + ` FROM turns AS nt INDEXED BY turns_thread_index
	          WHERE nt.thread_id = ` + thread + ` ORDER BY nt.turn_index DESC LIMIT 1)`
}

// turnOpenSQL is true for a newest turn row that is still in flight.
const turnOpenSQL = `nt.completed_at IS NULL`

// openTurnIDSQL is the turn id of a newest turn row still in flight, NULL
// for a settled one.
const openTurnIDSQL = `CASE WHEN ` + turnOpenSQL + ` THEN nt.turn_id END`

// Newest-turn states, as threadColumns derives them in one probe.
const (
	newestTurnSettled = iota
	newestTurnOpen
	newestTurnInterruptedUnread
)

// newestTurnStateSQL is the newest turn's state for a threads row: open, or
// settled as interrupted after the user last read the thread, or neither.
func newestTurnStateSQL() string {
	return `COALESCE(` + newestTurnSQL("threads.id", `CASE
	      WHEN `+turnOpenSQL+` THEN `+strconv.Itoa(newestTurnOpen)+`
	      WHEN nt.stop_reason = 'interrupted'
	        AND (threads.last_read_at IS NULL OR threads.last_read_at < nt.completed_at) THEN `+strconv.Itoa(newestTurnInterruptedUnread)+`
	      ELSE `+strconv.Itoa(newestTurnSettled)+`
	    END`) + `, ` + strconv.Itoa(newestTurnSettled) + `)`
}
