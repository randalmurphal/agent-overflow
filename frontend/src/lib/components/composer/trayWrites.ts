// Whether this device may make an activity rail tray write. Every one rides
// `threads:operate` on the computer its call goes to: a Stop, a withdrawal,
// Wake now and Cancel to the computer that owns the thread they act on,
// Show in Sidebar to the worker's. Without it the control stays and goes
// inert, saying why (`grantBlockedTitle`), as the sidebar's do.
import type { BackendKey } from '../../transport/backendKey';
import { threadScopeGrant } from '../../transport/entityScopes';
import { scopeGrant, type ScopeGrant } from '../../transport/scopes';

export function trayWriteGrant(to: { threadId: string; projectId?: string | null } | { computer: BackendKey }): ScopeGrant {
  return 'computer' in to
    ? scopeGrant('threads:operate', to.computer)
    : threadScopeGrant('threads:operate', to.threadId, to.projectId);
}
