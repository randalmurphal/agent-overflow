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
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

// ForkThread copies a source thread's timeline into a new fork and wires
// the provider-specific resume state. The whole sequence is atomic from
// the caller's point of view: if any step fails, the partially-created
// fork is torn down so no half-forked rows linger.
//
// When atTurnIndex is non-nil, the fork is sliced at that turn (0-indexed):
// items with turn_index > *atTurnIndex are dropped, the provider session
// is forked + truncated to match, and the fork's checkpoint baseline is
// captured at checkpointTurnCount = *atTurnIndex + 1. atTurnIndex == nil
// preserves the existing fork-at-tail behavior (clone everything, fork
// provider state at the latest message).
//
// The "atomic unit" is emulated in the app layer rather than a single
// SQLite transaction because the fork flow crosses a boundary — it has
// to talk to the Codex provider to fork a live session, can write a
// new Claude session JSONL on disk, and can capture a git ref. Wrapping
// the whole sequence in sql.Tx would hold a DB transaction open across
// a network-speed operation and break the rest of the store's
// single-connection model. Instead, we compose with a best-effort
// rollback: each step that has a side-effect appends an undo to a LIFO
// `cleanups` slice; on later failure the chain runs in reverse order
// and any cleanup errors are joined with the primary error.
func (a *App) ForkThread(sourceThreadID string, atTurnIndex *int) (store.Thread, error) {
	// Hold the source thread's send mutex for the duration of the fork so
	// concurrent SendMessage / RevertToCheckpoint / etc. can't write to
	// items mid-clone (would produce a torn snapshot in the new fork).
	// Mirrors RevertToCheckpoint's lockFor pattern.
	unlock := sendThreadMuRegistry.lockFor(sourceThreadID)
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
	// callers and races. Mirrors RevertToCheckpoint's check.
	if _, active, err := a.store.GetActiveTurn(sourceThreadID); err != nil {
		return store.Thread{}, fmt.Errorf("fork thread: active turn check: %w", err)
	} else if active {
		return store.Thread{}, fmt.Errorf("fork thread: cannot fork while a turn is in progress; interrupt or wait first")
	}

	fork := buildForkedThread(source)

	var cleanups []func() error
	runCleanups := func() error {
		var errs []error
		for i := len(cleanups) - 1; i >= 0; i-- {
			if err := cleanups[i](); err != nil {
				errs = append(errs, err)
			}
		}
		if len(errs) == 0 {
			return nil
		}
		return errors.Join(errs...)
	}

	if err := a.store.CreateThread(fork); err != nil {
		return store.Thread{}, fmt.Errorf("fork thread: create fork thread: %w", err)
	}
	cleanups = append(cleanups, func() error { return a.cleanupForkThread(fork.ID) })

	if err := a.store.CloneThreadItems(source.ID, fork.ID, atTurnIndex); err != nil {
		return store.Thread{}, errors.Join(
			fmt.Errorf("fork thread: clone timeline: %w", err),
			runCleanups(),
		)
	}

	sessionRef, pendingForkRef, providerCleanup, err := a.resolveForkResumeState(source, atTurnIndex)
	if err != nil {
		return store.Thread{}, errors.Join(err, runCleanups())
	}
	if providerCleanup != nil {
		cleanups = append(cleanups, providerCleanup)
	}
	fork.SessionRef = sessionRef
	fork.PendingForkRef = pendingForkRef
	fork.UpdatedAt = time.Now().UnixMilli()

	// Best-effort baseline capture so the fork has a checkpoint to revert
	// to from its very first new turn. Failure here is logged but does
	// not abort the fork — the next turn-boundary capture will recover.
	a.captureForkBaseline(fork, source, atTurnIndex)

	if err := a.store.UpdateThread(fork); err != nil {
		return store.Thread{}, errors.Join(
			fmt.Errorf("fork thread: persist fork state: %w", err),
			runCleanups(),
		)
	}

	return fork, nil
}

