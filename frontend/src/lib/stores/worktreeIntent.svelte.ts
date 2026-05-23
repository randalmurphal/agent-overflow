import type { Thread } from '../types/models';
import { getSettings } from './settings.svelte';
import { generateWorktreeBranchName } from '../utils/worktreeBranchName';

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
// the backend bindings expect. The sentinel resolves to the thread's
// current branch with carry=true; anything else passes through with
// carry=false.
export function resolveBaseForWire(
  base: string,
  currentBranch: string,
): { baseBranch: string; carryLocalChanges: boolean } {
  if (isLocalBase(base)) {
    return { baseBranch: currentBranch, carryLocalChanges: true };
  }
  return { baseBranch: base, carryLocalChanges: false };
}

// Per-thread quadrant: (workspace mode) × (creating a new branch?).
//
//   mode='local'         creatingBranch=false → no special intent (checkout existing in current workspace)
//   mode='local'         creatingBranch=true  → create branch off newBranchBase, checkout in current workspace
//   mode='new-worktree'  creatingBranch=false → create worktree pointing at attachBranch (existing branch)
//   mode='new-worktree'  creatingBranch=true  → create branch off newBranchBase, then worktree pointing at it
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
    newBranchName: generateWorktreeBranchName(getSettings().worktreeBranchPrefix),
    newBranchBase: thread.branch ?? '',
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

// User clicked "+ new branch" (either inline button in new-worktree
// mode, or "+ New branch…" inside the BranchPicker dropdown). Default
// base mirrors the destructive-default convention: dirty workspace
// pre-selects the LOCAL sentinel so checkout-style data loss is opt-in.
export function enterCreateBranchMode(
  thread: Thread,
  opts: { workspaceDirty: boolean; currentBranch: string },
): void {
  const current = worktreeIntentForThread(thread);
  const baseFallback = opts.workspaceDirty ? LOCAL_BASE_SENTINEL : opts.currentBranch;
  // Auto-fill the name in worktree mode (matches today's seed flow);
  // leave it blank in local mode so the input starts empty for the
  // user to type.
  const seedName =
    current.mode === 'new-worktree'
      ? generateWorktreeBranchName(getSettings().worktreeBranchPrefix)
      : '';
  intents = new Map(intents).set(thread.id, {
    ...current,
    creatingBranch: true,
    newBranchName: current.newBranchName || seedName,
    newBranchBase: current.newBranchBase || baseFallback,
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

// User picked an existing branch from BranchPicker while in
// new-worktree + !creatingBranch mode — stages it as the worktree's
// target. No-op outside that quadrant; callers handle dedup separately
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

export function clearWorktreeIntent(threadId: string): void {
  if (!intents.has(threadId)) return;
  const next = new Map(intents);
  next.delete(threadId);
  intents = next;
}

// Re-key a draft placeholder's intent under its newly-materialized
// thread id. Called from ThreadPane.ensureMaterializedThread after
// CreateThread returns so worktree/branch picks made on the placeholder
// (keyed by the synthetic placeholder id) survive into the real row.
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
}
