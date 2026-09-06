import { afterEach, expect, it, vi } from 'vitest';
import { BootstrapRejectedError, defaultBootstrap } from './bootstrap';
import { clearPairedSession, hasPairedSession, redeemPairing } from './deviceSession';
import { fetchBackendManifest } from './manifestBackends';
import { __resetHomeEndpointForTest, setHomeEndpoint, storeBackendEndpoint } from './homeEndpoint';

const endpoint = 'https://phone-host.test';
afterEach(() => {
  clearPairedSession();
  clearPairedSession('other');
  localStorage.clear();
  __resetHomeEndpointForTest();
  vi.unstubAllGlobals();
});
function grant(credential: string) {
  return new Response(JSON.stringify({
    sessionId: 'paired', credential, expiresAtMs: Date.now() + 3600000,
    refreshSecret: `refresh-${credential}`,
  }), { status: 200 });
}

for (const backend of ['', 'other']) {
  it.each(['renew', 'network', '503', '429', 'revoked'])(
    `recovers ${backend || 'home'} manifest with renewal outcome %s`, async (outcome) => {
      storeBackendEndpoint(backend, endpoint);
      if (backend === '') setHomeEndpoint(endpoint);
      await redeemPairing(
        { v: 1, backendId: 'host', endpoint, token: 'invitation' }, 'Phone',
        (async () => grant('old')) as typeof fetch, backend,
      );
      let manifests = 0;
      const fetcher = vi.fn(async (url: string, init?: RequestInit) => {
        if (url.endsWith('/auth/token')) {
          if (outcome === 'network') throw new TypeError('network unavailable');
          if (outcome === '503' || outcome === '429') return new Response('', { status: Number(outcome) });
          if (outcome === 'revoked') return new Response(JSON.stringify({ reason: 'revoked_session' }), { status: 401 });
          return grant('new');
        }
        const headers = init?.headers as Record<string, string>;
        expect(headers['X-AO-Session']).toBe(manifests++ === 0 ? 'old' : 'new');
        expect(headers['X-AO-Device-Key']).toBeTruthy();
        expect(init?.credentials).toBe('omit');
        return manifests === 1 ? new Response('', { status: 404 }) : new Response(
          JSON.stringify({ backendId: 'host', wsUrl: 'wss://phone-host.test/ws' }),
          { status: 200, headers: { 'content-type': 'application/json' } },
        );
      });
      vi.stubGlobal('fetch', fetcher);
      const request = backend === '' ? defaultBootstrap() : fetchBackendManifest({
        id: backend, backendId: 'host', name: 'Other', wsUrl: 'wss://phone-host.test/ws', bootstrapUrl: `${endpoint}/bootstrap.json`,
      });
      if (outcome === 'renew') {
        await expect(request).resolves.toMatchObject({ wsUrl: 'wss://phone-host.test/ws' });
        expect(manifests).toBe(2);
      } else {
        const error = await request.catch((error: unknown) => error);
        if (outcome === 'revoked') {
          expect(error).toBeInstanceOf(BootstrapRejectedError);
          expect((error as BootstrapRejectedError).paired).toBe(true);
          expect(hasPairedSession(backend)).toBe(false);
        } else {
          expect(error).toBeInstanceOf(Error);
          expect(error).not.toBeInstanceOf(BootstrapRejectedError);
          expect(hasPairedSession(backend)).toBe(true);
        }
      }
      expect(fetcher.mock.calls.filter(([url]) => url.endsWith('/auth/token'))).toHaveLength(1);
    },
  );
}
