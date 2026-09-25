package app

import (
	"fmt"
	"log"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// rollbackConversationLockedArgs bundles the parameters for the shared
// conversation-rollback tail. Callers prepare the thread, user item,
// message anchor, and composer draft, then hand off to
// rollbackConversationLocked which owns the destructive sequence
// (provider rollback → draft upsert → SQLite truncate). Event emission
// stays with the caller.
type rollbackConversationLockedArgs struct {
	thread   store.Thread
	userItem store.Item
	anchor   store.MessageAnchor
	// promptDraft is the composer draft that replaces the rolled-back
	// prompt, upserted BEFORE the truncation so the text is never
	// homeless (see the call site below).
	//
	// nil means the CALLER owns the draft row and this tail must not
	// write it at all — the edit-and-resend saga parks its own crash copy
	// there before calling in, and restoring the old prompt would clobber
	// it. Restoring a composer the user is about to replace would be
	// meaningless there anyway.
	promptDraft *store.ThreadDraft
	// errorPrefix scopes wrapped errors so the calling surface is
	// identifiable in logs and toasts.
	errorPrefix string
	// clearRunningBackgroundTasks terminates and hides still-running
	// background tray work as part of the rollback. Set only when the
	// user explicitly confirmed killing that work (the message-keyed
	// revert path); the un-send path declines the revert instead when
	// background tasks are live. Claude relies on stopSession's
	// process-group close; Codex uses its thread-wide
	// background-terminal clean RPC before the history cut.
	clearRunningBackgroundTasks bool
}

// rollbackConversationLocked is the destructive tail shared by the two
// entry points that mutate the thread in place: revert-on-interrupt (the
// Stop/Esc un-send) and the edit-and-resend saga
// (RevertConversationAndResendMessage). Fork-from-message shares the
// provider-slice helpers below but clones instead of truncating. The
// caller is responsible for the per-thread action lock, loading the
// user item + anchor, projecting the composer draft via
// composerdraft.FromUserItem, AND emitting whatever post-rollback event
// its surface needs once this returns nil.
//
// Sequence (in order — partial failures leave a clear cleanup point):
//
//  1. Provider rollback:
//     - A live paginated Codex thread uses `thread/revert beforeTurnId`
//     on its existing connection; upstream owns active-turn shutdown,
//     persistence, the cut and runtime reload. Cold/legacy fallback stops
//     first and uses a throwaway resume for `thread/revert` or
//     `thread/fork lastTurnId`, repointing SessionRef only for the fork.
//     - claude-tui reverts natively on the Esc the caller already
//     delivered; AO only mirrors the cut in its own timeline + draft.
//     - Claude stops the provider subprocess first, then writes a
//     sliced session file.
//  2. UpsertThreadDraft restores the composer draft — skipped entirely
//     when the caller owns the draft row (promptDraft nil).
//  3. SQLite truncation at the provider rollback's granularity: Codex
//     deletes whole turns from the user-item's turnIndex (inclusive,
//     matching the turn-boundary cut BOTH Codex truncations make);
//     Claude deletes from the user item itself
//     (DeleteConversationFromItem, matching the session slice at the
//     message uuid).
//
// Returns the anchor turn's surviving item ids (empty for the Codex
// whole-turn cut and whenever the anchor opened its turn) so the
// caller's `user_message:reverted` event can tell the frontend exactly
// which anchor-turn rows to keep — the item-granular cut is decided by
// DeleteConversationFromItem's promoted-row predicate, which must not
// be re-derived in UI code. The post-cut history stamp travels with
// them, read inside the deleting transaction so the event attests
// exactly this cut (docs/architecture/thread-replica-sync.md §4) — a client
// that mirrors the cut keeps its replica entry instead of dropping it.
type revertedConversationCut struct {
	KeptAnchorTurnItemIDs []string
	Stamp                 store.HistoryStamp
	// SettleFailure is the user-facing report of a session-end settle that
	// failed after the cut committed, or "". The caller reports it once
	// the cut is published, so no client drops it with the cut turn.
	SettleFailure string
}

// UserMessageRevertedEvent is the wire payload for the
// `user_message:reverted` event emitted at the end of a successful
// conversation revert. Two callers: the Stop/Esc un-send
// (InterruptAndRevertIfClean) and the edit-and-resend saga
// (RevertConversationAndResendMessage), which sets DraftPendingResend.
// The frontend consumes this to truncate its timeline to match the
// SQLite cut. Idempotent on the frontend: a removal of an
// already-absent id is a no-op.
type UserMessageRevertedEvent struct {
	TurnStartedSequence   uint64 `json:"turnStartedSequence"`
	TurnCompletedSequence uint64 `json:"turnCompletedSequence"`
	// Replacement is published with the cut once send preparation and persistence finish.
	Replacement *store.Item `json:"replacement,omitempty"`
	// ItemEventSequence fences item frames published before the destructive cut.
	ItemEventSequence uint64 `json:"itemEventSequence"`
	ThreadID          string `json:"threadId"`
	UserItemID        string `json:"userItemId"`
	TurnIndex         int    `json:"turnIndex"`
	// KeptAnchorTurnItemIDs lists the anchor turn's SURVIVING items.
	// Turns after TurnIndex are always fully removed; within the anchor
	// turn the frontend keeps exactly these ids and drops everything
	// else, including pane-only rows that were never persisted. Empty
	// (the common case) means the whole anchor turn is gone: Codex cuts
	// are always turn-granular, and a Claude anchor that opens its turn
	// keeps nothing. Non-empty only for Claude item-granular cuts to a
	// mid-turn anchor (a queued/steered message sharing its turn with an
	// earlier prompt), where the kept prefix is decided by
	// DeleteConversationFromItem's promoted-row predicate, carried here
	// as data so the frontend never re-derives it.
	KeptAnchorTurnItemIDs []string `json:"keptAnchorTurnItemIds,omitempty"`
	// HistoryRev / HistoryEpoch are the thread's history stamps AFTER the
	// cut, read inside the deleting transaction
	// (docs/architecture/thread-replica-sync.md §3, §4). A client that applies
	// this event has mirrored the cut exactly, so it may adopt them and
	// keep its cached window instead of dropping it. Never adopt them on
	// an event whose removal instruction was not fully applied: an
	// overstated stamp would show stale content as fresh (§3.4).
	HistoryRev   int64 `json:"historyRev"`
	HistoryEpoch int64 `json:"historyEpoch"`
	// DraftPendingResend identifies a replacement operation. It leaves the
	// ordinary composer draft alone; only the early un-send rehydrates it.
	DraftPendingResend bool `json:"draftPendingResend,omitempty"`
	// ConnectionID attributes replacement recovery to the requesting page load.
	// Every client applies the cut; only that connection records its local marker.
	ConnectionID string `json:"connectionId,omitempty"`
}

func (a *App) rollbackConversationLocked(args rollbackConversationLockedArgs) (cut revertedConversationCut, err error) {
	if err := a.store.CheckThreadExecutionAccess(args.thread); err != nil {
		return cut, err
	}
	// Confirmed background-task kill runs before the provider rollback:
	// the Codex terminal-clean RPC needs the session still live, and the
	// tray rows flip inactive immediately after provider-owned work is
	// terminated so killed work never stays advertised as running if a
	// later step fails.
	if args.clearRunningBackgroundTasks {
		if err := a.cleanRunningBackgroundTasksBeforeProviderRevert(args.thread, args.errorPrefix); err != nil {
			return revertedConversationCut{}, err
		}
	}

	if args.thread.Provider == string(provider.Codex) {
		if args.clearRunningBackgroundTasks {
			if err := a.markConfirmedBackgroundTasksInactiveAfterProviderCleanup(args.thread.ID, args.errorPrefix); err != nil {
				return revertedConversationCut{}, err
			}
		}
		if err := a.rollbackCodexThreadToMessage(args.thread, args.anchor); err != nil {
			return revertedConversationCut{}, fmt.Errorf("%s: %w", args.errorPrefix, err)
		}
	} else if args.thread.Provider == string(provider.ClaudeTUI) {
		// The interactive TUI reverts the just-sent prompt natively when it
		// receives the Esc: the Esc aborts the in-flight /v1/messages and the
		// dropped turn does not re-enter the next request (LIVE-confirmed in
		// the CLI capture spike hook_escrevert + hook_revertcontext).
		// InterruptAndRevertIfClean already delivered that Esc via the provider
		// Interrupt above, so — unlike headless Claude — AO must NOT stop the
		// session (it stays live for the next turn) or rewrite a session file (the
		// TUI owns its own conversation; AO has no fork file to write). AO only
		// mirrors the native revert in its own timeline + draft below. claude-tui
		// Send clears the composer before its next paste so the prompt the TUI
		// restored can't fuse with the re-send.
	} else {
		// Mark BEFORE the stop, mirroring the Codex branch: the stop's
		// teardown settles still-running background launches with
		// session_died completion siblings
		// (SettleBackgroundLaunchesForSessionEnd), and a consented
		// revert wants these rows flipped inactive silently instead —
		// the history tail they'd annotate is about to be truncated. An
		// inactive row is excluded from the settle's query, so the mark
		// doubles as the opt-out. The tasks themselves die with the
		// stop's process-group close one step later.
		if args.clearRunningBackgroundTasks {
			if err := a.markConfirmedBackgroundTasksInactiveAfterProviderCleanup(args.thread.ID, args.errorPrefix); err != nil {
				return revertedConversationCut{}, err
			}
		}
		if err := a.stopSession(args.thread.ID); err != nil {
			return revertedConversationCut{}, fmt.Errorf("%s: stop session: %w", args.errorPrefix, err)
		}
		if err := a.rollbackProviderConversationToMessage(args.thread, args.anchor, args.userItem); err != nil {
			return revertedConversationCut{}, fmt.Errorf("%s: %w", args.errorPrefix, err)
		}
	}

	// The activity-rail todo list (threads.live_todo) deliberately survives
	// the cut. Claude keys its task list by thread (claude.Config.TaskListID),
	// so the next session still holds the same tasks, tail ones included,
	// exactly as the native TUI's Esc-revert keeps them; clearing AO's copy
	// would only make the rail disagree with what the agent has.

	// The prompt draft is restored BEFORE the destructive truncation: the
	// provider slice above already removed the message from provider
	// history, so from here on the composer draft is the user's only copy.
	// If truncation then fails, the timeline still holds the rows and the
	// anchor, and a retry converges — the provider rollback re-runs
	// against the already-cut transcript (the already-cut detector clones
	// it whole) and this upsert is idempotent (round-4 review, CT4-4).
	// A nil promptDraft means the caller owns separate durable recovery.
	if args.promptDraft != nil {
		if err := a.writeThreadDraft(transport.ClientIdentity{}, *args.promptDraft); err != nil {
			return revertedConversationCut{}, fmt.Errorf("%s: restore prompt draft: %w", args.errorPrefix, err)
		}
	}

	// Truncation granularity must match the provider rollback above. Both
	// Codex cuts — thread/fork's inclusive lastTurnId and thread/revert's
	// exclusive beforeTurnId — land on the turn boundary before the
	// anchor's turn, so SQLite drops the whole turn. Claude's session
	// slice (and the TUI's native Esc-revert) cut at the message itself, so
	// only the anchor row and what follows it go — a queued flush message
	// that shares its turn with an earlier prompt keeps that prompt and the
	// agent work that preceded the queued send. The Codex coarseness is an
	// app-server API limit, not permanent: see the granularity note on
	// codex.Session.ForkAt for what upstream already has and when this
	// branch can move to a message-granular cut.
	if args.thread.Provider == string(provider.Codex) {
		_, stamp, err := a.store.DeleteConversationFromTurn(args.thread.ID, args.userItem.TurnIndex)
		if err != nil {
			return revertedConversationCut{}, fmt.Errorf("%s: truncate conversation: %w", args.errorPrefix, err)
		}
		if a.triage != nil {
			a.triage.ClearPendingSendsFromTurn(args.thread.ID, args.userItem.TurnIndex)
			// thread/revert keeps the session live across the cut.
			a.triage.ForgetToolCallLinks(args.thread.ID)
		}
		return revertedConversationCut{Stamp: stamp}, nil
	}
	keptAnchorTurnItemIDs, stamp, err := a.store.DeleteConversationFromItem(args.thread.ID, args.userItem.ID)
	if err != nil {
		return revertedConversationCut{}, fmt.Errorf("%s: truncate conversation: %w", args.errorPrefix, err)
	}
	if a.triage != nil {
		// The claude-tui native revert keeps the session live across the cut.
		a.triage.ForgetToolCallLinks(args.thread.ID)
	}
	var settleFailure string
	if args.thread.Provider == string(provider.Claude) && a.triage != nil {
		// Deleting a completion sibling whose launch sits before the cut makes
		// that launch live again (trg_items_revive_bg_launch_on_completion_delete).
		// The stop above already ran the session-end settle, so settle again:
		// the process that owned the work is gone and the resumed session will
		// report the task as unfinished.
		// The cut has committed, so a failure here must not fail the revert;
		// an unsettled launch stays in the tray until the next session end,
		// and the caller reports the failure (SettleFailure).
		settled, err := a.triage.SettleBackgroundLaunchesForSessionEnd(args.thread.ID)
		if err != nil {
			log.Printf("app: %s: settle background launches revived by the cut on thread %s: %v", args.errorPrefix, args.thread.ID, err)
			settleFailure = triage.BackgroundSettleFailureSummary(err)
		}
		// A surviving anchor turn is the write head, so the new siblings land
		// in it. The kept set tells clients which anchor-turn rows to keep, so
		// it must name them too.
		if settled > 0 && len(keptAnchorTurnItemIDs) > 0 {
			if ids, err := a.store.ListTurnTimelineItemIDs(args.thread.ID, args.userItem.TurnIndex); err != nil {
				log.Printf("app: %s: reread surviving anchor turn on thread %s: %v", args.errorPrefix, args.thread.ID, err)
			} else {
				keptAnchorTurnItemIDs = ids
			}
		}
	}
	return revertedConversationCut{KeptAnchorTurnItemIDs: keptAnchorTurnItemIDs, Stamp: stamp, SettleFailure: settleFailure}, nil
}

func (a *App) rollbackProviderConversationToMessage(thread store.Thread, anchor store.MessageAnchor, userItem store.Item) error {
	switch thread.Provider {
	case string(provider.Claude):
		return a.rollbackClaudeThreadToMessage(thread, anchor, userItem)
	default:
		return fmt.Errorf("unsupported provider %q", thread.Provider)
	}
}

// rollbackCodexThreadToMessage cuts Codex history at the last provider-backed
// turn before the rolled-back message. A live paginated thread stays on its
// connection for thread/revert because that handler owns active-turn shutdown,
// the durable cut, and runtime reload. Cold, legacy, empty-prefix, and fork
// fallback paths stop first. Their stop is load-bearing: CleanupThread flips
// the stopped-thread gate (invariant 29) so straggler wire events cannot land
// rows on the timeline the caller is about to truncate.
//
// The cut itself is whichever of upstream's two usable truncations the
// connected app-server and this thread support — `thread/revert` in
// place where it is available (codex >= 0.148 AND a paginated-history
// thread), `thread/fork` plus repoint everywhere else. A successful in-place
// revert keeps SessionRef and the live session. A fallback stops that session
// before forking. Either way the next send sees provider history ending at the
// kept prefix.
//
// Rolling back to turn 0 — or to a prefix with no provider-backed turns —
// needs neither cut: SessionRef clears and the next send starts a fresh
// Codex thread, mirroring rollbackClaudeThreadToMessage's turn-0 branch.
// Deliberately NOT routed through a revert-to-empty even where one would
// work: that branch spawns no app-server today, and paying for a process
// to preserve a provider thread whose history would be empty buys
// nothing AO can observe.
func (a *App) rollbackCodexThreadToMessage(thread store.Thread, anchor store.MessageAnchor) error {
	// Which provider thread this AO thread WAS, sampled before any cut can
	// change it — see the forget below.
	previousRef := thread.SessionRef
	forkAnchor := ""
	anchorFound := false
	if anchor.TurnIndex > 0 {
		var err error
		forkAnchor, anchorFound, err = a.resolveCodexForkAnchor(thread.ID, anchor.TurnIndex-1)
		if err != nil {
			return fmt.Errorf("codex rollback: %w", err)
		}
		// The thread reference is only required when a fork is actually
		// needed. A kept prefix of local-only failed sends resolves to
		// no anchor and takes the fresh-thread path below even on a
		// thread that never obtained a SessionRef.
		if anchorFound && thread.SessionRef == "" {
			return fmt.Errorf("codex rollback: turn %d has provider-backed history but thread %s has no Codex thread reference", anchor.TurnIndex, thread.ID)
		}
	}
	// BEFORE any cut, while the connection is still live: a row in the
	// PROVIDER's queue is not in AO's flushqueue at all. It is a
	// row in codex's own SQLite that survives stopSession and that
	// `QueuedItemService::on_thread_idle` dispatches on the next resume. Left
	// alone it would re-run a message the user just rolled back, onto a thread
	// that no longer contains it.
	//
	// Its failure ABORTS the rollback, and before the stop, so nothing has
	// been mutated when it does. A purge that cannot complete leaves messages
	// armed to run against history the user is deleting; refusing is visible
	// and retryable, whereas a message replaying onto a truncated thread days
	// later is neither.
	if err := a.purgeCodexProviderQueueForRollback(thread.ID); err != nil {
		return fmt.Errorf("codex rollback: %w", err)
	}
	a.clearFlushDispatchForRollback(thread.ID)
	newRef := ""
	if anchorFound {
		// The exclusive anchor the in-place cut needs. Resolved here
		// rather than inside the cut so a lookup failure aborts before
		// any app-server is spawned; not finding one is not a failure
		// (see resolveCodexRevertAnchor) and simply leaves the fork.
		revertAnchor, _, err := a.resolveCodexRevertAnchor(thread.ID, anchor.TurnIndex)
		if err != nil {
			return fmt.Errorf("codex rollback: %w", err)
		}

		cut := codexHistoryCut{}
		needsColdCut := true
		if live, ok := a.activeCodexSession(thread.ID); ok &&
			revertAnchor != "" && live.SupportsThreadRevert() {
			// thread/revert is the only Codex cut that owns active-turn
			// shutdown, persistence and runtime reload as one operation. Keep
			// this connection so upstream can enforce that ordering itself.
			var needsFork bool
			cut, needsFork, err = a.tryCodexThreadRevert(
				live, thread.ID, forkAnchor, revertAnchor,
			)
			if err != nil {
				return fmt.Errorf("codex rollback: cut history through %s: %w", forkAnchor, err)
			}
			needsColdCut = needsFork
			if needsFork {
				// A fork changes provider identity and cannot leave this
				// session bound to the source. Stop only after the in-place
				// attempt has given a definite fallback answer.
				if err := a.stopSession(thread.ID); err != nil {
					return fmt.Errorf("codex rollback: stop session for fork fallback: %w", err)
				}
				cut, err = a.forkCodexThreadHistory(thread, forkAnchor)
				if err != nil {
					return fmt.Errorf("codex rollback: fork history through %s: %w", forkAnchor, err)
				}
				needsColdCut = false
			}
		}
		if needsColdCut {
			// Cold, legacy and older Codex sessions still require the original
			// handoff. No live operation can safely keep their runtime while a
			// fork repoints the AO thread.
			if err := a.stopSession(thread.ID); err != nil {
				return fmt.Errorf("codex rollback: stop session: %w", err)
			}
			cut, err = a.cutCodexThreadHistory(thread, forkAnchor, revertAnchor)
			if err != nil {
				return fmt.Errorf("codex rollback: cut history through %s: %w", forkAnchor, err)
			}
		}
		// An in-place revert keeps the thread the user is editing: same
		// Codex thread id, so SessionRef, provider-side thread cost and
		// any external `codex resume` of it all stay pointed at it. That
		// identity is the entire reason to prefer the cut, so assert it
		// rather than trust it — the provider layer validates the
		// response echo, and this validates that nothing between here
		// and there swapped the thread out from under the rollback.
		if cut.Reverted && cut.ThreadRef != thread.SessionRef {
			return fmt.Errorf(
				"codex rollback: in-place revert of %s answered for thread %q — refusing to repoint a thread that was reverted in place",
				thread.SessionRef, cut.ThreadRef,
			)
		}
		newRef = cut.ThreadRef
	} else {
		// No provider-backed prefix survives. There is no history operation to
		// preserve, so close the active turn and let the next send start fresh.
		if err := a.stopSession(thread.ID); err != nil {
			return fmt.Errorf("codex rollback: stop empty-prefix session: %w", err)
		}
	}
	if _, err := a.store.UpdateSessionRef(thread.ID, newRef); err != nil {
		return fmt.Errorf("codex rollback: persist rolled-back state: %w", err)
	}
	if newRef != previousRef {
		// The stored provider-side cost is keyed by the AO thread id but
		// DESCRIBES the Codex thread it was read from. A fork moved this
		// thread onto a new one carrying a shorter history, and the turn-0
		// branch left it with no Codex thread at all — either way the figure
		// now belongs to a thread this one no longer is, and it would keep
		// being shown until some later settled turn happened to re-read it.
		// An in-place revert takes neither branch: same thread id, and its
		// next read updates the same row.
		//
		// After the persist, deliberately: a failed UpdateSessionRef leaves the
		// row still pointing at previousRef, which the stored figure still
		// correctly describes.
		a.forgetCodexThreadCost(thread.ID)
	}
	return nil
}

// knownCodexProviderTurnCountBefore counts the distinct AO turn
// indexes below beforeTurnIndex whose user message provably reached
// the provider (a stamped provider_item_id on a non-wire-only user
// row). resolveCodexForkAnchor uses it as the cross-check that an
// anchor miss really means "empty provider prefix" and not a
// legacy-data hole.
func (a *App) knownCodexProviderTurnCountBefore(threadID string, beforeTurnIndex int) (int, error) {
	items, err := a.store.ListItems(threadID)
	if err != nil {
		return 0, err
	}
	turns := make(map[int]struct{})
	for _, item := range items {
		if item.TurnIndex >= beforeTurnIndex {
			continue
		}
		if item.Kind != "user_text" || item.Role != "user" || store.IsWireOnlyUserItem(item) {
			continue
		}
		if usermessage.ReadProviderItemID(item.Meta) == "" {
			continue
		}
		turns[item.TurnIndex] = struct{}{}
	}
	return len(turns), nil
}
