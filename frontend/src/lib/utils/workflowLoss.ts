// What discarding one checkout costs, in one line (§4.5, D23). Used only by the
// discard dialog: project deletion deletes no branch and loses no commit, so it
// describes a cleanup instead — see utils/projectCleanup.ts. The two said the
// same thing once and stopped when deletion did; sharing a renderer between
// them now would only make one of them lie.
//
// Pure: no bindings, no Svelte. A worktree that could not be inspected reports
// its own failure rather than being summarised as if it were clean — a silent
// gap in a loss preview is the one thing this surface cannot do.

import type { WorkflowDiscardWorktree } from '../types/workflow';

function countLabel(count: number, singular: string): string {
  return `${count} ${singular}${count === 1 ? '' : 's'}`;
}

export function worktreeLossSummary(worktree: WorkflowDiscardWorktree): string {
  if (worktree.error) return worktree.error;
  const fragments = [
    worktree.branch || 'no branch',
    `${countLabel(worktree.dirtyFileCount, 'dirty file')}`,
    `${countLabel(worktree.unmergedCommitCount, 'unmerged commit')}`,
  ];
  if (!worktree.present) fragments.push('checkout already gone');
  else if (!worktree.registered) fragments.push('not a registered worktree — reported, not removed');
  return fragments.join(' · ');
}
