// Claude half of the conversation rollback (app_conversation_rollback.go):
// the session-JSONL slice shared by rollback and fork-from-message.
package app

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"agent-overflow/internal/itemmeta"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

func (a *App) rollbackClaudeThreadToMessage(thread store.Thread, anchor store.MessageAnchor, userItem store.Item) error {
	midTurn, err := claudeMidTurnAnchor(userItem)
	if err != nil {
		return fmt.Errorf("claude rollback: %w", err)
	}
	// A rollback to the row that opens turn 0 keeps nothing: drop the session
	// reference and let the next send start fresh. An anchor deeper in turn
	// 0 (a flush message queued during the very first turn) keeps that
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
	// Prefer UUID-keyed slicing when the anchor carries a wire id: it
	// is immune to synthetic-entry ordinal drift (e.g.
	// /compact-summary rows or `[Request interrupted by user]`
	// markers). Fall back to the ordinal walk when the anchor has no
	// stamped id (the user_text row pre-dates triage's
	// `provider_item_id` stamping path, or the synthesized at-send
	// record found nothing on the item meta; the fast send→escape
	// race lands here). `findmessage.isRealUserPrompt` filters the same
	// synthetic entries (boolean flags, content sentinels, injected XML)
	// so the fallback is correct as long as the wire shape stays in its
	// documented set.
	newID, newPath, err := a.writeRolledBackClaudeSession(srcPath, anchor, userItem, midTurn)
	if err != nil {
		return fmt.Errorf("write rolled-back session: %w", err)
	}
	// The slice keeps every uuid, so the surviving rows' stored provider
	// ids already name entries in the new file; SessionRef is the only
	// state that moves. It commits only after the slice succeeded, and a
	// failed commit removes the slice so no file outlives the attempt.
	if _, err := a.store.UpdateSessionRef(thread.ID, newID); err != nil {
		a.removeAbandonedSessionSlice(newPath)
		return fmt.Errorf("persist rolled-back claude state: %w", err)
	}
	return nil
}

// removeAbandonedSessionSlice deletes a Claude slice file whose rollback
// aborted before committing any store state. The file is inert (no
// thread references it), so a failed delete only leaks disk, but a
// silent leak across repeated failed rollbacks is invisible, so log it
// (round-7, R7-6).
func (a *App) removeAbandonedSessionSlice(path string) {
	if err := os.Remove(path); err != nil {
		log.Printf("app: claude rollback: remove abandoned session slice %s: %v", path, err)
	}
}

// claudeMidTurnAnchor reports whether a rollback/fork anchor row sits
// mid-turn in PROVIDER order: content the session slice must retain
// precedes it inside its own turn. Display position alone
// (ItemIndex > 0) undercounts: a promoted flush row healed at its
// dispatch-time index after a failed tail bump (round-10, R10-1) can
// sit at display index 0 while the interrupted round's tail
// (provider-order BEFORE the queued message) persists below it, and
// the ordinal whole-turn slice (or the turn-0 drop-SessionRef branch)
// would cut that retained prefix from the transcript while
// DeleteConversationFromItem's promoted predicate keeps it in SQLite
// (round-12, C12-1). The promotion marker is the durable record of
// that ordering. Head-healed deferred prompts (negative index,
// unmarked; round-7 R7-4 / round-8 R8-1) stay turn-initial. A
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
// writeClaudeSessionSlice. midTurnAnchor comes from claudeMidTurnAnchor:
// a queued flush row sharing its turn with content that precedes it in
// provider order.
func (a *App) writeRolledBackClaudeSession(srcPath string, anchor store.MessageAnchor, userItem store.Item, midTurnAnchor bool) (string, string, error) {
	return writeClaudeSessionSlice(
		srcPath, claudeSliceAnchorUUID(anchor, userItem), claudeSliceParentUUID(anchor, userItem),
		anchor.TurnIndex-1, midTurnAnchor, "claude rollback",
	)
}

