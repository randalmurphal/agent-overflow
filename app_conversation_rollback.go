package main

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
	// markReverted asks the triage router to flag the next
	// turn-completed emission as a revert (so the frontend can
	// suppress the Interrupted pill). The revert-on-interrupt path
	// interrupts a live turn, so there is an in-flight turn-complete
	// to flag.
	markReverted bool
	// clearRunningBackgroundTasks terminates and hides still-running
	// background tray work as part of the rollback. Set only when the
	// user explicitly confirmed killing that work (the message-keyed
	// revert path); the un-send path declines the revert instead when
	// background tasks are live. Claude relies on stopSession's
	// process-group close; Codex uses its thread-wide
	// background-terminal clean RPC before the session stops.
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
//  1. Optionally mark the triage router so the synthesized truncated
//     turn-complete fired by CleanupThread (during stopSession)
//     carries RevertedUserMessage=true.
//  2. Provider rollback:
//     - Codex stops the session first (closing the stopped-thread
//     gate), then forks its provider thread at the pre-rollback anchor
//     turn (thread/fork lastTurnId) through a throwaway resume session
//     and repoints SessionRef at the fork.
//     - claude-tui reverts natively on the Esc the caller already
//     delivered; AO only mirrors the cut in its own timeline + draft.
//     - Claude stops the provider subprocess first, then writes a
//     sliced session file.
//  3. UpsertThreadDraft restores the composer draft — skipped entirely
//     when the caller owns the draft row (promptDraft nil).
//  4. SQLite truncation at the provider rollback's granularity: Codex
//     deletes whole turns from the user-item's turnIndex (inclusive,
//     matching thread/fork's turn-boundary cut); Claude deletes from
//     the user item itself (DeleteConversationFromItem, matching the
//     session slice at the message uuid).
//
// Returns the anchor turn's surviving item ids (empty for the Codex
// whole-turn cut and whenever the anchor opened its turn) so the
// caller's `user_message:reverted` event can tell the frontend exactly
// which anchor-turn rows to keep — the item-granular cut is decided by
// DeleteConversationFromItem's promoted-row predicate, which must not
// be re-derived in UI code. The post-cut history stamp travels with
// them, read inside the deleting transaction so the event attests
// exactly this cut (docs/specs/thread-replica-sync.md §4) — a client
// that mirrors the cut keeps its replica entry instead of dropping it.
type revertedConversationCut struct {
	KeptAnchorTurnItemIDs []string
	Stamp                 store.HistoryStamp
}

