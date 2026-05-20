import {
  AttachThreadWorktree,
  GitCreateBranchFrom,
  PrepareThreadWorktree,
} from './bindings';
import { syncThread } from './panes.svelte';
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

export async function prepareThreadWorktreeIntent(
  opts: PrepareThreadWorktreeIntentOptions,
): Promise<Thread | null> {
  return materializeWorktreeIntentOnThread({
    targetThread: opts.thread,
    intent: worktreeIntentForThread(opts.thread),
    clearIntentThreadId: opts.thread.id,
    clearIntentOnSuccess: true,
    onWorktreePrepareStarted: opts.onWorktreePrepareStarted,
    onWorktreePrepareFinished: opts.onWorktreePrepareFinished,
  });
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
  }
  if (updated && opts.clearIntentOnSuccess) {
    clearWorktreeIntent(opts.clearIntentThreadId ?? opts.targetThread.id);
  }
  return updated;
}
