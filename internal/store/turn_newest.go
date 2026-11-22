package store

// A thread's newest turn row is the record of whether a turn runs there.
// Its open state (completed_at IS NULL) is the turn in flight, from the
// row's insert at the turn's start until the one UPDATE that settles it
// writes completed_at and stop_reason together (UpdateTurnCompleted, the
// boot sweep RecoverCrashedTurns, a fork settling its copies). A reader of
// this one row cannot see a turn between running and settled, and an
// interruption is what that write recorded (stop_reason 'interrupted'),
// never inferred from a live provider round having ended.
//
// The row stays open when nothing will settle it at runtime: its settle
// write failed, the boot sweep failed, or a start was refused after it
// wrote the row. It reads as running until the next boot's sweep records it
// as interrupted, as GetActiveTurn reads it for send placement and revert,
// so a wait on that thread runs to its timeout.

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

// newestTurnInterruptedUnreadSQL is true for a threads row whose newest
// turn settled as interrupted after the user last read the thread.
func newestTurnInterruptedUnreadSQL() string {
	return `COALESCE(` + newestTurnSQL("threads.id", `nt.stop_reason = 'interrupted' AND NOT (`+turnOpenSQL+`)
	      AND (threads.last_read_at IS NULL OR threads.last_read_at < nt.completed_at)`) + `, 0)`
}
