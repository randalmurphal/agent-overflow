// Fast-forward sync of a branch picker row from its upstream. Sync runs in
// the worktree that holds the branch: the pane's own checkout for a local
// row, another of the project's worktrees for a checked-out-elsewhere row,
// which is confirmed first. A tag has no upstream and never syncs.

import { GitSyncBranch } from '../../../stores/bindings';
import { addErrorToast, addToast } from '../../../stores/toast.svelte';
import type { ThreadPane } from '../../../stores/thread.svelte';
import type { GitBranch } from '../../../types/git';
import { userFacingError } from '../../../utils/userFacingError';
import { workspaceRefForProject } from '../../../utils/workspaceKey';
import type { PickerRow } from './branchPickerRows';
import { branchUsesAnotherWorktree } from './branchRowFacts';

// FF-only sync. Diverged rows render the icon disabled with a tooltip; the
// backend mirrors the gate via git's native non-FF refusal.
function canSync(branch: GitBranch): boolean {
  return (branch.behindCount ?? 0) > 0 && (branch.aheadCount ?? 0) === 0;
}

function isDiverged(branch: GitBranch): boolean {
  return (branch.behindCount ?? 0) > 0 && (branch.aheadCount ?? 0) > 0;
}

/** The branch a row syncs, or null for a row with nothing to sync. */
function syncableBranch(row: PickerRow): GitBranch | null {
  return row.kind === 'branch' && (canSync(row.branch) || isDiverged(row.branch)) ? row.branch : null;
}

export interface BranchSyncConfirmation {
  branchName: string;
  targetWorkspace: string;
}

export interface BranchSync {
  /** The branch being synced, or null. */
  readonly syncing: string | null;
  /** A cross-worktree sync waiting for confirmation, or null. */
  readonly confirmation: BranchSyncConfirmation | null;
  /** Whether the row shows the sync action at all. */
  shows(row: PickerRow): boolean;
  actionLabel(row: PickerRow): string;
  actionTitle(row: PickerRow): string | undefined;
  actionDisabled(row: PickerRow): boolean;
  /** Syncs a local row, or stages the confirmation and calls `onConfirmNeeded`. */
  request(row: PickerRow, onConfirmNeeded: () => void): Promise<void>;
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
    shows(row) {
      return syncableBranch(row) !== null;
    },
    actionLabel(row) {
      return syncing === row.name ? `Syncing ${row.name}` : `Sync ${row.name} from upstream`;
    },
    actionTitle(row) {
      const branch = syncableBranch(row);
      if (!branch) return undefined;
      if (isDiverged(branch)) {
        return `Branch has diverged from upstream (${branch.aheadCount} ahead, ${branch.behindCount} behind). Push or rebase first.`;
      }
      if (branchUsesAnotherWorktree(branch, currentWorkspace())) {
        return `Sync ${branch.name} in ${branch.worktreePath}`;
      }
      return undefined;
    },
    actionDisabled(row) {
      const branch = syncableBranch(row);
      return opts.pane().workspace === null || !branch || !canSync(branch) || syncing !== null;
    },
    async request(row, onConfirmNeeded) {
      const thread = opts.pane().thread;
      const branch = syncableBranch(row);
      if (!thread || !branch || !canSync(branch) || syncing) return;
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
