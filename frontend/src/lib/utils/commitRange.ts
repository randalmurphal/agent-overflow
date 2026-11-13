import type { BranchCommit } from '../types/git';

// A pick in the review pane's commit picker: one commit, or a contiguous
// run of the commit list (docs/specs/pull-requests.md#commit-ranges).
// The list arrives newest first; a run is named by its ends, `first` the
// older and `last` the newer, as `OpenPRCommitDiff(first, last)` takes
// them. Ordinals count from the oldest commit (1) to the head (T), so a
// run to the head reads `N–T/T`.

export interface CommitSelection {
  /** The older end; equal to `last` for one commit. */
  readonly first: string;
  /** The newer end. */
  readonly last: string;
}

/** The commit's ordinal in a newest-first list of `count`, oldest = 1. */
export function commitOrdinal(index: number, count: number): number {
  return count - index;
}

/**
 * The selection for a pick between two commits of the list, in either
 * order, or null for "All commits": a run of two or more commits that
 * covers the whole list is the whole PR, which has one representation.
 * One commit stays a selection even when it is the only one. Null too
 * when either end is not in the list.
 */
export function commitSelectionBetween(commits: readonly BranchCommit[], a: string, b: string): CommitSelection | null {
  const ia = commits.findIndex((commit) => commit.sha === a);
  const ib = commits.findIndex((commit) => commit.sha === b);
  if (ia < 0 || ib < 0) return null;
  // Newest first: the larger index is the older commit.
  const older = Math.max(ia, ib);
  const newer = Math.min(ia, ib);
  if (older !== newer && older === commits.length - 1 && newer === 0) return null;
  return { first: commits[older].sha, last: commits[newer].sha };
}

/** `From here`: the commit through the newest. */
export function commitSelectionFromHere(commits: readonly BranchCommit[], sha: string): CommitSelection | null {
  const head = commits[0];
  return head ? commitSelectionBetween(commits, sha, head.sha) : null;
}

/** The list indexes of a selection's ends (`newer` <= `older`), or null
 * when either end left the list. */
export function commitSelectionIndexes(
  commits: readonly BranchCommit[],
  selection: CommitSelection | null,
): { newer: number; older: number } | null {
  if (!selection) return null;
  const older = commits.findIndex((commit) => commit.sha === selection.first);
  const newer = commits.findIndex((commit) => commit.sha === selection.last);
  if (older < 0 || newer < 0 || newer > older) return null;
  return { newer, older };
}

/** Whether a selection names more than one commit. */
export function isCommitRange(selection: CommitSelection | null): boolean {
  return selection !== null && selection.first !== selection.last;
}

/**
 * The selection a load of a fresh list keeps: the same one when both ends
 * are still in it in order and it is not the whole list; else null (All
 * commits), with `lost` telling a held pick that vanished from one that
 * was never there.
 */
export function resolveCommitSelection(
  selection: CommitSelection | null,
  commits: readonly BranchCommit[],
): { selection: CommitSelection | null; lost: boolean } {
  if (!selection) return { selection: null, lost: false };
  const at = commitSelectionIndexes(commits, selection);
  if (!at) return { selection: null, lost: true };
  if (at.newer !== at.older && at.newer === 0 && at.older === commits.length - 1) return { selection: null, lost: false };
  return { selection, lost: false };
}

/** The picker's trigger text: `All commits`, `N/T` or `N–M/T`. */
export function commitSelectionLabel(commits: readonly BranchCommit[], selection: CommitSelection | null): string {
  const at = commitSelectionIndexes(commits, selection);
  if (!at) return 'All commits';
  const count = commits.length;
  const from = commitOrdinal(at.older, count);
  const to = commitOrdinal(at.newer, count);
  return from === to ? `${to}/${count}` : `${from}–${to}/${count}`;
}

/** The selection a (shift+)click on the picker's row of `sha` makes:
 * shift spans from the picked commit (a range's newer end when one is
 * picked) to it; a plain click picks it alone. */
export function commitSelectionForClick(
  commits: readonly BranchCommit[],
  current: CommitSelection | null,
  sha: string,
  shift: boolean,
): CommitSelection | null {
  if (shift && current) return commitSelectionBetween(commits, current.last, sha);
  return commitSelectionBetween(commits, sha, sha);
}

export function sameCommitSelection(a: CommitSelection | null, b: CommitSelection | null): boolean {
  if (a === null || b === null) return a === b;
  return a.first === b.first && a.last === b.last;
}
