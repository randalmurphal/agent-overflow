package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/agenthome"
	"agent-overflow/internal/composerdraft"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// New Thread, Same Assistant (docs/specs/standing-agents.md, "Next thread").
//
// The record moves to a new thread in one store transaction that also moves
// the loops, watches and every request the old thread made, and archives it
// as a past thread. Then the old thread's session ends, and everything the
// app had queued for it goes back to its owner the way an archive gives it
// back, except that every give-back path asks assistantCurrentThread first
// and sends the message on to the successor instead of disarming or dropping
// it. The successor opens with the handoff block.

// StartNextAssistantThread replaces an assistant's current thread with a new
// one and returns it. Refused for a thread that is not an assistant's
// current thread on this computer, for an archived one, and while the
// assistant's previous handoff is still running.
//
//ao:scope threads:autonomy
func (a *App) StartNextAssistantThread(ctx context.Context, threadID string) (store.Thread, error) {
	agent, found, err := a.store.GetStandingAgent(threadID)
	if err != nil {
		return store.Thread{}, err
	}
	if !found {
		return store.Thread{}, errNotAssistantThread
	}
	if !a.assistantHandOffs.begin(agent.ID, threadID) {
		return store.Thread{}, errorsx.Public(transport.ErrCodeAssistantHandOffRunning, "This assistant is already moving to a new thread.", nil)
	}
	successor, err := a.nextAssistantThread(ctx, threadID, false)
	a.endAssistantHandOff(agent.ID, threadID, successor.ID)
	return successor, err
}

// errAssistantSessionGone is a start of an assistant's current thread whose
// provider session the provider itself cleaned up: Claude's transcript file
// is gone, or Codex refuses thread/resume with no rollout. The start fails
// with it and the assistant continues in a new thread
// (recoverAssistantSessionLoss).
var errAssistantSessionGone = errors.New("the provider no longer has this assistant's session, so the assistant continues in a new thread")

// assistantSessionGoneMessage is what a person whose send found the session
// gone is told.
const assistantSessionGoneMessage = "The provider cleaned up this assistant's session. It continues in a new thread, and your message is waiting in that thread's composer."

// requireAssistantSession refuses a resume of an assistant's current thread
// whose transcript its provider says is gone, before the start tears
// anything down. A provider with no transcript to look for
// (provider.NoResumeTranscript) refuses the resume itself
// (startSessionNow). A check that cannot tell lets the start go on, and the
// provider's own refusal decides.
func (a *App) requireAssistantSession(t store.Thread, opts provider.SessionOptions) error {
	if opts.Resume == "" || opts.ForkSession || t.AgentID == "" {
		return nil
	}
	switch provider.CapabilitiesForProvider(t.Provider).ResumeTranscript {
	case provider.NoResumeTranscript:
		return nil
	case provider.ClaudeProjectsTranscript:
		projectsDir, err := a.claudeProjectsDir()
		if err != nil {
			return err
		}
		gone, err := sessionfork.ResumeTranscriptGone(projectsDir, opts.Resume, opts.WorkDir)
		if err != nil {
			log.Printf("assistants: check the transcript of %s: %v", t.ID, err)
			return nil
		}
		if gone {
			return errors.Join(errAssistantSessionGone, sessionfork.ErrSessionFileNotFound)
		}
		return nil
	default:
		return fmt.Errorf("check the session of %s: provider %s keeps its transcript where the app cannot look", t.ID, t.Provider)
	}
}

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

