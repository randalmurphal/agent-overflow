import type { Thread } from '../types/models';
import { threadMachine } from './attachedBackends.svelte';
import { getSettings } from './settings.svelte';
import { generateWorktreeBranchName } from '../utils/worktreeBranchName';
import { branchRef } from '../utils/gitRef';
import type { PaneCheckoutSource } from '../utils/checkout';

export type WorktreeIntentMode = 'local' | 'new-worktree';

// Sentinel `newBranchBase` value meaning "branch off the current branch
// AND carry the source workspace's uncommitted changes into the new
// worktree". Distinct from picking the current branch by name (which
// is a clean checkout from that branch's tip).
//
// Prefer the `isLocalBase` / `resolveBaseForWire` helpers below over
// raw string comparisons so the comparison and the wire mapping stay
// centralized — drift is the failure mode this constant is most
// vulnerable to.
export const LOCAL_BASE_SENTINEL = '__LOCAL__';

// Typed predicate for the sentinel.
export function isLocalBase(value: string | undefined | null): boolean {
  return value === LOCAL_BASE_SENTINEL;
}

// Maps a UI base selection to the (baseBranch, carryLocalChanges) pair
// the backend bindings expect. Bases are wire refs (utils/gitRef.ts); ''
// is the source checkout's current ref, which the backend resolves when the
// call runs. The sentinel is that base with carry=true, the only base the
// backend lets carry; anything else passes through with carry=false.
export function resolveBaseForWire(base: string): { baseBranch: string; carryLocalChanges: boolean } {
  if (isLocalBase(base)) {
    return { baseBranch: '', carryLocalChanges: true };
  }
  return { baseBranch: base, carryLocalChanges: false };
}

// Per-thread quadrant: (workspace mode) × (creating a new branch?).
//
//   mode='local'         creatingBranch=false → no special intent (checkout existing in current workspace)
//   mode='local'         creatingBranch=true  → create branch off newBranchBase, checkout in current workspace
//   mode='new-worktree'  creatingBranch=false → create worktree pointing at attachBranch (existing branch, or a tag ref checked out detached)
//   mode='new-worktree'  creatingBranch=true  → create branch off newBranchBase, then worktree pointing at it
//
// An empty newBranchBase is the current checkout, whatever it is on when
// the intent applies.
//
// `carryLocalChanges` is intentionally NOT a stored field. It is
// always `isLocalBase(newBranchBase)` — derived at the wire boundary
// via resolveBaseForWire so the (sentinel ↔ carry=true) invariant is
// structurally impossible to drift.
export interface WorktreeIntent {
  mode: WorktreeIntentMode;
  creatingBranch: boolean;
  newBranchName: string;
  newBranchBase: string;
  attachBranch: string;
}

const LOCAL_INTENT: WorktreeIntent = {
  mode: 'local',
  creatingBranch: false,
  newBranchName: '',
  newBranchBase: '',
  attachBranch: '',
};

let intents: Map<string, WorktreeIntent> = $state(new Map());

export function worktreeIntentForThread(thread: Thread | null | undefined): WorktreeIntent {
  if (!thread) return LOCAL_INTENT;
  return intents.get(thread.id) ?? LOCAL_INTENT;
}

// Seeded once at draft creation time. When the user has the
// "default to worktree" setting we pre-stage a new branch so the
// workflow continues unchanged — the user can still toggle off
// before sending.
export function seedDefaultWorktreeIntentForDraft(thread: Thread): void {
  if (thread.worktreePath || intents.has(thread.id)) return;
  if (getSettings().defaultThreadEnvMode !== 'worktree') return;
  intents = new Map(intents).set(thread.id, {
    mode: 'new-worktree',
    creatingBranch: true,
    newBranchName: generateWorktreeBranchName(getSettings(threadMachine(thread.id, thread.projectId)).worktreeBranchPrefix),
    newBranchBase: thread.branch ? branchRef(thread.branch) : '',
    attachBranch: '',
  });
}

// User flipped the workspace selector. Toggling INTO new-worktree
// leaves creatingBranch=false: the user picks an existing branch from
// BranchPicker, or clicks "+ new branch" to opt into creating one.
export function setThreadEnvMode(thread: Thread, mode: WorktreeIntentMode): void {
  const next = new Map(intents);
  if (mode === 'local') {
    next.set(thread.id, LOCAL_INTENT);
  } else {
    next.set(thread.id, {
      mode: 'new-worktree',
      creatingBranch: false,
      newBranchName: '',
      newBranchBase: '',
      attachBranch: '',
    });
  }
  intents = next;
}

// Enters the create-branch flow for `thread`, keeping a staged base and
// otherwise using `defaultBase`. The name is auto-filled in worktree mode
// (matching the seed flow) and left blank in local mode for the user to
// type.
export function enterCreateBranchMode(thread: Thread, defaultBase: string): void {
  const current = worktreeIntentForThread(thread);
  const seedName =
    current.mode === 'new-worktree'
      ? generateWorktreeBranchName(getSettings(threadMachine(thread.id, thread.projectId)).worktreeBranchPrefix)
      : '';
  intents = new Map(intents).set(thread.id, {
    ...current,
    creatingBranch: true,
    newBranchName: current.newBranchName || seedName,
    newBranchBase: current.newBranchBase || defaultBase,
    attachBranch: '',
  });
}

