import type { PRDetail } from '../types/models';

/** The pull request state a local PR read is computed at (`git.PRRevision`):
 *  the head and the base branch's tip as the forge last reported them. */
export interface PRRevision {
  baseRef: string;
  baseSHA: string;
  headSHA: string;
}

/**
 * The revision of a PR snapshot, or null while the forge has not reported
 * one: GitLab leaves the base tip empty while it prepares a merge request's
 * diff, and the next poll carries it.
 */
export function prRevision(detail: PRDetail | null | undefined, liveHeadSHA = ''): PRRevision | null {
  const baseRef = String(detail?.baseRefName ?? '');
  const baseSHA = String(detail?.baseRefSHA ?? '');
  const headSHA = String(liveHeadSHA || detail?.headSHA || '');
  if (baseRef === '' || baseSHA === '' || headSHA === '') return null;
  return { baseRef, baseSHA, headSHA };
}

export function prRevisionKey(revision: PRRevision | null): string {
  return revision ? `${revision.baseRef}\n${revision.baseSHA}\n${revision.headSHA}` : '';
}