// recoverAssistantSessionLoss runs New Thread, Same Assistant for an
// assistant whose session the provider cleaned up, off the start that found
// it (which holds the thread's action lock), unless a handoff of the
// assistant already runs, which takes the person's sends the same way. The
// new thread's opening block says why. A handoff that fails, or never starts
// because the app is stopping, puts every message a send left for it back
// in the old thread's composer.
func (a *App) recoverAssistantSessionLoss(threadID string) {
	agent, found, err := a.store.GetStandingAgent(threadID)
	if err != nil {
		log.Printf("assistants: %s lost its provider session; read its assistant: %v", threadID, err)
		return
	}
	if !found || !a.assistantHandOffs.begin(agent.ID, threadID) {
		return
	}
	started := a.goAgentNoticeWork(threadID, func() {
		if a.sessionLossHandOffForTest != nil {
			a.sessionLossHandOffForTest(threadID)
		}
		successor, err := a.nextAssistantThread(a.lifeCtx(), threadID, true)
		switch {
		case successor.ID == "":
			log.Printf("assistants: %s lost its provider session and could not move to a new thread: %v", threadID, err)
			a.emitErrorToThread(threadID, "The provider cleaned up this assistant's session, and moving it to a new thread failed: "+err.Error()+". Use New Thread, Same Assistant to try again.")
		case err != nil:
			log.Printf("assistants: %s lost its provider session and continues in %s: %v", threadID, successor.ID, err)
		default:
			log.Printf("assistants: %s lost its provider session; it continues in %s", threadID, successor.ID)
		}
		a.endAssistantHandOff(agent.ID, threadID, successor.ID)
	})
	if !started {
		a.returnSessionLossCarries(threadID, a.assistantHandOffs.finish(agent.ID))
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
		_, err = a.mergeAndUpsertThreadDraft(successorID, parts)
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
		_, err = a.mergeAndUpsertThreadDraft(threadID, parts)
	}
	if err != nil {
		log.Printf("assistants: put %d messages back in %s's composer: %v", len(items), threadID, err)
		a.emitErrorToThread(threadID, "Your messages could not be put back in the composer: "+err.Error())
	}
}

// nextAssistantThread is New Thread, Same Assistant; sessionLost says the
// provider cleaned up the old thread's session, which the opening block
// tells the assistant.
func (a *App) nextAssistantThread(ctx context.Context, threadID string, sessionLost bool) (store.Thread, error) {
	agent, found, err := a.store.GetStandingAgent(threadID)
	if err != nil {
		return store.Thread{}, err
	}
	if !found {
		return store.Thread{}, errNotAssistantThread
	}
	old, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, fmt.Errorf("read the assistant's thread: %w", err)
	}
	if err := a.store.CheckThreadTransferAccess(threadID); err != nil {
		return store.Thread{}, err
	}
	if old.Archived {
		return store.Thread{}, errors.New("this assistant is archived; unarchive it first")
	}
	// The successor lives where an assistant's thread does: outside every
	// project, in the home.
	standardPercent, extendedPercent := old.AutoCompactStandardPercent, old.AutoCompactExtendedPercent
	opts := CreateThreadOptions{
		Title:                      old.Title,
		Provider:                   old.Provider,
		Model:                      old.Model,
		Mode:                       string(threadmode.ModeChat),
		ReasoningEffort:            old.ReasoningEffort,
		FastMode:                   &old.FastMode,
		ContextWindow:              old.ContextWindow,
		AutoCompactStandardPercent: &standardPercent,
		AutoCompactExtendedPercent: &extendedPercent,
		RuntimeMode:                old.RuntimeMode,
		home:                       agent.Home,
	}
	endWork, err := a.workAdmission.begin(ctx)
	if err != nil {
		return store.Thread{}, err
	}
	defer endWork()
	successor, err := a.createThreadRow(ctx, opts, false)
	if err != nil {
		return store.Thread{}, fmt.Errorf("create the assistant's next thread: %w", err)
	}
	handOff, interrupted, carried, err := a.handOffAssistantLocked(threadID, successor.ID)
	if err != nil {
		if deleteErr := a.DeleteThread(successor.ID); deleteErr != nil {
			log.Printf("assistants: delete the next thread %s of a handoff that failed: %v", successor.ID, deleteErr)
		}
		return store.Thread{}, err
	}
	if a.handOffCommittedForTest != nil {
		a.handOffCommittedForTest(threadID, successor.ID)
	}
	a.continueAfterHandOff(threadID, successor.ID, handOff)
	if fresh, err := a.store.GetThread(successor.ID); err != nil {
		return successor, fmt.Errorf("the assistant moved to its new thread, which could not be read back: %w", err)
	} else {
		successor = fresh
	}
	// The successor takes the agent row's place before the old row leaves
	// the list as an archive does, so a client moves the panes that showed
	// the old thread to it instead of closing them; the workers now nest
	// under it.
	a.announceThread(triage.ThreadActionListed, successor.ID)
	a.threadAnnouncer().AnnounceHandOff(a.emitEvent, threadID, successor.ID)
	a.broadcastSpawnedThreadRows(successor.ID)
	body := agenthome.HandOffMessage(agenthome.HandOff{
		Name:             successor.Title,
		PreviousThreadID: threadID,
		MovedWakes:       len(handOff.MovedWakeIDs),
		MovedRequests:    handOff.MovedRequests,
		Interrupted:      a.interruptedByHandOff(interrupted),
		SessionLost:      sessionLost,
		Carried:          carried,
	})
	if err := a.queueAssistantMessage(successor.ID, assistantSendID(assistantWakeKindHandOff, threadID), successor.Title, body); err != nil {
		return successor, fmt.Errorf("the assistant moved to its new thread, but its opening block could not be queued: %w", err)
	}
	log.Printf("assistants: %s moved from thread %s to %s", agent.ID, threadID, successor.ID)
	return successor, nil
}

