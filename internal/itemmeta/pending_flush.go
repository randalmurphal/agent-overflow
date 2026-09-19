package itemmeta

const pendingFlushKey = "pendingFlush"

// SetPendingFlush marks a reserved user row that has not entered the visible
// timeline. Echo confirmation and interrupt promotion clear it in the same
// transaction that places the row. The live pending-send registry owns whether
// the message still has a queue preview; this marker distinguishes its quiet
// stored copy from a confirmed row waiting behind a client's reveal gate.
//
// The marker means exactly one thing: a row carries it while it is pending.
// Clearing therefore REMOVES the key rather than storing false, so a confirmed
// live row and an imported row of the same message carry identical meta
// (internal/sessionimport's live/import parity gate compares them).
func SetPendingFlush(raw string, pending bool) (string, error) {
	if !pending {
		return removeKeys(raw, pendingFlushKey)
	}
	return mergeKey(raw, pendingFlushKey, pending)
}
