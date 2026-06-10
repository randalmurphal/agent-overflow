package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"agent-overflow/internal/checkpoint"
	"agent-overflow/internal/closer"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// ForkThread copies a source thread's timeline into a new fork and wires
// the provider-specific resume state. The whole sequence is atomic from
// the caller's point of view: if any step fails, the partially-created
// fork is torn down so no half-forked rows linger.
//
// When atTurnIndex is non-nil, the fork is sliced at that turn (0-indexed):
// items with turn_index > *atTurnIndex are dropped, the provider session
// is forked + truncated to match. Checkpoint/revert history intentionally stays
// behind with the source thread; the fork starts with no checkpoint rows or
// copied Git refs. atTurnIndex == nil preserves the existing fork-at-tail
// behavior (clone everything, fork provider state at the latest message).
//
// The "atomic unit" is emulated in the app layer rather than a single
// SQLite transaction because the fork flow crosses a boundary — it has
// to talk to the Codex provider to fork a live session and can write a
// new Claude session JSONL on disk. Wrapping
// the whole sequence in sql.Tx would hold a DB transaction open across
// a network-speed operation and break the rest of the store's
// single-connection model. Instead, we compose with a best-effort
// rollback: each step that has a side-effect appends an undo to a LIFO
// `cleanups` slice; on later failure the chain runs in reverse order
// and any cleanup errors are joined with the primary error.
func (a *App) ForkThread(sourceThreadID string, atTurnIndex *int) (store.Thread, error) {
	// Hold the source thread's action lock for the duration of the fork so
	// concurrent SendMessage / RevertToMessageCheckpoint / etc. can't write to
	// items mid-clone (would produce a torn snapshot in the new fork).
	// Mirrors RevertToMessageCheckpoint's thread action lock.
	unlock := a.threadLocks().Lock(sourceThreadID)
	defer unlock()

	source, err := a.store.GetThread(sourceThreadID)
	if err != nil {
		return store.Thread{}, fmt.Errorf("fork thread: %w", err)
	}
	if err := a.ensureThreadCanFork(source, atTurnIndex); err != nil {
		return store.Thread{}, err
	}

	// Reject fork during an active turn on the source. The provider is
	// still writing to its session log (Claude JSONL) or in-memory state
	// (Codex), and forking the in-flight bytes produces a fork that
	// resumes mid-message. The popover already hides on
	// pane.activeTurn != null; this is defense-in-depth for script
	// callers and races. Mirrors RevertToMessageCheckpoint's check.
	if _, active, err := a.store.GetActiveTurn(sourceThreadID); err != nil {
		return store.Thread{}, fmt.Errorf("fork thread: active turn check: %w", err)
	} else if active {
		return store.Thread{}, fmt.Errorf("fork thread: cannot fork while a turn is in progress; interrupt or wait first")
	}

	fork := store.BuildForkedThread(source)

	var cleanups closer.Stack

	if err := a.store.CreateThread(fork); err != nil {
		return store.Thread{}, fmt.Errorf("fork thread: create fork thread: %w", err)
	}
	cleanups.Add(func() error { return a.cleanupForkThread(fork.ID) })

	if _, err := a.store.CloneThreadItems(source.ID, fork.ID, atTurnIndex); err != nil {
		return store.Thread{}, errors.Join(
			fmt.Errorf("fork thread: clone timeline: %w", err),
			cleanups.Run(),
		)
	}

	sessionRef, pendingForkRef, uuidMap, providerCleanup, err := a.resolveForkResumeState(source, atTurnIndex)
	if err != nil {
		return store.Thread{}, errors.Join(err, cleanups.Run())
	}
	if providerCleanup != nil {
		cleanups.Add(providerCleanup)
	}
	fork.SessionRef = sessionRef
	fork.PendingForkRef = pendingForkRef
	fork.UpdatedAt = time.Now().UnixMilli()
	if err := a.remapForkedClaudeUUIDs(fork.ID, uuidMap); err != nil {
		return store.Thread{}, errors.Join(err, cleanups.Run())
	}

	if err := a.store.UpdateThread(fork); err != nil {
		return store.Thread{}, errors.Join(
			fmt.Errorf("fork thread: persist fork state: %w", err),
			cleanups.Run(),
		)
	}

	return fork, nil
}

