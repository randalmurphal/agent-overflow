package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"

	"agent-overflow/internal/agentmeta"
	"agent-overflow/internal/composerdraft"
	"agent-overflow/internal/flushqueue"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// requeueUnconfirmedFlushItems is requeueUnconfirmedFlushItemsLocked taking
// the lock it needs, and telling the thread what it could not write.
func (a *App) requeueUnconfirmedFlushItems(threadID string, items []triage.UnconfirmedFlushItem, reason string) {
	unlock := a.lockForQueuedRows(threadID, items)
	_, outcome := a.requeueUnconfirmedFlushItemsLocked(threadID, items, reason)
	unlock()
	a.finishRequeue(threadID, outcome)
}

// requeueUnconfirmedFlushItemsLocked puts messages a restore could not
// return to the composer back on the queue, durable first like any queued
// message, and returns them as queued, for a caller that must queue them
// again after a cleanup wiped the queue.
//
// Each queued message an item carries decides for itself. One is known to
// have no row only when it never had one or the write that handed it to the
// provider retired it (triage.FlushQueueMember.NoRow). Any other whose row is
// gone was withdrawn, and is dropped, and a joined message keeps the rest
// (withoutQueueMembers); a failed read drops none. A joined message whose
// members still have their rows goes back as separate queue items, each of
// those messages from its own row and the rest as one, in the order they were
// joined, so a later withdrawal of one takes it alone; the dispatch joins
// them again. Its timeline row, which a later dispatch or restore retires, goes
// with the rest, or with the first of them when nothing else is left
// (QueuedFlushItem.StaleUserItemID); when nothing at all is left the row is
// retired now.
//
// A handed message whose write retired its row gets the row again under its
// queue id, which the dispatch or restore of the requeued message retires.
// One whose earlier row is still in the store (StaleUserItemID) keeps that
// row as its durable copy instead and is queued without a queue row
// (triage.QueuedFlushItem.NoRow): the boot restore takes an unconfirmed row
// back with its text, so a queue row as well would put the message in the
// composer twice.
//
// A joined message that cannot be told apart by message where it has to be,
// one of its messages withdrawn or still holding its own row, is never sent
// again: it goes to the composer whole (cutWhole), for reason, in its own
// composer write (writeFlushDraft), whose failure the thread is told of.
//
// The outcome names the rows that could not be written, whose messages stay
// queued without one, and those that could not be retired, and holds the
// restore of the messages that went to the composer (finishRequeue).
//
// The caller holds the thread's mutation lock when one of items has its row
// (lockForQueuedRows), so no withdrawal fits between the read and the
// queueing it guards.
func (a *App) requeueUnconfirmedFlushItemsLocked(threadID string, items []triage.UnconfirmedFlushItem, reason string) ([]triage.UnconfirmedFlushItem, requeueOutcome) {
	var outcome requeueOutcome
	if a.triage == nil {
		return nil, outcome
	}
	rows, read := a.queueRowsOf(threadID, queueRowIDs(items))
	if hook := a.requeueRowsReadForTest; hook != nil {
		hook(threadID)
	}
	requeued := make([]triage.UnconfirmedFlushItem, 0, len(items))
	var persistFailed, retireFailed []error
	var whole []triage.UnconfirmedFlushItem
	for _, item := range items {
		parts, withdrawnRow, unsplit := requeueParts(threadID, item, rows, read)
		if unsplit != nil {
			whole = append(whole, *unsplit)
			continue
		}
		if withdrawnRow != "" {
			if _, err := a.cleanupStaleFlushRow(threadID, withdrawnRow); err != nil {
				retireFailed = append(retireFailed, fmt.Errorf("%s: %w", withdrawnRow, err))
			}
		}
		for _, part := range parts {
			queued, rowless := part.queued, part.rowless
			if rowless && part.handed && queued.StaleUserItemID == "" {
				if err := a.persistRequeuedFlushItem(threadID, queued); err != nil {
					persistFailed = append(persistFailed, err)
				} else {
					rowless = false
				}
			}
			queued.NoRow = rowless
			a.triage.RegisterQueueItem(threadID, queued)
			requeued = append(requeued, unconfirmedFromQueued(queued))
		}
	}
	outcome.persist, outcome.retire = errors.Join(persistFailed...), errors.Join(retireFailed...)
	if len(whole) > 0 {
		restore, err := a.writeFlushDraft(threadID, whole, nil, reason)
		if err != nil {
			log.Printf("flush queue: restore draft (%s) for %s: %v — joined messages that do not split stay out of the queue", reason, threadID, err)
			// Not queued again either: what is owed for their delivery is
			// settled now.
			for _, item := range whole {
				item.Settlement.Settle("")
			}
			restore = flushRestore{failed: err}
		}
		outcome.composer = append(outcome.composer, restore)
	}
	return requeued, outcome
}

