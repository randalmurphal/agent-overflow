package itemmeta

const pendingFlushKey = "pendingFlush"

// SetPendingFlush marks a reserved user row that has not entered the visible
// timeline. Echo confirmation and interrupt promotion clear it in the same
// transaction that places the row. The live pending-send registry owns whether
// the message still has a queue preview; this marker distinguishes its quiet
// stored copy from a confirmed row waiting behind a client's reveal gate.
//
// The marker means exactly one thing: a row carries it while it is pending,
// that is while its send is in the live pending-send registry. The send
// leaves that registry in exactly one way, and the mark with it: the
// provider confirms it, an interrupt promotes it, or its session's teardown
// takes it back, to the composer or with a Stop's dropped queue
// (app settleQueueForTeardown). A mark found at boot is residue only in its
// thread's last turn (store unconfirmedUserRowResidueSQL).
// Clearing therefore REMOVES the key rather than storing false, so a confirmed
// live row and an imported row of the same message carry identical meta
// (internal/sessionimport's live/import parity gate compares them).
func SetPendingFlush(raw string, pending bool) (string, error) {
	if !pending {
		return removeKeys(raw, pendingFlushKey)
	}
	return mergeKey(raw, pendingFlushKey, pending)
}
