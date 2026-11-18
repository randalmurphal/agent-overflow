package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/composerdraft"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/usermessage"
)

// What a handoff moves into the successor's composer: the old thread's unsent
// draft and the messages of the person's sends that found the session gone
// while the handoff ran, or back to the old composer when it cannot.

// assistantHandOffState holds, by assistant, the thread its running handoff
// leaves, and by that thread the messages of the person's sends that found
// the session gone while it ran, in the order they arrived. An assistant
// runs one handoff at a time, whether the person or a session loss started
// it. One lock covers both maps, so a send adds its message only while a
// handoff runs that will take it, and the handoff takes what is left as it
// ends.
type assistantHandOffState struct {
	mu      sync.Mutex
	leaving map[string]string
	items   map[string][]store.Item
}

// begin marks a handoff from threadID running for the assistant, and
// reports false when one already is.
func (c *assistantHandOffState) begin(agentID, threadID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, running := c.leaving[agentID]; running {
		return false
	}
	if c.leaving == nil {
		c.leaving = map[string]string{}
	}
	c.leaving[agentID] = threadID
	return true
}

// add keeps a send's message for the handoff running from its thread, and
// reports false when none runs to take it.
func (c *assistantHandOffState) add(threadID string, item store.Item) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	running := false
	for _, leaving := range c.leaving {
		running = running || leaving == threadID
	}
	if !running {
		return false
	}
	if c.items == nil {
		c.items = map[string][]store.Item{}
	}
	c.items[threadID] = append(c.items[threadID], item)
	return true
}

// take returns the messages kept so far, oldest first.
func (c *assistantHandOffState) take(threadID string) []store.Item {
	c.mu.Lock()
	defer c.mu.Unlock()
	items := c.items[threadID]
	delete(c.items, threadID)
	return items
}

// finish ends the assistant's handoff and returns the messages it did not
// take.
func (c *assistantHandOffState) finish(agentID string) []store.Item {
	c.mu.Lock()
	defer c.mu.Unlock()
	threadID := c.leaving[agentID]
	delete(c.leaving, agentID)
	items := c.items[threadID]
	delete(c.items, threadID)
	return items
}

// endAssistantHandOff ends the assistant's running handoff from threadID;
// successorID is empty when the handoff made none. A send that found the
// session gone after the handoff took the carried messages added its own,
// which follows them to the successor. With no successor every message goes
// back to the old composer, and goes back before the handoff ends, so a send
// that finds the session gone meanwhile joins them rather than starting a
// handoff that could move the composer before they land in it.
func (a *App) endAssistantHandOff(agentID, threadID, successorID string) {
	if successorID == "" {
		a.returnSessionLossCarries(threadID, a.assistantHandOffs.take(threadID))
		a.returnSessionLossCarries(threadID, a.assistantHandOffs.finish(agentID))
		return
	}
	if left := a.assistantHandOffs.finish(agentID); len(left) > 0 {
		a.carrySessionLossItems(threadID, successorID, left)
	}
}

// moveComposerAcrossHandOffLocked moves what waits in the old thread's
// composer to the successor's, so the pane that follows the handoff shows
// it: the messages of sends that found the session gone, then the unsent
// draft, each with its attachments copied over, its terminal snippets and
// plan link kept, and clears the old draft. The caller holds the old
// thread's action lock, which a send holds from its check of the draft it
// consumes (refuseSendOfMovedDraft) to the consume, and its mutation lock,
// which every draft save and queued send takes, so the move lands wholly
// before or after each of them. A move that fails leaves the draft, puts the
// carried messages back in the old composer, and tells the successor where
// they are. It returns how many carried messages moved.
func (a *App) moveComposerAcrossHandOffLocked(oldThreadID, successorID string) int {
	items := a.assistantHandOffs.take(oldThreadID)
	err := a.moveComposerLocked(oldThreadID, successorID, items)
	if err == nil {
		return len(items)
	}
	log.Printf("assistants: move the composer of %s into %s: %v", oldThreadID, successorID, err)
	a.returnSessionLossCarriesLocked(oldThreadID, items)
	a.emitErrorToThread(successorID, "Unsent text in the previous thread's composer could not be moved here, and is still there: "+err.Error())
	return 0
}

