package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// deleteThreadTree removes a thread and everything owned by it: children
// first (recursively), then in-process state (session, terminals,
// deliberations, system prompt), then on-disk state (checkpoint git refs,
// attachment files), then the DB row itself.
//
// All cleanup steps are idempotent. If any step fails, we collect the
// error and continue so the work we CAN do still happens, then aggregate
// everything with errors.Join at the end. The DB row is only deleted if
// every preceding step ran without error — this keeps the invariant
// "parent row in DB <=> per-thread resources still claimable", and
// subsequent DeleteThread calls can resume from where this one stopped
// because the preserved row makes each retry idempotent.
//
// We do NOT wrap the whole operation in a single sql.Tx: cleanup touches
// the in-process session map, a live terminal process, the git ref
// store, and the attachment filesystem. A long DB transaction across
// those boundaries would serialise the rest of the store while git
// refs are deleted, which pushes user-visible latency well into the
// hundreds of ms. The idempotent-retry model gives us atomicity at the
// resource-ownership level without that cost.
func (a *App) deleteThreadTree(threadID string) error {
	thread, threadErr := a.store.GetThread(threadID)
	if threadErr != nil && !errors.Is(threadErr, sql.ErrNoRows) {
		return fmt.Errorf("delete thread %s: lookup: %w", threadID, threadErr)
	}
	threadFound := threadErr == nil

	var errs []error

	// Children first so a partial failure here doesn't strand the parent.
	// Child errors are recorded but don't short-circuit: we still try to
	// tear down our own resources before propagating.
	children, err := a.store.ListChildThreads(threadID)
	if err != nil {
		return fmt.Errorf("delete thread %s: list children: %w", threadID, err)
	}
	for _, child := range children {
		if err := a.deleteThreadTree(child.ID); err != nil {
			errs = append(errs, fmt.Errorf("delete child %s: %w", child.ID, err))
		}
	}

	// Clean Codex background terminals BEFORE stopping the session. The
	// `thread/backgroundTerminals/clean` RPC needs a live JSON-RPC
	// transport — once stopSession closes the subprocess, the wire is
	// gone and any PTYs Codex was managing leak (the child processes
	// linger until the OS eventually reaps them). Best-effort: if the
	// RPC fails we log and carry on so the delete still completes. User
	// intent on delete is "this thread is done" — a stuck RPC must not
	// block that.
	//
	// Threads with no active Codex session are a no-op: without a
	// transport to Codex we have no way to issue the clean RPC. Any PTYs
	// left by a previously-closed session were already killed when that
	// subprocess exited; deletion of a dormant thread only needs to drop
	// the store rows.
	//
	// Claude threads skip this entirely: their per-task stop primitive
	// is `stop_task`, which is a turn-scoped control, not a teardown
	// primitive. Deleting a Claude thread drops its subprocess via
	// stopSession; the kernel reaps any remaining child processes the
	// CLI spawned.
	if threadFound && thread.Provider == string(provider.Codex) {
		if _, hasSession := a.activeCodexSession(threadID); hasSession {
			if err := a.CleanCodexBackgroundTerminals(threadID); err != nil {
				// Not added to errs: the delete intent is terminal, and
				// the clean RPC is advisory. Log so we can diagnose PTY
				// leaks without blocking user-visible delete flow.
				log.Printf("delete thread %s: clean codex background terminals: %v", threadID, err)
			}
		}
	}

	if err := a.stopSession(threadID); err != nil {
		errs = append(errs, fmt.Errorf("stop session: %w", err))
	}
	if a.terminals != nil {
		if err := a.terminals.CloseThread(threadID); err != nil {
			errs = append(errs, fmt.Errorf("close terminals: %w", err))
		}
	}
	a.clearThreadSystemPrompt(threadID)
	a.removeDeliberation(thread)
	if err := a.cleanupThreadCheckpoints(threadID, thread, threadFound); err != nil {
		errs = append(errs, fmt.Errorf("cleanup checkpoints: %w", err))
	}
	if err := a.cleanupThreadAttachmentFiles(threadID); err != nil {
		errs = append(errs, fmt.Errorf("cleanup attachments: %w", err))
	}
	if err := a.cleanupThreadDesignWorkdir(threadID); err != nil {
		errs = append(errs, fmt.Errorf("cleanup design workdir: %w", err))
	}
	if a.replay != nil {
		if err := a.replay.RemoveThreadLog(threadID); err != nil {
			errs = append(errs, fmt.Errorf("cleanup replay log: %w", err))
		}
	}

	// If ANY step above errored, skip the DB row delete so the next
	// DeleteThread call can reconcile from a known state. Without this,
	// a DB-row-gone + resources-still-present state would be unreachable
	// for retry (we'd have no thread to enumerate children from).
	if len(errs) > 0 {
		return fmt.Errorf("delete thread %s: %w", threadID, errors.Join(errs...))
	}

	// Thread was already gone (e.g. partially deleted earlier). Children
	// and per-thread resources have been cleaned up above; there is
	// nothing left to delete from the store.
	if !threadFound {
		return nil
	}

	if err := a.store.DeleteThread(threadID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Raced with another DeleteThread; treat as success.
			return nil
		}
		return fmt.Errorf("delete thread %s: drop row: %w", threadID, err)
	}
	// Drop the per-thread sendMessage mutex so a long-lived process
	// doesn't accumulate dead mutexes for deleted threads. Safe after
	// the DB row is gone — no new sendMessage can arrive for this
	// thread.
	sendThreadMuRegistry.ForgetThread(threadID)
	return nil
}