// ForkThreadFromMessage creates a fork whose conversation stops before the
// selected user message. This is the message-keyed counterpart to revert: the
// selected prompt is not copied into the fork.
func (a *App) ForkThreadFromMessage(sourceThreadID string, userItemID string) (store.Thread, error) {
	unlock := a.threadLocks().Lock(sourceThreadID)
	defer unlock()

	source, err := a.store.GetThread(sourceThreadID)
	if err != nil {
		return store.Thread{}, fmt.Errorf("fork thread from message: %w", err)
	}
	if _, active, err := a.store.GetActiveTurn(sourceThreadID); err != nil {
		return store.Thread{}, fmt.Errorf("fork thread from message: active turn check: %w", err)
	} else if active {
		return store.Thread{}, fmt.Errorf("fork thread from message: cannot fork while a turn is in progress; interrupt or wait first")
	}

	item, found, err := a.store.GetThreadItem(sourceThreadID, userItemID)
	if err != nil {
		return store.Thread{}, fmt.Errorf("fork thread from message: load user item: %w", err)
	}
	if !found || item.Kind != "user_text" || item.Role != "user" || checkpoint.IsWireOnlyUserItem(item) {
		return store.Thread{}, fmt.Errorf("fork thread from message: %q is not a user message", userItemID)
	}

	checkpointRow, ok, err := a.store.GetCheckpointByUserItemID(sourceThreadID, userItemID)
	if err != nil {
		return store.Thread{}, fmt.Errorf("fork thread from message: load checkpoint: %w", err)
	}
	if !ok {
		return store.Thread{}, fmt.Errorf("fork thread from message: no checkpoint for user message %q", userItemID)
	}

	fork := store.BuildForkedThread(source)
	if _, err := usermessage.FromItem(item); err != nil {
		return store.Thread{}, fmt.Errorf("fork thread from message: build prompt draft: %w", err)
	}
	promptDraftUpdatedAt := time.Now().UnixMilli()

	var cleanups closer.Stack

	if err := a.store.CreateThread(fork); err != nil {
		return store.Thread{}, fmt.Errorf("fork thread from message: create fork thread: %w", err)
	}
	cleanups.Add(func() error { return a.cleanupForkThread(fork.ID) })
	promptDraft, err := a.composerDraftFromUserItemWithClonedAttachments(fork.ID, item, promptDraftUpdatedAt)
	if err != nil {
		return store.Thread{}, errors.Join(
			fmt.Errorf("fork thread from message: build prompt draft: %w", err),
			cleanups.Run(),
		)
	}

	var lastKeptTurnPtr *int
	if item.TurnIndex > 0 {
		lastKeptTurn := item.TurnIndex - 1
		lastKeptTurnPtr = &lastKeptTurn
		if _, err := a.store.CloneThreadItems(source.ID, fork.ID, lastKeptTurnPtr); err != nil {
			return store.Thread{}, errors.Join(
				fmt.Errorf("fork thread from message: clone timeline: %w", err),
				cleanups.Run(),
			)
		}
	}

	sessionRef, pendingForkRef, uuidMap, providerCleanup, err := a.resolveMessageForkResumeState(source, checkpointRow)
	if err != nil {
		return store.Thread{}, errors.Join(err, cleanups.Run())
	}
	if providerCleanup != nil {
		cleanups.Add(providerCleanup)
	}
	fork.SessionRef = sessionRef
	fork.PendingForkRef = pendingForkRef
	fork.UpdatedAt = time.Now().UnixMilli()
	if err := a.remapForkedClaudeUUIDs(fork.ID, uuidMap); err != nil {
		return store.Thread{}, errors.Join(err, cleanups.Run())
	}

	if err := a.store.UpdateThread(fork); err != nil {
		return store.Thread{}, errors.Join(
			fmt.Errorf("fork thread from message: persist fork state: %w", err),
			cleanups.Run(),
		)
	}
	if err := a.store.UpsertThreadDraft(promptDraft); err != nil {
		return store.Thread{}, errors.Join(
			fmt.Errorf("fork thread from message: restore prompt draft: %w", err),
			cleanups.Run(),
		)
	}
	return fork, nil
}

