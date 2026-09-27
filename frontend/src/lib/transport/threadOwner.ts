// The computer a thread id belongs to, shared by RPC routing (./runtime.ts),
// the permission checks beside it (./entityScopes.ts) and the machine a
// surface shows (../stores/attachedBackends.svelte.ts). A draft placeholder
// has no thread row until it materializes; the project its id names is on
// the computer that will create it.

import type { BackendKey } from './backendKey';
import { projectBackend, resolveThreadBackend, threadBackend } from './entityIndex';
import { draftPlaceholderProjectId } from '../stores/draftPlaceholderId';

/**
 * The owner of `threadId`, or undefined when it is unknown. Throws when two
 * computers claim the thread.
 */
export function threadOwner(threadId: string): BackendKey | undefined {
  return ownerOf(threadId, resolveThreadBackend);
}

/**
 * `threadOwner` without the refusal: a thread two computers claim answers
 * with its first claimant. For display and grant checks; routing refuses the
 * call itself.
 */
export function threadClaimant(threadId: string): BackendKey | undefined {
  return ownerOf(threadId, threadBackend);
}

function ownerOf(threadId: string, lookup: (threadId: string) => BackendKey | undefined): BackendKey | undefined {
  const draftProject = draftPlaceholderProjectId(threadId);
  return draftProject === undefined ? lookup(threadId) : projectBackend(draftProject);
}
