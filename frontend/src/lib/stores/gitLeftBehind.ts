// A checkout or branch switch away from a detached HEAD is never refused;
// the backend reports the commits only that HEAD reached
// (`GitWorkspaceState.leftBehind`) and this warns about them.

import type { GitWorkspaceState } from '../types/git';
import { shortSha } from '../utils/checkout';
import { addToast } from './toast.svelte';

function leftBehindMessage(state: GitWorkspaceState | null | undefined): string | null {
  const left = state?.leftBehind;
  if (!left || left.count <= 0 || !left.commit) return null;
  const sha = shortSha(left.commit);
  const commits = left.count === 1 ? '1 commit' : `${left.count} commits`;
  return `Left ${commits} on no branch at ${sha}. Create a branch from ${sha} to keep them.`;
}

/** Shows the warning when `state` left commits behind. */
export function warnLeftBehind(state: GitWorkspaceState | null | undefined): void {
  const message = leftBehindMessage(state);
  if (message) addToast('warning', message, 15_000);
}
