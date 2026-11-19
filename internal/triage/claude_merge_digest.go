package triage

import (
	"log"
	"slices"
)

// claude_merge_digest.go owns the evidence half of the queue-boundary merge:
// the per-thread ledger of what each flush send put on the wire, and the
// decomposition that decides whether an echo is one message's ack or the
// provider's merge of several.
//
// The fold that acts on the answer is in claude_merge_fold.go.

// flushSendDigest records one flush send's outbound content blocks as a
// fingerprint list (provider.MetaUserContentBlockDigestKey's vocabulary). The
// digest is per-block hashes, so retaining a thread's recent sends costs a
// fixed handful of bytes each however large the messages were.
type flushSendDigest struct {
	AOItemID string
	Digest   []string
}

// MaxOutstandingFlushSends is the most follow-up messages a thread has at
// once between their admission and the boundary that resolves them:
// queued, in flight to the provider, waiting for their echo, or kept as
// merge evidence (OutstandingFlushSendCount). The app refuses a message
// past it (queueMaxLength), so recentFlushDigests, which holds only
// sends whose boundary is unresolved (retireResolvedFlushDigests), never
// reaches its eviction for admitted work.
const MaxOutstandingFlushSends = 64

// OutstandingFlushSendCount counts the thread's flush sends the provider was
// handed and whose boundary has not resolved: those waiting for their echo
// and those kept as evidence of a merge still to be echoed
// (unresolvedFlushDigests). Each counts once. Evidence left by sends that
// went without an echo (given back, retired unread) counts for nothing: no
// confirmation is coming to retire it, and counting it would refuse new
// messages on a thread with nothing outstanding.
func (r *Router) OutstandingFlushSendCount(threadID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		return 0
	}
	count := st.unresolvedFlushDigests()
	for _, pending := range st.pendingSends {
		if pending.Shape != sendShapeFlush {
			continue
		}
		if !slices.ContainsFunc(st.recentFlushDigests, func(entry flushSendDigest) bool { return entry.AOItemID == pending.AOItemID }) {
			count++
		}
	}
	return count
}

// recordFlushSendDigest appends a flush send to the thread's ledger, evicting
// the oldest entry past the bound, which only a send the app did not admit
// can reach. A re-registration of the same row id (a
// requeued dispatch) replaces its previous entry rather than leaving a stale
// digest that a later decomposition could match against. Caller holds r.mu.
func (st *threadState) recordFlushSendDigest(aoItemID string, digest []string) {
	if aoItemID == "" || len(digest) == 0 {
		return
	}
	st.dropFlushSendDigests([]string{aoItemID})
	st.recentFlushDigests = append(st.recentFlushDigests, flushSendDigest{
		AOItemID: aoItemID,
		Digest:   slices.Clone(digest),
	})
	if overflow := len(st.recentFlushDigests) - MaxOutstandingFlushSends; overflow > 0 {
		st.recentFlushDigests = slices.Clone(st.recentFlushDigests[overflow:])
	}
}

// retireResolvedFlushDigests drops the ledger's entries that no merge still
// to be echoed can include. A merge's members are the commands queued before
// its survivor, and its survivor has no echo yet, so every entry after the
// last send still pending (pendingSends, including one put back for a
// retry) is resolved. Caller holds r.mu.
func (st *threadState) retireResolvedFlushDigests() {
	keep := st.unresolvedFlushDigests()
	if keep == len(st.recentFlushDigests) {
		return
	}
	if keep == 0 {
		st.recentFlushDigests = nil
		return
	}
	clear(st.recentFlushDigests[keep:])
	st.recentFlushDigests = st.recentFlushDigests[:keep]
}

// unresolvedFlushDigests is how many of the ledger's leading entries a merge
// still to be echoed can include: those up to the last send still pending.
// Caller holds r.mu.
func (st *threadState) unresolvedFlushDigests() int {
	keep := 0
	for i, entry := range st.recentFlushDigests {
		if slices.ContainsFunc(st.pendingSends, func(send pendingSend) bool { return send.AOItemID == entry.AOItemID }) {
			keep = i + 1
		}
	}
	return keep
}

