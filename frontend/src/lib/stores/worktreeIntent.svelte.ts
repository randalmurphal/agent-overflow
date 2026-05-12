import type { Thread } from '../types/models';
import { getSettings } from './settings.svelte';
import { generateWorktreeBranchName } from '../utils/worktreeBranchName';

export type WorktreeIntentMode = 'local' | 'new-worktree';

// Sentinel `baseBranch` value meaning "branch off the current branch
// AND carry the source workspace's uncommitted changes into the new
// worktree". Distinct from picking the current branch by name (which
// is a clean checkout from that branch's tip).
//
// Prefer the `isLocalBase` / `resolveBaseForWire` helpers below over
// raw string comparisons so the comparison and the wire mapping stay
// centralized — drift is the failure mode this constant is most
// vulnerable to.
export const LOCAL_BASE_SENTINEL = '__LOCAL__';

// Typed predicate for the sentinel. Exported so UI code stops
// comparing against the raw string at every call site.
export function isLocalBase(value: string | undefined | null): boolean {
  return value === LOCAL_BASE_SENTINEL;
}

// Maps a UI base selection to the (baseBranch, carryLocalChanges) pair
// the backend bindings expect. The sentinel resolves to the thread's
// current branch with carry=true; anything else passes through with
// carry=false. Shared by both the worktree-create flow (composerSend)
// and the branch-create flow (BranchCreateForm) so the wire mapping
// only lives in one place.
export function resolveBaseForWire(
  base: string,
  currentBranch: string,
): { baseBranch: string; carryLocalChanges: boolean } {
  if (isLocalBase(base)) {
    return { baseBranch: currentBranch, carryLocalChanges: true };
  }
  return { baseBranch: base, carryLocalChanges: false };
}

// `carryLocalChanges` is intentionally NOT a stored field. It is
// always `isLocalBase(baseBranch)` — the wire mapping is computed at
// the consumption site via `resolveBaseForWire`. Keeping it as
// derived-only state means the (sentinel ↔ carry=true) invariant is
// structurally impossible to drift.
export interface WorktreeIntent {
  mode: WorktreeIntentMode;
  baseBranch: string;
  branchName: string;
}

const LOCAL_INTENT: WorktreeIntent = {
  mode: 'local',
  baseBranch: '',
  branchName: '',
};

let intents: Map<string, WorktreeIntent> = $state(new Map());

export function worktreeIntentForThread(thread: Thread | null | undefined): WorktreeIntent {
  if (!thread) return LOCAL_INTENT;

  const explicit = intents.get(thread.id);
  if (explicit) return explicit;

  return LOCAL_INTENT;
}

export function seedDefaultWorktreeIntentForDraft(thread: Thread): void {
  if (thread.worktreePath || intents.has(thread.id)) return;
  if (getSettings().defaultThreadEnvMode !== 'worktree') return;
  intents = new Map(intents).set(thread.id, {
    mode: 'new-worktree',
    baseBranch: thread.branch ?? '',
    branchName: generateWorktreeBranchName(getSettings().worktreeBranchPrefix),
  });
}

export function setThreadEnvMode(thread: Thread, mode: WorktreeIntentMode): void {
  const next = new Map(intents);
  if (mode === 'local') {
    next.set(thread.id, LOCAL_INTENT);
  } else {
    const current = worktreeIntentForThread(thread);
    // Regenerate the branch name on every fresh local→new-worktree
    // transition. If the user toggled out and back, the previous
    // generated value is stale.
    const isFreshTransition = current.mode !== 'new-worktree' || !current.branchName;
    next.set(thread.id, {
      mode,
      baseBranch: current.baseBranch || thread.branch || '',
      branchName: isFreshTransition
        ? generateWorktreeBranchName(getSettings().worktreeBranchPrefix)
        : current.branchName,
    });
  }
  intents = next;
}

export function setWorktreeBaseBranch(thread: Thread, baseBranch: string): void {
  const current = worktreeIntentForThread(thread);
  intents = new Map(intents).set(thread.id, {
    mode: 'new-worktree',
    baseBranch,
    branchName: current.branchName,
  });
}

export function setWorktreeBranchName(thread: Thread, branchName: string): void {
  const current = worktreeIntentForThread(thread);
  intents = new Map(intents).set(thread.id, {
    mode: 'new-worktree',
    baseBranch: current.baseBranch || thread.branch || '',
    branchName,
  });
}

export function clearWorktreeIntent(threadId: string): void {
  if (!intents.has(threadId)) return;
  const next = new Map(intents);
  next.delete(threadId);
  intents = next;
}

export function resetForTest(): void {
  intents = new Map();
}