// requeuePart is one queue item a requeue registers: rowless when it has no
// row of its own, handed when the provider was handed it.
type requeuePart struct {
	queued          triage.QueuedFlushItem
	rowless, handed bool
}

// requeueParts is what of item a requeue queues again
// (requeueUnconfirmedFlushItemsLocked), the timeline row to retire now when
// nothing of it is left, or, when it does not split where it has to, the
// item as it goes to the composer whole instead. rows are the queue rows its
// messages keep, by id; read is false when they could not be read.
func requeueParts(threadID string, item triage.UnconfirmedFlushItem, rows map[string]store.FlushQueueItem, read bool) ([]requeuePart, string, *triage.UnconfirmedFlushItem) {
	if read {
		rest, cut := withoutQueueMembers(threadID, item, withdrawnMembers(item, func(id string) bool { _, ok := rows[id]; return ok }))
		switch cut {
		case cutGone:
			return nil, unconfirmedRowID(item), nil
		case cutWhole:
			return nil, "", &rest
		}
		item = rest
	}
	var own []triage.FlushQueueMember
	if read && len(item.QueueMembers) > 1 && unconfirmedMembersOf(item).joined {
		for _, member := range item.QueueMembers {
			if member.KeepsRow() && member.SendID != "" {
				own = append(own, member)
			}
		}
	}
	if len(own) == 0 {
		queued, ok := queuedFlushItemFromUnconfirmed(item)
		if !ok {
			return nil, "", nil
		}
		return []requeuePart{{queued: queued, rowless: !hasQueueRow(item), handed: !item.Queued}}, "", nil
	}
	// The rest, as one message, from the item less the messages with rows.
	var rest *requeuePart
	if len(own) < len(item.QueueMembers) {
		gone := make(map[string]bool, len(own))
		for _, member := range own {
			gone[member.QueueItemID] = true
		}
		remainder, cut := withoutQueueMembers(threadID, item, gone)
		if cut == cutWhole {
			// Not told apart by message, so a later withdrawal of one could
			// not take it alone: it goes to the composer whole, its messages
			// with their rows included.
			return nil, "", &item
		}
		if queued, ok := queuedFlushItemFromUnconfirmed(remainder); ok {
			rest = &requeuePart{queued: queued, rowless: true, handed: !item.Queued}
		}
	}
	var parts []requeuePart
	restAt := -1
	for _, part := range unconfirmedMembersOf(item).parts {
		at := slices.IndexFunc(own, func(member triage.FlushQueueMember) bool { return member.SendID == part.SendID })
		if at < 0 {
			if rest != nil && restAt < 0 {
				restAt = len(parts)
				parts = append(parts, *rest)
			}
			continue
		}
		row := rows[own[at].QueueItemID]
		parts = append(parts, requeuePart{queued: triage.QueuedFlushItem{
			ID: row.ID, Message: row.Message, Payload: append(json.RawMessage(nil), row.Payload...), EnqueuedAt: row.EnqueuedAt,
		}})
	}
	if rest != nil && restAt < 0 {
		parts = append(parts, *rest)
	}
	// One timeline row, one obligation: the rest's, or the first message's.
	if rest == nil && len(parts) > 0 {
		parts[0].queued.StaleUserItemID = item.StaleUserItemID
	}
	return parts, "", nil
}