// moveComposerLocked copies every attachment first, then writes the
// successor's draft and clears the old one in one transaction, so the text
// is in one composer whatever fails. A failure deletes the copies.
func (a *App) moveComposerLocked(oldThreadID, successorID string, items []store.Item) (err error) {
	old, found, err := a.store.GetThreadDraft(oldThreadID)
	if err != nil {
		return err
	}
	if len(items) == 0 && (!found || draftIsEmpty(old)) {
		return nil
	}
	var copies []string
	defer func() {
		if err != nil {
			err = errors.Join(err, a.deleteDraftCopies(successorID, copies))
		}
	}()
	now := time.Now().UnixMilli()
	parts := make([]composerdraft.Part, 0, len(items)+1)
	for _, item := range items {
		part, err := a.draftPartWithClonedAttachments(successorID, item, now)
		if err != nil {
			return err
		}
		copies = append(copies, part.AttachmentIDs...)
		parts = append(parts, part)
	}
	var oldIDs []string
	if found && strings.TrimSpace(old.Attachments) != "" {
		if err := json.Unmarshal([]byte(old.Attachments), &oldIDs); err != nil {
			return fmt.Errorf("read the draft's attachments: %w", err)
		}
	}
	cloned := make([]string, 0, len(oldIDs))
	for _, id := range oldIDs {
		if a.attachments == nil {
			return errors.New("attachment storage is unavailable")
		}
		copied, err := a.attachments.CopyToThread(oldThreadID, successorID, id, now)
		if err != nil {
			return fmt.Errorf("copy draft attachment %s: %w", id, err)
		}
		copies = append(copies, copied.ID)
		cloned = append(cloned, copied.ID)
	}
	parts = append(parts, composerdraft.Part{Content: old.Content, AttachmentIDs: cloned})
	current, _, err := a.store.GetThreadDraft(successorID)
	if err != nil {
		return err
	}
	merged, err := composerdraft.MergeParts(successorID, current, parts, now)
	if err != nil {
		return err
	}
	if merged.TerminalChips, err = joinTerminalChips(merged.TerminalChips, old.TerminalChips); err != nil {
		return err
	}
	if merged.PendingPlanImplementation == "" && old.PendingPlanImplementation != "" {
		// A link with no thread names a plan in the draft's own thread,
		// which is now the old one.
		var ref store.ProposedPlanSourceRef
		if err := json.Unmarshal([]byte(old.PendingPlanImplementation), &ref); err != nil {
			return fmt.Errorf("read the draft's plan link: %w", err)
		}
		if ref.ThreadID == "" {
			ref.ThreadID = oldThreadID
		}
		if merged.PendingPlanImplementation, err = usermessage.EncodeDraftSource(&ref); err != nil {
			return err
		}
	}
	var expected *store.ThreadDraft
	if found {
		expected = &old
	}
	return a.writeHandedOffThreadDraft(oldThreadID, expected, merged)
}

// refuseSendOfMovedDraft refuses a send from an assistant's past thread that
// consumes the draft the handoff took from it (MovedDraftOf): that text waits
// in the assistant's current thread's composer, so sending it here as well
// would leave it in two places. Every other send to a past thread goes
// through as on any thread, a consume snapshot that matches nothing stored
// included. The caller holds the lock the move holds through the consume:
// the action lock for a send, the mutation lock for a queued one.
func (a *App) refuseSendOfMovedDraft(threadID string, consume *DraftSnapshot) error {
	if consume == nil {
		return nil
	}
	moved, past, err := a.store.MovedDraftOf(threadID)
	if err != nil || !past || (moved.Content == "" && moved.Attachments == "[]") {
		return err
	}
	expected, err := encodeThreadDraft(threadID, *consume)
	if err != nil {
		return err
	}
	if moved.Content != expected.Content || moved.Attachments != expected.Attachments {
		return nil
	}
	stored, _, err := a.store.GetThreadDraft(threadID)
	if err != nil {
		return err
	}
	if store.SameDraft(stored, expected) {
		return nil
	}
	current, _, err := a.store.AssistantCurrentThreadOf(threadID)
	if err != nil {
		return err
	}
	thread, err := a.store.GetThread(current)
	if err != nil {
		return fmt.Errorf("read the assistant's current thread: %w", err)
	}
	return errorsx.Public(transport.ErrCodeAssistantDraftMoved, fmt.Sprintf("Your unsent text moved to %s; send it from there.", thread.Title), nil)
}

