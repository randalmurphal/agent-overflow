// The plain-language half of project deletion (D25): what the deletion is
// about to do, and what it left behind.
//
// Deletion is cleanup, not a loss — no branch is deleted and no commit becomes
// unreachable — so nothing here is written to alarm. It says what goes, says
// the branches stay, and names the checkouts git will refuse to remove so the
// user can finish those with their own tools.
//
// Pure: no bindings, no Svelte.

import type { ProjectCleanupWorktree, RetainedWorktree } from '../types/workflow';

function countLabel(count: number, singular: string): string {
  return `${count} ${singular}${count === 1 ? '' : 's'}`;
}

// cleanupSummary is the one line above the detail: what is deleted with the
// project, and how much of it is still working. Returns '' when the project
// owns no workflow work at all.
export function cleanupSummary(
  runCount: number,
  liveCount: number,
  automationCount: number,
): string {
  const parts: string[] = [];
  if (runCount > 0) parts.push(countLabel(runCount, 'workflow run'));
  if (automationCount > 0) parts.push(countLabel(automationCount, 'automation'));
  if (parts.length === 0) return '';
  const subject = parts.join(' and ');
  if (liveCount === 0) return `${subject} will be deleted with it.`;
  return `${subject} will be deleted with it, including ${liveCount} still working — ${
    liveCount === 1 ? 'it is' : 'they are'
  } stopped first.`;
}

// retainedInPreview is the subset the deletion will leave on disk. The dialog
// lists exactly these; the ones it will remove need no enumeration, because
// removing them costs the user nothing.
export function retainedInPreview(
  worktrees: ProjectCleanupWorktree[],
): ProjectCleanupWorktree[] {
  return worktrees.filter((worktree) => worktree.retained);
}

// retainedNotice is what the user is told after the deletion when git declined
// to remove something. It names the paths rather than counting them: a path is
// what the user needs to act on, and there are never many.
export function retainedNotice(retained: RetainedWorktree[]): string {
  if (retained.length === 0) return '';
  const detail = retained
    .map((worktree) => `${worktree.path} (${worktree.reason})`)
    .join('; ');
  return `${countLabel(retained.length, 'checkout')} left in place: ${detail}`;
}
