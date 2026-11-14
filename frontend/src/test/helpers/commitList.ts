/** A commit list answer (ListPRCommits, ListBranchCommits) holding every
 * commit of its range. */
export function commitList<T>(commits: T[]): { commits: T[]; total: number } {
  return { commits, total: commits.length };
}
