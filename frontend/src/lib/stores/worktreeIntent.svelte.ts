import type { Thread } from '../types/models';
import { getSettings } from './settings.svelte';
import { generateWorktreeBranchName } from '../utils/worktreeBranchName';

export type WorktreeIntentMode = 'local' | 'new-worktree';

// Sentinel `baseBranch` value meaning "branch off the current branch
// AND carry the source workspace's uncommitted changes into the new
// worktree". Distinct from picking the current branch by name (which
// is a clean checkout from that branch's tip).
export const LOCAL_BASE_SENTINEL = '__LOCAL__';

export interface WorktreeIntent {
  mode: WorktreeIntentMode;
  baseBranch: string;
  branchName: string;
  carryLocalChanges: boolean;
}

const LOCAL_INTENT: WorktreeIntent = {
  mode: 'local',
  baseBranch: '',
  branchName: '',
  carryLocalChanges: false,
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
    carryLocalChanges: false,
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
      carryLocalChanges: current.carryLocalChanges,
    });
  }
  intents = next;
}

export function setWorktreeBaseBranch(thread: Thread, baseBranch: string): void {
  const current = worktreeIntentForThread(thread);
  const isLocalSentinel = baseBranch === LOCAL_BASE_SENTINEL;
  intents = new Map(intents).set(thread.id, {
    mode: 'new-worktree',
    baseBranch,
    branchName: current.branchName,
    // Picking "Local (with changes)" sets carry; picking any real
    // branch name clears it so the dirty-check surface knows the user
    // wants a clean checkout.
    carryLocalChanges: isLocalSentinel,
  });
}

export function setWorktreeBranchName(thread: Thread, branchName: string): void {
  const current = worktreeIntentForThread(thread);
  intents = new Map(intents).set(thread.id, {
    mode: 'new-worktree',
    baseBranch: current.baseBranch || thread.branch || '',
    branchName,
    carryLocalChanges: current.carryLocalChanges,
  });
}

export function setWorktreeCarryLocal(thread: Thread, carry: boolean): void {
  const current = worktreeIntentForThread(thread);
  intents = new Map(intents).set(thread.id, {
    mode: 'new-worktree',
    baseBranch: current.baseBranch || thread.branch || '',
    branchName: current.branchName,
    carryLocalChanges: carry,
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
