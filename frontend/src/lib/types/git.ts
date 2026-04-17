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
  isRemote: boolean;
  isCurrent: boolean;
  isDefault: boolean;
  worktreePath?: string;
}

export interface GitActionResult {
  action: string;
  branch?: string;
  commitSha?: string;
  prUrl?: string;
  message?: string;
  error?: string;
}
