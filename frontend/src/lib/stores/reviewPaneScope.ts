// Per-thread persistence of the review pane's scope selection.
//
// Reopening the review companion should land where the user left it, so
// the scope (and, in branch scope, the base branch it was diffed against)
// round-trips through appStorage. A malformed or unknown value reads as
// "nothing persisted" rather than throwing — the pane's default is always
// a valid answer.

import { appStorageGet, appStorageSet } from './appStorage';
import type { DiffReviewScope } from '../types/models';

export interface PersistedReviewScope {
  scope: DiffReviewScope;
  baseBranch?: string | null;
}

function storageKey(threadId: string): string {
  return `reviewScope:${threadId}`;
}

export function readPersistedScope(threadId: string): PersistedReviewScope | null {
  const raw = appStorageGet(storageKey(threadId));
  if (!raw) return null;
  try {
    const parsed = JSON.parse(raw) as Partial<PersistedReviewScope>;
    if (!isReviewScope(parsed.scope)) return null;
    return {
      scope: parsed.scope,
      baseBranch: typeof parsed.baseBranch === 'string' ? parsed.baseBranch : null,
    };
  } catch {
    return null;
  }
}

export function persistScope(
  threadId: string,
  scope: DiffReviewScope,
  baseBranch: string | null,
): void {
  appStorageSet(storageKey(threadId), JSON.stringify({ scope, baseBranch }));
}

export function isReviewScope(value: unknown): value is DiffReviewScope {
  return value === 'workspace' || value === 'branch' || value === 'pr' || value === 'edits';
}
