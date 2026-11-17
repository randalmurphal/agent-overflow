package app

import (
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"agent-overflow/internal/composerdraft"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// restoreDurableFlushQueueAtBoot puts every queued message the previous
// process did not deliver back into its thread's composer draft, and deletes
// the rows: the durable queue rows, and the rows reserved for messages the
// process wrote to the provider and never saw confirmed (a Claude message
// queued behind a running turn, whose quiet row is still marked
// pendingFlush). The provider may have read such a message before the crash
// or never; the composer is where one that is not proven delivered goes, so
// it never reads as sent.
//
// What the app wrote is not the person's to send and goes elsewhere: a loop
// or watch wake, an outside send, a worker's prompt or another thread's
// request is discarded (discardAgentWakeAtBoot), and an answer, a reminder or
// a remote completion goes back to its owner (giveBackToOwners), as does one
// the previous process handed a provider that never confirmed it
// (giveBackUnconfirmedAtBoot).
//
// It NEVER re-dispatches. A queued message was written against a turn that no
// longer exists, on a provider session that is gone, in a conversation the
// user has not looked at since; sending it hours later on their behalf is the
// one outcome nobody asked for. The composer is where a message that has not
// been delivered belongs, and it is exactly where a session death already
// puts one — same merge rule, ahead of whatever the composer itself holds,
// same `draft:updated` event.
//
// Runs at boot, after the store is open and before any session can start, so
// nothing else could still be holding a queue row. A marked user row is taken
// back only while it is residue, in its thread's last turn
// (store.ListUnconfirmedUserRows); one a later turn follows is history and
// stays where it is, and the store refuses to retire it.
func (a *App) restoreDurableFlushQueueAtBoot() {
	// Once the rows are settled, the answers, reminders and completions the
	// previous process handed a provider and never saw confirmed go back
	// too.
	defer a.giveBackUnconfirmedAtBoot()
	threadIDs, err := a.store.ListThreadsWithFlushQueueItems()
	if err != nil {
		a.bootPhaseFailed(fmt.Errorf("list threads with queued messages: %w", err))
		return
	}
	unconfirmed, err := a.store.ListUnconfirmedUserRows()
	if err != nil {
		// The queue rows are still restored; the unconfirmed rows wait for
		// the next boot.
		a.bootPhaseFailed(fmt.Errorf("list unconfirmed queued messages: %w", err))
	}
	unconfirmedByThread := make(map[string][]string)
	for _, row := range unconfirmed {
		if _, seen := unconfirmedByThread[row.ThreadID]; !seen {
			threadIDs = append(threadIDs, row.ThreadID)
		}
		unconfirmedByThread[row.ThreadID] = append(unconfirmedByThread[row.ThreadID], row.ItemID)
	}
	threadIDs = slices.Compact(slices.Sorted(slices.Values(threadIDs)))
	var errs []error
	for _, threadID := range threadIDs {
		if err := a.restoreThreadQueueAtBoot(threadID, unconfirmedByThread[threadID]); err != nil {
			errs = append(errs, fmt.Errorf("thread %s: %w", threadID, err))
		}
	}
	if len(errs) > 0 {
		a.bootPhaseFailed(fmt.Errorf("restore queued messages into the composer: %w", errors.Join(errs...)))
	}
}

// restoreThreadQueueAtBoot is the boot restore of one thread. The rows of
// unconfirmed messages come first: they were dispatched, so they were queued
// before anything still on the queue.
//
// A queue row that is a copy of a message an unconfirmed row carries (its
// send id is among the row's) gives no text of its own. A queue row outlives
// its message's provider write when the process dies between the write and
// its settlement, or when the message is an answer to an agent's question,
// which keeps its row until the echo. The unconfirmed row is the unit of the
// person's text: restoring some members from their queue copies and the rest
// from the row would put a member in the composer twice or lose it. A joined
// row that carries the app's messages splits by the parts its dispatch
// recorded (unconfirmedRowAtBoot), so only the person's text reaches the
// composer; one whose parts no longer describe its text is restored whole.
//
// A queue row whose message a row the provider confirmed carries was
// delivered before the process died: it leaves with the rest, without
// putting the message in the composer.
//
// The draft, the queue rows and the unconfirmed rows with the review
// comments they claimed are written in one transaction, so a failure or a
// crash leaves every message where it was for the next boot. An answer, a
// reminder or a completion goes back to its owner with its queue row, in a
// transaction of its own; one that fails keeps its row for the next boot.
// One whose echo the store recorded loses its row and stays delivered: the
// owner's withdrawal never takes back a confirmed delivery
// (store.WithdrawThreadWake, store.WithdrawRemoteCompletion,
// store.ReturnAsyncAnswer).
// Nothing else writes the thread's rows while the boot runs (no session has
// started and no client call is served before Start returns), so the rows
// read here are the rows that transaction changes.
func (a *App) restoreThreadQueueAtBoot(threadID string, unconfirmedIDs []string) error {
	rows, err := a.store.ListFlushQueueItems(threadID)
	if err != nil {
		return fmt.Errorf("list queued messages: %w", err)
	}
	parts := make([]composerdraft.Part, 0, len(unconfirmedIDs)+len(rows))
	var retire []string
	carried := make(map[string]bool)
	for _, id := range unconfirmedIDs {
		item, found, err := a.store.GetThreadItem(threadID, id)
		if err != nil {
			return fmt.Errorf("read unconfirmed message %s: %w", id, err)
		}
		if !found {
			continue
		}
		retire = append(retire, id)
		for _, sendID := range userRowSendIDs(item) {
			carried[sendID] = true
		}
		if part, restore := a.unconfirmedRowAtBoot(threadID, item); restore && !emptyDraftPart(part) {
			parts = append(parts, part)
		}
	}
	ids := make([]string, 0, len(rows))
	var owned []ownedMessage
	for _, row := range rows {
		if ownedSendID(row.SendID) {
			owned = append(owned, ownedMessage{sendID: row.SendID, rowID: row.ID})
			continue
		}
		ids = append(ids, row.ID)
		if row.SendID != "" && carried[row.SendID] {
			continue
		}
		delivered, err := a.queuedSendDelivered(threadID, row.SendID)
		if err != nil {
			return fmt.Errorf("read the row of queued message %s: %w", row.ID, err)
		}
		if delivered {
			log.Printf("app: queued message %s of thread %s was delivered before the restart; dropping its queue row", row.ID, threadID)
			continue
		}
		// Recorded on the row before the queue row goes, so a failed
		// restore leaves both for the next boot to try again.
		if a.discardAgentWakeAtBoot(row) {
			continue
		}
		if part := draftPartFromQueueRow(row); !emptyDraftPart(part) {
			parts = append(parts, part)
		}
	}
	if owned = a.confirmEchoedAtBoot(threadID, owned); len(owned) > 0 {
		a.giveBackToOwners(threadID, owned)
	}
	if len(parts) == 0 && len(ids) == 0 && len(retire) == 0 {
		return nil
	}
	if err := a.restoreQueuedDraft(threadID, parts, store.QueueDraftRestore{
		QueueItemIDs: ids, RetireItemIDs: retire,
	}); err != nil {
		// Nothing was written: the next boot tries again, which is the
		// whole point of the message being durable.
		return fmt.Errorf("restore into the draft: %w", err)
	}
	a.emit(eventchan.ProviderAsyncQuestionsChanged, map[string]string{"threadId": threadID})
	if len(parts) > 0 {
		log.Printf("app: restored %d queued message(s) into the composer for thread %s", len(parts), threadID)
	}
	return nil
}

// unconfirmedRowAtBoot is what of an unconfirmed row the boot restores to
// the composer, false when nothing is. The app's messages it carries are
// handled as if each were still queued at quit (discardAgentWakeAtBoot),
// with the text its part recorded; its answers, reminders and completions
// are giveBackUnconfirmedAtBoot's. A row whose parts no longer describe its
// text cannot be told apart from the person's and is restored whole, as a
// session end restores one (agentPartsNotTakenIn).
func (a *App) unconfirmedRowAtBoot(threadID string, item store.Item) (composerdraft.Part, bool) {
	part := draftPartFromUserItem(item)
	members := unconfirmedMembersOf(triage.UnconfirmedFlushItem{QuietItem: &item})
	if !slices.ContainsFunc(members.ids, appWrittenSendID) {
		return part, true
	}
	messages, person, split := splitSessionEndParts(members, part.Content)
	if !split {
		log.Printf("flush queue: an unconfirmed message of %s does not split into the app messages it names; restoring all of it", threadID)
		return part, true
	}
	for _, id := range members.ids {
		if ownedSendID(id) || (agentAppPartRoot(id) == "" && threadRequestSendIDToken(id) == "") {
			continue
		}
		a.discardAgentWakeAtBoot(store.FlushQueueItem{ThreadID: threadID, SendID: id, Message: messages[id].Text()})
	}
	if person == nil {
		return composerdraft.Part{}, false
	}
	part.Content = *person
	return part, true
}

// queuedSendDelivered reports whether the provider confirmed the message a
// queue row with this send id is a copy of: a user row answers for the send
// id and carries the provider's id for it. A confirmation can land between
// the provider write and the settlement that deletes the queue row, so a
// crash there leaves both. A row the provider has not confirmed (a revealed
// interrupt row still awaiting its echo) is no proof of delivery.
func (a *App) queuedSendDelivered(threadID, sendID string) (bool, error) {
	itemID, err := a.deliveredSendRow(threadID, sendID)
	return itemID != "", err
}

// deliveredSendRow is the row that proves the provider confirmed the message
// sendID (queuedSendDelivered), empty when none does.
func (a *App) deliveredSendRow(threadID, sendID string) (string, error) {
	if sendID == "" {
		return "", nil
	}
	item, found, err := a.store.FindUserTextItemBySendID(threadID, sendID)
	if err != nil || !found || usermessage.ReadProviderItemID(item.Meta) == "" {
		return "", err
	}
	return item.ID, nil
}

// confirmEchoedAtBoot writes the confirmation of each answer, reminder,
// completion or spawner's answer whose echo a row of the last run proves
// (deliveredSendRow), before the boot gives them back: the last run can die
// between the echo's row write and its confirmation, or after a
// confirmation write that failed, which only memory kept. The row is the
// provider's word that the agent took the message in, and the owner's
// withdrawal then only drops a queue row that outlived it
// (giveBackToOwners); a confirmation whose write fails again is kept as an
// echo, which giveBackToOwners skips. A message whose row cannot be read is
// left out, with its queue row or its unconfirmed mark, for the next boot.
func (a *App) confirmEchoedAtBoot(threadID string, messages []ownedMessage) []ownedMessage {
	now := time.Now().UnixMilli()
	return slices.DeleteFunc(messages, func(m ownedMessage) bool {
		itemID, err := a.deliveredSendRow(threadID, m.sendID)
		if err != nil {
			logThreadRequestSweep("read the echo of "+m.sendID+" in "+threadID+" at boot; left for the next boot", err)
			return true
		}
		if itemID != "" {
			log.Printf("app: %s of thread %s was taken in before the restart; recording its confirmation", m.sendID, threadID)
			if err := a.confirmOwnedEcho(m.sendID, ownedEcho{threadID: threadID, itemID: itemID, at: now}); err != nil {
				log.Printf("thread tools: confirm %s in %s at boot, kept to try again: %v", m.sendID, threadID, err)
			}
		}
		return false
	})
}
