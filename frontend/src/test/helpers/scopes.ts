// Enrol a paired session holding an exact grant set, the way the pairing
// flow does it: redeem a link, then re-resolve — which is
// `wsClient.redialAfterPairing()`'s job in production.
//
// Shared because half a dozen suites stage the same three lines, and
// because the OBSERVE set is a fact about the backend
// (identity.ObserveScopes) that a test restating it by hand would drift
// from silently.

import { vi } from 'vitest';
import {
  clearPairedSession,
  redeemPairing,
  type PairingPayload,
} from '../../lib/transport/deviceSession';
import { refreshGrantedScopes, setPageGrantsFromBootstrap } from '../../lib/transport/scopes';

const PAYLOAD: PairingPayload = {
  v: 1,
  backendId: 'backend-1',
  endpoint: 'http://192.168.1.20:8123',
  token: 'link-token',
};

/**
 * The grant set a `view-only` pairing link mints. Mirrors
 * identity.ObserveScopes, which is pinned to transport's observe tier by
 * `TestObserveScopesAreTheObserveTier`.
 */
export const OBSERVE_SCOPES = ['threads:read', 'files:read', 'settings:read'] as const;

/** Put the page in a paired session holding exactly `scopes`. */
export async function pairWithScopes(scopes: readonly string[]): Promise<void> {
  // The page is networked: a paired session wins over locality anyway,
  // but staging a loopback page would hide a gate written against the
  // wrong axis.
  setPageGrantsFromBootstrap(true);
  const fetcher = vi.fn(async () => new Response(
    JSON.stringify({
      sessionId: 'sess-1',
      credential: 'cred-1',
      expiresAtMs: Date.now() + 15 * 60_000,
      scopes: [...scopes],
    }),
    { status: 200, headers: { 'Content-Type': 'application/json' } },
  )) as unknown as typeof fetch;
  await redeemPairing(PAYLOAD, 'a paired device', fetcher);
  refreshGrantedScopes();
}

/** Put the page in a view-only session: the observe set and nothing else. */
export function pairViewOnly(): Promise<void> {
  return pairWithScopes(OBSERVE_SCOPES);
}

/** Back to the owner's own screen, which holds every grantable scope. */
export function resetToLocalPage(): void {
  clearPairedSession();
  setPageGrantsFromBootstrap(false);
  refreshGrantedScopes();
}
