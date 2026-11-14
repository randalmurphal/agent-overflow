// What a branch picker row shows about its ref, independent of the open
// menu's state.

import type { GitBranch } from '../../../types/git';
import { sameNormalizedPath } from '../../../utils/path';
import type { PickerRow } from './branchPickerRows';

// Visible labels cap at 20 chars (19 + ellipsis); the full name is the
// row's title.
export const ROW_LABEL_MAX_CHARS = 20;

export function truncateRowLabel(name: string): string {
  if (name.length <= ROW_LABEL_MAX_CHARS) return name;
  return name.slice(0, ROW_LABEL_MAX_CHARS - 1) + '…';
}

export function branchUsesAnotherWorktree(branch: GitBranch, currentWorkspace: string): boolean {
  return !!branch.worktreePath && !sameNormalizedPath(branch.worktreePath, currentWorkspace);
}

/** The trailing marker: ahead/behind arrows, then "worktree", "default" or "tag". */
export function rowSuffix(row: PickerRow, currentWorkspace: string): string | undefined {
  if (row.kind === 'tag') return 'tag';
  const branch = row.branch;
  const counts: string[] = [];
  if ((branch.aheadCount ?? 0) > 0) counts.push(`↑${branch.aheadCount}`);
  if ((branch.behindCount ?? 0) > 0) counts.push(`↓${branch.behindCount}`);
  let marker: string | undefined;
  if (branchUsesAnotherWorktree(branch, currentWorkspace)) {
    marker = 'worktree';
  } else if (branch.isDefault) {
    marker = 'default';
  }
  if (counts.length === 0) return marker;
  const arrows = counts.join(' ');
  return marker ? `${arrows} · ${marker}` : arrows;
}
