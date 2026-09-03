// Wire shape of one row in the review pane's commit selector
// (`ListBranchCommits`). Re-exported so feature code doesn't import
// from the generated bindings tree directly.
export type { BranchCommit } from '../../../bindings/agent-overflow/internal/app/models';

// Branch-prune wire shapes (GitListBranchPruneCandidates /
// GitPruneBranches), re-exported for the same reason.
export type {
  BranchPruneCandidate,
  BranchPruneCandidates,
  BranchPruneResult,
  BranchPruneSelection,
} from '../../../bindings/agent-overflow/internal/app/models';

export interface GitStatus {
  isRepo: boolean;
  branch: string;
  isDefaultBranch: boolean;
  hasChanges: boolean;
  insertions: number;
  deletions: number;
  fileCount: number;
  hasUpstream: boolean;
  aheadCount: number;
  behindCount: number;
  hasOriginRemote: boolean;
  /**
   * Canonical id of the origin remote's forge. Drives PR/MR label
   * adaptation and gates the "Create PR" action.
   * Values: "github" | "gitlab" | "" (unknown / no origin).
   */
  forge?: string;
  openPrUrl?: string;
  openPrNumber?: number;
  /**
   * Set when checking the current branch's open PR/MR failed. Distinct from
   * a successful lookup that found no open PR/MR.
   */
  openPrLookupError?: string;
  /**
   * Identifier of an in-progress multi-step git operation that blocks new
   * commits. Empty string when the repo is idle. Known values: "merge",
   * "rebase", "bisect".
   */
  pendingOperation?: string;
}

export interface GitBranch {
  name: string;
  isCurrent: boolean;
  isDefault: boolean;
  worktreePath?: string;
  aheadCount?: number;
  behindCount?: number;
}

export interface GitActionResult {
  action: string;
  branch?: string;
  commitSha?: string;
  prUrl?: string;
  message?: string;
  error?: string;
}

// Wire shape of a CHECKOUT — the subject of every workspace-scoped git RPC
// (`internal/gitapp.WorkspaceRef`). Re-exported so feature code names it
// through this module rather than the generated tree, and so there is one
// import path for the type every `workspaceRefFor*` helper returns.
export type { WorkspaceRef } from '../../../bindings/agent-overflow/internal/app/models';

// The caller's checkout after a mutation that may have moved its branch
// (GitCheckout / GitCreateBranchFrom / RemoveOtherWorktree).
export type { GitWorkspaceState } from '../../../bindings/agent-overflow/internal/app/models';
