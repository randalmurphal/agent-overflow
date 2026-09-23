package app

import (
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"

	"agent-overflow/internal/itemmeta"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
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
		}
		return revertedConversationCut{Stamp: stamp}, nil
	}
	keptAnchorTurnItemIDs, stamp, err := a.store.DeleteConversationFromItem(args.thread.ID, args.userItem.ID)
	if err != nil {
		return revertedConversationCut{}, fmt.Errorf("%s: truncate conversation: %w", args.errorPrefix, err)
	}
	if args.thread.Provider == string(provider.Claude) && a.triage != nil {
		// Deleting a completion sibling whose launch sits before the cut makes
		// that launch live again (trg_items_revive_bg_launch_on_completion_delete).
		// The stop above already ran the session-end settle, so settle again:
		// the process that owned the work is gone and the resumed session will
		// report the task as unfinished.
		// The cut has committed, so a failure here must not fail the revert;
		// an unsettled launch stays in the tray until the next session end.
		settled, err := a.triage.SettleBackgroundLaunchesForSessionEnd(args.thread.ID)
		if err != nil {
			log.Printf("app: %s: settle background launches revived by the cut on thread %s: %v", args.errorPrefix, args.thread.ID, err)
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
	return revertedConversationCut{KeptAnchorTurnItemIDs: keptAnchorTurnItemIDs, Stamp: stamp}, nil
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

func (a *App) rollbackClaudeThreadToMessage(thread store.Thread, anchor store.MessageAnchor, userItem store.Item) error {
	midTurn, err := claudeMidTurnAnchor(userItem)
	if err != nil {
		return fmt.Errorf("claude rollback: %w", err)
	}
	// A rollback to the row that opens turn 0 keeps nothing: drop the session
	// reference and let the next send start fresh. An anchor deeper in turn
	// 0 — a flush message queued during the very first turn — keeps that
	// turn's prefix, so it needs the session slice like any later turn.
	if anchor.TurnIndex == 0 && !midTurn {
		_, err := a.store.UpdateSessionRef(thread.ID, "")
		return err
	}
	sourceSessionRef := thread.ResolvedSessionRef()
	if sourceSessionRef == "" {
		return fmt.Errorf("claude rollback: anchor for turn %d requires Claude session reference", anchor.TurnIndex)
	}
	projectsDir, err := a.claudeProjectsDir()
	if err != nil {
		return fmt.Errorf("claude rollback: %w", err)
	}
	srcPath, err := sessionfork.LocateSessionFile(projectsDir, sourceSessionRef, thread.WorkspacePath)
	if err != nil {
		return fmt.Errorf("locate claude session: %w", err)
	}
	// Prefer UUID-keyed slicing when the anchor carries a wire id — it
	// is immune to synthetic-entry ordinal drift (e.g.
	// /compact-summary rows or `[Request interrupted by user]`
	// markers). Fall back to the ordinal walk when the anchor has no
	// stamped id (the user_text row pre-dates triage's
	// `provider_item_id` stamping path, or the synthesized at-send
	// record found nothing on the item meta — the fast send→escape
	// race lands here); `findmessage.isRealUserPrompt` filters the same
	// synthetic entries (boolean flags, content sentinels, injected XML)
	// so the fallback is correct as long as the wire shape stays in its
	// documented set.
	newID, newPath, uuidMap, err := a.writeRolledBackClaudeSession(srcPath, anchor, userItem, midTurn)
	if err != nil {
		return fmt.Errorf("write rolled-back session: %w", err)
	}
	// The slice reminted every uuid, so surviving items' provider_item_id
	// and surviving anchors' provider ids all point at the OLD session
	// file. Compute the rewrites BEFORE committing anything — a failure
	// here aborts with the thread untouched (the slice file is an inert
	// orphan) — then commit SessionRef + remap in ONE store transaction.
	// Committed separately, a crash between the two left ids one fork
	// generation stale, and a retried rollback on top of that lost the
	// single-generation forkedFrom provenance the anchor lookups heal
	// through (round-6, R6-5). Rows at or past the rollback anchor are
	// about to be truncated by the caller and are absent from the map —
	// unmapped ids are left untouched.
	itemUpdates, anchorUpdates, err := a.computeClaudeProviderIDRemap(thread.ID, uuidMap)
	if err != nil {
		a.removeAbandonedSessionSlice(newPath)
		return fmt.Errorf("claude rollback: compute provider id remap: %w", err)
	}
	if _, err := a.store.UpdateSessionRefAndRemapProviderIDs(thread.ID, newID, itemUpdates, anchorUpdates); err != nil {
		a.removeAbandonedSessionSlice(newPath)
		return fmt.Errorf("persist rolled-back claude state: %w", err)
	}
	return nil
}

// removeAbandonedSessionSlice deletes a Claude slice file whose rollback
// aborted before committing any store state. The file is inert (no
// thread references it), so a failed delete only leaks disk — but a
// silent leak across repeated failed rollbacks is invisible, so log it
// (round-7, R7-6).
func (a *App) removeAbandonedSessionSlice(path string) {
	if err := os.Remove(path); err != nil {
		log.Printf("app: claude rollback: remove abandoned session slice %s: %v", path, err)
	}
}

// claudeMidTurnAnchor reports whether a rollback/fork anchor row sits
// mid-turn in PROVIDER order — content the session slice must retain
// precedes it inside its own turn. Display position alone
// (ItemIndex > 0) undercounts: a promoted flush row healed at its
// dispatch-time index after a failed tail bump (round-10, R10-1) can
// sit at display index 0 while the interrupted round's tail —
// provider-order BEFORE the queued message — persists below it, and
// the ordinal whole-turn slice (or the turn-0 drop-SessionRef branch)
// would cut that retained prefix from the transcript while
// DeleteConversationFromItem's promoted predicate keeps it in SQLite
// (round-12, C12-1). The promotion marker is the durable record of
// that ordering. Head-healed deferred prompts (negative index,
// unmarked — round-7 R7-4 / round-8 R8-1) stay turn-initial. A
// malformed marker fails loudly per the corrupt-metadata posture
// (round-9, R9-4).
func claudeMidTurnAnchor(userItem store.Item) (bool, error) {
	state, err := itemmeta.DecodePromotionState(userItem.Meta)
	if err != nil {
		return false, fmt.Errorf("decode promotion state for %s/%s: %w", userItem.ThreadID, userItem.ID, err)
	}
	return userItem.ItemIndex > 0 || state.Promoted, nil
}

// writeRolledBackClaudeSession is the rollback-path call into
// writeClaudeSessionSlice. Returns the slice's uuidMap (old → new for
// every kept row) so the caller can refresh stored provider ids — the
// slice remints every uuid, exactly like a fork. midTurnAnchor comes
// from claudeMidTurnAnchor: a queued flush row sharing its turn with
// content that precedes it in provider order.
func (a *App) writeRolledBackClaudeSession(srcPath string, anchor store.MessageAnchor, userItem store.Item, midTurnAnchor bool) (string, string, map[string]string, error) {
	return writeClaudeSessionSlice(
		srcPath, claudeSliceAnchorUUIDs(anchor, userItem), claudeSliceParentUUIDs(anchor, userItem),
		anchor.TurnIndex-1, midTurnAnchor, "claude rollback",
	)
}

// claudeSliceParentUUIDs returns the anchor's transcript-parent uuid
// candidates for the already-cut retry, in the same trust order as
// claudeSliceAnchorUUIDs: the anchor row's provider_parent_uuid, then
// the item row's meta stamp. The item copy is written atomically with
// the item id at the echo (round-5, R5-8), so an anchor whose
// follow-up update failed — previously the ONLY durable parent copy —
// no longer strands the retry without a slice-through point.
func claudeSliceParentUUIDs(anchor store.MessageAnchor, userItem store.Item) []string {
	var candidates []string
	if anchor.ProviderParentUUID != "" {
		candidates = append(candidates, anchor.ProviderParentUUID)
	}
	if p := usermessage.ReadProviderParentUUID(userItem.Meta); p != "" && p != anchor.ProviderParentUUID {
		candidates = append(candidates, p)
	}
	return candidates
}

// claudeSliceAnchorUUIDs returns the wire uuid candidates keying a
// Claude slice at userItem, in trust order: the anchor row's
// provider_user_message_id, then the item row's own durable meta stamp
// when it differs. The two copies are written at different moments —
// the item meta at the echo's stamp, the anchor by a follow-up
// UpdateMessageAnchorProviderIDs that can fail after the stamp
// committed (round-4 review, CT4-6) — and refreshed by different remap
// loops (remapClaudeProviderIDs updates items before anchors, each row
// autocommitting), so either copy can be a remap generation staler than
// the other. writeClaudeSessionSlice tries each candidate before
// declaring the anchor missing (round-5, R5-7); an anchor without
// any id does NOT mean the message never reached the provider — the
// item-meta candidate keeps a consumed mid-turn message on the exact
// UUID-keyed slice instead of misclassifying it into the unconsumed
// full-clone path, which would truncate the timeline while the provider
// transcript keeps the message.
func claudeSliceAnchorUUIDs(anchor store.MessageAnchor, userItem store.Item) []string {
	var candidates []string
	if anchor.ProviderUserMessageID != "" {
		candidates = append(candidates, anchor.ProviderUserMessageID)
	}
	if id := usermessage.ReadProviderItemID(userItem.Meta); id != "" && id != anchor.ProviderUserMessageID {
		if len(candidates) == 0 {
			log.Printf("claude slice: anchor for %s/%s carries no provider uuid — using the item row's durable stamp %q", userItem.ThreadID, userItem.ID, id)
		}
		candidates = append(candidates, id)
	}
	return candidates
}

// writeClaudeSessionSlice tries the UUID-keyed fork-slice for each
// anchor candidate in order (anchor-row copy first, then the item
// row's meta stamp — see claudeSliceAnchorUUIDs; either can be a remap
// generation staler than the other, so a miss on the first is retried
// on the next before any fallback, round-5 R5-7). Other errors from the
// UUID-keyed branch propagate verbatim. logCtx prefixes the fallback log
// so the operator can tell which entry point hit the stale id.
//
// When EVERY candidate is ErrMessageNotFound the slice falls to
// anchorParentUUIDs (the anchor row's provider_parent_uuid, then the item
// meta's copy — see claudeSliceParentUUIDs, round-5 R5-8) and then FAILS. It
// does NOT fall back to the ordinal walk, whichever side of its turn the
// anchor sits on:
//
//   - Parent PRESENT in the transcript: a prior slice already cut this
//     transcript exactly at the anchor — the post-slice remap refreshed
//     the surviving parent's id while the cut-away anchor's id had
//     nothing to map to. This is the retry of a rollback whose later
//     step (SQLite truncation) failed after the provider commit;
//     re-slice keeping through the parent and let the caller redo the
//     remaining steps. Through-the-parent, not a whole clone: anything
//     appended after the failed rollback (a resumed session's rows)
//     must not be resurrected into the retried cut (round-5, R5-6).
//
//   - Parent ABSENT (or unknown): the row names a provider id the
//     transcript does not contain, which a stale stored id (fork remap
//     regression) is the known way to reach. The ordinal walk cannot
//     repair it: AO's row count and the transcript's prompt count are no
//     longer known to agree, so the walk can slice a turn too far and the
//     resumed session contradicts the timeline the user is looking at. A
//     mid-turn anchor's ordinal walk drops the shared turn's kept prefix
//     on top of that. Both silently diverge, so the operation FAILS —
//     loud and recoverable beats a session whose context contradicts what
//     the user rolled back.
//
//     The Claude CLI's own queue-boundary merge does NOT arrive here. It
//     keeps only the last member's uuid, but AO folds its rows into one
//     at echo time (internal/triage/claude_merge_fold.go), so the row a
//     revert anchors at names the uuid the transcript holds.
//
//     The single exception is the transcript ENDING before the anchor's
//     turn. That absence cannot be a merge or a stale remap: the CLI died
//     before persisting this prompt, its file is already at the right cut,
//     and cloning it whole is the bricked-session recovery AO's composer
//     rehydration completes. It is probed, not sliced, so the refusal
//     above leaves no orphan file.
//
// midTurnAnchor still changes the NO-uuid handling. The ordinal walk keeps
// whole turns, so for an anchor that does not open its turn (a queued flush
// row sharing turn N with an earlier prompt) it would slice at
// end-of-turn-N-1 and drop the shared turn's kept prefix. A mid-turn anchor
// with an EMPTY uuid was never consumed — the anchor's provider id is stamped
// only by the consumption echo — so the transcript is already at the right cut
// and is cloned whole (the common case: rollback of an interrupt-promoted row
// before its echo). An id-less anchor that DOES open its turn predates the
// wire-id stamp and keeps the ordinal walk.
//
// Returns (newSessionID, newPath, uuidMap, err). Fork and rollback
// callers both thread the uuidMap into `remapClaudeProviderIDs` so
// stored ids track the reminted session.
func writeClaudeSessionSlice(
	srcPath string,
	anchorUUIDs []string,
	anchorParentUUIDs []string,
	fallbackLastKeptTurn int,
	midTurnAnchor bool,
	logCtx string,
) (string, string, map[string]string, error) {
	dedupNonEmpty := func(uuids []string) []string {
		var out []string
		for _, candidate := range uuids {
			if uuid := strings.TrimSpace(candidate); uuid != "" && !slices.Contains(out, uuid) {
				out = append(out, uuid)
			}
		}
		return out
	}
	candidates := dedupNonEmpty(anchorUUIDs)
	for i, uuid := range candidates {
		newID, newPath, uuidMap, err := sessionfork.WriteForkFileForUserMessageUUID(srcPath, uuid, "")
		if err == nil {
			if i > 0 {
				log.Printf("%s: anchor uuid %q missed but candidate %q matched session %s — the missed copy is a remap generation stale (round-5, R5-7)", logCtx, candidates[0], uuid, srcPath)
			}
			return newID, newPath, uuidMap, nil
		}
		if !errors.Is(err, sessionfork.ErrMessageNotFound) {
			return "", "", nil, err
		}
	}
	if len(candidates) > 0 {
		missed := strings.Join(candidates, ", ")
		for _, parent := range dedupNonEmpty(anchorParentUUIDs) {
			// Beside the source: srcPath was located from the thread's
			// CURRENT workspace, and a workspace change relocates a
			// ref-carrying transcript before it gets here
			// (copyClaudeSessionForWorkspaceChange), so the source
			// directory already IS the current workspace's slug.
			newID, newPath, uuidMap, parentErr := sessionfork.WriteForkFileThroughUUID(sessionfork.ForkCut{
				SourcePath:   srcPath,
				LastKeptUUID: parent,
			})
			if parentErr == nil {
				log.Printf("%s: anchor uuids [%s] absent but the parent %q is present in session %s — a prior slice already cut this transcript at the anchor; re-slicing through the parent", logCtx, missed, parent, srcPath)
				return newID, newPath, uuidMap, nil
			}
			if !errors.Is(parentErr, sessionfork.ErrMessageNotFound) {
				return "", "", nil, parentErr
			}
		}
		if !midTurnAnchor {
			// The one absence that provably is NOT a merge or a stale id:
			// the transcript ENDS before the anchor's turn, so the CLI died
			// before writing this prompt and its own file is already at the
			// right cut. Probed rather than sliced so the refusal below
			// leaves no orphan file behind.
			if _, probeErr := sessionfork.SliceUUIDForLastKeptTurn(srcPath, fallbackLastKeptTurn); errors.Is(probeErr, sessionfork.ErrUserTurnAtTranscriptEnd) {
				log.Printf("%s: anchor uuids [%s] absent and the transcript ends before turn %d (%v) — the CLI never persisted this prompt; cloning full transcript", logCtx, missed, fallbackLastKeptTurn+1, probeErr)
				return sessionfork.WriteForkFileFullTranscript(srcPath, "")
			}
		}
		return "", "", nil, fmt.Errorf(
			"%s: stored provider uuid %q is missing from session %s — this message reached the provider but the transcript has no entry under that id, so AO cannot tell which entry to cut at; refusing a slice that would silently diverge from the timeline. The known cause is a fork remap that left the stored ids stale",
			logCtx, missed, srcPath,
		)
	}
	if midTurnAnchor {
		return sessionfork.WriteForkFileFullTranscript(srcPath, "")
	}
	newID, newPath, uuidMap, err := sessionfork.WriteForkFileForLastKeptTurn(srcPath, fallbackLastKeptTurn, "")
	if err == nil {
		return newID, newPath, uuidMap, nil
	}
	if errors.Is(err, sessionfork.ErrUserTurnAtTranscriptEnd) {
		// Slice anchor lands one past the last persisted user prompt:
		// AO recorded the user_text row but the Claude CLI died before
		// writing that prompt to the JSONL. Clone the JSONL as-is — the
		// file is already at the right cut point from Claude's side
		// (it never saw the missing prompt), and AO's composer
		// rehydration restores the missing message from the DB so the
		// user can re-edit and resend.
		log.Printf("%s: rollback anchor past JSONL end (%v) — cloning full transcript", logCtx, err)
		return sessionfork.WriteForkFileFullTranscript(srcPath, "")
	}
	return "", "", nil, err
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
