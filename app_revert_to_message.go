package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/composerdraft"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// RevertConversationToMessage rolls a thread back to the selected user
// message IN PLACE: the provider session is cut before that message,
// every later turn is truncated from SQLite, and the reverted prompt is
// restored to the composer draft. It is the message-keyed, idle-thread
// counterpart to the two existing rollback entry points:
//
//   - InterruptAndRevertIfClean un-sends the LATEST message while its
//     turn is still live (Stop button); it interrupts the turn first.
//   - ForkThreadFromMessage clones the kept prefix into a NEW thread and
//     leaves the source thread untouched.
//
// This one mutates the current thread and keeps it — the "revert to
// here" affordance on a past user message. It shares the entire
// destructive tail (provider rollback -> draft restore -> truncate) with
// InterruptAndRevertIfClean through rollbackConversationLocked and emits
// the same `user_message:reverted` event, so the frontend truncates the
// timeline and rehydrates the composer through one code path.
//
// Reverting stops the provider session, which kills any background work
// it owns (Claude background tasks, Codex background terminals /
// subagents). killRunningBackgroundTasks is the caller's explicit
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
func (a *App) RevertConversationToMessage(threadID string, userItemID string, killRunningBackgroundTasks bool) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	if strings.TrimSpace(threadID) == "" {
		return errors.New("revert to message: thread id is required")
	}
	if strings.TrimSpace(userItemID) == "" {
		return errors.New("revert to message: user item id is required")
	}

	unlock := a.threadLocks().Lock(threadID)
	defer unlock()

	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return fmt.Errorf("revert to message: %w", err)
	}

	if thread.Provider == string(provider.ClaudeTUI) {
		return fmt.Errorf("revert to message: provider %q does not support in-place revert", thread.Provider)
	}

	// Reject an in-place revert while a turn is live: truncating a
	// timeline the provider is still writing to would race its session
	// log (Claude JSONL) or in-memory turn (Codex). The button is hidden
	// while a turn runs (UserMessage's actionsTurnLocked); this guard is
	// defense-in-depth for script callers and races, mirroring
	// ForkThreadFromMessage. The live-turn un-send has its own entry
	// point (InterruptAndRevertIfClean), which interrupts first.
	if _, active, err := a.store.GetActiveTurn(threadID); err != nil {
		return fmt.Errorf("revert to message: active turn check: %w", err)
	} else if active {
		return fmt.Errorf("revert to message: cannot revert while a turn is in progress; interrupt or wait first")
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
		return errors.New("revert to message: cannot revert while a send is awaiting provider confirmation; wait for the turn to start")
	}

	if !killRunningBackgroundTasks {
		if running, err := a.hasRunningBackgroundTasks(threadID); err != nil {
			return fmt.Errorf("revert to message: check background tasks: %w", err)
		} else if running {
			return errors.New("revert to message: running background tasks must be killed before reverting")
		}
	}

	item, found, err := a.store.GetThreadItem(threadID, userItemID)
	if err != nil {
		return fmt.Errorf("revert to message: load user item: %w", err)
	}
	if !found || item.Kind != "user_text" || item.Role != "user" || store.IsWireOnlyUserItem(item) {
		return fmt.Errorf("revert to message: %q is not a user message", userItemID)
	}

	// Same-thread revert: the attachments already belong to this thread,
	// so project the draft directly from the item — no attachment cloning
	// (that's a fork-only concern). Matches InterruptAndRevertIfClean.
	promptDraft, err := composerdraft.FromUserItem(threadID, item, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("revert to message: build prompt draft: %w", err)
	}

	// resolveMessageAnchor synthesizes an anchor from the item row when
	// the persisted one is missing or its turn index drifted, so the
	// SQLite truncation and the provider cut agree. Same contract as the
	// un-send and fork-from-message paths.
	anchor := a.resolveMessageAnchor("revert to message", threadID, item)

	// markReverted stays false: there is no in-flight turn-complete to
	// tag (the guard above rejected an active turn), so no Interrupted
	// pill to suppress.
	keptAnchorTurnItemIDs, err := a.rollbackConversationLocked(rollbackConversationLockedArgs{
		thread:                      thread,
		userItem:                    item,
		anchor:                      anchor,
		promptDraft:                 promptDraft,
		errorPrefix:                 "revert to message",
		markReverted:                false,
		clearRunningBackgroundTasks: killRunningBackgroundTasks,
	})
	if err != nil {
		return err
	}

	a.emit("user_message:reverted", UserMessageRevertedEvent{
		ThreadID:              threadID,
		UserItemID:            item.ID,
		TurnIndex:             item.TurnIndex,
		KeptAnchorTurnItemIDs: keptAnchorTurnItemIDs,
	})
	return nil
}