// handOffAssistantLocked runs the store handoff, moves the composer and ends
// the old thread's session under its action lock, as an archive does. The
// requests the old thread had read and not answered settle first, saying
// where the assistant went; it returns their tokens, and how many carried
// messages the composer move took.
func (a *App) handOffAssistantLocked(oldThreadID, newThreadID string) (store.AssistantHandOff, []string, int, error) {
	unlock := a.threadLocks().Lock(oldThreadID)
	defer unlock()
	unlockMutable, err := a.threadApplication().LockMutable(context.Background(), oldThreadID)
	if err != nil {
		return store.AssistantHandOff{}, nil, 0, err
	}
	handOff, err := a.store.HandOffAssistant(oldThreadID, newThreadID, time.Now().UnixMilli())
	if err != nil {
		unlockMutable()
		return store.AssistantHandOff{}, nil, 0, err
	}
	carried := a.moveComposerAcrossHandOffLocked(oldThreadID, newThreadID)
	unlockMutable()
	interrupted := a.settleReadRequestsForHandOff(oldThreadID)
	if sess, ok := a.sessionManager().take(oldThreadID); ok {
		if err := a.teardownAndCloseSession(oldThreadID, sess); err != nil {
			log.Printf("assistants: hand off %s: close its provider session: %v", oldThreadID, err)
		}
	}
	if err := a.closeThreadBrowserPages(oldThreadID); err != nil {
		log.Printf("assistants: hand off %s: close browser pages: %v", oldThreadID, err)
	}
	return handOff, interrupted, carried, nil
}

// settleReadRequestsForHandOff settles the requests addressed to the old
// thread that it had read and not answered: their turn is the one the
// handoff ends. One not read yet is the teardown's to give back, and it goes
// on to the successor's queue.
func (a *App) settleReadRequestsForHandOff(oldThreadID string) []string {
	receipts, err := a.store.ListThreadRequestReceiptsForThread(oldThreadID)
	if err != nil {
		log.Printf("assistants: hand off %s: read its requests: %v", oldThreadID, err)
		return nil
	}
	var settled []string
	why := threadtools.InterruptHandedOff(oldThreadID)
	for _, receipt := range receipts {
		if receipt.State != store.ThreadReceiptRunning {
			continue
		}
		if receipt.TurnID == "" && !a.receiptTakenIn(oldThreadID, receipt.Token) {
			continue
		}
		applied, err := a.settleThreadReceiptNoScratch(receipt.Token, []string{store.ThreadReceiptRunning}, store.ThreadRequestSettlement{
			State:      store.ThreadReceiptInterrupted,
			Answer:     []byte(why),
			AnswerKind: store.ThreadAnswerError,
		})
		if err != nil {
			log.Printf("assistants: hand off %s: settle request %s: %v", oldThreadID, receipt.Token, err)
			continue
		}
		if applied {
			settled = append(settled, receipt.Token)
		}
	}
	return settled
}

