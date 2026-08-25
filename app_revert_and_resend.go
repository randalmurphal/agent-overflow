package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/composerdraft"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
)

// RevertAndResendOptions carries the resend half of
// RevertConversationAndResendMessage — everything past "which message".
// Content is required; the zero value of the rest is an attachment-less
// prose resend with no background-kill consent. A struct rather than
// positionals because the method crossed the arity where a transposed
// bool still type-checks (mirrors SendMessageOptions).
type RevertAndResendOptions struct {
	// Content is the edited replacement message.
	Content       string   `json:"content"`
	AttachmentIDs []string `json:"attachmentIds,omitempty"`
	// KillRunningBackgroundTasks is the caller's explicit consent to kill
	// background work the revert orphans; see the method doc.
	KillRunningBackgroundTasks bool `json:"killRunningBackgroundTasks,omitempty"`
}

// RevertConversationAndResendMessage rolls a thread back to the selected
// user message and sends the caller's EDITED replacement in its place,
// atomically under one per-thread action lock. It is the backend half of
// the edit-in-place affordance on a past user message: the frontend
// opens an editor on the message and submits once, so there is never an
// intermediate state where the conversation is truncated and the user
// still has to press Enter on a rehydrated composer.
//
// It is the message-keyed, idle-thread counterpart to the two other
// rollback entry points:
//
//   - InterruptAndRevertIfClean un-sends the LATEST message while its
//     turn is still live (Stop button); it interrupts the turn first and
//     DOES restore the prompt to the composer, because it has no
//     replacement to send.
//   - ForkThreadFromMessage clones the kept prefix into a NEW thread and
//     leaves the source thread untouched.
//
// This one mutates the current thread and keeps it. It shares the whole
// destructive tail (provider rollback -> truncate) with
// InterruptAndRevertIfClean through rollbackConversationLocked and emits
// the same `user_message:reverted` event, so the frontend truncates the
// timeline through one code path — distinguished only by
// DraftPendingResend, which tells it a replacement message is already on
// the way and the draft row is saga state rather than composer content.
//
// Reverting stops the provider session, which kills any background work
// it owns (Claude background tasks, Codex background terminals /
// subagents). opts.KillRunningBackgroundTasks is the caller's explicit
// consent to that: false refuses the revert while background tasks are
// live (re-checked under the thread lock — the frontend preflights the
// count to decide whether to show its confirmation dialog, but this
// check is what makes an unconsented kill impossible); true additionally
// runs the provider-appropriate cleanup and flips the persisted
// "running" tray rows inactive so dead work doesn't survive the rollback
// as stale spinners. Mirrors the un-send path's posture, which declines
// the revert outright to preserve background work.
//
// Only claude and codex are supported: claude-tui reverts natively
// inside the TUI (Esc), and rollbackConversationLocked's claude-tui
// branch assumes that Esc was already delivered for a LIVE turn — an
// assumption this idle-thread path can never satisfy. The UI never
// wires the button for claude-tui (fork:false in the capability
// matrix), but the guard below rejects it structurally too: a wire
// caller reaching this method on a claude-tui thread would otherwise
// truncate AO's history cache while the live TUI keeps the full
// conversation.
//
// Workflow-mode threads are rejected for the same structural reason. A
// send into a taken-over workflow run has to detach the run first, and
// that preparation round-trips the engine command loop — which
// re-acquires this thread's action lock and would deadlock against the
// lock this saga holds across the whole sequence. Rather than reach
// half of the takeover machinery from inside the lock, an unguarded
// call fails loudly.
func (a *App) RevertConversationAndResendMessage(
	threadID string,
	userItemID string,
	opts RevertAndResendOptions,
) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	if strings.TrimSpace(threadID) == "" {
		return errors.New("revert and resend: thread id is required")
	}
	if strings.TrimSpace(userItemID) == "" {
		return errors.New("revert and resend: user item id is required")
	}
	// An edit-resend with no text is a caller bug, not an empty send:
	// this method's whole contract is "replace that message with this
	// one", and there is no replacement to send.
	if strings.TrimSpace(opts.Content) == "" {
		return errors.New("revert and resend: edited message content is required")
	}

	unlock := a.threadLocks().Lock(threadID)
	defer unlock()

	thread, item, err := a.resolveRevertAndResendTarget(threadID, userItemID, opts.KillRunningBackgroundTasks)
	if err != nil {
		return err
	}

	// Crash durability, BEFORE anything destructive. The edited text has
	// no durable home yet: the old user row is about to be truncated and
	// the replacement row does not exist until the resend persists it.
	// Park the edit in the thread's draft row — merged AHEAD of whatever
	// unsent composer WIP the user already had — so that from here until
	// the resend commits, a crash at ANY point leaves both texts
	// recoverable in the composer rather than silently destroying one.
	// MergeParts carries the WIP's terminal chips and pending-plan link
	// through untouched, which is what makes the restore at the end a
	// byte-identical round-trip.
	staged, err := a.mergeAndUpsertThreadDraft(threadID, []composerdraft.Part{{
		Content:       opts.Content,
		AttachmentIDs: opts.AttachmentIDs,
	}})
	if err != nil {
		return fmt.Errorf("revert and resend: stage edited message: %w", err)
	}

	// resolveMessageAnchor synthesizes an anchor from the item row when
	// the persisted one is missing or its turn index drifted, so the
	// SQLite truncation and the provider cut agree. Same contract as the
	// un-send and fork-from-message paths.
	anchor := a.resolveMessageAnchor("revert and resend", threadID, item)

	// promptDraft nil: the rollback tail must NOT restore the old prompt
	// to the composer. This saga owns the draft row (the crash copy
	// staged above lives there), and there is no composer to rehydrate —
	// the replacement is sent below.
	//
	// markReverted stays false: there is no in-flight turn-complete to
	// tag (resolveRevertAndResendTarget rejected an active turn), so no
	// Interrupted pill to suppress.
	cut, err := a.rollbackConversationLocked(rollbackConversationLockedArgs{
		thread:                      thread,
		userItem:                    item,
		anchor:                      anchor,
		promptDraft:                 nil,
		errorPrefix:                 "revert and resend",
		markReverted:                false,
		clearRunningBackgroundTasks: opts.KillRunningBackgroundTasks,
	})
	if err != nil {
		return err
	}

	// Emitted BEFORE the resend dispatches. Both frames travel the same
	// FIFO WebSocket, so the frontend observes truncate-then-new-message
	// and never sees the replacement user row land in a timeline it is
	// about to cut.
	a.emit(eventchan.UserMessageReverted, UserMessageRevertedEvent{
		ThreadID:              threadID,
		UserItemID:            item.ID,
		TurnIndex:             item.TurnIndex,
		KeptAnchorTurnItemIDs: cut.KeptAnchorTurnItemIDs,
		HistoryRev:            cut.Stamp.Rev,
		HistoryEpoch:          cut.Stamp.Epoch,
		DraftPendingResend:    true,
	})

	// sendMessageLocked, not sendMessageWithOptions: the whole saga runs
	// under one acquisition of this thread's action lock, so nothing can
	// slip a send, revert, or session start into the window between the
	// truncation and the replacement.
	//
	// The option set is exact parity with the composer's own send
	// (SendMessageWithOptions): this text was typed into a composer, so
	// it gets D31 command expansion. PreserveDraft keeps the send from
	// consuming the crash copy — this saga settles that row itself, below.
	if _, err := a.sendMessageLocked(context.Background(), threadID, opts.Content, sendMessageOptions{
		AttachmentIDs:          opts.AttachmentIDs,
		ExpandComposerCommands: true,
		PreserveDraft:          true,
	}, sendMessagePrepared{}); err != nil {
		// The conversation is already truncated and the event already
		// told the frontend so. The merged draft row stays exactly as
		// staged: it is the process-crash backstop — a LIVE frontend
		// rebuilds its recovery from its own in-memory copy of the edit
		// (composer saves may interleave with this saga, so the row is
		// not guaranteed pristine by the time the error lands there),
		// while a dead one finds both texts here on the next hydrate.
		// The distinct prefix is what lets the caller tell "the revert
		// never happened" (every guard in resolveRevertAndResendTarget)
		// from "the revert happened, the resend did not".
		return fmt.Errorf("revert and resend: resend failed: %w", err)
	}

	a.settleRevertAndResendDraft(threadID, staged)
	return nil
}

