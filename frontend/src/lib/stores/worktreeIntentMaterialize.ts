import {
  AttachThreadWorktree,
  GitCreateBranchFrom,
  PrepareThreadWorktree,
} from './bindings';
import { syncThread } from './panes.svelte';
import { recordBranchSelection } from './branchMru';
import type { Thread } from '../types/models';
import {
  clearWorktreeIntent,
  resolveBaseForWire,
  type WorktreeIntent,
  worktreeIntentForThread,
} from './worktreeIntent.svelte';

export interface WorktreePrepareCallbacks {
  onWorktreePrepareStarted?: () => void;
  onWorktreePrepareFinished?: () => void;
}

interface PrepareThreadWorktreeIntentOptions extends WorktreePrepareCallbacks {
  thread: Thread;
}

interface MaterializeWorktreeIntentOnThreadOptions extends WorktreePrepareCallbacks {
  targetThread: Thread;
  intent: WorktreeIntent;
  clearIntentThreadId?: string;
  clearIntentOnSuccess?: boolean;
}

// Coalesces concurrent prepares per thread: the apply-now button and a
// send racing each other must not create the branch/worktree twice. The
// loser awaits the winner's result; the intent clears on success so a
// follow-up call is a no-op.
const prepareInFlight = new Map<string, Promise<Thread | null>>();

export async function prepareThreadWorktreeIntent(
  opts: PrepareThreadWorktreeIntentOptions,
): Promise<Thread | null> {
  const existing = prepareInFlight.get(opts.thread.id);
  if (existing) return existing;
  const run = materializeWorktreeIntentOnThread({
    targetThread: opts.thread,
    intent: worktreeIntentForThread(opts.thread),
    clearIntentThreadId: opts.thread.id,
    clearIntentOnSuccess: true,
    onWorktreePrepareStarted: opts.onWorktreePrepareStarted,
    onWorktreePrepareFinished: opts.onWorktreePrepareFinished,
  }).finally(() => {
    prepareInFlight.delete(opts.thread.id);
  });
  prepareInFlight.set(opts.thread.id, run);
  return run;
}

// The minimal pane surface applyWorktreeIntentNow needs; ThreadPane
// satisfies it structurally without this store importing the pane
// factory.
export interface PaneForIntentApply {
  readonly thread: Thread | null;
  ensureMaterializedThread(): Promise<string | null>;
}

/**
 * Materialize the pane's staged branch/worktree intent immediately —
 * the confirm-button path. Identical to what a send would do (draft
 * placeholders materialize their thread row first), minus the message.
 * Returns the updated thread, or null when there was nothing to apply.
 * Backend errors propagate to the caller for surfacing.
 */
export async function applyWorktreeIntentNow(pane: PaneForIntentApply): Promise<Thread | null> {
  const staged = worktreeIntentForThread(pane.thread);
  if (staged.mode !== 'new-worktree' && !staged.creatingBranch) return null;
  if (!(await pane.ensureMaterializedThread())) return null;
  const thread = pane.thread;
  if (!thread) return null;
  return prepareThreadWorktreeIntent({ thread });
}

export async function materializeWorktreeIntentOnThread(
  opts: MaterializeWorktreeIntentOnThreadOptions,
): Promise<Thread | null> {
  const intent = opts.intent;
  const needsWorkspaceWork = intent.mode === 'new-worktree' || intent.creatingBranch;
  if (!needsWorkspaceWork) return null;

  opts.onWorktreePrepareStarted?.();
  let updated: Thread | null = null;
  try {
    if (intent.mode === 'new-worktree' && intent.creatingBranch) {
      // New worktree off a brand-new branch. LOCAL sentinel resolves via
      // resolveBaseForWire to (currentBranch, carry=true) so the backend
      // stash-carry path engages.
      const wire = resolveBaseForWire(
        intent.newBranchBase,
        opts.targetThread.branch ?? '',
      );
      updated = (await PrepareThreadWorktree(
        opts.targetThread.id,
        wire.baseBranch,
        intent.newBranchName,
        wire.carryLocalChanges,
      )) as Thread;
    } else if (intent.mode === 'new-worktree' && !intent.creatingBranch) {
      // New worktree pointing at an existing branch. BranchPicker catches
      // already-checked-out branches on the normal path; if an inherited
      // intent still violates git's invariant, the binding error surfaces.
      const branch = intent.attachBranch || (opts.targetThread.branch ?? '');
      updated = (await AttachThreadWorktree(opts.targetThread.id, branch)) as Thread;
    } else if (intent.mode === 'local' && intent.creatingBranch) {
      // Stay in the current workspace, create a new branch off the picked
      // base, and check it out.
      const wire = resolveBaseForWire(
        intent.newBranchBase,
        opts.targetThread.branch ?? '',
      );
      updated = (await GitCreateBranchFrom(
        opts.targetThread.id,
        intent.newBranchName,
        wire.baseBranch,
        wire.carryLocalChanges,
      )) as Thread;
    }
  } finally {
    opts.onWorktreePrepareFinished?.();
  }

  if (updated) {
    syncThread(updated);
    // The materialized branch is now the user's working branch — surface
    // it at the top of the picker the same as an explicit checkout.
    const selectedBranch = intent.creatingBranch ? intent.newBranchName : intent.attachBranch;
    if (updated.projectId && selectedBranch) {
      recordBranchSelection(updated.projectId, selectedBranch);
    }
  }
  if (updated && opts.clearIntentOnSuccess) {
    clearWorktreeIntent(opts.clearIntentThreadId ?? opts.targetThread.id);
  }
  return updated;
}
