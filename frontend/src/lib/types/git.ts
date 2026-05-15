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

export interface Worktree {
  path: string;
  branch?: string;
  head: string;
}

export interface GitActionResult {
  action: string;
  branch?: string;
  commitSha?: string;
  prUrl?: string;
  message?: string;
  error?: string;
}
