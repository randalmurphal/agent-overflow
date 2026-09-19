package itemmeta

const pendingFlushKey = "pendingFlush"

// SetPendingFlush marks a reserved user row that has not entered the visible
// timeline. Echo confirmation and interrupt promotion clear it in the same
// transaction that places the row. The live pending-send registry owns whether
// the message still has a queue preview; this marker distinguishes its quiet
// stored copy from a confirmed row waiting behind a client's reveal gate.
func SetPendingFlush(raw string, pending bool) (string, error) {
	return mergeKey(raw, pendingFlushKey, pending)
}
