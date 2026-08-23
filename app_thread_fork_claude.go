// Claude half of the fork saga (app_thread_fork.go): session-JSONL
// slicing at a turn / message cut, the mid-turn capture that pins a
// live-source tail fork's lazy cut, and the provider-id remap that
// keeps cloned rows pointing at a slice's reminted uuids.
package main

import (
	"errors"
	"fmt"
	"log"
	"os"

	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// forkClaudeThread wires Claude's resume state for the new fork.
//
// Fork at tail (atTurnIndex == nil OR atTurnIndex >= lastTurn) on an
// IDLE source: the plain "lazy fork" — stamp PendingForkRef =
// source.SessionRef, and the next session start passes --fork-session
// to the Claude CLI, which forks from the source JSONL's tail at
// startup. With no session live, that tail is exactly the tail the
// timeline was cloned at, so no pin is needed.
//
// Fork at tail, LIVE source (midTurnCut != nil): the PINNED lazy fork.
// Same PendingForkRef mechanism, plus PinnedResumeAt = the leaf the
// caller captured before the clone. The fork's first session start
// repairs that pin against the CLI's resume filters and passes
// `--resume-session-at <cursor> --fork-session`, which makes the CLI
// cut its fork copy exactly at the pin even when the source has kept
// streaming since (spike-verified 2.1.237: rows after the cursor are
// dropped from the fork copy). Unpinned lazy on a live source is
// forbidden — it snapshots whatever the transcript looks like at first
// send, minutes or turns later (the 2026-08-22 skew incident).
//
// Fork at point: slice the source JSONL ourselves (the official
// recipe — see internal/provider/claude/sessionfork). The new
// <newID>.jsonl on disk is a complete, resume-loadable session
// truncated through the END of atTurnIndex's turn (so the previous
// turn's full assistant response is preserved — slicing at the user
// prompt itself would leave Claude waiting to respond on resume,
// which is the wrong semantics). SessionRef points at the new ID
// directly — no --fork-session needed since the JSONL is already the
// fork. This path is unchanged mid-turn: an anchored fork always cuts
// strictly below the in-flight turn (the caller normalizes anything
// higher to a tail fork), so its anchor rows are old and already on
// disk, and ParseTranscript tolerates the source's torn final line.
func (a *App) forkClaudeThread(source store.Thread, atTurnIndex *int, midTurnCut *claudeMidTurnCut) (forkResumeState, error) {
	// The mid-turn cut is decided BEFORE the missing-session-ref
	// refusal: mid-turn, a tail fork of a thread whose session has not
	// landed yet is the sanctioned degenerate case (fresh provider
	// thread on first send), not an error. Its presence already implies
	// atTurnIndex == nil — the caller only captures one for a tail fork.
	if midTurnCut != nil {
		if midTurnCut.degenerate() {
			return forkResumeState{}, nil
		}
		return forkResumeState{
			PendingForkRef: midTurnCut.SessionRef,
			PinnedResumeAt: midTurnCut.Leaf,
		}, nil
	}

	tail := atTurnIndex == nil
	if !tail {
		lastTurn, err := a.store.LastTurnIndex(source.ID)
		if err != nil {
			return forkResumeState{}, fmt.Errorf("fork thread: load last turn index: %w", err)
		}
		// Forking at or past the last turn is equivalent to fork-at-tail.
		tail = *atTurnIndex >= lastTurn
	}

	if source.SessionRef == "" {
		return forkResumeState{}, fmt.Errorf("fork thread: source thread %q is missing a Claude session reference", source.ID)
	}

	if tail {
		// Tripwire, not a reachable path: ForkThread captures a mid-turn
		// cut whenever a live session is registered, so arriving here with
		// one means a caller skipped the capture — and the unpinned lazy
		// path below would snapshot the transcript at the fork's FIRST
		// SEND, minutes or turns after the timeline was cloned (the
		// 2026-08-22 skew incident). Refuse loudly instead of silently
		// deferring the cut.
		if _, ok := a.activeClaudeSession(source.ID); ok {
			return forkResumeState{}, fmt.Errorf(
				"fork thread: source thread %s has a live Claude session but no mid-turn cut was captured — refusing the unpinned lazy --fork-session path, which would snapshot at first send instead of now",
				source.ID,
			)
		}
		// Lazy fork-at-tail — startSession will pass --fork-session.
		// No inline slice happens here so there's nothing to remap;
		// the fork's --fork-session start will mint new UUIDs that the
		// AO row's stored provider_item_id never sees. A subsequent
		// revert in the fork falls back to the ordinal walk (now
		// synthetic-flag-safe) via the ErrMessageNotFound branch in
		// `writeRevertedClaudeSession`.
		return forkResumeState{PendingForkRef: source.SessionRef}, nil
	}

	srcPath, err := sessionfork.LocateSessionFile(source.SessionRef, source.WorkspacePath)
	if err != nil {
		return forkResumeState{}, fmt.Errorf("fork thread: locate claude session: %w", err)
	}

	// Prefer UUID-keyed slicing when the user_text at turn
	// `*atTurnIndex+1` carries a stamped wire UUID — the slice is then
	// immune to synthetic-entry ordinal drift (e.g. /compact). Falls
	// back to the ordinal walk for legacy rows that pre-date the
	// stamp.
	newID, newPath, uuidMap, err := a.writeForkedClaudeSession(srcPath, source.ID, *atTurnIndex)
	if err != nil {
		return forkResumeState{}, fmt.Errorf("fork thread: write forked session: %w", err)
	}
	return forkResumeState{
		SessionRef: newID,
		UUIDMap:    uuidMap,
		Cleanup: func() error {
			// Best-effort: a missing file is OK (already cleaned up elsewhere).
			if err := os.Remove(newPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("fork thread cleanup: remove %s: %w", newPath, err)
			}
			return nil
		},
	}, nil
}

// claudeMidTurnCut is the transcript cut for a live-source tail fork,
// resolved BEFORE the SQLite clone so the pin and the cloned timeline
// describe the same moment (see ForkThread). A zero SourcePath or Leaf
// is the sanctioned degenerate case: the fork starts a FRESH provider
// thread on its first send and the cloned prompt is its whole
// transcript.
type claudeMidTurnCut struct {
	SessionRef    string
	WorkspacePath string
	SourcePath    string
	Leaf          string
}

func (c claudeMidTurnCut) degenerate() bool {
	return c.SourcePath == "" || c.Leaf == ""
}

// captureClaudeMidTurnCut resolves where a mid-turn tail fork will cut
// the source transcript. It runs before the clone and performs no
// writes.
//
// The leaf comes from the live session's stdout tracker
// (CanonicalLeafUUID). When no session is registered — an open turn row
// with a dead process, the crash window before the boot sweep settles
// it — the cold scan over the on-disk file answers instead; that is
// exactly what ScanSessionLeaf is for.
//
// Exactly three shapes are allowed to answer "no cut", and each is a
// genuinely-early state rather than a failure:
//
//   - the thread has no Claude session reference yet;
//   - LocateSessionFile reports ErrSessionFileNotFound (the CLI has not
//     created the transcript yet);
//   - ScanSessionLeaf SUCCEEDS and finds no settled leaf in the file.
//
// Every other error — a projects-dir/home resolution failure, a stat or
// open error, a transcript over the scanner's size or row bounds —
// FAILS the fork. Those are real I/O faults, and laundering one into a
// context-less fork with a fully-populated timeline would hand the user
// a thread that silently lost its history (core principle 5), while the
// idle and anchored sibling paths hard-error on the very same class.
func (a *App) captureClaudeMidTurnCut(source store.Thread) (claudeMidTurnCut, error) {
	const op = "fork thread mid-turn"

	sourceRef := source.ResolvedSessionRef()
	if sourceRef == "" {
		log.Printf("%s: thread %s has no Claude session reference yet — fork starts a fresh provider thread", op, source.ID)
		return claudeMidTurnCut{}, nil
	}
	srcPath, err := sessionfork.LocateSessionFile(sourceRef, source.WorkspacePath)
	if err != nil {
		if errors.Is(err, sessionfork.ErrSessionFileNotFound) {
			log.Printf("%s: thread %s session %s not on disk yet — fork starts a fresh provider thread", op, source.ID, sourceRef)
			return claudeMidTurnCut{}, nil
		}
		return claudeMidTurnCut{}, fmt.Errorf("%s: locate claude session %s: %w", op, sourceRef, err)
	}

	cut := claudeMidTurnCut{SessionRef: sourceRef, WorkspacePath: source.WorkspacePath, SourcePath: srcPath}
	if sess, ok := a.activeClaudeSession(source.ID); ok {
		if leaf := sess.CanonicalLeafUUID(); leaf != "" {
			cut.Leaf = leaf
			return cut, nil
		}
	}
	// The cold scan is deliberately the fallback: it re-reads the whole
	// file, and the live tracker answers on the overwhelmingly common
	// path.
	leaf, err := a.scanClaudeSessionLeaf(op, cut)
	if err != nil {
		return claudeMidTurnCut{}, err
	}
	if leaf == "" {
		log.Printf("%s: thread %s session %s has no settled leaf — fork starts a fresh provider thread", op, source.ID, sourceRef)
		return claudeMidTurnCut{}, nil
	}
	cut.Leaf = leaf
	return cut, nil
}

// scanClaudeSessionLeaf is the cold-scan read, with the failure/absence
// split captureClaudeMidTurnCut documents: ("", nil) means the file
// parsed and holds no settled leaf; an error is a real I/O fault (stat,
// open, over the scanner's byte or row bound) and fails the fork.
func (a *App) scanClaudeSessionLeaf(op string, cut claudeMidTurnCut) (string, error) {
	state, err := claude.ScanSessionLeaf(cut.SessionRef, cut.WorkspacePath)
	if err != nil {
		return "", fmt.Errorf("%s: scan claude session leaf for %s: %w", op, cut.SessionRef, err)
	}
	return state.CanonicalLeafUUID, nil
}

func (a *App) forkClaudeThreadBeforeMessage(source store.Thread, anchor store.MessageAnchor, anchorItem store.Item) (forkResumeState, error) {
	midTurn, err := claudeMidTurnAnchor(anchorItem)
	if err != nil {
		return forkResumeState{}, fmt.Errorf("fork thread from message: %w", err)
	}
	// Only an anchor that OPENS turn 0 keeps nothing — the fork then
	// starts a fresh provider session. A mid-turn-0 anchor (a message
	// queued during the very first turn) keeps that turn's prefix and
	// needs the session slice like any later anchor.
	if anchor.TurnIndex == 0 && !midTurn {
		return forkResumeState{}, nil
	}
	sourceSessionRef := source.ResolvedSessionRef()
	if sourceSessionRef == "" {
		return forkResumeState{}, fmt.Errorf("fork thread from message: source thread %q is missing a Claude session reference", source.ID)
	}
	srcPath, err := sessionfork.LocateSessionFile(sourceSessionRef, source.WorkspacePath)
	if err != nil {
		return forkResumeState{}, fmt.Errorf("fork thread from message: locate claude session: %w", err)
	}
	newID, newPath, uuidMap, err := a.writeMessageForkedClaudeSession(srcPath, anchor, anchorItem, midTurn)
	if err != nil {
		return forkResumeState{}, fmt.Errorf("fork thread from message: write forked session: %w", err)
	}
	return forkResumeState{
		SessionRef: newID,
		UUIDMap:    uuidMap,
		Cleanup: func() error {
			if err := os.Remove(newPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("fork thread from message cleanup: remove %s: %w", newPath, err)
			}
			return nil
		},
	}, nil
}

// writeForkedClaudeSession is the turn-keyed-fork call into
// writeClaudeSessionSlice. The slice anchor is the user_text at
// turn `atTurnIndex+1` — that is the first turn dropped from the
// fork, so its parent is the end of the last kept turn.
func (a *App) writeForkedClaudeSession(srcPath, sourceThreadID string, atTurnIndex int) (string, string, map[string]string, error) {
	anchorUUID := a.lookupTurnAnchorClaudeUUID(sourceThreadID, atTurnIndex+1)
	logCtx := fmt.Sprintf("fork thread (turn %d)", atTurnIndex+1)
	// Turn-keyed forks anchor at a turn boundary by construction, so the
	// ordinal fallback's whole-turn granularity is exact here (and the
	// mid-turn parent-uuid retry detection never applies — no parent).
	return writeClaudeSessionSlice(srcPath, []string{anchorUUID}, nil, atTurnIndex, false, logCtx)
}

// writeMessageForkedClaudeSession is the message-keyed-fork call
// into writeClaudeSessionSlice. The slice anchors are the dropped
// user message's wire UUID candidates — the anchor row's copy,
// then the item row's durable meta stamp (claudeSliceAnchorUUIDs);
// midTurnAnchor comes from the anchor item's position, same as the
// un-send path.
func (a *App) writeMessageForkedClaudeSession(srcPath string, anchor store.MessageAnchor, anchorItem store.Item, midTurnAnchor bool) (string, string, map[string]string, error) {
	return writeClaudeSessionSlice(
		srcPath, claudeSliceAnchorUUIDs(anchor, anchorItem), claudeSliceParentUUIDs(anchor, anchorItem),
		anchor.TurnIndex-1, midTurnAnchor, "fork thread from message",
	)
}

// lookupTurnAnchorClaudeUUID returns the wire UUID stamped on the
// user_text row at turnIndex, or "" if no such row carries a
// stable id. Used by the fork-slice helpers to pick the UUID-keyed
// branch when available.
func (a *App) lookupTurnAnchorClaudeUUID(threadID string, turnIndex int) string {
	items, err := a.store.ListItemsForTurn(threadID, turnIndex)
	if err != nil {
		log.Printf("fork thread: load turn %d items for anchor lookup: %v", turnIndex, err)
		return ""
	}
	for _, it := range items {
		if it.Kind != "user_text" || it.Role != "user" {
			continue
		}
		if store.IsWireOnlyUserItem(it) {
			// Cascade-injected user rows (task_notification echo,
			// future Codex MCP injection) are mid-turn anchors that
			// don't bound a turn boundary — skip them so the lookup
			// picks the AO-authored row that opens the turn.
			continue
		}
		if id := usermessage.ReadProviderItemID(it.Meta); id != "" {
			return id
		}
	}
	return ""
}

// remapClaudeProviderIDs rewrites every stored provider id that points
// into the OLD session file to the NEW session's reminted UUIDs:
// items' `meta.provider_item_id` and message anchors'
// `provider_user_message_id` / `provider_parent_uuid`. Every fork-slice
// remints every uuid (sessionfork.buildLines), so any id left pointing
// at the source session silently degrades the next un-send/fork to the
// ordinal-walk fallback. Maintains the invariant "stored UUID always
// matches the active session's JSONL".
//
// Callers: the fork pipeline (cloned items; forks carry no anchor rows
// — that loop is a no-op there) and rollbackClaudeThreadToMessage
// (surviving items + anchors of the SAME thread after its
// SessionRef moves to the slice).
//
// uuidMap may have entries beyond just user-message UUIDs (assistant /
// system entries also remap). Anything unmapped (legacy rows,
// mismatched ids) is left alone rather than blanking the column —
// UpdateMessageAnchorProviderIDs's empty-string-preserves contract gives
// the same semantics on the anchor side.
//
// Returns nil when the thread has no Claude-stamped rows (Codex fork,
// lazy fork-at-tail, fork of a pre-stamp thread).
//
// Atomicity note: per-row UPDATEs run outside a single SQL transaction.
// In the fork pipeline that is safe because every caller wraps the
// remap in a `closer.Stack` whose rollback deletes the fork thread
// (and cascades to its items + anchors) on any error — a mid-remap
// failure never leaves a partially-remapped fork visible to readers.
// The un-send path does NOT use this method: it commits the same
// rewrites atomically with its SessionRef move via
// computeClaudeProviderIDRemap + UpdateThreadAndRemapProviderIDs
// (round-6, R6-5).
func (a *App) remapClaudeProviderIDs(threadID string, uuidMap map[string]string) error {
	itemUpdates, anchorUpdates, err := a.computeClaudeProviderIDRemap(threadID, uuidMap)
	if err != nil {
		return err
	}
	for _, update := range itemUpdates {
		if err := a.store.UpdateItemMeta(threadID, update.ItemID, update.Meta); err != nil {
			return fmt.Errorf("remap claude provider ids: update item %s/%s meta: %w", threadID, update.ItemID, err)
		}
	}
	for _, update := range anchorUpdates {
		if err := a.store.UpdateMessageAnchorProviderIDs(threadID, update.UserItemID, update.ProviderUserMessageID, update.ProviderParentUUID); err != nil {
			return fmt.Errorf("remap claude provider ids: update anchor %s/%s: %w", threadID, update.UserItemID, err)
		}
	}
	return nil
}

// computeClaudeProviderIDRemap reads the thread's user rows and
// message anchors and returns the rewrites uuidMap implies, without
// applying anything. Shared by remapClaudeProviderIDs (fork pipeline,
// per-row writes under the saga rollback) and the un-send path (which
// hands the result to UpdateThreadAndRemapProviderIDs so the rewrites
// commit atomically with the SessionRef move — round-6, R6-5).
func (a *App) computeClaudeProviderIDRemap(threadID string, uuidMap map[string]string) ([]store.ItemMetaUpdate, []store.MessageAnchorProviderIDsUpdate, error) {
	if len(uuidMap) == 0 {
		return nil, nil, nil
	}

	// 1. user_text items. Read all items, filter, remap meta.
	items, err := a.store.ListItems(threadID)
	if err != nil {
		return nil, nil, fmt.Errorf("remap claude provider ids: list items: %w", err)
	}
	var itemUpdates []store.ItemMetaUpdate
	for _, it := range items {
		if it.Kind != "user_text" || it.Role != "user" {
			continue
		}
		// Both meta ids remap in one write: the item id and the parent
		// uuid stamped alongside it (round-5, R5-8). Unmapped lookups
		// yield "", which MergeProviderIDs preserves — same semantics as
		// UpdateMessageAnchorProviderIDs on the anchor side.
		newUUID := uuidMap[usermessage.ReadProviderItemID(it.Meta)]
		newParent := uuidMap[usermessage.ReadProviderParentUUID(it.Meta)]
		if newUUID == "" && newParent == "" {
			continue
		}
		newMeta, err := usermessage.MergeProviderIDs(it.Meta, newUUID, newParent)
		if err != nil {
			return nil, nil, fmt.Errorf("remap claude provider ids: merge item %s/%s meta: %w", threadID, it.ID, err)
		}
		if newMeta == it.Meta {
			continue
		}
		itemUpdates = append(itemUpdates, store.ItemMetaUpdate{ItemID: it.ID, Meta: newMeta})
	}

	// 2. Anchor provider ids — the un-send slice anchor
	// (provider_user_message_id) and the fork parent cursor
	// (provider_parent_uuid). uuidMap[""] is "" and unmapped lookups
	// yield "", both of which the empty-preserves UPDATE keeps.
	anchors, err := a.store.ListMessageAnchors(threadID)
	if err != nil {
		return nil, nil, fmt.Errorf("remap claude provider ids: list message anchors: %w", err)
	}
	var anchorUpdates []store.MessageAnchorProviderIDsUpdate
	for _, anchor := range anchors {
		newMsgID := uuidMap[anchor.ProviderUserMessageID]
		newParent := uuidMap[anchor.ProviderParentUUID]
		if newMsgID == "" && newParent == "" {
			continue
		}
		anchorUpdates = append(anchorUpdates, store.MessageAnchorProviderIDsUpdate{
			UserItemID:            anchor.UserItemID,
			ProviderUserMessageID: newMsgID,
			ProviderParentUUID:    newParent,
		})
	}

	return itemUpdates, anchorUpdates, nil
}

// activeClaudeSession is the Claude sibling of activeCodexSession. The
// mid-turn fork reads CanonicalLeafUUID off it — the live stdout
// tracker's answer, which is ahead of the transcript file by however
// long the CLI takes to append.
func (a *App) activeClaudeSession(threadID string) (*claude.Session, bool) {
	sess, ok := a.sessionManager().get(threadID)
	if !ok || sess.claude == nil {
		return nil, false
	}
	return sess.claude, true
}
