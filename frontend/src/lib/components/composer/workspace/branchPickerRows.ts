// The branch picker's row list: branches and tags in one order, with the
// tag rows bounded so a repository with thousands of tags renders a fixed
// number of rows.
//
// No search text: default branch; recent picks (branches and tags, most
// recent first); the remaining branches in backend order; the remaining
// tags in backend order (newest first). Searching: default branch; recent
// picks; the remaining matching branches and tags merged by date, newest
// first. Recent picks are keyed by wire ref, so a branch and a tag of one
// name stay distinct.

import type { GitBranch, GitTag } from '../../../types/git';
import { branchRef, tagRef } from '../../../utils/gitRef';

/** Most non-recent tag rows rendered at once. */
export const TAG_ROW_CAP = 50;

export type PickerRow =
  | { kind: 'branch'; ref: string; name: string; branch: GitBranch }
  | { kind: 'tag'; ref: string; name: string; tag: GitTag };

export interface PickerRows {
  rows: PickerRow[];
  /** Matching non-recent tags left out by the cap. */
  hiddenTags: number;
}

function branchRow(branch: GitBranch): PickerRow {
  return { kind: 'branch', ref: branchRef(branch.name), name: branch.name, branch };
}

function tagRow(tag: GitTag): PickerRow {
  return { kind: 'tag', ref: tagRef(tag.name), name: tag.name, tag };
}

function rowDate(row: PickerRow): number {
  return (row.kind === 'branch' ? row.branch.date : row.tag.date) ?? 0;
}

export function orderPickerRows(input: {
  branches: readonly GitBranch[];
  tags: readonly GitTag[];
  /** Recent picks as wire refs, most recent first. */
  recent: readonly string[];
  query: string;
}): PickerRows {
  const needle = input.query.trim().toLowerCase();
  const matches = (name: string): boolean => !needle || name.toLowerCase().includes(needle);
  const recentRank = new Map(input.recent.map((ref, rank) => [ref, rank]));

  const defaults: PickerRow[] = [];
  const recents: { row: PickerRow; rank: number }[] = [];
  const otherBranches: PickerRow[] = [];
  for (const branch of input.branches) {
    if (!matches(branch.name)) continue;
    const row = branchRow(branch);
    const rank = recentRank.get(row.ref);
    if (branch.isDefault) defaults.push(row);
    else if (rank !== undefined) recents.push({ row, rank });
    else otherBranches.push(row);
  }

  // Backend order is newest first, so the first matches in each class are
  // the newest. While searching, a tag named exactly as the query is always
  // kept and tags starting with it fill the cap before other matches, so no
  // tag is out of reach by typing its name. Each bucket holds at most the cap.
  const exactTags: PickerRow[] = [];
  const prefixTags: PickerRow[] = [];
  const innerTags: PickerRow[] = [];
  let matchingTags = 0;
  for (const tag of input.tags) {
    if (!matches(tag.name)) continue;
    const row = tagRow(tag);
    const rank = recentRank.get(row.ref);
    if (rank !== undefined) {
      recents.push({ row, rank });
      continue;
    }
    matchingTags++;
    const name = tag.name.toLowerCase();
    if (needle && name === needle) exactTags.push(row);
    else if (needle && name.startsWith(needle)) {
      if (prefixTags.length < TAG_ROW_CAP) prefixTags.push(row);
    } else if (innerTags.length < TAG_ROW_CAP) innerTags.push(row);
  }
  const otherTags = [...exactTags, ...prefixTags, ...innerTags].slice(
    0,
    Math.max(TAG_ROW_CAP, exactTags.length),
  );
  const hiddenTags = matchingTags - otherTags.length;

  recents.sort((left, right) => left.rank - right.rank);
  let rest: PickerRow[];
  if (needle) {
    // Stable: equal dates keep branches before tags, each in backend order.
    rest = [...otherBranches, ...otherTags].sort((left, right) => rowDate(right) - rowDate(left));
  } else {
    rest = [...otherBranches, ...otherTags];
  }
  return { rows: [...defaults, ...recents.map(({ row }) => row), ...rest], hiddenTags };
}