// queueRowsOf reads the queue rows of threadID named by ids. ok is false
// when the read failed.
func (a *App) queueRowsOf(threadID string, ids []string) (map[string]store.FlushQueueItem, bool) {
	rows := make(map[string]store.FlushQueueItem, len(ids))
	if len(ids) == 0 {
		return rows, true
	}
	listed, err := a.store.ListFlushQueueItems(threadID)
	if err != nil {
		log.Printf("flush queue: read which requeued messages of %s have rows: %v", threadID, err)
		return nil, false
	}
	for _, row := range listed {
		if slices.Contains(ids, row.ID) {
			rows[row.ID] = row
		}
	}
	return rows, true
}

// persistRequeuedFlushItem writes the durable row of a message queued again
// after the provider was handed it.
func (a *App) persistRequeuedFlushItem(threadID string, queued triage.QueuedFlushItem) error {
	var payload flushQueuePayload
	if len(queued.Payload) > 0 {
		if err := json.Unmarshal(queued.Payload, &payload); err != nil {
			log.Printf("flush queue: decode requeued payload %s/%s for its send id: %v", threadID, queued.ID, err)
		}
	}
	if err := a.store.EnsureFlushQueueItem(store.FlushQueueItem{
		ID:         queued.ID,
		ThreadID:   threadID,
		SendID:     payload.SendID,
		Message:    queued.Message,
		Payload:    queued.Payload,
		EnqueuedAt: queued.EnqueuedAt,
	}); err != nil {
		log.Printf("flush queue: persist requeued message %s/%s: %v", threadID, queued.ID, err)
		return err
	}
	return nil
}

// requeueOutcome is what a requeue owes once the mutation lock is released
// (requeueUnconfirmedFlushItemsLocked): what it could not write, the durable
// rows of messages it queued again (persist) and the timeline rows of
// messages withdrawn meanwhile (retire), and the restores of the joined
// messages it sent to the composer whole instead (composer).
type requeueOutcome struct {
	persist, retire error
	composer        []flushRestore
}

func (f requeueOutcome) join(other requeueOutcome) requeueOutcome {
	return requeueOutcome{
		persist:  errors.Join(f.persist, other.persist),
		retire:   errors.Join(f.retire, other.retire),
		composer: append(slices.Clone(f.composer), other.composer...),
	}
}

// finishRequeue tells the thread what a requeue could not write, and
// finishes its composer restores (finishFlushRestore). The caller holds the
// thread action lock and not its mutation lock.
func (a *App) finishRequeue(threadID string, f requeueOutcome) {
	if f.persist != nil {
		a.emitErrorToThread(threadID, "Saving a queued message failed. It stays queued but will not survive an app restart: "+f.persist.Error())
	}
	if f.retire != nil {
		a.emitErrorToThread(threadID, "A withdrawn message could not be removed from the timeline: "+f.retire.Error())
	}
	for _, restore := range f.composer {
		a.finishFlushRestore(threadID, restore)
	}
}

// queuedFlushItemFromUnconfirmed is the queue item that sends a message
// taken back from the provider again: its row's summary and the payload
// rebuilt from the row (requeuePayloadFromMeta), or the item's own message
// and payload when it has no row.
//
// The person's text goes back without the white space around it. A
// message that holds an agent's or the app's goes back as it was written:
// the facts it carries locate its body by offsets into that text
// (threadtools.MessageFacts), and a reader shows text they do not describe
// whole.
func queuedFlushItemFromUnconfirmed(item triage.UnconfirmedFlushItem) (triage.QueuedFlushItem, bool) {
	queueItemID := strings.TrimSpace(item.QueueItemID)
	message := item.Message
	payload := append(json.RawMessage(nil), item.Payload...)
	if !queuedPayloadAgentWritten(payload) {
		message = strings.TrimSpace(message)
	}
	if item.DeferredItem != nil {
		message, payload = queuePayloadFromUserItem(*item.DeferredItem, item.JoinedParts, payload)
	}
	if item.QuietItem != nil {
		message, payload = queuePayloadFromUserItem(*item.QuietItem, item.JoinedParts, payload)
	}
	if queueItemID == "" {
		queueItemID = flushqueue.NewItemID()
	}
	if message == "" && len(payload) == 0 {
		return triage.QueuedFlushItem{}, false
	}
	return triage.QueuedFlushItem{
		ID:              queueItemID,
		Message:         message,
		Payload:         payload,
		EnqueuedAt:      item.EnqueuedAt,
		StaleUserItemID: item.StaleUserItemID,
		Settlement:      item.Settlement,
	}, true
}

