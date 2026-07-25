// The one description of what a checkout is about to cost, shared by the
// single-run discard preview (§4.5, D23) and the project-deletion preview
// (D25). Both surfaces render the same rows, so they read the same way.
//
// Pure: no bindings, no Svelte. A worktree that could not be inspected reports
// its own failure rather than being summarised as if it were clean — a silent
// gap in a loss preview is the one thing these surfaces cannot do.

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

// runLossSummary is the one-line headline above the rows: how much is going,
// and how much of it is still moving. Returns '' when there is nothing to say.
export function runLossSummary(runCount: number, liveCount: number, automationCount: number): string {
  const parts: string[] = [];
  if (runCount > 0) parts.push(countLabel(runCount, 'workflow run'));
  if (automationCount > 0) parts.push(countLabel(automationCount, 'automation'));
  if (parts.length === 0) return '';
  const subject = parts.join(' and ');
  if (liveCount === 0) return `${subject} will be deleted.`;
  return `${subject} will be deleted, including ${liveCount} still working — ${
    liveCount === 1 ? 'it is' : 'they are'
  } stopped first.`;
}