// dropFlushSendDigests removes the named rows from the ledger — used when a
// fold deletes them, so a later echo cannot decompose against a row that no
// longer exists. Caller holds r.mu.
func (st *threadState) dropFlushSendDigests(aoItemIDs []string) {
	if len(st.recentFlushDigests) == 0 || len(aoItemIDs) == 0 {
		return
	}
	kept := make([]flushSendDigest, 0, len(st.recentFlushDigests))
	for _, entry := range st.recentFlushDigests {
		if slices.Contains(aoItemIDs, entry.AOItemID) {
			continue
		}
		kept = append(kept, entry)
	}
	if len(kept) == 0 {
		st.recentFlushDigests = nil
		return
	}
	st.recentFlushDigests = kept
}

// mergedFlushPrefix reports the earlier flush sends a provider merged INTO the
// message this echo acknowledges, in queue order, or nil when the echo is an
// ordinary ack.
//
// The test is structural, never heuristic. Claude's boundary drain builds the
// merged command's content as the flat concatenation of every batched
// command's blocks and acknowledges it under the LAST command's uuid
// (claude-wire.md §Queued-message consumption). So a merge shows up as an echo
// that:
//
//   - carries strictly MORE blocks than AO sent under this uuid, and
//   - ENDS with exactly the blocks AO sent under this uuid, and
//   - whose leading blocks decompose, walking backwards, into the complete
//     outbound block sequences of the immediately preceding flush sends on
//     this thread, in order.
//
// All three must hold. Anything else — a longer echo whose tail is not this
// send's blocks, a prefix that does not divide cleanly at a send boundary, a
// leftover remainder — is content this function cannot explain, and it returns
// nil after logging rather than guessing. Today's behaviour (one row per AO
// message, a revert that refuses the missing uuid) is the safe residue.
//
// An echo SHORTER than or EQUAL to the expectation is the ordinary path and
// costs one length comparison.
func (r *Router) mergedFlushPrefix(threadID string, survivor *pendingSend, echoDigest []string) []flushSendDigest {
	if survivor.Shape != sendShapeFlush || len(survivor.ExpectedContentBlockDigest) == 0 || len(echoDigest) == 0 {
		return nil
	}
	expected := survivor.ExpectedContentBlockDigest
	if len(echoDigest) <= len(expected) {
		return nil
	}
	if !digestHasSuffix(echoDigest, expected) {
		log.Printf(
			"triage: claude echo for %s/%s carries %d content blocks where the send wrote %d, but does not END with the sent blocks — not a queue-boundary merge AO can fold; leaving the rows as dispatched",
			threadID, survivor.AOItemID, len(echoDigest), len(expected),
		)
		return nil
	}
	prefix := echoDigest[:len(echoDigest)-len(expected)]

	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		return nil
	}
	ledger := st.recentFlushDigests
	at := -1
	for i := len(ledger) - 1; i >= 0; i-- {
		if ledger[i].AOItemID == survivor.AOItemID {
			at = i
			break
		}
	}
	if at < 0 {
		log.Printf(
			"triage: claude echo for %s/%s carries %d extra leading content blocks but the send is no longer in the flush ledger — cannot identify what was merged in; leaving the rows as dispatched",
			threadID, survivor.AOItemID, len(prefix),
		)
		return nil
	}

	var members []flushSendDigest
	for i := at - 1; i >= 0 && len(prefix) > 0; i-- {
		candidate := ledger[i]
		if len(candidate.Digest) == 0 || len(candidate.Digest) > len(prefix) {
			break
		}
		if !digestHasSuffix(prefix, candidate.Digest) {
			break
		}
		prefix = prefix[:len(prefix)-len(candidate.Digest)]
		members = append(members, candidate)
	}
	if len(prefix) > 0 || len(members) == 0 {
		log.Printf(
			"triage: claude echo for %s/%s carries %d extra leading content blocks that do not decompose into this thread's preceding flush sends — refusing to guess which messages the CLI merged; leaving the rows as dispatched",
			threadID, survivor.AOItemID, len(echoDigest)-len(expected),
		)
		return nil
	}
	slices.Reverse(members)
	return members
}

func digestHasSuffix(digest, suffix []string) bool {
	if len(suffix) > len(digest) {
		return false
	}
	tail := digest[len(digest)-len(suffix):]
	for i := range suffix {
		if tail[i] != suffix[i] {
			return false
		}
	}
	return true
}
