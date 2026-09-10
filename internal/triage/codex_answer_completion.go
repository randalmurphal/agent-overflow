package triage

import (
	"fmt"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// A Codex child's FINAL_ANSWER is built by Codex core FROM the child's
// terminal status (session_prefix.rs: format_inter_agent_completion_message
// renders Completed/Errored/Shutdown/NotFound; Interrupted yields nothing),
// so the envelope always reaches the parent AFTER the child's turn/completed
// and only when the parent's model samples again, which happens inside the
// parent's open turn or, if that turn already ended, in a later one.
//
// The completion row is what the card is built at, and the answer is its
// collapsed line, payload and preview (docs/specs/agent-visibility.md).
// A row written at the terminal would be answerless forever (completed
// history is immutable), so a terminal that will deliver an envelope holds
// its prepared completion here and persists it when the answer lands. The
// hold ends without an answer when the parent turn completes, the child
// starts a new execution, or the session is torn down; a late answer then
// stays the separate delivery row it already is.
type pendingCodexCompletion struct {
	// evt is the terminal status event carrying the execution snapshot
	// meta; persisting it unchanged is exactly the answerless row.
	evt          provider.ProviderEvent
	completionID string
}

// codexTerminalStatusDeliversAnswer mirrors Codex core's completion-message
// table: these statuses produce a FINAL_ANSWER envelope, `interrupted`
// does not.
func codexTerminalStatusDeliversAnswer(status string) bool {
	switch strings.TrimSpace(status) {
	case "completed", "errored", "shutdown", "notFound":
		return true
	default:
		return false
	}
}

// shouldDeferCodexCompletion reports whether a terminal execution should
// wait for its answer: only a root-level spawn (a nested child's envelope
// goes to its parent agent's scope, not this thread's root), only for a
// status that yields an envelope, only for a live observation (a recovery
// snapshot's envelope was consumed long ago), only for an execution fenced
// by the child's native turn (an unfenced legacy status has no envelope
// bound to it), and only while the parent turn is open, because the
// envelope cannot arrive before the next parent turn otherwise.
func (r *Router) shouldDeferCodexCompletion(threadID string, launch store.Item, turnID, status string, recovered bool) bool {
	if recovered || turnID == "" || launch.ParentID != "" || !codexTerminalStatusDeliversAnswer(status) {
		return false
	}
	_, open := r.openTurnIndex(threadID)
	return open
}

func (r *Router) deferCodexCompletion(threadID, launchID string, pending pendingCodexCompletion) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.codexBackgroundForThread(threadID).pendingAnswer[launchID] = pending
}

func (r *Router) takePendingCodexCompletion(threadID, launchID string) (pendingCodexCompletion, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.codexBackgroundIfPresent(threadID)
	if state == nil {
		return pendingCodexCompletion{}, false
	}
	pending, ok := state.pendingAnswer[launchID]
	if ok {
		delete(state.pendingAnswer, launchID)
	}
	return pending, ok
}

// takePendingCodexCompletionsLocked hands every held completion to the
// caller and empties the hold. Must be called with r.mu held; cleanup
// uses it while it still owns the thread state it is about to delete.
func takePendingCodexCompletionsLocked(state *codexBackgroundState) map[string]pendingCodexCompletion {
	if state == nil || len(state.pendingAnswer) == 0 {
		return nil
	}
	taken := state.pendingAnswer
	state.pendingAnswer = make(map[string]pendingCodexCompletion)
	return taken
}

// persistPendingCodexCompletion writes a held completion, with the
// delivered answer as its payload and preview or answerless when the hold
// ended first. `synthesizeCodexBackgroundCompletion` keeps the write
// idempotent by completion id, so a flush racing a delivery cannot mint
// two rows.
func (r *Router) persistPendingCodexCompletion(launchID string, pending pendingCodexCompletion, answer string) error {
	completion := pending.evt
	completion.Content = answer
	if err := r.synthesizeCodexBackgroundCompletion(completion, launchID, codexBackgroundCompletionOptions{completionID: pending.completionID}); err != nil {
		return fmt.Errorf("persist held Codex completion %s: %w", pending.completionID, err)
	}
	return nil
}

// flushPendingCodexCompletion ends one launch's hold without an answer.
func (r *Router) flushPendingCodexCompletion(threadID, launchID string) error {
	pending, ok := r.takePendingCodexCompletion(threadID, launchID)
	if !ok {
		return nil
	}
	return r.persistPendingCodexCompletion(launchID, pending, "")
}

// persistHeldCodexCompletionsAnswerless writes every completion a caller
// took out of the hold, answerless: the parent turn ended or the session
// is going away, so no envelope can land for them. A write failure is
// reported the way every other completed-history write failure is.
func (r *Router) persistHeldCodexCompletionsAnswerless(threadID string, held map[string]pendingCodexCompletion) {
	for launchID, pending := range held {
		if err := r.persistPendingCodexCompletion(launchID, pending, ""); err != nil {
			r.reportCompletedHistoryConflict(threadID, pending.evt.ParentToolUseID, err)
		}
	}
}

// codexAnswerText is the delivered answer as the completion payload shows
// it: the readable body, or the same placeholder the delivery row would
// carry when Codex encrypted it.
func codexAnswerText(parsed codexSubagentSignalMeta) string {
	if parsed.Encrypted {
		return codexEncryptedMessagePlaceholder
	}
	return strings.TrimSpace(parsed.Message)
}