// queuePayloadFromUserItem is the text and requeue payload of a row's
// message (requeuePayloadFromMeta), or its text and fallback when the row's
// meta does not decode. parts are a joined row's members
// (agentmeta.JoinedPart), kept while they describe the requeued text. The
// text is the row's without the white space around it, unless the row holds
// an agent's message, which goes back as written (queuedFlushItemFromUnconfirmed).
func queuePayloadFromUserItem(item store.Item, parts []agentmeta.JoinedPart, fallback json.RawMessage) (string, json.RawMessage) {
	meta, err := usermessage.FromItem(item)
	if err != nil {
		return strings.TrimSpace(item.Summary), fallback
	}
	requeued := requeuePayloadFromMeta(meta)
	text := item.Summary
	if agentWritten(meta.Origin, parts) {
		if _, ok := agentmeta.SplitJoinedParts(text, parts); ok {
			requeued.JoinedParts = parts
		}
	} else {
		text = strings.TrimSpace(text)
		requeued.JoinedParts = usermessage.TrimJoinedParts(item.Summary, parts)
	}
	payload, err := json.Marshal(requeued)
	if err != nil {
		return text, fallback
	}
	return text, payload
}

// agentWritten reports whether a message of this origin, or any of the
// members parts records, is an agent's or the app's.
func agentWritten(origin string, parts []agentmeta.JoinedPart) bool {
	return agentmeta.IsAgentMessage(agentmeta.Meta{Origin: origin}) || slices.ContainsFunc(parts, agentmeta.JoinedPart.IsAgent)
}

// queuedPayloadAgentWritten is agentWritten of a queue item's payload. A
// payload that does not decode is the person's.
func queuedPayloadAgentWritten(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var payload flushQueuePayload
	if json.Unmarshal(raw, &payload) != nil {
		return false
	}
	return agentWritten(payload.Origin, payload.JoinedParts)
}

// draftPartFromUserItem is the composer part of a message taken back
// undelivered from its row: its text without the review comments, which
// the row's retire returned to drafts, so the composer and the drafts never
// both carry them.
func draftPartFromUserItem(item store.Item) composerdraft.Part {
	part, err := composerdraft.PartFromUserItem(item)
	if err != nil {
		log.Printf("flush queue: decode restored user item meta %s/%s: %v", item.ThreadID, item.ID, err)
		return composerdraft.Part{Content: item.Summary}
	}
	part.Content = rowTextWithoutReviewComments(item.Summary, item.Meta)
	return part
}

func (a *App) restoredDraftPartFromQueuedPayload(threadID string, item triage.UnconfirmedFlushItem) composerdraft.Part {
	var payload flushQueuePayload
	if len(item.Payload) > 0 {
		if err := json.Unmarshal(item.Payload, &payload); err != nil {
			log.Printf("flush queue: decode queued restore payload %s/%s: %v", threadID, item.QueueItemID, err)
			return composerdraft.Part{Content: item.Message}
		}
	}
	content := item.Message
	if payload.TextBeforeReviewComments != nil {
		// A requeued message renders its review comments, which the
		// restore returned to drafts.
		content = *payload.TextBeforeReviewComments
	}
	return composerdraft.Part{
		Content:            content,
		AttachmentIDs:      payload.AttachmentIDs,
		SourceProposedPlan: payload.SourceProposedPlan,
	}
}