// cleanupForkThread removes the fork row created by a failed fork. The
// FK CASCADE on items.thread_id, thread_drafts.thread_id,
// thread_checkpoints.thread_id, and attachments.thread_id handles cloned
// rows; DeleteThreadDir clears any attachment bytes already written for the
// fork. Returns nil on success OR when the row was already gone (ErrNoRows is
// treated as idempotent). Any other error is returned so the caller can
// errors.Join it with the primary fork error — swallowing cleanup failures
// lets orphan fork rows accumulate silently.
func (a *App) cleanupForkThread(threadID string) error {
	if threadID == "" {
		return nil
	}
	var errs []error
	if a.attachments != nil {
		if err := a.attachments.DeleteThreadDir(threadID); err != nil {
			errs = append(errs, fmt.Errorf("fork thread cleanup: delete attachment files for %s: %w", threadID, err))
		}
	}
	if err := a.store.DeleteThread(threadID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.Join(errs...)
		}
		errs = append(errs, fmt.Errorf("fork thread cleanup: delete fork %s: %w", threadID, err))
	}
	return errors.Join(errs...)
}

// ensureThreadCanFork rejects forks against threads that have no
// messages or where atTurnIndex points outside the existing turn range.
func (a *App) ensureThreadCanFork(source store.Thread, atTurnIndex *int) error {
	items, err := a.store.ListItems(source.ID)
	if err != nil {
		return fmt.Errorf("fork thread: list source items: %w", err)
	}
	if len(items) == 0 {
		return fmt.Errorf("fork thread: thread %q has no messages and cannot be forked", source.ID)
	}
	if atTurnIndex != nil {
		if *atTurnIndex < 0 {
			return fmt.Errorf("fork thread: atTurnIndex must be >= 0, got %d", *atTurnIndex)
		}
		lastTurn, err := a.store.LastTurnIndex(source.ID)
		if err != nil {
			return fmt.Errorf("fork thread: load source last turn index: %w", err)
		}
		if *atTurnIndex > lastTurn {
			return fmt.Errorf("fork thread: atTurnIndex %d exceeds source last turn %d", *atTurnIndex, lastTurn)
		}
	}
	return nil
}

// resolveForkResumeState wires the provider-specific resume reference for
// the new fork and returns an optional cleanup callback. The cleanup runs
// only if a later step in the fork sequence fails — it is responsible for
// any provider-side artifacts the fork created (e.g. a Claude JSONL slice
// on disk). Codex thread/fork already-spawned forks cannot be deleted via
// JSON-RPC; orphan rollouts are accepted there.
//
// uuidMap is the source-UUID → fork-UUID rewrite produced by the
// inline Claude JSONL slice (nil for Codex, nil for lazy fork-at-tail
// where the actual fork happens at `--fork-session` start time and we
// have no slice yet). When non-nil, the caller must call
// `remapForkedClaudeUUIDs(fork.ID, uuidMap)` so cloned items'
// `meta.provider_item_id` points at the fork's NEW UUIDs — keeping the
// "stored UUID matches active session JSONL" invariant intact for
// forks-of-forks.
func (a *App) resolveForkResumeState(source store.Thread, atTurnIndex *int) (
	sessionRef string,
	pendingForkRef string,
	uuidMap map[string]string,
	cleanup func() error,
	err error,
) {
	switch source.Provider {
	case string(provider.Codex):
		ref, err := a.forkCodexThread(source, atTurnIndex)
		if err != nil {
			return "", "", nil, nil, fmt.Errorf("fork thread: fork codex provider state: %w", err)
		}
		return ref, "", nil, nil, nil
	case string(provider.Claude):
		return a.forkClaudeThread(source, atTurnIndex)
	default:
		return "", "", nil, nil, fmt.Errorf("fork thread: unsupported provider %q", source.Provider)
	}
}