// resolveRevertAndResendTarget runs every guard that needs the thread
// row or the durable timeline, and returns the pair the saga operates
// on. It runs UNDER the thread lock — each check is a statement about
// state another goroutine could otherwise change between the check and
// the destructive tail (a turn starting, a send dispatching, a
// background task appearing).
//
// Every rejection here means "the revert never happened": nothing has
// been staged, cut, or emitted at this point.
func (a *App) resolveRevertAndResendTarget(
	threadID, userItemID string, killRunningBackgroundTasks bool,
) (store.Thread, store.Item, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, store.Item{}, fmt.Errorf("revert and resend: %w", err)
	}

	if thread.Provider == string(provider.ClaudeTUI) {
		return store.Thread{}, store.Item{}, fmt.Errorf("revert and resend: provider %q does not support in-place revert", thread.Provider)
	}
	if thread.Mode == threadmode.ModeWorkflow {
		return store.Thread{}, store.Item{}, errors.New("revert and resend: workflow threads cannot edit and resend a past message")
	}

	// Reject an in-place revert while a turn is live: truncating a
	// timeline the provider is still writing to would race its session
	// log (Claude JSONL) or in-memory turn (Codex). The editor is hidden
	// while a turn runs (UserMessage's actionsTurnLocked); this guard is
	// defense-in-depth for script callers and races, mirroring
	// ForkThreadFromMessage. The live-turn un-send has its own entry
	// point (InterruptAndRevertIfClean), which interrupts first.
	if _, active, err := a.store.GetActiveTurn(threadID); err != nil {
		return store.Thread{}, store.Item{}, fmt.Errorf("revert and resend: active turn check: %w", err)
	} else if active {
		return store.Thread{}, store.Item{}, errors.New("revert and resend: cannot revert while a turn is in progress; interrupt or wait first")
	}

	// GetActiveTurn only sees turns whose wire turn-start already landed
	// a `turns` row. A just-dispatched send lives in the gap before that
	// echo: the user item is persisted and the provider is working, but
	// no active turn exists yet. The triage pending-send FIFO is
	// registered under this same thread lock BEFORE the stdin write, so
	// it is the authoritative "send in flight" signal for that window —
	// without this check a revert clicked in the echo gap would kill the
	// in-flight send and truncate its just-persisted message.
	if a.triage != nil && a.triage.HasPendingSendForThread(threadID) {
		return store.Thread{}, store.Item{}, errors.New("revert and resend: cannot revert while a send is awaiting provider confirmation; wait for the turn to start")
	}

	if !killRunningBackgroundTasks {
		if running, err := a.hasRunningBackgroundTasks(threadID); err != nil {
			return store.Thread{}, store.Item{}, fmt.Errorf("revert and resend: check background tasks: %w", err)
		} else if running {
			return store.Thread{}, store.Item{}, errors.New("revert and resend: running background tasks must be killed before reverting")
		}
	}

	item, found, err := a.store.GetThreadItem(threadID, userItemID)
	if err != nil {
		return store.Thread{}, store.Item{}, fmt.Errorf("revert and resend: load user item: %w", err)
	}
	if !found || item.Kind != "user_text" || item.Role != "user" || store.IsWireOnlyUserItem(item) {
		return store.Thread{}, store.Item{}, fmt.Errorf("revert and resend: %q is not a user message", userItemID)
	}
	return thread, item, nil
}