// cleanupForkThread removes the fork row created by a failed fork. The
// FK CASCADE on items.thread_id, thread_drafts.thread_id, and
// thread_checkpoints.thread_id handles the cloned timeline and any
// derived state. Returns nil on success OR when the row was already gone
// (ErrNoRows is treated as idempotent). Any other error is returned so
// the caller can errors.Join it with the primary fork error — swallowing
// cleanup failures lets orphan fork rows accumulate silently.
func (a *App) cleanupForkThread(threadID string) error {
	if threadID == "" {
		return nil
	}
	if err := a.store.DeleteThread(threadID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("fork thread cleanup: delete fork %s: %w", threadID, err)
	}
	return nil
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

func buildForkedThread(source store.Thread) store.Thread {
	now := time.Now().UnixMilli()
	return store.Thread{
		ID:                 uuid.NewString(),
		ProjectID:          source.ProjectID,
		Title:              source.Title + " (fork)",
		Provider:           source.Provider,
		WorkspacePath:      source.WorkspacePath,
		Model:              source.Model,
		WorktreePath:       source.WorktreePath,
		Branch:             source.Branch,
		Mode:               source.Mode,
		ReasoningEffort:    source.ReasoningEffort,
		FastMode:           source.FastMode,
		ContextWindow:      source.ContextWindow,
		RuntimeMode:        source.RuntimeMode,
		ForkedFromThreadID: source.ID,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
}

// resolveForkResumeState wires the provider-specific resume reference for
// the new fork and returns an optional cleanup callback. The cleanup runs
// only if a later step in the fork sequence fails — it is responsible for
// any provider-side artifacts the fork created (e.g. a Claude JSONL slice
// on disk). Codex thread/fork already-spawned forks cannot be deleted via
// JSON-RPC; orphan rollouts are accepted there.
func (a *App) resolveForkResumeState(source store.Thread, atTurnIndex *int) (
	sessionRef string,
	pendingForkRef string,
	cleanup func() error,
	err error,
) {
	switch source.Provider {
	case string(provider.Codex):
		ref, err := a.forkCodexThread(source, atTurnIndex)
		if err != nil {
			return "", "", nil, fmt.Errorf("fork thread: fork codex provider state: %w", err)
		}
		return ref, "", nil, nil
	case string(provider.Claude):
		return a.forkClaudeThread(source, atTurnIndex)
	default:
		return "", "", nil, fmt.Errorf("fork thread: unsupported provider %q", source.Provider)
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
			if err := activeSession.RollbackThread(context.Background(), forkedID, numRollback); err != nil {
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
		if err := tempSession.RollbackThread(context.Background(), forkedID, numRollback); err != nil {
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
	cleanup func() error,
	err error,
) {
	if source.SessionRef == "" {
		return "", "", nil, fmt.Errorf("fork thread: source thread %q is missing a Claude session reference", source.ID)
	}

	if atTurnIndex == nil {
		// Lazy fork-at-tail — startSession will pass --fork-session.
		return "", source.SessionRef, nil, nil
	}

	lastTurn, err := a.store.LastTurnIndex(source.ID)
	if err != nil {
		return "", "", nil, fmt.Errorf("fork thread: load last turn index: %w", err)
	}
	if *atTurnIndex >= lastTurn {
		// Forking at or past the last turn is equivalent to fork-at-tail.
		return "", source.SessionRef, nil, nil
	}

	srcPath, err := sessionfork.LocateSessionFile(source.SessionRef, source.WorkspacePath)
	if err != nil {
		return "", "", nil, fmt.Errorf("fork thread: locate claude session: %w", err)
	}

	// Single-pass: open the source JSONL once, parse + compute slice
	// point + write in one operation. Closes the double-open TOCTOU
	// window that a separate SliceUUIDForLastKeptTurn + WriteForkFile
	// would have. lastKept = atTurnIndex means we keep turns
	// 0..atTurnIndex inclusive.
	newID, newPath, err := sessionfork.WriteForkFileForLastKeptTurn(srcPath, *atTurnIndex, "")
	if err != nil {
		return "", "", nil, fmt.Errorf("fork thread: write forked session: %w", err)
	}
	cleanup = func() error {
		// Best-effort: a missing file is OK (already cleaned up elsewhere).
		if err := os.Remove(newPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("fork thread cleanup: remove %s: %w", newPath, err)
		}
		return nil
	}
	return newID, "", cleanup, nil
}

// captureForkBaseline writes a checkpoint baseline for the fork so the
// next-turn capture has a predecessor to diff against. checkpointTurnCount
// is set to (lastClonedTurnIndex + 1) — semantically "state right before
// the fork's first new turn", which matches how the rest of the
// checkpoint pipeline interprets the count (see
// internal/triage/turn_lifecycle.go captureBaselineForTurn).
//
// Best-effort. Logged on failure but never blocks the fork.
func (a *App) captureForkBaseline(fork, source store.Thread, atTurnIndex *int) {
	cap := a.checkpointStore()
	if cap == nil {
		return
	}
	workspace := checkpointWorkspaceForThread(fork)
	if workspace == "" {
		return
	}
	ctx := context.Background()
	if !cap.IsGitRepository(ctx, workspace) {
		return
	}

	turnCount := 0
	if atTurnIndex != nil {
		turnCount = *atTurnIndex + 1
	} else {
		// Fork at tail — turnCount = source's lastTurnIndex + 1, the
		// state after the last cloned turn.
		lastTurn, err := a.store.LastTurnIndex(source.ID)
		if err == nil && lastTurn >= 0 {
			turnCount = lastTurn + 1
		}
	}

	ref := checkpoint.RefForThreadTurn(fork.ID, turnCount)
	if err := cap.CaptureRef(ctx, workspace, ref); err != nil {
		log.Printf("fork: capture baseline ref for %s turn=%d: %v", fork.ID, turnCount, err)
		return
	}
	now := time.Now().UnixMilli()
	record := store.Checkpoint{
		ID:                  uuid.NewString(),
		ThreadID:            fork.ID,
		TurnIndex:           turnCount,
		CheckpointTurnCount: turnCount,
		RefName:             ref,
		Status:              "ready",
		CapturedAt:          now,
		WorkspacePath:       workspace,
	}
	if err := a.store.SaveCheckpoint(record); err != nil {
		_ = cap.DeleteRef(ctx, workspace, ref)
		log.Printf("fork: persist baseline checkpoint for %s turn=%d: %v", fork.ID, turnCount, err)
	}
}

func (a *App) activeCodexSession(threadID string) (*codex.Session, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	sess, ok := a.sessions[threadID]
	if !ok || sess.codex == nil {
		return nil, false
	}
	return sess.codex, true
}