func (a *App) resolveMessageForkResumeState(source store.Thread, checkpointRow store.Checkpoint) (
	sessionRef string,
	pendingForkRef string,
	uuidMap map[string]string,
	cleanup func() error,
	err error,
) {
	switch source.Provider {
	case string(provider.Codex):
		if checkpointRow.TurnIndex == 0 {
			return "", "", nil, nil, nil
		}
		lastKeptTurn := checkpointRow.TurnIndex - 1
		ref, err := a.forkCodexThread(source, &lastKeptTurn)
		if err != nil {
			return "", "", nil, nil, fmt.Errorf("fork thread from message: fork codex provider state: %w", err)
		}
		return ref, "", nil, nil, nil
	case string(provider.Claude):
		return a.forkClaudeThreadBeforeMessage(source, checkpointRow)
	default:
		return "", "", nil, nil, fmt.Errorf("fork thread from message: unsupported provider %q", source.Provider)
	}
}

// forkCodexThread creates a new Codex thread that mirrors the source up
// to atTurnIndex (or the tail when atTurnIndex == nil). Issues
// thread/fork on the source's session, then thread/rollback against the
// returned fork ID over the same stdio session — verified by the spike
// at /tmp/spike-codex-fork that the rollback writes only to the FORK's
// rollout file, leaving the source byte-stable.
func (a *App) forkCodexThread(source store.Thread, atTurnIndex *int) (string, error) {
	const op = "fork codex thread"
	if source.SessionRef == "" {
		return "", fmt.Errorf("%s: source thread %q is missing a Codex thread reference", op, source.ID)
	}

	numRollback := 0
	if atTurnIndex != nil {
		lastTurn, err := a.store.LastTurnIndex(source.ID)
		if err != nil {
			return "", fmt.Errorf("%s: load last turn index: %w", op, err)
		}
		numRollback = lastTurn - *atTurnIndex
		if numRollback < 0 {
			return "", fmt.Errorf("%s: atTurnIndex %d > lastTurnIndex %d", op, *atTurnIndex, lastTurn)
		}
	}

	if activeSession, ok := a.activeCodexSession(source.ID); ok {
		forkedID, err := activeSession.Fork(context.Background())
		if err != nil {
			return "", fmt.Errorf("%s: %w", op, err)
		}
		if numRollback > 0 {
			result, err := activeSession.RollbackThread(context.Background(), forkedID, numRollback)
			if err != nil {
				return "", fmt.Errorf("%s: truncate fork: %w", op, err)
			}
			if err := a.validateCodexRollbackSurvivors(source.ID, op, result, *atTurnIndex+1); err != nil {
				return "", fmt.Errorf("%s: truncate fork: %w", op, err)
			}
		}
		return forkedID, nil
	}

	tempSession, err := codex.NewSession(context.Background(), source.ID, codex.Config{
		Binary:         a.providerBinaryPath(source.Provider),
		Model:          source.Model,
		WorkDir:        source.WorkspacePath,
		ResumeThreadID: source.SessionRef,
		EventLogger:    a.logger,
	}, func(provider.ProviderEvent) {})
	if err != nil {
		return "", fmt.Errorf("%s: resume source thread: %w", op, err)
	}
	defer tempSession.Close()

	forkedID, err := tempSession.Fork(context.Background())
	if err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	if numRollback > 0 {
		// Same stdio session — Codex's app-server routes thread/rollback
		// by threadId to the fork's rollout. See spike for verification.
		result, err := tempSession.RollbackThread(context.Background(), forkedID, numRollback)
		if err != nil {
			return "", fmt.Errorf("%s: truncate fork: %w", op, err)
		}
		if err := a.validateCodexRollbackSurvivors(source.ID, op, result, *atTurnIndex+1); err != nil {
			return "", fmt.Errorf("%s: truncate fork: %w", op, err)
		}
	}
	return forkedID, nil
}