// settleRevertAndResendDraft retires the crash copy once the edited text
// has a durable home in the persisted user row: the draft row goes back
// to the untouched WIP, or away entirely when there was none.
//
// Conditional on the row still BEING the crash copy. The composer stays
// typeable for the whole saga — only sending is suspended, and SaveDraft
// takes no thread lock — so a debounced composer save can land between
// the staging and here. Restoring the pre-saga snapshot over it would
// silently destroy text the user typed, which is the exact loss the
// crash copy exists to prevent. A row that moved is a newer, more
// authoritative write, so this leaves it entirely alone: no restore, no
// delete.
//
// Failing to settle would report a send that already went out as failed,
// and nothing is lost either way (the row still holds recoverable text),
// so every error logs instead.
func (a *App) settleRevertAndResendDraft(threadID string, staged stagedThreadDraft) {
	current, exists, err := a.store.GetThreadDraft(threadID)
	if err != nil {
		log.Printf("app: revert and resend: re-read staged draft for thread %s: %v", threadID, err)
		return
	}
	if !exists || current != staged.merged {
		return
	}
	if staged.priorExisted {
		if err := a.store.UpsertThreadDraft(staged.prior); err != nil {
			log.Printf("app: revert and resend: restore composer draft for thread %s: %v", threadID, err)
		}
		return
	}
	if err := a.store.DeleteThreadDraft(threadID); err != nil {
		log.Printf("app: revert and resend: clear staged draft for thread %s: %v", threadID, err)
	}
}
