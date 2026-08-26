// Applying a staged branch/worktree intent.
//
// Every workspace mutation belongs to a persisted thread. Selecting a
// worktree materializes a draft before this module runs, and the defensive
// ensure below makes that invariant hold for non-UI callers too. The backend's
// thread-scoped operations then create and bind the checkout in one call, so
// workspace-keyed status, MR lookup, terminals, and setup state all observe
// the same owner immediately.

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
  isStagedWorktreeIntent,
  markWorktreeIntentApplying,
  resolveBaseForWire,
  type WorktreeIntent,
  worktreeIntentForThread,
} from './worktreeIntent.svelte';

export interface AppliedWorkspace {
  worktreePath: string;
  branch: string;
}

export interface WorktreePrepareCallbacks {
  onWorktreePrepareStarted?: () => void;
  onWorktreePrepareFinished?: () => void;
}

export interface PaneForIntentApply {
  readonly thread: Thread | null;
  readonly hasDraftPlaceholder: boolean;
  ensureMaterializedThread(): Promise<string | null>;
}

interface PrepareThreadWorktreeIntentOptions extends WorktreePrepareCallbacks {
  pane: PaneForIntentApply;
}

interface MaterializeWorktreeIntentOnThreadOptions extends WorktreePrepareCallbacks {
  targetThread: Thread;
  intent: WorktreeIntent;
  clearIntentThreadId?: string;
  clearIntentOnSuccess?: boolean;
}

// One mutation per row. Confirm and send can race, and a double worktree cut
// is not idempotent. Every caller awaits the same promise while its own UI
// callback bracket remains balanced.
const inFlight = new Map<string, Promise<Thread | null>>();

async function withCallbacks<T>(
  callbacks: WorktreePrepareCallbacks,
  run: () => Promise<T>,
): Promise<T> {
  callbacks.onWorktreePrepareStarted?.();
  try {
    return await run();
  } finally {
    callbacks.onWorktreePrepareFinished?.();
  }
}

/** Apply the pane's staged choice now and return its bound workspace. */
export function applyWorktreeIntentNow(
  pane: PaneForIntentApply,
): Promise<AppliedWorkspace | null> {
  return runPaneIntent(pane);
}

/** Send-path counterpart. The workspace is already bound when this returns. */
export async function prepareThreadWorktreeIntent(
  opts: PrepareThreadWorktreeIntentOptions,
): Promise<AppliedWorkspace | null> {
  return withCallbacks(opts, () => runPaneIntent(opts.pane));
}

function runPaneIntent(pane: PaneForIntentApply): Promise<AppliedWorkspace | null> {
  const apply = async (thread: Thread | null): Promise<AppliedWorkspace | null> => {
    if (!thread) return null;
    const updated = await applyIntentToThread({
      targetThread: thread,
      intent: worktreeIntentForThread(thread),
      clearIntentOnSuccess: true,
    });
    if (!updated) return null;
    return { worktreePath: updated.worktreePath ?? '', branch: updated.branch ?? '' };
  };

  if (!pane.thread) return Promise.resolve(null);
  if (!pane.hasDraftPlaceholder) return apply(pane.thread);
  return pane.ensureMaterializedThread().then((threadId) =>
    apply(threadId && pane.thread?.id === threadId ? pane.thread : null),
  );
}

/** Apply an explicit intent to an already-persisted target thread. */
export async function materializeWorktreeIntentOnThread(
  opts: MaterializeWorktreeIntentOnThreadOptions,
): Promise<Thread | null> {
  return withCallbacks(opts, () => applyIntentToThread(opts));
}

function applyIntentToThread(
  opts: MaterializeWorktreeIntentOnThreadOptions,
): Promise<Thread | null> {
  if (!isStagedWorktreeIntent(opts.intent)) return Promise.resolve(null);

  const threadId = opts.targetThread.id;
  const existing = inFlight.get(threadId);
  if (existing) return existing;

  markWorktreeIntentApplying(threadId, true);
  const run = runThreadMutation(opts).finally(() => {
    markWorktreeIntentApplying(threadId, false);
    inFlight.delete(threadId);
  });
  inFlight.set(threadId, run);
  return run;
}

async function runThreadMutation(
  opts: MaterializeWorktreeIntentOnThreadOptions,
): Promise<Thread | null> {
  const { targetThread, intent } = opts;
  let updated: Thread | null = null;
  if (intent.mode === 'new-worktree' && intent.creatingBranch) {
    const wire = resolveBaseForWire(intent.newBranchBase, targetThread.branch ?? '');
    updated = (await PrepareThreadWorktree(
      targetThread.id,
      wire.baseBranch,
      intent.newBranchName,
      wire.carryLocalChanges,
    )) as Thread;
  } else if (intent.mode === 'new-worktree') {
    updated = (await AttachThreadWorktree(
      targetThread.id,
      intent.attachBranch || (targetThread.branch ?? ''),
    )) as Thread;
  } else if (intent.creatingBranch) {
    const wire = resolveBaseForWire(intent.newBranchBase, targetThread.branch ?? '');
    updated = (await GitCreateBranchFrom(
      targetThread.id,
      intent.newBranchName,
      wire.baseBranch,
      wire.carryLocalChanges,
    )) as Thread;
  }
  if (!updated) return null;

  syncThread(updated);
  const selectedBranch = intent.creatingBranch ? intent.newBranchName : intent.attachBranch;
  if (updated.projectId && selectedBranch) {
    recordBranchSelection(updated.projectId, selectedBranch);
  }
  if (opts.clearIntentOnSuccess) {
    clearWorktreeIntent(opts.clearIntentThreadId ?? targetThread.id);
  }
  return updated;
}

/** Test isolation only. */
export function resetWorktreeIntentMaterializeForTest(): void {
  inFlight.clear();
}
