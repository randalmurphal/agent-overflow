import type { Thread, ThreadGroup } from '../../types/models';
import { isHiddenThreadMode } from '../../utils/threadModes';

/**
 * Does a thread match the active sidebar search query?
 *
 * `query` must be pre-normalised (trimmed + lowercased); `''` means "no
 * filter" and always matches. A thread matches when the query is a substring
 * of its title or workspace path — the single source of truth for what
 * "matches the search" means in the project-grouped list (`threadsByProject`).
 */
export function threadMatchesQuery(thread: Thread, query: string): boolean {
  if (isHiddenThreadMode(thread.mode)) return false;
  if (!query) return true;
  const hay = `${thread.title ?? ''} ${thread.workspacePath ?? ''}`.toLowerCase();
  return hay.includes(query);
}

/**
 * Does a thread group match the active sidebar search query?
 *
 * Same contract as threadMatchesQuery: `query` is pre-normalised and `''`
 * always matches. A group matches on its NAME only — its members are
 * matched by threadMatchesQuery, and a name match is what pulls the
 * non-matching ones back into view (ProjectsSection).
 */
export function threadGroupMatchesQuery(group: ThreadGroup, query: string): boolean {
  if (!query) return true;
  return group.name.toLowerCase().includes(query);
}
