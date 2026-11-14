// A checkout or branch switch away from a detached HEAD is never refused;
// the backend reports the commits only that HEAD reached
// (`GitWorkspaceState.leftBehind`) and this warns about them.

import type { GitWorkspaceState } from '../types/git';
import { addToast } from './toast.svelte';

const SHORT_SHA_LENGTH = 8;

export function leftBehindMessage(state: GitWorkspaceState | null | undefined): string | null {
  const left = state?.leftBehind;
  if (!left || left.count <= 0 || !left.commit) return null;
  const sha = left.commit.slice(0, SHORT_SHA_LENGTH);
  const commits = left.count === 1 ? '1 commit' : `${left.count} commits`;
  return `Left ${commits} on no branch at ${sha}. Create a branch from ${sha} to keep them.`;
}

/** Shows the warning when `state` left commits behind. */
export function warnLeftBehind(state: GitWorkspaceState | null | undefined): void {
  const message = leftBehindMessage(state);
  if (message) addToast('warning', message, 15_000);
}
