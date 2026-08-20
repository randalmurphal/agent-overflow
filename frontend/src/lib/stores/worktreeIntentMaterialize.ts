// Applying a staged branch/worktree intent.
//
// WHICH ENGINE RUNS IS A QUESTION ABOUT THE ROW, not about the caller. Two
// exist and they are not interchangeable:
//
//   - PROJECT-scoped (`PrepareProjectWorktree`, `AttachProjectWorktree`,
//     `CreateProjectBranch`) needs no thread. It is what a DRAFT uses — a
//     placeholder, which has no row at all, and a materialized-but-item-less
//     draft row, which has one the empty-draft cleanup may still delete. Going
//     thread-scoped there is what used to force a materialization the cleanup
//     then raced ("sql: no rows in result set"). The pane's current workspace
//     rides along as `sourceWorkspace`: a draft can already be parked in a
//     worktree, and the carry stash / branch checkout belong in THAT checkout,
//     not in the project root.
//   - THREAD-scoped (`PrepareThreadWorktree`, `AttachThreadWorktree`,
//     `GitCreateBranchFrom`) is what a thread WITH HISTORY uses, and what the
//     plan-implementation flow uses on the child row it just created. It moves
//     the row as part of the operation, which is the semantics a real thread
//     wants: there is no window in which the pane and the row disagree.
//
// BIND is the extra step the project-scoped path needs, and it happens at SEND
// time, never at apply time:
//   - a placeholder is stamped, so `CreateThread` carries worktreePath /
//     workspaceOverride / branch and the backend adopts the unbound setup run;
//   - a materialized draft row takes one `UpdateThreadWorkspace`.
// Deferring it is deliberate: an emptied draft whose worktree was cut must
// still dematerialize, and a row already moved into that worktree could not.
// Between apply and bind the intent's `applied` field is the truth, read
// through `effective{WorkspacePath,Branch}ForThread`. `applied` is therefore
// never set for a non-draft row — the thread-scoped engine has already moved
// it, so there is nothing pending to describe.

import {
  AttachProjectWorktree,
  AttachThreadWorktree,
  CreateProjectBranch,
  GitCreateBranchFrom,
  PrepareProjectWorktree,
  PrepareThreadWorktree,
  UpdateThreadWorkspace,
} from './bindings';
import { syncThread } from './panes.svelte';
import { recordBranchSelection } from './branchMru';
import type { Thread } from '../types/models';
import { sameNormalizedPath } from '../utils/path';
import {
  type AppliedWorkspace,
  clearWorktreeIntent,
  isStagedWorktreeIntent,
  markWorktreeIntentApplied,
  markWorktreeIntentApplying,
  onWorktreeIntentMigrated,
  resolveBaseForWire,
  type WorktreeIntent,
  worktreeIntentForThread,
} from './worktreeIntent.svelte';

export type { AppliedWorkspace };

export interface WorktreePrepareCallbacks {
  onWorktreePrepareStarted?: () => void;
  onWorktreePrepareFinished?: () => void;
}

