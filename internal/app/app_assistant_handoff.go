package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/agenthome"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
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
// current thread on this computer, and for an archived one.
//
//ao:scope threads:autonomy
func (a *App) StartNextAssistantThread(ctx context.Context, threadID string) (store.Thread, error) {
	return a.nextAssistantThread(ctx, threadID, false)
}

// errAssistantSessionGone is a start of an assistant's current thread whose
// provider session the provider itself cleaned up: Claude's transcript file
// is gone, or Codex refuses thread/resume with no rollout. The start fails
// with it and the assistant continues in a new thread
// (recoverAssistantSessionLoss).
var errAssistantSessionGone = errors.New("the provider no longer has this assistant's session, so the assistant continues in a new thread")

// requireAssistantClaudeSession refuses a Claude or Claude TUI resume of an
// assistant's current thread whose transcript is gone, before the start
// tears anything down. Codex has no file to look for; its thread/resume
// refusal is the signal (startSessionNow).
func (a *App) requireAssistantClaudeSession(t store.Thread, opts provider.SessionOptions) error {
	if opts.Resume == "" || opts.ForkSession || (t.Provider != string(provider.Claude) && t.Provider != string(provider.ClaudeTUI)) {
		return nil
	}
	if _, found, err := a.store.GetStandingAgent(t.ID); err != nil || !found {
		return err
	}
	projectsDir, err := a.claudeProjectsDir()
	if err != nil {
		return err
	}
	if _, err := sessionfork.LocateSessionFile(projectsDir, opts.Resume, opts.WorkDir); errors.Is(err, sessionfork.ErrSessionFileNotFound) {
		return errors.Join(errAssistantSessionGone, err)
	}
	return nil
}

// recoverAssistantSessionLoss runs New Thread, Same Assistant for an
// assistant whose session the provider cleaned up, off the start that found
// it (which holds the thread's action lock), once per thread at a time. The
// new thread's opening block says why.
func (a *App) recoverAssistantSessionLoss(threadID string) {
	if _, running := a.assistantSessionLoss.LoadOrStore(threadID, struct{}{}); running {
		return
	}
	started := a.goAgentNoticeWork(threadID, func() {
		defer a.assistantSessionLoss.Delete(threadID)
		successor, err := a.nextAssistantThread(a.lifeCtx(), threadID, true)
		if err != nil {
			log.Printf("assistants: %s lost its provider session and could not move to a new thread: %v", threadID, err)
			a.emitErrorToThread(threadID, "The provider cleaned up this assistant's session, and moving it to a new thread failed: "+err.Error()+". Use New Thread, Same Assistant to try again.")
			return
		}
		log.Printf("assistants: %s lost its provider session; it continues in %s", threadID, successor.ID)
	})
	if !started {
		a.assistantSessionLoss.Delete(threadID)
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
	standardPercent, extendedPercent := old.AutoCompactStandardPercent, old.AutoCompactExtendedPercent
	opts := CreateThreadOptions{
		ProjectID:                  old.ProjectID,
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
		WorktreePath:               old.WorktreePath,
		Branch:                     old.Branch,
	}
	if old.WorktreePath == "" {
		opts.WorkspaceOverride = old.WorkspacePath
	}
	successor, err := a.createThreadRow(ctx, opts, false)
	if err != nil {
		return store.Thread{}, fmt.Errorf("create the assistant's next thread: %w", err)
	}
	handOff, interrupted, err := a.handOffAssistantLocked(threadID, successor.ID)
	if err != nil {
		if deleteErr := a.DeleteThread(successor.ID); deleteErr != nil {
			log.Printf("assistants: delete the next thread %s of a handoff that failed: %v", successor.ID, deleteErr)
		}
		return store.Thread{}, err
	}
	a.continueAfterHandOff(threadID, successor.ID, handOff)
	if successor, err = a.store.GetThread(successor.ID); err != nil {
		return store.Thread{}, fmt.Errorf("read the assistant's next thread: %w", err)
	}
	// The old row leaves the list as an archive does; the successor takes
	// the agent row's place; the workers now nest under it.
	a.broadcastThreadRow(triage.ThreadActionUnlisted, handOff.Old)
	a.broadcastThreadRow(triage.ThreadActionListed, successor)
	a.broadcastSpawnedThreadRows(successor.ID)
	body := agenthome.HandOffMessage(agenthome.HandOff{
		Name:             successor.Title,
		PreviousThreadID: threadID,
		MovedWakes:       len(handOff.MovedWakeIDs),
		MovedRequests:    handOff.MovedRequests,
		Interrupted:      a.interruptedByHandOff(interrupted),
		SessionLost:      sessionLost,
	})
	if err := a.queueAssistantMessage(successor.ID, assistantSendID(assistantWakeKindHandOff, threadID), successor.Title, body); err != nil {
		return successor, fmt.Errorf("the assistant moved to its new thread, but its opening block could not be queued: %w", err)
	}
	log.Printf("assistants: %s moved from thread %s to %s", agent.ID, threadID, successor.ID)
	return successor, nil
}

// handOffAssistantLocked runs the store handoff and ends the old thread's
// session under its action lock, as an archive does. The requests the old
// thread had read and not answered settle first, saying where the assistant
// went; it returns their tokens.
func (a *App) handOffAssistantLocked(oldThreadID, newThreadID string) (store.AssistantHandOff, []string, error) {
	unlock := a.threadLocks().Lock(oldThreadID)
	defer unlock()
	unlockMutable, err := a.threadApplication().LockMutable(context.Background(), oldThreadID)
	if err != nil {
		return store.AssistantHandOff{}, nil, err
	}
	handOff, err := a.store.HandOffAssistant(oldThreadID, newThreadID, time.Now().UnixMilli())
	unlockMutable()
	if err != nil {
		return store.AssistantHandOff{}, nil, err
	}
	interrupted := a.settleReadRequestsForHandOff(oldThreadID)
	if sess, ok := a.sessionManager().take(oldThreadID); ok {
		if err := a.teardownAndCloseSession(oldThreadID, sess); err != nil {
			log.Printf("assistants: hand off %s: close its provider session: %v", oldThreadID, err)
		}
	}
	if err := a.closeThreadBrowserPages(oldThreadID); err != nil {
		log.Printf("assistants: hand off %s: close browser pages: %v", oldThreadID, err)
	}
	return handOff, interrupted, nil
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
	// Loop and watch wakes still queued for the old thread are recorded on
	// their rows, now the successor's, as not delivered: the next fire says
	// so. Everything else app-written goes on (withdrawQueuedAgentMessages,
	// redirected by its give-back).
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
			if wakeID, _, ok := agentWakeFromSendID(row.SendID); ok {
				removal, err := a.removeQueuedItem(a.lifeCtx(), oldThreadID, row.SendID)
				if err != nil {
					log.Printf("assistants: hand off %s: withdraw %s: %v", oldThreadID, row.SendID, err)
					continue
				}
				if removal == queueRemovalRemoved {
					a.noteAgentWakeUndelivered(newThreadID, wakeID, agentWakeOutcomeNotDelivered+": the assistant moved to a new thread", row.Message)
				}
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
