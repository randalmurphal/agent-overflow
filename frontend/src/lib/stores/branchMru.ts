// Per-project MRU of branches the user explicitly selected in the
// BranchPicker (checkout, worktree attach, branch creation). The picker
// reads this once per open to lift recently-used branches above the
// backend's committerdate ordering. Bounded so a long-lived project's
// entry can't grow with repo age. Base picks during branch creation are
// deliberately NOT recorded — they're inputs to a new branch, not a
// switch to the base, and would crowd the list with main/develop.
//
// appStorage stores opaque strings; this module owns the JSON shape.

import { appStorageGet, appStorageSet } from './appStorage';

const KEY_PREFIX = 'branch-mru:';
export const BRANCH_MRU_MAX_ENTRIES = 15;

function keyFor(projectId: string): string {
  return `${KEY_PREFIX}${projectId}`;
}

/** Most-recently-selected first. Unknown/corrupt storage reads as empty. */
export function recentBranchSelections(projectId: string): string[] {
  if (!projectId) return [];
  const raw = appStorageGet(keyFor(projectId));
  if (!raw) return [];
  try {
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) {
      console.warn('branch MRU storage held a non-array; treating as empty:', raw);
      return [];
    }
    return parsed.filter((value): value is string => typeof value === 'string' && value !== '');
  } catch (err) {
    console.warn('branch MRU storage was unparseable; treating as empty:', err);
    return [];
  }
}

export function recordBranchSelection(projectId: string, branch: string): void {
  const name = branch.trim();
  if (!projectId || !name) return;
  const next = [
    name,
    ...recentBranchSelections(projectId).filter((existing) => existing !== name),
  ].slice(0, BRANCH_MRU_MAX_ENTRIES);
  appStorageSet(keyFor(projectId), JSON.stringify(next));
}
