import type { Thread } from '../types/models';
import { getSettings } from './settings.svelte';

export type WorktreeIntentMode = 'local' | 'new-worktree';

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
    branchName: '',
  });
}

export function setThreadEnvMode(thread: Thread, mode: WorktreeIntentMode): void {
  const next = new Map(intents);
  if (mode === 'local') {
    next.set(thread.id, LOCAL_INTENT);
  } else {
    const current = worktreeIntentForThread(thread);
    next.set(thread.id, {
      mode,
      baseBranch: current.baseBranch || thread.branch || '',
      branchName: current.branchName,
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
