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
