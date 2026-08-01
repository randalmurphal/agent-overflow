import type { Thread } from '../../types/models';
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
