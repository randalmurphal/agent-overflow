package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

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
// is forked + truncated to match. Message-anchor rows intentionally stay
// behind with the source thread; the fork starts with none (rollback/fork
// helpers synthesize from item meta when a row is absent). atTurnIndex ==
// nil preserves the existing fork-at-tail behavior (clone everything,
// fork provider state at the latest message).
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
	// concurrent SendMessage / InterruptAndRevertIfClean / etc. can't write
	// to items mid-clone (would produce a torn snapshot in the new fork).
	// Mirrors the un-send path's thread action lock.
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
	// callers and races. Mirrors InterruptAndRevertIfClean's predicate.
	if _, active, err := a.store.GetActiveTurn(sourceThreadID); err != nil {
		return store.Thread{}, fmt.Errorf("fork thread: active turn check: %w", err)
	} else if active {
		return store.Thread{}, fmt.Errorf("fork thread: cannot fork while a turn is in progress; interrupt or wait first")
	}

	fork := store.BuildForkedThread(source)

	// The source lock alone leaves the FORK startable mid-build: a
	// client listing threads right after CreateThread commits can start
	// a session on the half-built fork, which the final UpdateThread
	// below would then clobber (round-7, R7-8). The id is freshly
	// minted, so taking its lock before the row exists is uncontended.
	unlockFork := a.threadLocks().Lock(fork.ID)
	defer unlockFork()

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
	if err := a.store.CloneThreadTurns(source.ID, fork.ID, atTurnIndex); err != nil {
		return store.Thread{}, errors.Join(
			fmt.Errorf("fork thread: clone turns: %w", err),
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
	if err := a.remapClaudeProviderIDs(fork.ID, uuidMap); err != nil {
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
	if !found || item.Kind != "user_text" || item.Role != "user" || store.IsWireOnlyUserItem(item) {
		return store.Thread{}, fmt.Errorf("fork thread from message: %q is not a user message", userItemID)
	}

	// The SQLite clone cuts at the item's position and the provider cut
	// derives from the anchor; resolveMessageAnchor guarantees the two
	// agree by synthesizing from the item row when the persisted anchor
	// is missing or its turn index drifted. Same contract as the un-send
	// path (InterruptAndRevertIfClean).
	anchor := a.resolveMessageAnchor("fork thread from message", sourceThreadID, item)

	fork := store.BuildForkedThread(source)
	if _, err := usermessage.FromItem(item); err != nil {
		return store.Thread{}, fmt.Errorf("fork thread from message: build prompt draft: %w", err)
	}
	promptDraftUpdatedAt := time.Now().UnixMilli()

	// Same mid-build startability guard as ForkThread (round-7, R7-8).
	unlockFork := a.threadLocks().Lock(fork.ID)
	defer unlockFork()

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

	// SQLite truncation granularity must match the provider's fork cut
	// (mirrors rollbackConversationLocked): Codex thread/fork cuts at a turn
	// boundary, so the clone drops the whole anchor turn; Claude's session
	// slice cuts at the message itself, so the clone keeps the anchor
	// turn's provider-order prefix (queued flush messages can share a turn
	// with the prompt that was running when they were enqueued).
	if source.Provider == string(provider.Codex) {
		if item.TurnIndex > 0 {
			lastKeptTurn := item.TurnIndex - 1
			if _, err := a.store.CloneThreadItems(source.ID, fork.ID, &lastKeptTurn); err != nil {
				return store.Thread{}, errors.Join(
					fmt.Errorf("fork thread from message: clone timeline: %w", err),
					cleanups.Run(),
				)
			}
			if err := a.store.CloneThreadTurns(source.ID, fork.ID, &lastKeptTurn); err != nil {
				return store.Thread{}, errors.Join(
					fmt.Errorf("fork thread from message: clone turns: %w", err),
					cleanups.Run(),
				)
			}
		}
	} else {
		if _, err := a.store.CloneThreadHistoryBeforeItem(source.ID, fork.ID, userItemID); err != nil {
			return store.Thread{}, errors.Join(
				fmt.Errorf("fork thread from message: clone timeline: %w", err),
				cleanups.Run(),
			)
		}
	}

	sessionRef, pendingForkRef, uuidMap, providerCleanup, err := a.resolveMessageForkResumeState(source, anchor, item)
	if err != nil {
		return store.Thread{}, errors.Join(err, cleanups.Run())
	}
	if providerCleanup != nil {
		cleanups.Add(providerCleanup)
	}
	fork.SessionRef = sessionRef
	fork.PendingForkRef = pendingForkRef
	fork.UpdatedAt = time.Now().UnixMilli()
	if err := a.remapClaudeProviderIDs(fork.ID, uuidMap); err != nil {
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
// message_anchors.thread_id, and attachments.thread_id handles cloned
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
// `remapClaudeProviderIDs(fork.ID, uuidMap)` so cloned items'
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

func (a *App) resolveMessageForkResumeState(source store.Thread, anchor store.MessageAnchor, anchorItem store.Item) (
	sessionRef string,
	pendingForkRef string,
	uuidMap map[string]string,
	cleanup func() error,
	err error,
) {
	switch source.Provider {
	case string(provider.Codex):
		// Codex forks are turn-granular (thread/fork cuts at a turn
		// boundary), so the anchor's intra-turn position is irrelevant:
		// the whole anchor turn is dropped, matching the turn-granular
		// SQLite clone.
		if anchor.TurnIndex == 0 {
			return "", "", nil, nil, nil
		}
		lastKeptTurn := anchor.TurnIndex - 1
		ref, err := a.forkCodexThread(source, &lastKeptTurn)
		if err != nil {
			return "", "", nil, nil, fmt.Errorf("fork thread from message: fork codex provider state: %w", err)
		}
		return ref, "", nil, nil, nil
	case string(provider.Claude):
		return a.forkClaudeThreadBeforeMessage(source, anchor, anchorItem)
	default:
		return "", "", nil, nil, fmt.Errorf("fork thread from message: unsupported provider %q", source.Provider)
	}
}

// forkCodexThread creates a new Codex thread that mirrors the source up
// to atTurnIndex (or the tail when atTurnIndex == nil). One
// `thread/fork` with the resolved lastTurnId anchor does the whole cut
// (Codex >= 0.143); the source thread is untouched. Returns "" (no
// error) when the kept prefix has no provider-backed turns at all —
// the fork then starts a fresh provider thread on its first send, the
// same contract as resolveMessageForkResumeState's turn-0 branch.
func (a *App) forkCodexThread(source store.Thread, atTurnIndex *int) (string, error) {
	const op = "fork codex thread"
	lastTurnID := ""
	if atTurnIndex != nil {
		anchor, found, err := a.resolveCodexForkAnchor(source.ID, *atTurnIndex)
		if err != nil {
			return "", fmt.Errorf("%s: %w", op, err)
		}
		if !found {
			log.Printf("%s: thread %s has no provider-backed turns at or before %d — fork starts a fresh provider thread", op, source.ID, *atTurnIndex)
			return "", nil
		}
		lastTurnID = anchor
	}
	// Required only once a provider fork is actually happening: an
	// anchored fork of a local-only prefix returned fresh above, so
	// reaching here with no thread reference is an inconsistent row
	// (tail fork of a never-connected thread, or provider-backed turns
	// on a thread that lost its ref).
	if source.SessionRef == "" {
		return "", fmt.Errorf("%s: source thread %q is missing a Codex thread reference", op, source.ID)
	}
	forkedID, err := a.forkCodexThreadAt(source, lastTurnID)
	if err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	return forkedID, nil
}

// resolveCodexForkAnchor picks the `thread/fork` lastTurnId for a cut
// that keeps turns <= lastKeptTurnIndex: the provider turn id of the
// LATEST provider-backed turn at or before that index. Walking down
// (instead of taking lastKeptTurnIndex verbatim) skips AO turn indexes
// that never became provider turns — failed sends that errored before
// the wire, and any turn whose row predates provider-id stamping.
//
// (found=false, err=nil) means the kept prefix holds no provider-backed
// turns at all; the caller starts a fresh provider thread. That answer
// is only trusted when the ITEMS agree — a prefix that carries
// provider-confirmed user messages but no provider turn ids is a
// legacy-data hole (a fork cloned before turns rows were copied), and
// silently discarding its provider history would be worse than failing.
func (a *App) resolveCodexForkAnchor(threadID string, lastKeptTurnIndex int) (string, bool, error) {
	for idx := lastKeptTurnIndex; idx >= 0; idx-- {
		turn, found, err := a.store.GetTurnByThreadIndex(threadID, idx)
		if err != nil {
			return "", false, fmt.Errorf("resolve codex fork anchor: %w", err)
		}
		if found && turn.ProviderTurnID != "" {
			return turn.ProviderTurnID, true, nil
		}
	}
	providerBacked, err := a.knownCodexProviderTurnCountBefore(threadID, lastKeptTurnIndex+1)
	if err != nil {
		return "", false, fmt.Errorf("resolve codex fork anchor: %w", err)
	}
	if providerBacked > 0 {
		return "", false, fmt.Errorf(
			"resolve codex fork anchor: thread %s has %d provider-backed turns at or before %d but no recorded provider turn id — likely a fork created before turn rows were cloned; fork the thread again from the desired message",
			threadID, providerBacked, lastKeptTurnIndex,
		)
	}
	return "", false, nil
}

// forkCodexThreadAt issues `thread/fork` (cut at lastTurnID, or full
// history when "") through the thread's live app-server session, or a
// throwaway resume session when none is active.
func (a *App) forkCodexThreadAt(source store.Thread, lastTurnID string) (string, error) {
	if activeSession, ok := a.activeCodexSession(source.ID); ok {
		return activeSession.ForkAt(context.Background(), lastTurnID)
	}

	tempSession, err := codex.NewSession(context.Background(), source.ID, codex.Config{
		Binary:         a.providerBinaryPath(source.Provider),
		Model:          source.Model,
		WorkDir:        source.WorkspacePath,
		ResumeThreadID: source.SessionRef,
		// Boot-mode overrides only, deliberately no `ao` credential: this is a
		// throwaway app-server used to issue one fork request, not a session an
		// agent takes a turn in.
		Env:         a.sessionProcessEnv(nil, aoSessionCredential{}),
		EventLogger: a.logger,
	}, func(provider.ProviderEvent) {})
	if err != nil {
		return "", fmt.Errorf("resume source thread: %w", err)
	}
	defer tempSession.Close()

	return tempSession.ForkAt(context.Background(), lastTurnID)
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

func (a *App) forkClaudeThreadBeforeMessage(source store.Thread, anchor store.MessageAnchor, anchorItem store.Item) (
	sessionRef string,
	pendingForkRef string,
	uuidMap map[string]string,
	cleanup func() error,
	err error,
) {
	midTurn, err := claudeMidTurnAnchor(anchorItem)
	if err != nil {
		return "", "", nil, nil, fmt.Errorf("fork thread from message: %w", err)
	}
	// Only an anchor that OPENS turn 0 keeps nothing — the fork then
	// starts a fresh provider session. A mid-turn-0 anchor (a message
	// queued during the very first turn) keeps that turn's prefix and
	// needs the session slice like any later anchor.
	if anchor.TurnIndex == 0 && !midTurn {
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
	newID, newPath, uuidMap, err := a.writeMessageForkedClaudeSession(srcPath, anchor, anchorItem, midTurn)
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

func (a *App) activeCodexSession(threadID string) (*codex.Session, bool) {
	sess, ok := a.sessionManager().get(threadID)
	if !ok || sess.codex == nil {
		return nil, false
	}
	return sess.codex, true
}
