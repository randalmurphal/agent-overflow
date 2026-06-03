import type { Thread } from '../../types/models';
import { threadMatchesQuery } from './threadSearch';

/**
 * Select the project-less "home" terminal threads that belong in the
 * standalone Terminals sidebar group.
 *
 * Excluded: archived threads, non-terminal threads, and per-project terminals
 * (those carry a `projectId` and stay mixed into their project's list,
 * badge-distinguished). Order is preserved from the input — the sidebar passes
 * `getThreads()`, which the backend already returns newest-touched-first.
 *
 * `query` must be pre-normalised (trimmed + lowercased); pass `''` for no
 * filter. When set, a terminal is kept only if it matches the query — using
 * the same `threadMatchesQuery` predicate the project list filters with.
 */
export function selectStandaloneTerminals(
  threads: readonly Thread[],
  query: string,
): Thread[] {
  const out: Thread[] = [];
  for (const t of threads) {
    if (t.archived) continue;
    if (t.mode !== 'terminal') continue;
    if (t.projectId) continue;
    if (!threadMatchesQuery(t, query)) continue;
    out.push(t);
  }
  return out;
}