// cleanupThreadDesignWorkdir tears down any active design watcher and
// removes the on-disk per-thread design directory (main/, options/,
// snapshots/). The design_snapshots SQLite rows cascade via FK on the
// thread row delete; this sweeps the bytes. Returns nil when the
// design subsystem isn't wired (tests, threads created before design
// mode existed) or the directory was already gone.
func (a *App) cleanupThreadDesignWorkdir(threadID string) error {
	a.teardownDesignThread(threadID)
	if a.designWorkdir == nil {
		return nil
	}
	if err := a.designWorkdir.Wipe(threadID); err != nil {
		return fmt.Errorf("wipe design workdir: %w", err)
	}
	return nil
}

// cleanupThreadAttachmentFiles removes the on-disk attachment directory
// for a thread. The SQLite FK cascade handles the metadata rows; this
// sweeps the bytes. Returns nil when there are no attachments configured
// or the directory was already gone.
func (a *App) cleanupThreadAttachmentFiles(threadID string) error {
	if a.attachments == nil {
		return nil
	}
	if err := a.attachments.DeleteThreadDir(threadID); err != nil {
		return fmt.Errorf("remove attachment files for %s: %w", threadID, err)
	}
	return nil
}

// cleanupThreadCheckpoints removes both the Git refs and the SQLite
// bookkeeping rows for a thread's checkpoints. Each step is idempotent:
// CleanupThread tolerates missing refs, DeleteCheckpointsForThread
// tolerates a missing thread row. Errors are aggregated so a failure on
// one side doesn't hide a failure on the other. Threads whose workspace
// is not a git repo (tests, newly-created threads before any capture)
// skip the ref cleanup — the ref namespace is guaranteed empty there.
func (a *App) cleanupThreadCheckpoints(threadID string, thread store.Thread, threadFound bool) error {
	var errs []error
	if threadFound {
		workspace := checkpointWorkspaceForThread(thread)
		if workspace != "" {
			ctx := context.Background()
			cp := a.checkpointStore()
			if cp != nil && cp.IsGitRepository(ctx, workspace) {
				if err := cp.CleanupThread(ctx, workspace, threadID); err != nil {
					errs = append(errs, fmt.Errorf("cleanup checkpoint refs: %w", err))
				}
			}
		}
	}
	if a.store != nil {
		if err := a.store.DeleteCheckpointsForThread(threadID); err != nil {
			errs = append(errs, fmt.Errorf("drop checkpoint rows: %w", err))
		}
	}
	return errors.Join(errs...)
}

func (a *App) removeDeliberation(thread store.Thread) {
	if thread.DiscussionID == "" || thread.ParentThreadID != "" {
		return
	}

	a.removeDeliberationByID(thread.DiscussionID)
}

func (a *App) removeDeliberationByID(channelID string) {
	if channelID == "" {
		return
	}
	a.mu.Lock()
	delete(a.deliberations, channelID)
	a.mu.Unlock()
}