// deleteDraftCopies deletes the attachment copies of a draft move that
// failed.
func (a *App) deleteDraftCopies(threadID string, ids []string) error {
	var errs []error
	for _, id := range ids {
		if err := a.attachments.Delete(threadID, id); err != nil {
			errs = append(errs, fmt.Errorf("delete the attachment copy %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

func draftIsEmpty(d store.ThreadDraft) bool {
	empty := func(list string) bool { s := strings.TrimSpace(list); return s == "" || s == "[]" }
	return strings.TrimSpace(d.Content) == "" && empty(d.Attachments) && empty(d.TerminalChips) && d.PendingPlanImplementation == ""
}

// joinTerminalChips appends the chips of b to those of a, each a JSON array.
func joinTerminalChips(a, b string) (string, error) {
	var all []json.RawMessage
	for _, list := range []string{a, b} {
		if strings.TrimSpace(list) == "" {
			continue
		}
		var chips []json.RawMessage
		if err := json.Unmarshal([]byte(list), &chips); err != nil {
			return "", fmt.Errorf("read terminal snippets: %w", err)
		}
		all = append(all, chips...)
	}
	if len(all) == 0 {
		return "[]", nil
	}
	out, err := json.Marshal(all)
	return string(out), err
}

// carrySessionLossItems merges the messages into the successor's composer,
// one blank line between them, each with its attachments copied over. A
// write that fails puts them back in the old thread's composer.
func (a *App) carrySessionLossItems(oldThreadID, successorID string, items []store.Item) bool {
	now := time.Now().UnixMilli()
	parts := make([]composerdraft.Part, 0, len(items))
	var err error
	for _, item := range items {
		var part composerdraft.Part
		if part, err = a.draftPartWithClonedAttachments(successorID, item, now); err != nil {
			break
		}
		parts = append(parts, part)
	}
	if err == nil {
		err = a.restoreQueuedDraft(successorID, parts, store.QueueDraftRestore{})
	}
	if err != nil {
		log.Printf("assistants: carry the messages to %s into %s: %v", oldThreadID, successorID, err)
		a.returnSessionLossCarries(oldThreadID, items)
		return false
	}
	return true
}

// returnSessionLossCarries puts the messages back in the old thread's
// composer, oldest first, ahead of anything typed there since.
func (a *App) returnSessionLossCarries(threadID string, items []store.Item) {
	if len(items) == 0 {
		return
	}
	// The draft saves and a handoff's composer move hold this lock.
	unlock := a.threadApplication().LockMutations(threadID)
	defer unlock()
	a.returnSessionLossCarriesLocked(threadID, items)
}

// returnSessionLossCarriesLocked is returnSessionLossCarries for a caller
// that holds the thread's mutation lock.
func (a *App) returnSessionLossCarriesLocked(threadID string, items []store.Item) {
	if len(items) == 0 {
		return
	}
	parts := make([]composerdraft.Part, 0, len(items))
	var err error
	for _, item := range items {
		var part composerdraft.Part
		if part, err = composerdraft.PartFromUserItem(item); err != nil {
			break
		}
		parts = append(parts, part)
	}
	if err == nil {
		err = a.restoreQueuedDraft(threadID, parts, store.QueueDraftRestore{})
	}
	if err != nil {
		log.Printf("assistants: put %d messages back in %s's composer: %v", len(items), threadID, err)
		a.emitErrorToThread(threadID, "Your messages could not be put back in the composer: "+err.Error())
	}
}
