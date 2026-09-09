package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/composerdraft"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/itemwire"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/transport"
)

// RevertAndResendOptions carries the resend half of
// RevertConversationAndResendMessage — everything past "which message".
// Content is required; the zero value of the rest is an attachment-less
// prose resend with no background-kill consent. A struct rather than
// positionals because the method crossed the arity where a transposed
// bool still type-checks (mirrors SendMessageOptions).
type RevertAndResendOptions struct {
	SendID string `json:"sendId,omitempty"`
	// Content is the edited replacement message.
	Content       string   `json:"content"`
	AttachmentIDs []string `json:"attachmentIds,omitempty"`
	// KillRunningBackgroundTasks is the caller's explicit consent to kill
	// background work the revert orphans; see the method doc.
	KillRunningBackgroundTasks bool `json:"killRunningBackgroundTasks,omitempty"`
}

// RevertAndResendResult separates a refusal from a committed cut whose send failed.
// Event delivery and RPC completion are independently scheduled.
type RevertAndResendResult struct {
	Warning string                    `json:"warning,omitempty"`
	Cut     *UserMessageRevertedEvent `json:"cut,omitempty"`
	Failure string                    `json:"failure,omitempty"`
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
// the same `user_message:reverted` event. A prepared replacement travels with
// its cut, so clients can replace the tail in one render transaction. Recovery
// staging lives in thread_draft_recoveries, independently of composer autosaves.
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
//
// ctx is here for the CALLER's identity, not for cancellation: the cut event
// below is stamped with the connection that asked, because the frontend's
// own failure handler keys on it (see UserMessageRevertedEvent.ConnectionID).
// The generated TS bindings strip a leading ctx, so the wire signature is
// unchanged.
//
//ao:scope threads:operate
func (a *App) RevertConversationAndResendMessage(
	ctx context.Context,
	threadID string,
	userItemID string,
	opts RevertAndResendOptions,
) (result RevertAndResendResult, err error) {
	if a.shuttingDown.Load() {
		return result, ErrShuttingDown
	}
	if strings.TrimSpace(threadID) == "" {
		return result, errors.New("revert and resend: thread id is required")
	}
	if strings.TrimSpace(userItemID) == "" {
		return result, errors.New("revert and resend: user item id is required")
	}
	// An edit-resend with no text is a caller bug, not an empty send:
	// this method's whole contract is "replace that message with this
	// one", and there is no replacement to send.
	if strings.TrimSpace(opts.Content) == "" {
		return result, errors.New("revert and resend: edited message content is required")
	}

	releaseAdmission, admissionErr := a.lockSendAdmission(context.WithoutCancel(ctx), threadID, opts.SendID)
	if admissionErr != nil {
		return result, admissionErr
	}
	defer releaseAdmission()
	unlock := a.threadLocks().Lock(threadID)
	defer unlock()
	if err := a.threadApplication().CheckMutable(threadID); err != nil {
		return result, err
	}

	if _, found, lookupErr := a.findRecordedSend(threadID, opts.SendID); lookupErr != nil {
		return result, lookupErr
	} else if found {
		return result, nil
	}
	thread, item, err := a.resolveRevertAndResendTarget(threadID, userItemID, opts.KillRunningBackgroundTasks)
	if err != nil {
		return result, err
	}

	// Keep recovery independent from the editable composer row. Autosaves and
	// other clients must never observe or overwrite transaction staging.
	if opts.SendID == "" {
		opts.SendID = uuid.NewString()
	}
	attachmentJSON, err := json.Marshal(opts.AttachmentIDs)
	if err != nil {
		return result, fmt.Errorf("revert and resend: encode attachments: %w", err)
	}
	recovery := store.ThreadDraftRecovery{ThreadID: threadID, SendID: opts.SendID, Content: opts.Content, Attachments: string(attachmentJSON)}
	if err := a.store.StageThreadDraftRecovery(recovery); err != nil {
		return result, fmt.Errorf("revert and resend: stage edited message: %w", err)
	}
	// A failed cut still preserves the edit. Recovery failures remain durable
	// and are included in the operation's error, never silently discarded.
	defer func() {
		if err != nil {
			if recoveryErr := a.recoverReplacementDraft(clientOf(ctx), recovery); recoveryErr != nil {
				err = errors.Join(err, fmt.Errorf("restore edited draft: %w", recoveryErr))
			}
			if result.Cut != nil {
				result.Failure = err.Error()
				err = nil
			}
		}
	}()

	// resolveMessageAnchor synthesizes an anchor from the item row when
	// the persisted one is missing or its turn index drifted, so the
	// SQLite truncation and the provider cut agree. Same contract as the
	// un-send and fork-from-message paths.
	anchor := a.resolveMessageAnchor("revert and resend", threadID, item)

	// Recovery is already durable; the ordinary composer draft stays editable.
	cut, err := a.rollbackConversationLocked(rollbackConversationLockedArgs{
		thread:                      thread,
		userItem:                    item,
		anchor:                      anchor,
		promptDraft:                 nil,
		errorPrefix:                 "revert and resend",
		clearRunningBackgroundTasks: opts.KillRunningBackgroundTasks,
	})
	if err != nil {
		return result, err
	}

	cutEvent := UserMessageRevertedEvent{
		TurnStartedSequence:   a.eventSequence(eventchan.ProviderTurnStarted),
		TurnCompletedSequence: a.eventSequence(eventchan.ProviderTurnCompleted),
		ThreadID:              threadID, UserItemID: item.ID, TurnIndex: item.TurnIndex,
		KeptAnchorTurnItemIDs: cut.KeptAnchorTurnItemIDs,
		HistoryRev:            cut.Stamp.Rev, HistoryEpoch: cut.Stamp.Epoch,
		ItemEventSequence:  a.itemEventSequence(),
		DraftPendingResend: true, ConnectionID: clientOf(ctx).ConnectionID,
	}
	result.Cut = &cutEvent
	published := false
	defer func() {
		// Startup or persistence can fail after the native cut. Publish that cut
		// and return its outcome explicitly even if the event arrives after the RPC.
		if !published {
			a.emit(eventchan.UserMessageReverted, cutEvent)
		}

	}()

	// sendMessageLocked, not sendMessageWithOptions: the whole saga runs
	// under one acquisition of this thread's action lock, so nothing can
	// slip a send, revert, or session start into the window between the
	// truncation and the replacement.
	//
	// PreserveDraft leaves composer work untouched on successful replacement.
	if _, sendErr := a.sendMessageLocked(context.Background(), threadID, opts.Content, sendMessageOptions{
		SendID:                 opts.SendID,
		AttachmentIDs:          opts.AttachmentIDs,
		ExpandComposerCommands: true,
		PreserveDraft:          true,
		onUserMessageReady: func(replacement store.Item) {
			projected := itemwire.Project(replacement, true)
			cutEvent.Replacement = &projected
			a.emit(eventchan.UserMessageReverted, cutEvent)
			published = true
		},
	}, sendMessagePrepared{}); sendErr != nil {
		// The deferred recovery returns the edit to the composer and retains an
		// explicit committed-cut outcome even if the event has not reached clients.
		return result, fmt.Errorf("revert and resend: resend failed: %w", sendErr)
	}

	if err := a.store.DeleteThreadDraftRecovery(threadID, opts.SendID); err != nil {
		// The message is accepted; return a cleanup outcome without reporting a
		// failed send. Boot recovery recognizes its SendID and retires the copy.
		result.Warning = fmt.Sprintf("Message sent; recovery cleanup failed: %v", err)
		return result, nil
	}
	return result, nil
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
	// point (InterruptAndRevertIfClean), whose Codex path lets
	// thread/revert own active-turn shutdown.
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

func (a *App) recoverReplacementDraft(who transport.ClientIdentity, recovery store.ThreadDraftRecovery) error {
	var ids []string
	if err := json.Unmarshal([]byte(recovery.Attachments), &ids); err != nil {
		return err
	}
	for attempt := 0; attempt < 8; attempt++ {
		current, _, err := a.store.GetThreadDraft(recovery.ThreadID)
		if err != nil {
			return err
		}
		merged, err := composerdraft.MergeParts(recovery.ThreadID, current, []composerdraft.Part{{Content: recovery.Content, AttachmentIDs: ids}}, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		committed, err := a.writeRecoveredThreadDraft(who, recovery, current, merged)
		if err != nil {
			return err
		}
		if committed {
			return nil
		}
	}
	return errors.New("composer kept changing while recovering the edited message; recovery retained")
}

func (a *App) restoreReplacementDraftsAtBoot() error {
	rows, err := a.store.ListThreadDraftRecoveries()
	if err != nil {
		return err
	}
	var failures []error
	for _, row := range rows {
		if _, found, err := a.findRecordedSend(row.ThreadID, row.SendID); err != nil {
			failures = append(failures, err)
		} else if found {
			if err := a.store.DeleteThreadDraftRecovery(row.ThreadID, row.SendID); err != nil {
				failures = append(failures, err)
			}
		} else if err := a.recoverReplacementDraft(transport.ClientIdentity{}, row); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