func (a *App) rollbackConversationLocked(args rollbackConversationLockedArgs) (cut revertedConversationCut, err error) {
	if args.markReverted && a.triage != nil {
		a.triage.MarkTurnReverted(args.thread.ID)
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
		// spike/claude-mitm/probe_hook_escrevert.py + probe_hook_revertcontext.py).
		// InterruptAndRevertIfClean already delivered that Esc via the provider
		// Interrupt above, so — unlike headless Claude — AO must NOT stop the
		// session (it stays live for the next turn) or rewrite a session file (the
		// TUI owns its own conversation; AO has no fork file to write). AO only
		// mirrors the native revert in its own timeline + draft below. claude-tui
		// Send clears the composer before its next paste so the prompt the TUI
		// restored can't fuse with the re-send.
	} else {
		if err := a.stopSession(args.thread.ID); err != nil {
			return revertedConversationCut{}, fmt.Errorf("%s: stop session: %w", args.errorPrefix, err)
		}
		if args.clearRunningBackgroundTasks {
			if err := a.markConfirmedBackgroundTasksInactiveAfterProviderCleanup(args.thread.ID, args.errorPrefix); err != nil {
				return revertedConversationCut{}, err
			}
		}
		if err := a.rollbackProviderConversationToMessage(args.thread, args.anchor, args.userItem); err != nil {
			return revertedConversationCut{}, fmt.Errorf("%s: %w", args.errorPrefix, err)
		}
	}

	// The prompt draft is restored BEFORE the destructive truncation: the
	// provider slice above already removed the message from provider
	// history, so from here on the composer draft is the user's only copy.
	// If truncation then fails, the timeline still holds the rows and the
	// anchor, and a retry converges — the provider rollback re-runs
	// against the already-cut transcript (the already-cut detector clones
	// it whole) and this upsert is idempotent (round-4 review, CT4-4).
	// A nil promptDraft means the caller already put a durable copy in
	// that row itself and owns settling it.
	if args.promptDraft != nil {
		if err := a.store.UpsertThreadDraft(*args.promptDraft); err != nil {
			return revertedConversationCut{}, fmt.Errorf("%s: restore prompt draft: %w", args.errorPrefix, err)
		}
	}

	// Truncation granularity must match the provider rollback above. Codex
	// thread/fork cuts provider history at the turn boundary before the
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
		return revertedConversationCut{Stamp: stamp}, nil
	}
	keptAnchorTurnItemIDs, stamp, err := a.store.DeleteConversationFromItem(args.thread.ID, args.userItem.ID)
	if err != nil {
		return revertedConversationCut{}, fmt.Errorf("%s: truncate conversation: %w", args.errorPrefix, err)
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

// rollbackCodexThreadToMessage STOPS the session, then moves the
// thread's provider cursor to a `thread/fork` of its Codex thread cut
// at the last provider-backed turn before the rolled-back message. The
// stop is load-bearing, not cleanup: CleanupThread flips the stopped-
// thread gate (invariant 29) so straggler wire events from the old
// thread — late notifications, an interrupt-triggered turn completion
// — cannot land rows on the timeline the caller is about to truncate.
// (The old thread/rollback flow kept the session live through the
// rollback, which is exactly the window the 2026-07 deprecation-notice-
// on-a-settled-turn race lived in.) The next send resumes on the fork.
//
// Rolling back to turn 0 — or to a prefix with no provider-backed turns —
// needs no fork: SessionRef clears and the next send starts a fresh
// Codex thread, mirroring rollbackClaudeThreadToMessage's turn-0 branch.
func (a *App) rollbackCodexThreadToMessage(thread store.Thread, anchor store.MessageAnchor) error {
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
	// Stop BEFORE forking: the stopped-thread gate must be closed for
	// the whole mutation window. Forking through a still-live session
	// would leave its read loop delivering source-thread events during
	// the RPC. forkCodexThreadAt runs the fork through a throwaway
	// resume session whose events go nowhere.
	if err := a.stopSession(thread.ID); err != nil {
		return fmt.Errorf("codex rollback: stop session: %w", err)
	}
	a.clearFlushDispatchForRollback(thread.ID)
	forkRef := ""
	if anchorFound {
		var err error
		forkRef, err = a.forkCodexThreadAt(thread, forkAnchor)
		if err != nil {
			return fmt.Errorf("codex rollback: fork at %s: %w", forkAnchor, err)
		}
	}
	thread.SessionRef = forkRef
	thread.PendingForkRef = ""
	if err := a.store.UpdateThread(thread); err != nil {
		return fmt.Errorf("codex rollback: persist rolled-back state: %w", err)
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
		thread.SessionRef = ""
		thread.PendingForkRef = ""
		return a.store.UpdateThread(thread)
	}
	sourceSessionRef := thread.ResolvedSessionRef()
	if sourceSessionRef == "" {
		return fmt.Errorf("claude rollback: anchor for turn %d requires Claude session reference", anchor.TurnIndex)
	}
	srcPath, err := sessionfork.LocateSessionFile(sourceSessionRef, thread.WorkspacePath)
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
	thread.SessionRef = newID
	thread.PendingForkRef = ""
	if err := a.store.UpdateThreadAndRemapProviderIDs(thread, itemUpdates, anchorUpdates); err != nil {
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
// on the next before any fallback, round-5 R5-7). Only when EVERY
// candidate is ErrMessageNotFound — most often: the stored UUIDs are
// stale because the session was forked but the post-fork remap
// regressed — does it fall back to the ordinal walk at
// fallbackLastKeptTurn, so a known-imperfect slice still beats a
// hard error. Other errors from the UUID-keyed branch propagate
// verbatim. logCtx prefixes the fallback log so the operator can
// tell which entry point hit the stale id; a loud log here is
// deliberate because a wrong-source slice is worse than the
// ordinal walk's known synthetic-entry sensitivity.
//
// midTurnAnchor changes the no-UUID handling: the ordinal walk keeps
// whole turns, so for an anchor that does NOT open its turn (a queued
// flush row sharing turn N with an earlier prompt) it would slice at
// end-of-turn-N-1 and drop the shared turn's kept prefix from the
// provider session while SQLite retains it. A mid-turn anchor with an
// EMPTY uuid was never consumed — the anchor's provider id is
// stamped only by the consumption echo — so the transcript is already
// at the right cut and is cloned whole (the common case: rollback of an
// interrupt-promoted row before its echo). A mid-turn anchor with a
// NON-EMPTY uuid that the transcript doesn't contain splits on
// anchorParentUUIDs (the anchor row's provider_parent_uuid, then the
// item meta's copy — see claudeSliceParentUUIDs, round-5 R5-8):
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
//   - Parent ABSENT (or unknown): the stored ids went stale wholesale
//     (fork remap regression). Cloning the full transcript would resume
//     a session that still contains the rolled-back prompt and its
//     response; slicing ordinally would drop the shared turn's kept
//     prefix. Both silently diverge from the visible timeline, so the
//     operation FAILS — loud and recoverable beats a session whose
//     context contradicts what the user rolled back.
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
		if midTurnAnchor {
			for _, parent := range dedupNonEmpty(anchorParentUUIDs) {
				newID, newPath, uuidMap, parentErr := sessionfork.WriteForkFileThroughUUID(srcPath, parent, "")
				if parentErr == nil {
					log.Printf("%s: anchor uuids [%s] absent but the parent %q is present in session %s — a prior slice already cut this transcript at the anchor; re-slicing through the parent", logCtx, missed, parent, srcPath)
					return newID, newPath, uuidMap, nil
				}
				if !errors.Is(parentErr, sessionfork.ErrMessageNotFound) {
					return "", "", nil, parentErr
				}
			}
			return "", "", nil, fmt.Errorf(
				"%s: stored provider uuid %q is missing from session %s — the queued message was consumed but its stored id no longer matches the transcript (fork remap drift); refusing a mid-turn cut that would silently diverge from the timeline",
				logCtx, missed, srcPath,
			)
		}
		log.Printf("%s: stored provider uuids [%s] not in session %s — falling back to ordinal slice; check fork remap coverage", logCtx, missed, srcPath)
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