// forkClaudeThread wires Claude's resume state for the new fork.
//
// Fork at tail (atTurnIndex == nil OR atTurnIndex >= lastTurn): use the
// existing "lazy fork" mechanism — stamp PendingForkRef =
// source.SessionRef, and the next session start passes --fork-session
// to the Claude CLI which forks from the source JSONL's tail at
// startup.
//
// Fork at point: slice the source JSONL ourselves (the official
// recipe — see internal/provider/claude/sessionfork). The new
// <newID>.jsonl on disk is a complete, resume-loadable session
// truncated through the END of atTurnIndex's turn (so the previous
// turn's full assistant response is preserved — slicing at the user
// prompt itself would leave Claude waiting to respond on resume,
// which is the wrong semantics). SessionRef points at the new ID
// directly — no --fork-session needed since the JSONL is already the
// fork.
func (a *App) forkClaudeThread(source store.Thread, atTurnIndex *int) (
	sessionRef string,
	pendingForkRef string,
	uuidMap map[string]string,
	cleanup func() error,
	err error,
) {
	if source.SessionRef == "" {
		return "", "", nil, nil, fmt.Errorf("fork thread: source thread %q is missing a Claude session reference", source.ID)
	}

	if atTurnIndex == nil {
		// Lazy fork-at-tail — startSession will pass --fork-session.
		// No inline slice happens here so there's nothing to remap;
		// the fork's --fork-session start will mint new UUIDs that the
		// AO row's stored provider_item_id never sees. A subsequent
		// revert in the fork falls back to the ordinal walk (now
		// synthetic-flag-safe) via the ErrMessageNotFound branch in
		// `writeRevertedClaudeSession`.
		return "", source.SessionRef, nil, nil, nil
	}

	lastTurn, err := a.store.LastTurnIndex(source.ID)
	if err != nil {
		return "", "", nil, nil, fmt.Errorf("fork thread: load last turn index: %w", err)
	}
	if *atTurnIndex >= lastTurn {
		// Forking at or past the last turn is equivalent to fork-at-tail.
		return "", source.SessionRef, nil, nil, nil
	}

	srcPath, err := sessionfork.LocateSessionFile(source.SessionRef, source.WorkspacePath)
	if err != nil {
		return "", "", nil, nil, fmt.Errorf("fork thread: locate claude session: %w", err)
	}

	// Prefer UUID-keyed slicing when the user_text at turn
	// `*atTurnIndex+1` carries a stamped wire UUID — the slice is then
	// immune to synthetic-entry ordinal drift (e.g. /compact). Falls
	// back to the ordinal walk for legacy rows that pre-date the
	// stamp.
	newID, newPath, uuidMap, err := a.writeForkedClaudeSession(srcPath, source.ID, *atTurnIndex)
	if err != nil {
		return "", "", nil, nil, fmt.Errorf("fork thread: write forked session: %w", err)
	}
	cleanup = func() error {
		// Best-effort: a missing file is OK (already cleaned up elsewhere).
		if err := os.Remove(newPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("fork thread cleanup: remove %s: %w", newPath, err)
		}
		return nil
	}
	return newID, "", uuidMap, cleanup, nil
}

func (a *App) forkClaudeThreadBeforeMessage(source store.Thread, checkpointRow store.Checkpoint) (
	sessionRef string,
	pendingForkRef string,
	uuidMap map[string]string,
	cleanup func() error,
	err error,
) {
	if checkpointRow.TurnIndex == 0 {
		return "", "", nil, nil, nil
	}
	sourceSessionRef := source.ResolvedSessionRef()
	if sourceSessionRef == "" {
		return "", "", nil, nil, fmt.Errorf("fork thread from message: source thread %q is missing a Claude session reference", source.ID)
	}
	srcPath, err := sessionfork.LocateSessionFile(sourceSessionRef, source.WorkspacePath)
	if err != nil {
		return "", "", nil, nil, fmt.Errorf("fork thread from message: locate claude session: %w", err)
	}
	newID, newPath, uuidMap, err := a.writeMessageForkedClaudeSession(srcPath, checkpointRow)
	if err != nil {
		return "", "", nil, nil, fmt.Errorf("fork thread from message: write forked session: %w", err)
	}
	cleanup = func() error {
		if err := os.Remove(newPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("fork thread from message cleanup: remove %s: %w", newPath, err)
		}
		return nil
	}
	return newID, "", uuidMap, cleanup, nil
}