// interruptedByHandOff names the senders of the requests the handoff
// settled, for the successor's opening block.
func (a *App) interruptedByHandOff(tokens []string) []agenthome.InterruptedRequest {
	out := make([]agenthome.InterruptedRequest, 0, len(tokens))
	for _, token := range tokens {
		receipt, found, err := a.store.GetThreadRequestReceipt(token)
		if err != nil || !found {
			if err != nil {
				log.Printf("assistants: read interrupted request %s: %v", token, err)
			}
			continue
		}
		out = append(out, agenthome.InterruptedRequest{Token: token, SenderTitle: receipt.SourceThreadTitle, SenderID: receipt.SourceThreadID})
	}
	return out
}

// continueAfterHandOff carries over what the old thread held once its
// session is gone: its queued messages go to their owners, which send them
// on to the successor, and the loops and watches that moved run there.
func (a *App) continueAfterHandOff(oldThreadID, newThreadID string, handOff store.AssistantHandOff) {
	// Loop and watch wakes and other threads' requests still queued for the
	// old thread go on to the successor's queue. Everything else
	// app-written goes on through its give-back
	// (withdrawQueuedAgentMessages).
	rows, err := a.store.ListFlushQueueItems(oldThreadID)
	if err != nil {
		log.Printf("assistants: hand off %s: read its queue: %v", oldThreadID, err)
	}
	for _, row := range rows {
		switch {
		case strings.HasPrefix(row.SendID, assistantSendIDRoot):
			// An opening the old thread never read is not the successor's.
			if _, err := a.removeQueuedItem(a.lifeCtx(), oldThreadID, row.SendID); err != nil {
				log.Printf("assistants: hand off %s: drop %s: %v", oldThreadID, row.SendID, err)
			}
		case threadRequestSendIDToken(row.SendID) != "":
			a.moveQueuedRequest(oldThreadID, row)
		default:
			if _, _, ok := agentWakeFromSendID(row.SendID); ok {
				a.moveQueuedAgentWake(oldThreadID, row)
			}
		}
	}
	if err := a.withdrawQueuedAgentMessages(oldThreadID, func(string) bool { return true }, "the assistant moved to a new thread before the agent took it in"); err != nil {
		log.Printf("assistants: hand off %s: %v", oldThreadID, err)
	}
	if err := a.withdrawHeldThreadWakes(oldThreadID); err != nil {
		log.Printf("assistants: hand off %s: %v", oldThreadID, err)
	}
	if err := a.withdrawHeldRemoteCompletions(oldThreadID); err != nil {
		log.Printf("assistants: hand off %s: %v", oldThreadID, err)
	}
	a.restartMovedAgentWakes(oldThreadID, newThreadID, handOff.MovedWakeIDs)
}

// moveQueuedRequest takes another thread's request still queued on the old
// thread back and queues it on the successor (requeueRequest follows the
// handoff).
func (a *App) moveQueuedRequest(oldThreadID string, row store.FlushQueueItem) {
	token := threadRequestSendIDToken(row.SendID)
	removal, err := a.removeQueuedItem(a.lifeCtx(), oldThreadID, row.SendID)
	if err != nil {
		log.Printf("assistants: hand off %s: withdraw request %s: %v", oldThreadID, token, err)
		return
	}
	if removal != queueRemovalRemoved {
		return
	}
	a.giveBackRequest(oldThreadID, token, row.Message)
}