// The minimal pane surface the apply/bind paths need; ThreadPane satisfies
// it structurally without this store importing the pane factory.
export interface PaneForIntentApply {
  readonly thread: Thread | null;
  readonly hasDraftPlaceholder: boolean;
  applyDraftPlaceholderWorkspace(workspace: {
    workspacePath: string;
    worktreePath?: string;
    branch?: string;
  }): boolean;
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

// Coalesces concurrent APPLIES per thread: the confirm button and a send
// racing each other must not cut two worktrees. The loser awaits the
// winner's result; once it settles the intent carries `applied`, so a later
// call short-circuits without touching the wire at all.
const applyInFlight = new Map<string, Promise<AppliedWorkspace | null>>();

// Same for the send path's apply+bind pair. Separate from the map above
// because the confirm button deliberately does NOT bind — it only creates
// the workspace — so the two operations cannot share a promise.
const prepareInFlight = new Map<string, Promise<AppliedWorkspace | null>>();

// Nesting depth of the applying bracket, per thread. runPrepare brackets the
// whole apply+bind and applyStagedIntent brackets the apply inside it, so a
// plain boolean would clear the flag when the INNER one finished — with the
// bind still to come, and the empty-draft cleanup free to delete the row
// underneath it.
const applyingDepth = new Map<string, number>();

/**
 * Every id-keyed map in this module follows a re-key, because an apply
 * routinely outlives one: typing into a placeholder mid-apply materializes the
 * row (`migrateWorktreeIntent(placeholderId, createdId)`), and the empty-draft
 * cleanup migrates back the other way. A promise left under the dead id is
 * invisible to the next send, which then cuts the same branch a second time
 * and fails on it forever.
 */
onWorktreeIntentMigrated((fromThreadId, toThreadId) => {
  rekey(applyInFlight, fromThreadId, toThreadId);
  rekey(prepareInFlight, fromThreadId, toThreadId);
  rekey(applyingDepth, fromThreadId, toThreadId);
});

function rekey<T>(map: Map<string, T>, from: string, to: string): void {
  const value = map.get(from);
  if (value === undefined) return;
  map.delete(from);
  map.set(to, value);
}

function beginApplying(threadId: string): void {
  if (!threadId) return;
  const depth = (applyingDepth.get(threadId) ?? 0) + 1;
  applyingDepth.set(threadId, depth);
  if (depth === 1) markWorktreeIntentApplying(threadId, true);
}

function endApplying(threadId: string): void {
  if (!threadId) return;
  const depth = (applyingDepth.get(threadId) ?? 1) - 1;
  if (depth > 0) {
    applyingDepth.set(threadId, depth);
    return;
  }
  applyingDepth.delete(threadId);
  markWorktreeIntentApplying(threadId, false);
}

/**
 * Does this pane's intent apply against the PROJECT rather than against the
 * row? True for a draft in either of its two shapes — a placeholder with no
 * row, and a materialized row the empty-draft cleanup can still delete.
 */
function isDraftOwner(pane: PaneForIntentApply, thread: Thread): boolean {
  return pane.hasDraftPlaceholder || thread.isDraft === true;
}

/**
 * Materialize the pane's staged branch/worktree intent immediately — the
 * confirm-button path. A draft creates the branch/worktree against the PROJECT
 * and records it on the intent, leaving the row (if there is one) where it is
 * until a send binds it; a thread with history runs the thread-scoped engine,
 * which moves the row as part of the operation.
 *
 * Returns what was created, or null when there was nothing staged. Backend
 * errors propagate to the caller for surfacing.
 */
export async function applyWorktreeIntentNow(
  pane: PaneForIntentApply,
): Promise<AppliedWorkspace | null> {
  const thread = pane.thread;
  if (!thread) return null;
  const staged = worktreeIntentForThread(thread);
  if (!isStagedWorktreeIntent(staged)) return null;
  if (staged.applied) return staged.applied;
  return applyStagedIntent(pane, thread, staged);
}

/**
 * The one door into either engine, and the one place the ownership question is
 * answered. Both routes share `applyInFlight`, so the confirm button and a send
 * racing each other cut one worktree whichever engine is in play.
 */
function applyStagedIntent(
  pane: PaneForIntentApply,
  thread: Thread,
  intent: WorktreeIntent,
): Promise<AppliedWorkspace | null> {
  const existing = applyInFlight.get(thread.id);
  if (existing) return existing;
  // A placeholder has no row for the empty-draft cleanup to delete and
  // deliberately stays out of the applying set. Anything else is a real row,
  // and the bracket has to be up before the first await — including on the
  // confirm-button path, which reaches here without runPrepare's bracket.
  const rowId = pane.hasDraftPlaceholder ? '' : thread.id;
  beginApplying(rowId);
  const engine = isDraftOwner(pane, thread)
    ? runApply(pane, thread, intent)
    : runThreadScopedApply(thread, intent);
  const run = engine.finally(() => {
    endApplying(rowId);
    applyInFlight.delete(thread.id);
  });
  applyInFlight.set(thread.id, run);
  return run;
}

/**
 * A thread with history. The thread-scoped engine creates AND moves the row in
 * one call, so nothing is left pending: the intent is cleared, `applied` is
 * never set, and the returned workspace exists only so the confirm button can
 * name what it made.
 */
async function runThreadScopedApply(
  thread: Thread,
  intent: WorktreeIntent,
): Promise<AppliedWorkspace | null> {
  const updated = await materializeWorktreeIntentOnThread({
    targetThread: thread,
    intent,
    clearIntentOnSuccess: true,
  });
  if (!updated) return null;
  return { worktreePath: updated.worktreePath ?? '', branch: updated.branch ?? '' };
}

async function runApply(
  pane: PaneForIntentApply,
  thread: Thread,
  intent: WorktreeIntent,
): Promise<AppliedWorkspace | null> {
  const projectId = thread.projectId;
  if (!projectId) throw new Error('cannot create a workspace: the thread has no project');

  // Where the source-side git runs: the stash a carry pushes, the checkout a
  // branch create moves. A draft parked in a worktree must not have either of
  // those happen in the project root.
  const sourceWorkspace = thread.workspacePath ?? '';

  let applied: AppliedWorkspace | null = null;
  if (intent.mode === 'new-worktree' && intent.creatingBranch) {
    // New worktree off a brand-new branch. The LOCAL sentinel resolves via
    // resolveBaseForWire to (currentBranch, carry=true) so the backend's
    // stash-carry path engages.
    const wire = resolveBaseForWire(intent.newBranchBase, thread.branch ?? '');
    applied = normalize(
      await PrepareProjectWorktree(
        projectId,
        wire.baseBranch,
        intent.newBranchName,
        wire.carryLocalChanges,
        sourceWorkspace,
      ),
    );
  } else if (intent.mode === 'new-worktree') {
    // New worktree pointing at an existing branch. BranchPicker catches
    // already-checked-out branches on the normal path; if an inherited
    // intent still violates git's invariant, the binding error surfaces.
    applied = normalize(
      await AttachProjectWorktree(projectId, intent.attachBranch || (thread.branch ?? '')),
    );
  } else if (intent.creatingBranch) {
    // Stay put, create the branch off the picked base and check it out.
    // No worktree comes back — the workspace did not move.
    const wire = resolveBaseForWire(intent.newBranchBase, thread.branch ?? '');
    applied = normalize(
      await CreateProjectBranch(
        projectId,
        intent.newBranchName,
        wire.baseBranch,
        wire.carryLocalChanges,
        sourceWorkspace,
      ),
    );
  }
  if (!applied) return null;

  // Resolve the intent's key at COMPLETION, not at launch. Typing into a
  // placeholder while this RPC was in flight materializes the row and re-keys
  // the intent onto the new id; writing to the id we started with would strand
  // the worktree — invisible to the pane, and re-cut on the next send.
  markWorktreeIntentApplied(pane.thread?.id ?? thread.id, applied);
  // The materialized branch is now the user's working branch — surface it at
  // the top of the picker the same as an explicit checkout.
  const selectedBranch = applied.branch
    || (intent.creatingBranch ? intent.newBranchName : intent.attachBranch);
  if (selectedBranch) recordBranchSelection(projectId, selectedBranch);

  // A placeholder is synthetic — nothing syncs over it — so stamping it is
  // how the whole pane (workspace strip, git status attach, terminal cwd)
  // follows the applied workspace, and it is what makes CreateThread carry
  // the choice at send time. A real row is left alone: backend syncs would
  // fight the stamp, and its consumers read the effective* helpers instead.
  if (pane.hasDraftPlaceholder && pane.thread?.id === thread.id) {
    pane.applyDraftPlaceholderWorkspace(
      applied.worktreePath
        ? {
            workspacePath: applied.worktreePath,
            worktreePath: applied.worktreePath,
            branch: applied.branch,
          }
        : {
            workspacePath: thread.workspacePath,
            worktreePath: thread.worktreePath ?? '',
            branch: applied.branch || (thread.branch ?? ''),
          },
    );
  }
  return applied;
}

function normalize(result: unknown): AppliedWorkspace {
  const record = (result ?? {}) as Partial<AppliedWorkspace>;
  return { worktreePath: record.worktreePath ?? '', branch: record.branch ?? '' };
}

/**
 * The send path's apply-then-bind. Applies whatever is staged (or reuses an
 * apply the confirm button already ran) and attaches the result to the
 * thread the message is about to go to.
 *
 * Placeholders return after the apply: the pane's placeholder is stamped, so
 * the CreateThread that follows carries worktreePath / workspaceOverride /
 * branch and the backend adopts the unbound setup run. Callers MUST run this
 * before materializing, which is why it takes the pane rather than a thread.
 */
export async function prepareThreadWorktreeIntent(
  opts: PrepareThreadWorktreeIntentOptions,
): Promise<AppliedWorkspace | null> {
  const thread = opts.pane.thread;
  if (!thread) return null;
  const staged = worktreeIntentForThread(thread);
  if (!isStagedWorktreeIntent(staged)) return null;
  const existing = prepareInFlight.get(thread.id);
  if (existing) return existing;
  const run = runPrepare(opts, thread, staged).finally(() => {
    prepareInFlight.delete(thread.id);
  });
  prepareInFlight.set(thread.id, run);
  return run;
}

function runPrepare(
  opts: PrepareThreadWorktreeIntentOptions,
  thread: Thread,
  staged: WorktreeIntent,
): Promise<AppliedWorkspace | null> {
  const pane = opts.pane;
  const isPlaceholder = pane.hasDraftPlaceholder;
  opts.onWorktreePrepareStarted?.();
  // Synchronous, before the first await: the empty-draft cleanup must never
  // see a window where the row's workspace RPC is live and the flag is not.
  // Placeholders have no row to protect and deliberately stay out of it.
  const rowId = isPlaceholder ? '' : thread.id;
  beginApplying(rowId);
  const draftOwned = isDraftOwner(pane, thread);
  return (async () => {
    try {
      const applied = staged.applied ?? (await applyStagedIntent(pane, thread, staged));
      if (!applied) return null;
      // The thread-scoped engine already moved the row and cleared the intent.
      // There is nothing left to bind.
      if (!draftOwned) return applied;
      // The placeholder's stamp is the binding; CreateThread does the rest.
      if (isPlaceholder) return applied;
      // A materialized draft row binds here, at send time — not at apply time,
      // so an emptied draft with a cut worktree can still dematerialize.
      const bindId = pane.thread?.id ?? thread.id;
      if (
        applied.worktreePath
        && !sameNormalizedPath(applied.worktreePath, thread.workspacePath)
      ) {
        syncThread((await UpdateThreadWorkspace(bindId, applied.worktreePath)) as Thread);
      }
      // A branch-only apply needs no bind: the checkout already moved the
      // shared workspace, and the row's branch heals through the backend's
      // workspace-keyed branch persist.
      clearWorktreeIntent(bindId);
      return applied;
    } finally {
      endApplying(rowId);
      opts.onWorktreePrepareFinished?.();
    }
  })();
}

/**
 * THREAD-scoped apply: the engine for any row the project-scoped path does not
 * own — a thread with history moving its own workspace, and the
 * plan-implementation flow's freshly created child row. Its carry semantics
 * stash from THAT thread's workspace and its branch create runs in THAT
 * thread's checkout, which is the whole reason it is a separate call.
 */
export async function materializeWorktreeIntentOnThread(
  opts: MaterializeWorktreeIntentOnThreadOptions,
): Promise<Thread | null> {
  const intent = opts.intent;
  if (!isStagedWorktreeIntent(intent)) return null;

  opts.onWorktreePrepareStarted?.();
  // Synchronous bracket, same contract as runPrepare's: this path drives RPCs
  // against a real row from the first statement on.
  const rowId = opts.targetThread.id;
  beginApplying(rowId);
  let updated: Thread | null = null;
  try {
    if (intent.mode === 'new-worktree' && intent.creatingBranch) {
      const wire = resolveBaseForWire(intent.newBranchBase, opts.targetThread.branch ?? '');
      updated = (await PrepareThreadWorktree(
        opts.targetThread.id,
        wire.baseBranch,
        intent.newBranchName,
        wire.carryLocalChanges,
      )) as Thread;
    } else if (intent.mode === 'new-worktree') {
      const branch = intent.attachBranch || (opts.targetThread.branch ?? '');
      updated = (await AttachThreadWorktree(opts.targetThread.id, branch)) as Thread;
    } else if (intent.creatingBranch) {
      const wire = resolveBaseForWire(intent.newBranchBase, opts.targetThread.branch ?? '');
      updated = (await GitCreateBranchFrom(
        opts.targetThread.id,
        intent.newBranchName,
        wire.baseBranch,
        wire.carryLocalChanges,
      )) as Thread;
    }
  } finally {
    endApplying(rowId);
    opts.onWorktreePrepareFinished?.();
  }

  if (updated) {
    syncThread(updated);
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

/** Test isolation only: the module-level in-flight and bracket bookkeeping. */
export function resetWorktreeIntentMaterializeForTest(): void {
  applyInFlight.clear();
  prepareInFlight.clear();
  applyingDepth.clear();
}
