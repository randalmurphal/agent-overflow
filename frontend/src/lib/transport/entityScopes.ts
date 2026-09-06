// Permission checks use the same owner index as RPC routing. A component
// showing a background pane or sidebar row must not borrow the focused
// computer's grants. Unknown entities use only an unambiguous sole computer,
// matching RPC routing; ambiguity must never borrow local host authority.
import type { BackendKey } from './backendKey';
import { requireEntityBackend } from './backends';
import { automationBackend, projectBackend, threadBackend, workflowItemBackend } from './entityIndex';
import { hasScope, type Scope } from './scopes';

function entityHasScope(scope: Scope, owner: BackendKey | undefined): boolean {
  let backend: BackendKey;
  try { backend = requireEntityBackend(owner); }
  catch { return false; }
  return hasScope(scope, backend);
}

export function threadHasScope(scope: Scope, threadId: string | null | undefined, projectId?: string | null): boolean {
  return entityHasScope(scope, (threadId ? threadBackend(threadId) : undefined)
    ?? (projectId ? projectBackend(projectId) : undefined));
}
export function projectHasScope(scope: Scope, projectId: string | null | undefined): boolean {
  return entityHasScope(scope, projectId ? projectBackend(projectId) : undefined);
}
export function workflowItemHasScope(scope: Scope, itemId: string): boolean {
  return entityHasScope(scope, workflowItemBackend(itemId));
}
export function automationHasScope(scope: Scope, automationId: string): boolean {
  return entityHasScope(scope, automationBackend(automationId));
}