// moveQueuedAgentWake takes a loop or watch wake still queued on the old
// thread with no session to drop it back and queues it on the successor,
// whose row it now is. A live session's teardown takes the wakes it drops
// back the same way (returnRequestsAtTeardown).
func (a *App) moveQueuedAgentWake(oldThreadID string, row store.FlushQueueItem) {
	removal, err := a.removeQueuedItem(a.lifeCtx(), oldThreadID, row.SendID)
	if err != nil {
		log.Printf("assistants: hand off %s: withdraw %s: %v", oldThreadID, row.SendID, err)
		return
	}
	if removal == queueRemovalRemoved {
		a.redeliverMovedAgentWake(oldThreadID, returnedWake{sendID: row.SendID, body: row.Message})
	}
}

// redeliverMovedAgentWake queues a wake taken back from a thread a handoff
// superseded on the thread that holds its row now. One that cannot be
// queued there is recorded on the row as not delivered, a watch keeping its
// output, and the next fire says so.
func (a *App) redeliverMovedAgentWake(oldThreadID string, w returnedWake) {
	wakeID, fireAt, _ := agentWakeFromSendID(w.sendID)
	row, found, err := a.store.GetAgentWake(wakeID)
	if err == nil && !found {
		return
	}
	if err == nil {
		err = a.deliverAgentWake(a.lifeCtx(), row, fireAt, w.body)
	}
	if err != nil {
		log.Printf("assistants: hand off %s: move %s on: %v", oldThreadID, w.sendID, err)
		a.noteMovedAgentWakeUndelivered(oldThreadID, w)
	}
}

// noteMovedAgentWakeUndelivered records on its row a wake a handoff could
// not move.
func (a *App) noteMovedAgentWakeUndelivered(oldThreadID string, w returnedWake) {
	wakeID, _, _ := agentWakeFromSendID(w.sendID)
	threadID := oldThreadID
	if current, ok := a.assistantCurrentThread(oldThreadID); ok {
		threadID = current
	}
	a.noteAgentWakeUndelivered(threadID, wakeID, agentWakeOutcomeNotDelivered+": the assistant moved to a new thread", w.body)
}

// restartMovedAgentWakes moves the runs of the loops and watches the
// handoff moved. A long-running watch's process is restarted under the
// successor, as at boot; an interval run in flight finishes, and its wake
// follows the row.
func (a *App) restartMovedAgentWakes(oldThreadID, newThreadID string, ids []string) {
	if len(ids) == 0 {
		return
	}
	a.emitAgentWakesRemoved(oldThreadID, ids)
	for _, id := range ids {
		row, found, err := a.store.GetAgentWake(id)
		if err != nil || !found {
			if err != nil {
				log.Printf("assistants: read moved wake %s: %v", id, err)
			}
			continue
		}
		if row.Kind == store.AgentWakeWatch && row.LongRunning {
			a.stopLongRunningAgentWatch(id)
			a.startAgentWatchProcess(row)
		}
		a.emitAgentWakeChanged(newThreadID, id)
	}
	a.nudgeThreadRequestSweep()
}

// redeliverOutsideSend queues an outside `thread send` the old thread never
// took in on the assistant's current thread.
func (a *App) redeliverOutsideSend(threadID, body string) {
	err := a.queueAgentNotice(agentNotice{
		threadID:   threadID,
		sendID:     agentSendIDRoot + newAgentWakeID(),
		origin:     usermessage.OriginAgentWake,
		originWake: &usermessage.OriginWake{Kind: agentWakeKindSend},
		prepare:    func() (string, bool, error) { return body, true, nil },
		startFailed: func(err error) string {
			return "A message sent with agent-overflow thread send is queued, but the agent could not start: " + err.Error()
		},
	})
	if err != nil {
		log.Printf("assistants: send an outside message on to %s: %v", threadID, err)
		a.noteAgentSendNotDelivered(threadID, "the assistant moved to a new thread and the message could not follow it")
	}
}
