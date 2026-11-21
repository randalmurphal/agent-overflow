package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/agenthome"
	"agent-overflow/internal/agentmeta"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
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
	if err := a.refuseAssistantsOff(); err != nil {
		return store.Thread{}, err
	}
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
		a.endAssistantHandOff(agent.ID, threadID, "")
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
	// The successor lives where an assistant's thread does: outside every
	// project, in the home. The handoff's transaction refuses an archived
	// assistant and replaces this title and these settings with the old
	// thread's as they stand then, so a rename or a settings change made
	// meanwhile is kept.
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
	if a.handOffSuccessorMadeForTest != nil {
		a.handOffSuccessorMadeForTest(threadID, successor.ID)
	}
	handOff, interrupted, carried, err := a.handOffAssistantLocked(threadID, successor.ID)
	if err != nil {
		if deleteErr := a.deleteThread(successor.ID); deleteErr != nil {
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
	if err := a.queueAssistantMessage(successor.ID, assistantSendID(string(agentmeta.ReportHandOff), threadID), successor.Title, body); err != nil {
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
	var works []threadSettlementWork
	why := interruptedSettlement(threadtools.Cause{Code: threadtools.CauseHandedOff, Arg: oldThreadID}, "")
	for _, receipt := range receipts {
		if receipt.State != store.ThreadReceiptRunning {
			continue
		}
		if receipt.TurnID == "" && !a.receiptTakenIn(oldThreadID, receipt.Token) {
			// Unread, unless a delivery whose binding failed to write
			// named the turn that read it.
			delivered, found, err := a.deliveredReceipt(receipt.Token)
			if err != nil {
				log.Printf("assistants: hand off %s: read request %s: %v", oldThreadID, receipt.Token, err)
			}
			if err != nil || !found || delivered.TurnID == "" {
				continue
			}
		}
		work, applied, err := a.settleThreadReceiptNoScratch(receipt.Token, []string{store.ThreadReceiptRunning}, why)
		if err != nil {
			log.Printf("assistants: hand off %s: settle request %s: %v", oldThreadID, receipt.Token, err)
			continue
		}
		works = append(works, work)
		if applied {
			settled = append(settled, receipt.Token)
		}
	}
	// Together, so a caller with several requests the old thread read is
	// told once.
	a.runThreadSettlementWorksDetached(works)
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
