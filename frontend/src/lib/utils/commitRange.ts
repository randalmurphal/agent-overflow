import type { BranchCommit } from '../types/git';

// A pick in the review pane's commit picker: one commit, or a contiguous
// run of the commit list (docs/specs/pull-requests.md#commit-ranges).
// The list arrives newest first; a run is named by its ends, `first` the
// older and `last` the newer, as `OpenPRCommitDiff(first, last)` takes
// them. Ordinals count from the PR's first commit (1) to the head (T), so a
// run to the head reads `N–T/T`. The backend lists at most the newest 300
// commits and says how many the range holds (`total`, null when it could
// not count them): the list is complete exactly when `total` is its
// length. T is `total`; with no total there are no ordinals, and only a
// complete list's ends are the whole PR.

export interface CommitSelection {
  /** The older end; equal to `last` for one commit. */
  readonly first: string;
  /** The newer end. */
  readonly last: string;
}

/** The commit's ordinal at `index` of a newest-first list of the newest
 * of `total` commits, the first commit = 1; null with no total. */
export function commitOrdinal(index: number, total: number | null): number | null {
  return total === null ? null : total - index;
}

/**
 * A commit list answer (ListPRCommits, ListBranchCommits): the newest
 * commits up to the backend's cap and how many the range holds. A total
 * smaller than the list cannot describe it, so it is no total.
 */
export function commitList(
  answer: { commits: BranchCommit[] | null; total: number | null } | null,
): { commits: BranchCommit[]; commitsTotal: number | null } {
  if (!answer) return { commits: [], commitsTotal: 0 };
  const commits = answer.commits ?? [];
  const total = answer.total !== null && answer.total >= commits.length ? answer.total : null;
  return { commits, commitsTotal: total };
}

/** Whether the list holds every commit of the range. */
export function commitListComplete(commits: readonly BranchCommit[], total: number | null): boolean {
  return total === commits.length;
}

// Whether `older`–`newer` (list indexes) spans a complete list's ends:
// the whole PR, which has one representation (All commits).
function wholeList(commits: readonly BranchCommit[], total: number | null, older: number, newer: number): boolean {
  return older !== newer && newer === 0 && older === commits.length - 1 && commitListComplete(commits, total);
}

/**
 * The selection for a pick between two commits of the list, in either
 * order, or null for "All commits": a run of two or more commits that
 * covers a complete list is the whole PR, which has one representation (a
 * truncated list's ends are not the PR's first commit).
 * One commit stays a selection even when it is the only one. Null too
 * when either end is not in the list.
 */
export function commitSelectionBetween(commits: readonly BranchCommit[], total: number | null, a: string, b: string): CommitSelection | null {
  const ia = commits.findIndex((commit) => commit.sha === a);
  const ib = commits.findIndex((commit) => commit.sha === b);
  if (ia < 0 || ib < 0) return null;
  // Newest first: the larger index is the older commit.
  const older = Math.max(ia, ib);
  const newer = Math.min(ia, ib);
  if (wholeList(commits, total, older, newer)) return null;
  return { first: commits[older].sha, last: commits[newer].sha };
}

/** `From here`: the commit through the newest. */
export function commitSelectionFromHere(commits: readonly BranchCommit[], total: number | null, sha: string): CommitSelection | null {
  const head = commits[0];
  return head ? commitSelectionBetween(commits, total, sha, head.sha) : null;
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
 * are still in it in order and it is not a complete list's whole; else
 * null (All commits), with `lost` telling a held pick that vanished from
 * one that was never there.
 */
export function resolveCommitSelection(
  selection: CommitSelection | null,
  commits: readonly BranchCommit[],
  total: number | null,
): { selection: CommitSelection | null; lost: boolean } {
  if (!selection) return { selection: null, lost: false };
  const at = commitSelectionIndexes(commits, selection);
  if (!at) return { selection: null, lost: true };
  if (wholeList(commits, total, at.older, at.newer)) return { selection: null, lost: false };
  return { selection, lost: false };
}

/** The picker's trigger text: `All commits`, `N/T` or `N–M/T`; with no
 * total, the short hash or `<sha>–<sha>` instead of ordinals. */
export function commitSelectionLabel(commits: readonly BranchCommit[], total: number | null, selection: CommitSelection | null): string {
  const at = commitSelectionIndexes(commits, selection);
  if (!at) return 'All commits';
  const from = commitOrdinal(at.older, total);
  const to = commitOrdinal(at.newer, total);
  if (from === null || to === null) {
    const [older, newer] = [commits[at.older].shortSha, commits[at.newer].shortSha];
    return older === newer ? newer : `${older}–${newer}`;
  }
  return from === to ? `${to}/${total}` : `${from}–${to}/${total}`;
}

/** The menu's quiet note on a truncated list: how much of it is listed,
 * or null when the list is complete. */
export function commitListNote(commits: readonly BranchCommit[], total: number | null): string | null {
  if (commitListComplete(commits, total)) return null;
  return total === null
    ? `Only the newest ${commits.length} commits are listed.`
    : `Only the newest ${commits.length} of ${total} commits are listed.`;
}

/** The selection a (shift+)click on the picker's row of `sha` makes:
 * shift spans from the picked commit (a range's newer end when one is
 * picked) to it; a plain click picks it alone. */
export function commitSelectionForClick(
  commits: readonly BranchCommit[],
  total: number | null,
  current: CommitSelection | null,
  sha: string,
  shift: boolean,
): CommitSelection | null {
  if (shift && current) return commitSelectionBetween(commits, total, current.last, sha);
  return commitSelectionBetween(commits, total, sha, sha);
}

export function sameCommitSelection(a: CommitSelection | null, b: CommitSelection | null): boolean {
  if (a === null || b === null) return a === b;
  return a.first === b.first && a.last === b.last;
}
