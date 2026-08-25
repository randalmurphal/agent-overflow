package triage

// pending_send_transitions.go holds the named state transitions of the
// pending-send registry that are DRIVEN from outside pending_send.go —
// the echo path (handle_user_text.go) and the session-death drain
// (flush_queue.go). It is the same registry and the same invariants as
// pending_send.go; it is a separate file only because that one is
// already at its size ceiling. Nothing else in the package may write a
// pendingSend field directly: every mutation reachable from another
// file goes through one of the functions here, so the answer to "when
// is this legal" lives next to the answer to "what does it mean".
//
// Two classes live here, and they have opposite locking rules:
//
//   - *pendingSend methods mutate the POPPED COPY the echo path owns.
//     That copy left the registry at consumeMatchingPendingSendForEcho
//     and is either dropped (the write landed) or handed back whole to
//     reinsertPendingSendHead (the write failed). r.mu must NOT be held
//     and would protect nothing: no other goroutine can see the copy.
//     Copy semantics are the point — a reinserted entry carries exactly
//     the fields the echo path stashed on it, and an entry that was
//     consumed successfully carries them nowhere.
//   - (*Router) transitions mutate the LIVE registry through the
//     shared backing array and state the lock they need themselves.
//
// AOItemID and Shape are absent from both classes on purpose. They are
// stamped once by registerPendingSend and immutable afterwards, which is
// what lets every reader trust the shape stamp over an id sniff (see
// AGENTS.md "Send shape is stamped at registration"). A transition that
// rewrote either would break that, so there is no transition that can.

// stashEchoIdentity records the consuming echo's wire identity on the
// popped copy, before the fallible handlers run.
//
// WHEN: exactly once, on the entry handleUserText just popped, BEFORE
// dispatching to persistDeferredUserText / attachProviderItemIDToUserRow.
// The echo will not necessarily be re-delivered, so an entry reinserted
// as EchoConsumed after a failed write is the only thing that still
// knows the transcript uuid and parent the failed stamp lost — the
// session-death self-heal merges them into the healed row and its
// anchor (round-6, R6-1).
//
// LOCKING: r.mu must NOT be held; the caller owns the copy. The caller
// does hold the thread's flush anchor lock, which is what orders the pop
// against the interrupt paths, not what protects this write.
func (p *pendingSend) stashEchoIdentity(providerItemID, parentUUID string) {
	p.EchoProviderItemID = providerItemID
	p.EchoParentUUID = parentUUID
}

// recordFirstEchoTurnOccupancy records whether the deferred prompt's
// turn held any rows when its FIRST echo arrived.
//
// WHEN: only on an entry that has not been echo-consumed before
// (!EchoConsumed). This is first-echo information: a retry or a
// session-death self-heal running later finds the RESPONSE occupying the
// turn and can no longer tell response rows (prompt goes above them)
// from pre-dispatch content (prompt goes below) — round-7, R7-4. The
// call is unconditional on that branch, including the failed-sample
// fallback, so a reinserted entry never carries an unrecorded zero
// value (round-14, D14-1).
//
// LOCKING: r.mu must NOT be held; the caller owns the copy. When the
// value is derived from router turn-open state the caller samples that
// under r.mu and releases before calling.
func (p *pendingSend) recordFirstEchoTurnOccupancy(turnWasEmpty bool) {
	p.EchoTurnWasEmpty = turnWasEmpty
}

// recordEchoPromotedBoundary stashes the provider-order boundary the
// echo path computed for a promoted (or about-to-be-bumped eager) flush
// row, so a failed write does not lose it.
//
// WHEN: after the boundary has been sampled under the thread's drain
// lock and BEFORE the fallible store write it describes. The value is
// echo-time information only: by session-death drain time the response
// rows have persisted and a recomputed MAX would misclassify them as
// interrupted tail, so the failed write's value is the only correct
// source (round-6, R6-1; round-10, R10-1). -1 means "no boundary" and
// is what registration already stamped, so a branch whose turn had no
// rows to sample writes it back harmlessly; this is not a clear, and
// there is no caller that needs one.
//
// LOCKING: r.mu must NOT be held; the caller owns the copy.
func (p *pendingSend) recordEchoPromotedBoundary(boundary int) {
	p.EchoPromotedBoundary = boundary
}

// markAnchorRecordedAtEcho claims that this entry's message anchor was
// recorded on the failure path of its FIRST echo — the row existed and
// only the stamp write failed.
//
// WHEN: immediately after the confirmed hook ran on that failure path,
// and only there. That first echo is the true consumption boundary, so a
// retry's success path must not run the hook again; it only folds the
// retry's provider ids into the existing anchor through
// UpdateMessageAnchorProviderIDs (round-10, R10-2). Idempotent — the
// claim is never withdrawn, because the boundary it names cannot move.
//
// LOCKING: r.mu must NOT be held; the caller owns the copy. The caller
// holds the thread's flush anchor lock, same as the success-path hook
// sites.
func (p *pendingSend) markAnchorRecordedAtEcho() {
	p.AnchorRecordedAtEcho = true
}

// takeUnconfirmedFlushSendsLocked removes every unconfirmed QUEUED
// FLUSH entry for threadID from the registry and returns it partitioned
// by whether the provider provably consumed the message. FIFO order is
// preserved within each partition, and entries the drain does not own —
// direct sends, steers, and the Codex post-interrupt flush re-send,
// none of which carry a queue item id — stay in the registry untouched.
//
// The partition is a registry fact, not a caller policy:
//
//   - echoConsumed entries had their echo arrive, so the provider
//     transcript contains the message and only AO's write failed. They
//     are NOT restorable: returning one to the draft or the queue would
//     re-send content the resumed session already has (round-5, R5-3).
//     The caller self-heals the timeline row from the retained copy.
//   - restorable entries never reached a consuming echo, so the message
//     is AO's to hand back with its Zone 1 identity intact.
//
// WHEN: the session-death drain, once per teardown. Removal is total
// for both partitions — an entry that stayed behind would be matched by
// a replacement session's echo.
//
// LOCKING: caller MUST hold r.mu, and must already hold the thread's
// flush anchor lock so an in-flight interrupt transition has committed
// (or unclaimed) before the snapshot is taken.
func (r *Router) takeUnconfirmedFlushSendsLocked(threadID string) (restorable, echoConsumed []pendingSend) {
	st := r.threadStateIfPresent(threadID)
	if st == nil || len(st.pendingSends) == 0 {
		return nil, nil
	}
	kept := make([]pendingSend, 0, len(st.pendingSends))
	for _, entry := range st.pendingSends {
		if entry.QueueItemID == "" || entry.Shape != sendShapeFlush {
			kept = append(kept, entry)
			continue
		}
		if entry.EchoConsumed {
			echoConsumed = append(echoConsumed, entry)
			continue
		}
		restorable = append(restorable, entry)
	}
	if len(kept) == 0 {
		kept = nil
	}
	st.pendingSends = kept
	return restorable, echoConsumed
}
