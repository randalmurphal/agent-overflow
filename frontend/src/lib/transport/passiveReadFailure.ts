import { DisconnectedError, TransportError } from './wsClient';
import { ReadDeadlineError } from '../utils/readBeforeDeadline';

/** Connection/auth banners own these failures during automatic reads. Keep
 * ordinary RPC/file errors visible. Never use this to hide a failed mutation
 * or an explicit user action: their outcome must still be reported. Restoring
 * focus/read-marker bookkeeping is automatic and carries no user work. */
export function isPassiveConnectionFailure(error: unknown): boolean {
  return error instanceof DisconnectedError
    || error instanceof ReadDeadlineError
    || (error instanceof TransportError && error.code === 'auth_failed');
}
