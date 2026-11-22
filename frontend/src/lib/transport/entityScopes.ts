// Permission checks use the same owner index as RPC routing. A component
// showing a background pane or sidebar row must not borrow the focused
// computer's grants. Unknown entities use only an unambiguous sole computer,
// matching RPC routing; ambiguity must never borrow local host authority.
import type { BackendKey } from './backendKey';
import { requireEntityBackend } from './backends';
import { projectBackend, workflowItemBackend } from './entityIndex';
import { hasScope, scopeGrant, type Scope, type ScopeGrant } from './scopes';
import { threadClaimant } from './threadOwner';

function entityHasScope(scope: Scope, owner: BackendKey | undefined): boolean {
  let backend: BackendKey;
  try { backend = requireEntityBackend(owner); }
  catch { return false; }
  return hasScope(scope, backend);
}

export function threadHasScope(scope: Scope, threadId: string | null | undefined, projectId?: string | null): boolean {
  return entityHasScope(scope, threadOwnerOf(threadId, projectId));
}

/**
 * `threadHasScope` with its third answer kept (`scopeGrant`), for a
 * surface that tells the person a capability was not granted. A thread
 * whose computer is ambiguous has no known grant.
 */
export function threadScopeGrant(
  scope: Exclude<Scope, 'host'>, threadId: string | null | undefined, projectId?: string | null,
): ScopeGrant {
  let backend: BackendKey;
  try { backend = requireEntityBackend(threadOwnerOf(threadId, projectId)); }
  catch { return 'unknown'; }
  return scopeGrant(scope, backend);
}

function threadOwnerOf(threadId: string | null | undefined, projectId?: string | null): BackendKey | undefined {
  return (threadId ? threadClaimant(threadId) : undefined) ?? (projectId ? projectBackend(projectId) : undefined);
}
export function projectHasScope(scope: Scope, projectId: string | null | undefined): boolean {
  return entityHasScope(scope, projectId ? projectBackend(projectId) : undefined);
}
export function workflowItemHasScope(scope: Scope, itemId: string): boolean {
  return entityHasScope(scope, workflowItemBackend(itemId));
}