// writeForkedClaudeSession is the turn-keyed-fork call into
// writeClaudeSessionSlice. The slice anchor is the user_text at
// turn `atTurnIndex+1` — that is the first turn dropped from the
// fork, so its parent is the end of the last kept turn.
func (a *App) writeForkedClaudeSession(srcPath, sourceThreadID string, atTurnIndex int) (string, string, map[string]string, error) {
	anchorUUID := a.lookupTurnAnchorClaudeUUID(sourceThreadID, atTurnIndex+1)
	logCtx := fmt.Sprintf("fork thread (turn %d)", atTurnIndex+1)
	return writeClaudeSessionSlice(srcPath, anchorUUID, atTurnIndex, logCtx)
}

// writeMessageForkedClaudeSession is the message-keyed-fork call
// into writeClaudeSessionSlice. The slice anchor is the reverted
// user message's wire UUID, lifted directly from the checkpoint row.
func (a *App) writeMessageForkedClaudeSession(srcPath string, checkpointRow store.Checkpoint) (string, string, map[string]string, error) {
	return writeClaudeSessionSlice(
		srcPath, checkpointRow.ProviderUserMessageID, checkpointRow.TurnIndex-1, "fork thread from message",
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
		if checkpoint.IsWireOnlyUserItem(it) {
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

// remapForkedClaudeUUIDs rewrites cloned items' `meta.provider_item_id` from
// their source-session UUIDs to the fork's NEW UUIDs. Maintains the invariant
// "stored UUID always matches the active session's JSONL" so future forks keep
// slicing from the fork's own transcript rather than chasing `forkedFrom`
// backpointers across session files.
//
// uuidMap may have entries beyond just user-message UUIDs (assistant /
// system entries also remap). Items only carry the user-message UUID; anything
// unmapped (legacy rows, mismatched ids) is left alone rather than blanking the
// column.
//
// Returns nil when the fork has no Claude-stamped items (Codex fork,
// lazy fork-at-tail, fork of a pre-stamp thread).
//
// Atomicity note: per-row UPDATEs run outside a single SQL transaction.
// This is safe in the fork pipeline because every caller wraps the
// remap in a `closer.Stack` whose rollback deletes the fork thread
// (and cascades to its items + checkpoints) on any error — so a
// mid-remap failure never leaves a partially-remapped fork visible to
// readers. The only window during which inconsistent rows exist is
// before the rollback runs, and no concurrent reader has a handle to
// a fork that hasn't returned from ForkThread* yet.
func (a *App) remapForkedClaudeUUIDs(forkThreadID string, uuidMap map[string]string) error {
	if len(uuidMap) == 0 {
		return nil
	}

	// 1. Cloned user_text items. Read all items, filter, remap meta.
	items, err := a.store.ListItems(forkThreadID)
	if err != nil {
		return fmt.Errorf("remap forked claude uuids: list fork items: %w", err)
	}
	for _, it := range items {
		if it.Kind != "user_text" || it.Role != "user" {
			continue
		}
		oldUUID := usermessage.ReadProviderItemID(it.Meta)
		if oldUUID == "" {
			continue
		}
		newUUID, ok := uuidMap[oldUUID]
		if !ok {
			continue
		}
		newMeta, err := usermessage.MergeProviderItemID(it.Meta, newUUID)
		if err != nil {
			return fmt.Errorf("remap forked claude uuids: merge item %s/%s meta: %w", forkThreadID, it.ID, err)
		}
		if newMeta == it.Meta {
			continue
		}
		if err := a.store.UpdateItemMeta(forkThreadID, it.ID, newMeta); err != nil {
			return fmt.Errorf("remap forked claude uuids: update item %s/%s meta: %w", forkThreadID, it.ID, err)
		}
	}

	return nil
}

func (a *App) activeCodexSession(threadID string) (*codex.Session, bool) {
	sess, ok := a.sessionManager().get(threadID)
	if !ok || sess.codex == nil {
		return nil, false
	}
	return sess.codex, true
}