// claudeSliceAnchorUUID returns the wire uuid keying a Claude slice at
// userItem: the item row's durable meta stamp, else the anchor row's
// provider_user_message_id. The item meta is stamped in the echo's own
// transaction and the anchor copy only follows it (the anchor is recorded
// from the item meta, then refreshed by a follow-up
// UpdateMessageAnchorProviderIDs that can fail; round-4 review, CT4-6), so
// the item copy is never the older of the two. An empty id therefore does
// NOT mean the message never reached the provider when the other copy
// holds one: a consumed mid-turn message stays on the exact UUID-keyed
// slice instead of being misclassified into the unconsumed full-clone
// path, which would truncate the timeline while the provider transcript
// keeps the message.
func claudeSliceAnchorUUID(anchor store.MessageAnchor, userItem store.Item) string {
	if id := usermessage.ReadProviderItemID(userItem.Meta); id != "" {
		return id
	}
	if anchor.ProviderUserMessageID != "" {
		log.Printf("claude slice: item %s/%s carries no provider uuid; using the anchor row's copy %q", userItem.ThreadID, userItem.ID, anchor.ProviderUserMessageID)
	}
	return anchor.ProviderUserMessageID
}

// claudeSliceParentUUID returns the anchor's transcript-parent uuid for
// the already-cut retry, in the same order as claudeSliceAnchorUUID: the
// item row's meta copy, written atomically with the item id at the echo
// (round-5, R5-8), else the anchor row's provider_parent_uuid.
func claudeSliceParentUUID(anchor store.MessageAnchor, userItem store.Item) string {
	if parent := usermessage.ReadProviderParentUUID(userItem.Meta); parent != "" {
		return parent
	}
	return anchor.ProviderParentUUID
}

