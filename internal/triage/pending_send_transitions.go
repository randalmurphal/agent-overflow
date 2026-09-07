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
// WHEN: on each matched echo, BEFORE
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
	// A retry may omit known fields. A conflicting item ID cannot enrich
	// the original record with another item's parent, even if clientId matched.
	if p.EchoProviderItemID != "" && providerItemID != "" && p.EchoProviderItemID != providerItemID {
		return
	}
	if p.EchoProviderItemID == "" {
		p.EchoProviderItemID = providerItemID
	}
	if p.EchoParentUUID == "" {
		p.EchoParentUUID = parentUUID
	}
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
