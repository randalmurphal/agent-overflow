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
// The row stays open when its settle write failed, the boot sweep failed,
// or a start was refused after it wrote the row. It reads as running until
// a later end of the turn saves or the next boot's sweep records it as
// interrupted, as GetActiveTurn reads it for send placement and revert, so
// a wait on that thread runs to its timeout. Triage tells the user of a
// failed settle write (triage/unsaved_rows.go).

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
	return `LEFT JOIN turns AS ` + alias + ` ON ` + alias + `.turn_id = ` + newestTurnSQL(thread, `CASE WHEN `+turnOpenSQL+` THEN nt.turn_id END`)
}

// turnOpenSQL is true for a newest turn row that is still in flight.
const turnOpenSQL = `nt.completed_at IS NULL`

// newestTurnState is what a threads row reads of its newest turn row. The
// states exclude one another, so a row is never both open and interrupted.
type newestTurnState int

const (
	// newestTurnAtRest: no turn, or one that settled and is not an unread
	// interruption or error.
	newestTurnAtRest newestTurnState = iota
	// newestTurnOpen: the row is still in flight.
	newestTurnOpen
	// newestTurnInterruptedUnread: the row settled as interrupted after the
	// user last read the thread.
	newestTurnInterruptedUnread
	// newestTurnErrorUnread: the row settled as error after the user last
	// read the thread.
	newestTurnErrorUnread
)

// newestTurnStateSQL is a threads row's newestTurnState, one probe of its
// newest turn row.
func newestTurnStateSQL() string {
	return `COALESCE(` + newestTurnSQL("threads.id", `CASE
	        WHEN `+turnOpenSQL+` THEN `+strconv.Itoa(int(newestTurnOpen))+`
	        WHEN nt.stop_reason = 'interrupted'
	         AND (threads.last_read_at IS NULL OR threads.last_read_at < nt.completed_at)
	          THEN `+strconv.Itoa(int(newestTurnInterruptedUnread))+`
	        WHEN nt.stop_reason = 'error'
	         AND (threads.last_read_at IS NULL OR threads.last_read_at < nt.completed_at)
	          THEN `+strconv.Itoa(int(newestTurnErrorUnread))+`
	        ELSE `+strconv.Itoa(int(newestTurnAtRest))+` END`) + `, ` + strconv.Itoa(int(newestTurnAtRest)) + `)`
}