// writeClaudeSessionSlice writes the Claude session slice that ends just
// before the anchored message. Every slice keeps each surviving entry's
// uuid, so an id AO stored against any earlier generation of this
// session names the same entry here. logCtx prefixes the logs so the
// operator can tell which entry point hit a fallback.
//
// With a stored anchorUUID the slice is UUID-keyed. When the transcript
// does not contain that uuid, the slice falls to anchorParentUUID and
// then FAILS. It does NOT fall back to the ordinal walk, whichever side
// of its turn the anchor sits on:
//
//   - Parent PRESENT in the transcript: a prior slice already cut this
//     transcript exactly at the anchor, which the rollback wrote before
//     a later step (draft restore or SQLite truncation) failed. The
//     retry re-slices keeping through the parent and lets the caller
//     redo the remaining steps. Through the parent, not a whole clone:
//     anything appended after the failed rollback (a resumed session's
//     rows) must not be resurrected into the retried cut (round-5,
//     R5-6).
//
//   - Parent ABSENT (or unknown): the row names a provider id the
//     transcript does not contain. The ordinal walk cannot repair it:
//     AO's row count and the transcript's prompt count are no longer
//     known to agree, so the walk can slice a turn too far and the
//     resumed session contradicts the timeline the user is looking at. A
//     mid-turn anchor's ordinal walk drops the shared turn's kept prefix
//     on top of that. Both silently diverge, so the operation FAILS:
//     loud and recoverable beats a session whose context contradicts what
//     the user rolled back.
//
//     The Claude CLI's own queue-boundary merge does not normally
//     arrive here. It keeps only the last member's uuid, but AO folds
//     its rows into one at echo time (internal/triage/claude_merge_fold.go),
//     so the row a revert anchors at names the uuid the transcript
//     holds. A merge the fold could not prove is left unfolded and is
//     the known way to reach this refusal.
//
//     The single exception is the transcript ENDING before the anchor's
//     turn. That absence cannot be a merge: the CLI died before
//     persisting this prompt, its file is already at the right cut, and
//     cloning it whole is the bricked-session recovery AO's composer
//     rehydration completes. It is probed, not sliced, so the refusal
//     above leaves no orphan file.
//
// midTurnAnchor still changes the NO-uuid handling. The ordinal walk keeps
// whole turns, so for an anchor that does not open its turn (a queued flush
// row sharing turn N with an earlier prompt) it would slice at
// end-of-turn-N-1 and drop the shared turn's kept prefix. A mid-turn anchor
// with an EMPTY uuid was never consumed (the anchor's provider id is stamped
// only by the consumption echo), so the transcript is already at the right cut
// and is cloned whole (the common case: rollback of an interrupt-promoted row
// before its echo). An id-less anchor that DOES open its turn predates the
// wire-id stamp and keeps the ordinal walk.
func writeClaudeSessionSlice(
	srcPath string,
	anchorUUID string,
	anchorParentUUID string,
	fallbackLastKeptTurn int,
	midTurnAnchor bool,
	logCtx string,
) (string, string, error) {
	anchorUUID = strings.TrimSpace(anchorUUID)
	if anchorUUID != "" {
		newID, newPath, err := sessionfork.WriteForkFileForUserMessageUUID(srcPath, anchorUUID, "")
		if err == nil {
			return newID, newPath, nil
		}
		if !errors.Is(err, sessionfork.ErrMessageNotFound) {
			return "", "", err
		}
		if parent := strings.TrimSpace(anchorParentUUID); parent != "" {
			// Beside the source: srcPath was located from the thread's
			// CURRENT workspace, and a workspace change relocates a
			// ref-carrying transcript before it gets here
			// (copyClaudeSessionForWorkspaceChange), so the source
			// directory already IS the current workspace's slug.
			newID, newPath, parentErr := sessionfork.WriteForkFileThroughUUID(sessionfork.ForkCut{
				SourcePath:   srcPath,
				LastKeptUUID: parent,
			})
			if parentErr == nil {
				log.Printf("%s: anchor uuid %q absent but its parent %q is present in session %s; a prior slice already cut this transcript at the anchor, re-slicing through the parent", logCtx, anchorUUID, parent, srcPath)
				return newID, newPath, nil
			}
			if !errors.Is(parentErr, sessionfork.ErrMessageNotFound) {
				return "", "", parentErr
			}
		}
		if !midTurnAnchor {
			// The one absence that provably is NOT a merge: the
			// transcript ENDS before the anchor's turn, so the CLI died
			// before writing this prompt and its own file is already at
			// the right cut. Probed rather than sliced so the refusal
			// below leaves no orphan file behind.
			if _, probeErr := sessionfork.SliceUUIDForLastKeptTurn(srcPath, fallbackLastKeptTurn); errors.Is(probeErr, sessionfork.ErrUserTurnAtTranscriptEnd) {
				log.Printf("%s: anchor uuid %q absent and the transcript ends before turn %d (%v); the CLI never persisted this prompt, cloning full transcript", logCtx, anchorUUID, fallbackLastKeptTurn+1, probeErr)
				return sessionfork.WriteForkFileFullTranscript(srcPath, "")
			}
		}
		return "", "", fmt.Errorf(
			"%s: stored provider uuid %q is missing from session %s: this message reached the provider but the transcript has no entry under that id, so AO cannot tell which entry to cut at; refusing a slice that would silently diverge from the timeline. The known cause is a Claude queue merge AO could not fold into one message",
			logCtx, anchorUUID, srcPath,
		)
	}
	if midTurnAnchor {
		return sessionfork.WriteForkFileFullTranscript(srcPath, "")
	}
	newID, newPath, err := sessionfork.WriteForkFileForLastKeptTurn(srcPath, fallbackLastKeptTurn, "")
	if err == nil {
		return newID, newPath, nil
	}
	if errors.Is(err, sessionfork.ErrUserTurnAtTranscriptEnd) {
		// Slice anchor lands one past the last persisted user prompt:
		// AO recorded the user_text row but the Claude CLI died before
		// writing that prompt to the JSONL. Clone the JSONL as-is: the
		// file is already at the right cut point from Claude's side
		// (it never saw the missing prompt), and AO's composer
		// rehydration restores the missing message from the DB so the
		// user can re-edit and resend.
		log.Printf("%s: rollback anchor past JSONL end (%v); cloning full transcript", logCtx, err)
		return sessionfork.WriteForkFileFullTranscript(srcPath, "")
	}
	return "", "", err
}
