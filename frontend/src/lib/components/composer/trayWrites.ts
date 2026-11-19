// Whether this device may make an activity rail tray write. Every one rides
// `threads:operate` on the computer its call goes to: a Stop, a withdrawal,
// Wake now and Cancel to the computer that owns the thread they act on,
// Show in Sidebar to the worker's. Without it the control stays and goes
// inert, saying why, as the sidebar's do.
import type { BackendKey } from '../../transport/backendKey';
import { threadHasScope } from '../../transport/entityScopes';
import { hasScope } from '../../transport/scopes';

export const TRAY_WRITE_UNGRANTED = 'Not granted to this device';

export function trayWriteGranted(to: { threadId: string; projectId?: string | null } | { computer: BackendKey }): boolean {
  return 'computer' in to
    ? hasScope('threads:operate', to.computer)
    : threadHasScope('threads:operate', to.threadId, to.projectId);
}
