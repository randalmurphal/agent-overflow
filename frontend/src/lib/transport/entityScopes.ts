// Permission checks use the same owner index as RPC routing. A component
// showing a background pane or sidebar row must not borrow the focused
// computer's grants. Unknown entities use only an unambiguous sole computer,
// matching RPC routing; ambiguity must never borrow local host authority.
//
// Each entity answers twice: `…HasScope` for a gate, which is false for a
// refusal and for a computer that has not answered, and `…ScopeGrant` for
// a surface that says why a control is off (`grantBlockedTitle`). Both
// read one rule, `entityScopeGrant`.
import type { BackendKey } from './backendKey';
import { requireEntityBackend } from './backends';
import { projectBackend, workflowItemBackend } from './entityIndex';
import { scopeGrant, type EntityScopeGrant, type Scope } from './scopes';
import { threadClaimant } from './threadOwner';

type GrantableScope = Exclude<Scope, 'host'>;

/** An entity whose computer routing cannot name is `unrouted`. */
function entityScopeGrant(scope: GrantableScope, owner: BackendKey | undefined): EntityScopeGrant {
  let backend: BackendKey;
  try { backend = requireEntityBackend(owner); }
  catch { return 'unrouted'; }
  return scopeGrant(scope, backend);
}

function threadOwnerOf(threadId: string | null | undefined, projectId?: string | null): BackendKey | undefined {
  return (threadId ? threadClaimant(threadId) : undefined) ?? (projectId ? projectBackend(projectId) : undefined);
}

export function threadScopeGrant(
  scope: GrantableScope, threadId: string | null | undefined, projectId?: string | null,
): EntityScopeGrant {
  return entityScopeGrant(scope, threadOwnerOf(threadId, projectId));
}
export function threadHasScope(scope: GrantableScope, threadId: string | null | undefined, projectId?: string | null): boolean {
  return threadScopeGrant(scope, threadId, projectId) === 'granted';
}

export function projectScopeGrant(scope: GrantableScope, projectId: string | null | undefined): EntityScopeGrant {
  return entityScopeGrant(scope, projectId ? projectBackend(projectId) : undefined);
}
export function projectHasScope(scope: GrantableScope, projectId: string | null | undefined): boolean {
  return projectScopeGrant(scope, projectId) === 'granted';
}

export function workflowItemScopeGrant(scope: GrantableScope, itemId: string): EntityScopeGrant {
  return entityScopeGrant(scope, workflowItemBackend(itemId));
}
export function workflowItemHasScope(scope: GrantableScope, itemId: string): boolean {
  return workflowItemScopeGrant(scope, itemId) === 'granted';
}
