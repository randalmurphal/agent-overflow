// Fast-forward sync of a branch picker row from its upstream. Sync runs in
// the worktree that holds the branch: the pane's own checkout for a local
// row, another of the project's worktrees for a checked-out-elsewhere row,
// which is confirmed first.

import { GitSyncBranch } from '../../../stores/bindings';
import { addErrorToast, addToast } from '../../../stores/toast.svelte';
import type { ThreadPane } from '../../../stores/thread.svelte';
import type { GitBranch } from '../../../types/git';
import { userFacingError } from '../../../utils/userFacingError';
import { workspaceRefForProject } from '../../../utils/workspaceKey';
import { branchUsesAnotherWorktree, canSync, syncDisabledTitle } from './branchRowFacts';

export interface BranchSyncConfirmation {
  branchName: string;
  targetWorkspace: string;
}

export interface BranchSync {
  /** The branch being synced, or null. */
  readonly syncing: string | null;
  /** A cross-worktree sync waiting for confirmation, or null. */
  readonly confirmation: BranchSyncConfirmation | null;
  actionTitle(branch: GitBranch): string | undefined;
  actionDisabled(branch: GitBranch): boolean;
  /** Syncs a local row, or stages the confirmation and calls `onConfirmNeeded`. */
  request(branch: GitBranch, onConfirmNeeded: () => void): Promise<void>;
  confirm(): void;
  cancel(): void;
}

export function createBranchSync(opts: {
  pane: () => Pick<ThreadPane, 'thread' | 'workspace'>;
  /** The branch list the sync answered with, for the thread it ran for. */
  onBranches: (threadId: string, branches: GitBranch[]) => void;
}): BranchSync {
  let syncing = $state<string | null>(null);
  let confirmation = $state<BranchSyncConfirmation | null>(null);

  const currentWorkspace = (): string => opts.pane().thread?.workspacePath ?? '';

  async function perform(branchName: string, targetWorkspace: string): Promise<void> {
    const thread = opts.pane().thread;
    if (!thread || syncing) return;
    const target = workspaceRefForProject(thread.projectId, targetWorkspace);
    if (!target) {
      addToast('error', 'Project is unavailable for sync.');
      return;
    }
    syncing = branchName;
    try {
      const res = (await GitSyncBranch(target, branchName)) as GitBranch[] | null;
      if (Array.isArray(res)) opts.onBranches(thread.id, res);
      addToast('info', `Synced ${branchName}`);
    } catch (err) {
      addErrorToast(`Sync failed: ${userFacingError(err)}`, err);
    } finally {
      syncing = null;
    }
  }

  return {
    get syncing() {
      return syncing;
    },
    get confirmation() {
      return confirmation;
    },
    actionTitle(branch) {
      const disabledTitle = syncDisabledTitle(branch);
      if (disabledTitle) return disabledTitle;
      if (branchUsesAnotherWorktree(branch, currentWorkspace())) {
        return `Sync ${branch.name} in ${branch.worktreePath}`;
      }
      return undefined;
    },
    actionDisabled(branch) {
      return opts.pane().workspace === null || !canSync(branch) || syncing !== null;
    },
    async request(branch, onConfirmNeeded) {
      const thread = opts.pane().thread;
      if (!thread || !canSync(branch) || syncing) return;
      const elsewhere = branchUsesAnotherWorktree(branch, currentWorkspace());
      const targetWorkspace = elsewhere ? branch.worktreePath ?? '' : thread.workspacePath ?? '';
      if (!targetWorkspace) return;
      if (elsewhere) {
        confirmation = { branchName: branch.name, targetWorkspace };
        onConfirmNeeded();
        return;
      }
      await perform(branch.name, targetWorkspace);
    },
    confirm() {
      const pending = confirmation;
      if (!pending) return;
      confirmation = null;
      void perform(pending.branchName, pending.targetWorkspace);
    },
    cancel() {
      confirmation = null;
    },
  };
}
