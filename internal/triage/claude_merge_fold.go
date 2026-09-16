package triage

import (
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// claude_merge_fold.go folds the rows of a provider queue-boundary merge into
// the one row the transcript actually contains.
//
// AO already joins a drain it hands the dispatcher as a batch
// (internal/app/app_flush_dispatch_join.go). What is left is the merge AO
// cannot see coming: a message flushed at one drain that the CLI has not
// consumed when a LATER message's drain reaches the turn boundary. The CLI
// batches both, writes one `type:"user"` transcript entry under the later
// uuid, and acknowledges the earlier one only on stdout. Left alone AO holds
// two rows, one of them stamped with an id the session file never contains,
// and a revert to it refuses (app_conversation_rollback.go
// writeClaudeSessionSlice).
//
// The evidence is in claude_merge_digest.go: the survivor's echo carries the
// concatenated blocks of every member. On that evidence — and only on it —
// this file rebuilds the survivor as the join and deletes the merged-away
// rows, so one AO message is one transcript entry again.

// mergedFlushRow is one member of a confirmed merge, resolved to the durable
// state the fold needs from it.
type mergedFlushRow struct {
	aoItemID string
	// summary / meta are the member's stored content. They come from the
	// persisted row when one exists and from the pending entry's retained
	// copy when the member's row was still deferred at merge time.
	summary string
	meta    usermessage.Meta
	// persisted marks a member with a row in SQLite, which the fold deletes.
	// A never-persisted deferred row contributes its text and send ids and
	// has nothing to delete.
	persisted bool
	kind      string
}

// foldMergedFlushRows rebuilds the survivor row as the join of members +
// survivor and deletes the members' rows.
//
// Runs under the thread's flush anchor lock (handleUserText's echo pop holds
// it) and AFTER the survivor's own confirmation committed, so the row this
// rewrites is the one the echo already stamped with the transcript uuid and
// parent — the correlation keys the rewrite preserves
// (usermessage.MergeJoinedMeta) and the anchor a later revert slices at.
//
// Nothing is consumed or mutated until the single store write succeeds, so any
// earlier refusal leaves the conversation exactly as the pre-fold code left
// it: several rows, one of which a revert will refuse. That is a loud,
// recoverable state, and strictly better than a partially folded one.
func (r *Router) foldMergedFlushRows(threadID string, survivor *pendingSend, digests []flushSendDigest) error {
	members := make([]mergedFlushRow, 0, len(digests)+1)
	for _, entry := range digests {
		member, ok, err := r.resolveMergedFlushRow(threadID, entry.AOItemID)
		if err != nil {
			return fmt.Errorf("triage: resolve merged flush row %s/%s: %w", threadID, entry.AOItemID, err)
		}
		if !ok {
			log.Printf(
				"triage: claude merged %s into %s on %s but that message has no row and no retained copy — leaving the fold undone rather than guessing its text",
				entry.AOItemID, survivor.AOItemID, threadID,
			)
			return nil
		}
		members = append(members, member)
	}

	survivorRow, found, err := r.store.GetThreadItem(threadID, survivor.AOItemID)
	if err != nil {
		return fmt.Errorf("triage: load merge survivor %s/%s: %w", threadID, survivor.AOItemID, err)
	}
	if !found {
		log.Printf(
			"triage: claude merge survivor %s/%s has no row to fold into — leaving the merged-away rows in place",
			threadID, survivor.AOItemID,
		)
		return nil
	}
	survivorMeta, err := usermessage.FromItem(survivorRow)
	if err != nil {
		return fmt.Errorf("triage: decode merge survivor meta %s/%s: %w", threadID, survivor.AOItemID, err)
	}
	members = append(members, mergedFlushRow{
		aoItemID:  survivorRow.ID,
		summary:   survivorRow.Summary,
		meta:      survivorMeta,
		persisted: true,
		kind:      survivorRow.Kind,
	})

	summary, meta, err := joinMergedFlushContent(survivorRow.Meta, members)
	if err != nil {
		return fmt.Errorf("triage: join merged flush rows on %s: %w", threadID, err)
	}

	folded := members[:len(members)-1]
	foldedIDs := make([]string, 0, len(folded))
	for _, member := range folded {
		if member.persisted {
			foldedIDs = append(foldedIDs, member.aoItemID)
		}
	}

	now := time.Now().UnixMilli()
	var persisted store.Item
	if len(foldedIDs) == 0 {
		// Every member was still deferred, so there is nothing to delete —
		// only the survivor's own text and send ids to widen.
		persisted, err = r.store.UpsertItem(withFoldedContent(survivorRow, summary, meta, now), nil)
	} else {
		persisted, err = r.store.FoldUserTextRows(threadID, survivorRow.ID, foldedIDs, summary, meta, now)
	}
	if err != nil {
		return fmt.Errorf("triage: fold merged flush rows on %s: %w", threadID, err)
	}

	r.settleFoldedMergeState(threadID, survivor, digests)

	r.emitItemUpsert(persisted)
	for _, member := range folded {
		// Removed for EVERY member, including one whose row was still
		// deferred and so had nothing to delete. The send-queue overlay
		// holds an entry per flushed message from the dispatch event, not
		// from a row, and the removal is what retires it; a member the
		// client never rendered is simply a removal it drops.
		r.emitItemRemove(threadID, member.aoItemID, member.kind)
	}
	log.Printf(
		"triage: folded %d claude queue-boundary merged message(s) into %s/%s — the CLI wrote one transcript entry under its uuid",
		len(folded), threadID, survivorRow.ID,
	)
	return nil
}

// settleFoldedMergeState retires the in-memory records of the merged-away
// messages once their rows are gone: the flush ledger entries (so a later echo
// cannot decompose against a row that no longer exists) and any pending-send
// entry whose own echo had not arrived yet.
//
// Consuming a still-pending entry is not optional. The message is provably in
// provider context — the survivor's echo carried its blocks — so leaving the
// entry would let the late echo pop it and stamp a row this fold absorbed; and
// dropping the entry without marking its uuid seen would route that echo to
// the injected-context branch. Both are done together here.
func (r *Router) settleFoldedMergeState(threadID string, survivor *pendingSend, digests []flushSendDigest) {
	var joinedDigest []string
	dropped := make([]string, 0, len(digests))
	for _, entry := range digests {
		dropped = append(dropped, entry.AOItemID)
		joinedDigest = append(joinedDigest, entry.Digest...)
	}
	joinedDigest = append(joinedDigest, survivor.ExpectedContentBlockDigest...)

	var consumedIDs []string
	r.mu.Lock()
	if st := r.threadStateIfPresent(threadID); st != nil {
		st.dropFlushSendDigests(dropped)
		// The survivor now answers for the whole merged message, so its
		// ledger digest becomes the concatenation. Nothing upstream emits a
		// second merge over an already-folded entry, but a ledger that
		// disagreed with the row would decompose a later echo wrongly.
		st.recordFlushSendDigest(survivor.AOItemID, joinedDigest)
		for _, id := range dropped {
			if entry, ok := r.popPendingSendByItemIDLocked(threadID, id); ok && entry.ExpectedProviderItemID != "" {
				consumedIDs = append(consumedIDs, entry.ExpectedProviderItemID)
			}
		}
	}
	r.mu.Unlock()
	for _, providerItemID := range consumedIDs {
		r.markWireOnlyUserTextSeen(threadID, providerItemID)
	}
}

// resolveMergedFlushRow finds a merged-away member's durable content. The row
// is the normal answer: its own merged-away echo arrives before the survivor's
// (the CLI emits them at drain time, ahead of the batch's lifecycle frames), so
// by now it is persisted and stamped. A member whose row was still deferred
// falls back to the copy its pending entry retains — read here, consumed only
// once the fold has committed.
func (r *Router) resolveMergedFlushRow(threadID, aoItemID string) (mergedFlushRow, bool, error) {
	row, found, err := r.store.GetThreadItem(threadID, aoItemID)
	if err != nil {
		return mergedFlushRow{}, false, err
	}
	if found {
		meta, err := usermessage.FromItem(row)
		if err != nil {
			return mergedFlushRow{}, false, err
		}
		return mergedFlushRow{
			aoItemID:  row.ID,
			summary:   row.Summary,
			meta:      meta,
			persisted: true,
			kind:      row.Kind,
		}, true, nil
	}
	entry, ok := r.peekPendingSendByItemID(threadID, aoItemID)
	if !ok {
		return mergedFlushRow{}, false, nil
	}
	retained := entry.DeferredItem
	if retained == nil {
		retained = entry.QuietItem
	}
	if retained == nil {
		return mergedFlushRow{}, false, nil
	}
	meta, err := usermessage.FromItem(*retained)
	if err != nil {
		return mergedFlushRow{}, false, err
	}
	return mergedFlushRow{
		aoItemID: retained.ID,
		summary:  retained.Summary,
		meta:     meta,
		kind:     retained.Kind,
	}, true, nil
}

// joinMergedFlushContent builds the survivor's new summary and meta: the
// members' texts in queue order joined by the shared separator with their
// `[Image #N]` markers renumbered into the combined attachment list, and the
// union of their metadata merged onto the survivor's existing blob so its
// provider correlation keys and promotion markers survive.
func joinMergedFlushContent(survivorMetaJSON string, members []mergedFlushRow) (string, string, error) {
	parts := make([]usermessage.TextPart, 0, len(members))
	metas := make([]usermessage.Meta, 0, len(members))
	for _, member := range members {
		parts = append(parts, usermessage.TextPart{
			Text:       member.summary,
			ImageCount: usermessage.ImageAttachmentCount(member.meta.Attachments),
		})
		metas = append(metas, member.meta)
	}
	meta, err := usermessage.MergeJoinedMeta(survivorMetaJSON, usermessage.JoinMetas(metas))
	if err != nil {
		return "", "", err
	}
	return usermessage.JoinRenumberedText(parts), meta, nil
}

func withFoldedContent(row store.Item, summary, meta string, now int64) store.Item {
	row.Summary = summary
	row.Meta = meta
	row.UpdatedAt = now
	return row
}
