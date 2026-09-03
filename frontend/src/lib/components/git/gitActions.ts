// Git action handlers + primary-action classification.
//
// Extracted from GitActionsControl.svelte so the shell focuses on the
// split-button + dropdown markup. Each action wraps a single Wails
// binding and reports success/failure through the supplied callbacks.
// The primary-action decision table is a pure function of `GitStatus`
// so it can be unit-tested without mounting the component.

import {
  GitCreatePR,
  GitPull,
  GitPush,
  RemoveOtherWorktree,
} from '../../stores/bindings';
import {
  applyToDraftPlaceholdersInWorkspace,
  placeholderWorkspaceOf,
} from '../../stores/draftWorkspaceSync';
import { addToast } from '../../stores/toast.svelte';
import { errString } from '../../utils/errors';
import { forgeLabels } from '../../utils/forgeLabels';
import type {
  GitActionResult,
  GitStatus,
  GitWorkspaceState,
  WorkspaceRef,
} from '../../types/git';

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

/**
 * Removing the pane's own worktree is still a workspace action — the
 * directory is the subject — so it takes the same ref, plus the path being
 * removed. `GitRemoveWorktree(threadID)` remains for the thread-centric
 * callers (sidebar row, archived thread, proposed-plan implementation),
 * where the subject really is "the worktree THIS thread is attached to".
 */
export interface RemoveWorktreeCtx extends GitActionCtx {
  worktreePath: string;
}

export interface GitActionCtx {
  /** The checkout every action below acts on. */
  workspace: WorkspaceRef;
  reportError: (message: string) => void;
  refreshStatus: () => Promise<void>;
  /**
   * Forge id (`status.forge`) for label adaptation in toasts and errors.
   * Optional — when omitted, falls back to GitHub strings via forgeLabels.
   */
  forge?: string;
}

export async function runPushAction(ctx: GitActionCtx): Promise<void> {
  try {
    const result = (await GitPush(ctx.workspace)) as GitActionResult;
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
    const result = (await GitPull(ctx.workspace)) as GitActionResult;
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
  const labels = forgeLabels(ctx.forge);
  try {
    const result = (await GitCreatePR(ctx.workspace, '', '', false)) as GitActionResult;
    if (result.error) {
      console.error(`${labels.createAction} failed:`, result.error);
      ctx.reportError(`${labels.createAction} failed: ${result.error}`);
      return;
    }
    addToast(
      'success',
      result.prUrl ? `${labels.noun} created: ${result.prUrl}` : `${labels.noun} created`,
    );
    await ctx.refreshStatus();
  } catch (err) {
    console.error(`${labels.createAction} failed:`, err);
    ctx.reportError(`${labels.createAction} failed: ${errString(err)}`);
  }
}

export async function runRemoveWorktreeAction(ctx: RemoveWorktreeCtx): Promise<void> {
  try {
    const next = (await RemoveOtherWorktree(
      ctx.workspace,
      ctx.worktreePath,
      false,
    )) as GitWorkspaceState;
    // Thread rows attached to the removed worktree are reattached and
    // broadcast by the backend; draft placeholders have no row to broadcast,
    // so the returned state is applied to them here.
    applyToDraftPlaceholdersInWorkspace(ctx.workspace, placeholderWorkspaceOf(next));
    addToast('success', 'Worktree removed');
    await ctx.refreshStatus();
  } catch (err) {
    console.error('Remove worktree failed:', err);
    ctx.reportError(`Remove worktree failed: ${errString(err)}`);
  }
}
