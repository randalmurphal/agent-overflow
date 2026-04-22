// Git action handlers + primary-action classification.
//
// Extracted from GitActionsControl.svelte so the shell focuses on the
// split-button + dropdown markup. Each action wraps a single Wails
// binding and reports success/failure through the supplied callbacks.
// The primary-action decision table is a pure function of `GitStatus`
// so it can be unit-tested without mounting the component.

import {
  GetGitStatus,
  GetThread,
  GitCreatePR,
  GitPull,
  GitPush,
  GitRemoveWorktree,
} from '../../stores/bindings';
import { replaceThread } from '../../stores/threads.svelte';
import { addToast } from '../../stores/toast.svelte';
import { errString } from '../../utils/errors';
import type { GitActionResult, GitStatus } from '../../types/git';
import type { Thread } from '../../types/models';

export type PrimaryActionKind = 'commit' | 'push' | 'pull';

export interface PrimaryAction {
  label: string;
  action: PrimaryActionKind;
  disabled: boolean;
  tooltip: string;
}

/**
 * Classify what the primary split-button should do given the current git
 * status. Priority: uncommitted changes → push if ahead → pull if behind.
 * When no useful action applies, surface the commit action disabled.
 */
export function primaryActionFor(status: GitStatus | null): PrimaryAction {
  if (!status) {
    return { label: 'Commit', action: 'commit', disabled: true, tooltip: 'Loading...' };
  }
  if (status.hasChanges) {
    return { label: 'Commit', action: 'commit', disabled: false, tooltip: 'Stage and commit changes' };
  }
  if (status.aheadCount > 0) {
    return {
      label: 'Push',
      action: 'push',
      disabled: false,
      tooltip: `Push ${status.aheadCount} commit${status.aheadCount !== 1 ? 's' : ''}`,
    };
  }
  if (status.behindCount > 0) {
    return {
      label: 'Pull',
      action: 'pull',
      disabled: false,
      tooltip: `Pull ${status.behindCount} commit${status.behindCount !== 1 ? 's' : ''}`,
    };
  }
  return { label: 'Commit', action: 'commit', disabled: true, tooltip: 'No changes to commit' };
}

export interface GitActionCtx {
  threadId: string;
  reportError: (message: string) => void;
  refreshStatus: () => Promise<void>;
  replacePaneThread: (thread: Thread) => void;
}

export async function loadGitStatus(threadId: string): Promise<GitStatus> {
  return (await GetGitStatus(threadId)) as GitStatus;
}

export async function runPushAction(ctx: GitActionCtx): Promise<void> {
  try {
    const result = (await GitPush(ctx.threadId)) as GitActionResult;
    if (result.error) {
      console.error('Push failed:', result.error);
      ctx.reportError(`Push failed: ${result.error}`);
      return;
    }
    addToast('success', 'Pushed successfully');
    await ctx.refreshStatus();
  } catch (err) {
    console.error('Push failed:', err);
    ctx.reportError(`Push failed: ${errString(err)}`);
  }
}

export async function runPullAction(ctx: GitActionCtx): Promise<void> {
  try {
    const result = (await GitPull(ctx.threadId)) as GitActionResult;
    if (result.error) {
      console.error('Pull failed:', result.error);
      ctx.reportError(`Pull failed: ${result.error}`);
      return;
    }
    addToast('success', 'Pulled successfully');
    await ctx.refreshStatus();
  } catch (err) {
    console.error('Pull failed:', err);
    ctx.reportError(`Pull failed: ${errString(err)}`);
  }
}

export async function runCreatePRAction(ctx: GitActionCtx): Promise<void> {
  try {
    const result = (await GitCreatePR(ctx.threadId, '', '', false)) as GitActionResult;
    if (result.error) {
      console.error('Create PR failed:', result.error);
      ctx.reportError(`Create PR failed: ${result.error}`);
      return;
    }
    addToast('success', result.prUrl ? `PR created: ${result.prUrl}` : 'PR created');
    await ctx.refreshStatus();
  } catch (err) {
    console.error('Create PR failed:', err);
    ctx.reportError(`Create PR failed: ${errString(err)}`);
  }
}

export async function runRemoveWorktreeAction(ctx: GitActionCtx): Promise<void> {
  try {
    await GitRemoveWorktree(ctx.threadId);
    const refreshedThread = (await GetThread(ctx.threadId)) as Thread;
    ctx.replacePaneThread(refreshedThread);
    replaceThread(refreshedThread);
    addToast('success', 'Worktree removed');
    await ctx.refreshStatus();
  } catch (err) {
    console.error('Remove worktree failed:', err);
    ctx.reportError(`Remove worktree failed: ${errString(err)}`);
  }
}