/**
 * The branch picker's "New branch…" and the inline "+ new branch": keep a
 * staged base, else branch from the current checkout (''). A dirty tree in
 * local mode pre-selects the carry sentinel instead, so that checking out
 * another base, which discards the changes, is opt-in. Worktree mode leaves
 * the source tree where it is, so nothing is carried by default.
 */
export function startNewBranch(pane: PaneCheckoutSource): void {
  if (!pane.thread) return;
  const carry = worktreeIntentForThread(pane.thread).mode === 'local' && !!pane.gitStatus.status?.hasChanges;
  enterCreateBranchMode(pane.thread, carry ? LOCAL_BASE_SENTINEL : '');
}

/**
 * The git menu's "Create branch from here": a branch at the current
 * checkout, in place, replacing any staged base or worktree. Uncommitted
 * changes stay with the new branch.
 */
export function startNewBranchFromHere(pane: PaneCheckoutSource): void {
  if (!pane.thread) return;
  const current = worktreeIntentForThread(pane.thread);
  intents = new Map(intents).set(pane.thread.id, {
    mode: 'local',
    creatingBranch: true,
    newBranchName: current.newBranchName,
    newBranchBase: '',
    attachBranch: '',
  });
}

// User cancelled the inline new-branch UI. Drops creatingBranch + the
// associated fields. Workspace mode (local vs new-worktree) survives
// so the user is back to "pick existing branch" in whichever mode they
// were in.
export function exitCreateBranchMode(thread: Thread): void {
  const current = worktreeIntentForThread(thread);
  intents = new Map(intents).set(thread.id, {
    ...current,
    creatingBranch: false,
    newBranchName: '',
    newBranchBase: '',
  });
}

export function setNewBranchName(thread: Thread, name: string): void {
  const current = worktreeIntentForThread(thread);
  if (!current.creatingBranch) return;
  intents = new Map(intents).set(thread.id, {
    ...current,
    newBranchName: name,
  });
}

export function setNewBranchBase(thread: Thread, base: string): void {
  const current = worktreeIntentForThread(thread);
  if (!current.creatingBranch) return;
  intents = new Map(intents).set(thread.id, {
    ...current,
    newBranchBase: base,
  });
}

// User picked an existing branch or a tag (as its wire ref) from
// BranchPicker while in new-worktree + !creatingBranch mode — stages it
// as the worktree's target. No-op outside that quadrant; callers handle dedup separately
// (a branch that already has a worktree should flip mode='local'
// instead of staging an attach).
export function setAttachBranch(thread: Thread, branch: string): void {
  const current = worktreeIntentForThread(thread);
  if (current.mode !== 'new-worktree' || current.creatingBranch) return;
  intents = new Map(intents).set(thread.id, {
    ...current,
    attachBranch: branch,
  });
}

/** Is there a workspace choice staged at all? The apply's whole precondition. */
export function isStagedWorktreeIntent(intent: WorktreeIntent): boolean {
  return intent.mode === 'new-worktree' || intent.creatingBranch;
}

/**
 * Synchronous "would a send have to apply anything?" read. The send path uses
 * it as a gate so an unstaged send does not pick up an extra microtask on the
 * way to the wire — the mid-turn queue path is ordering-sensitive.
 */
export function hasStagedWorktreeIntent(thread: Thread | null | undefined): boolean {
  return isStagedWorktreeIntent(worktreeIntentForThread(thread));
}

export function clearWorktreeIntent(threadId: string): void {
  if (!intents.has(threadId)) return;
  const next = new Map(intents);
  next.delete(threadId);
  intents = next;
}

// Threads with a workspace RPC in flight against their persisted row.
// Reactive so the composer's empty-draft cleanup cannot delete the row
// while the backend is creating and binding its branch or worktree.
let applyingThreadIds: ReadonlySet<string> = $state(new Set());

export function markWorktreeIntentApplying(threadId: string, applying: boolean): void {
  if (threadId === '' || applyingThreadIds.has(threadId) === applying) return;
  const next = new Set(applyingThreadIds);
  if (applying) next.add(threadId);
  else next.delete(threadId);
  applyingThreadIds = next;
}

export function isWorktreeIntentApplying(threadId: string | null | undefined): boolean {
  return threadId != null && applyingThreadIds.has(threadId);
}

// Re-key a draft placeholder's intent under its newly-materialized
// thread id. Called from ThreadPane.ensureMaterializedThread after
// CreateThread returns so worktree/branch picks made on the placeholder
// (keyed by the synthetic placeholder id) survive into the real row, and from
// dematerializeEmptyDraftThread in the other direction.
export function migrateWorktreeIntent(fromThreadId: string, toThreadId: string): void {
  if (fromThreadId === toThreadId) return;
  const existing = intents.get(fromThreadId);
  if (!existing) return;
  const next = new Map(intents);
  next.delete(fromThreadId);
  next.set(toThreadId, existing);
  intents = next;
}

export function resetForTest(): void {
  intents = new Map();
  applyingThreadIds = new Set();
}
