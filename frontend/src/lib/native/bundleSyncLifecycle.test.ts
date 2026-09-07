import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { deferred } from '../../test/helpers/providerAccounts';
import type { BundleState } from './plugins';
import type { TransportHello } from '../transport/wsClient';
import type { BackendKey } from '../transport/backendKey';

const mocks = vi.hoisted(() => ({
  hello: null as ((backend: BackendKey, hello: TransportHello | null) => void) | null,
  state: {} as BundleState,
  fetch: vi.fn(), stage: vi.fn(), discard: vi.fn(), ready: vi.fn(), clear: vi.fn(),
}));
vi.mock('./platform', () => ({ isNativeShell: () => true }));
vi.mock('./plugins', () => ({ bundlePlugin: async () => ({
  state: async () => ({ ...mocks.state }), ready: async () => {},
  stage: mocks.stage, discardPending: mocks.discard,
}) }));
vi.mock('../stores/transportStatus.svelte', () => ({ onBackendHelloChange: (listener: typeof mocks.hello) => {
  mocks.hello = listener;
  return () => { mocks.hello = null; };
} }));
vi.mock('../stores/bundleNotice.svelte', () => ({ noteBundleReady: mocks.ready, clearBundleNotice: mocks.clear, noteBundleTooOld: vi.fn() }));
vi.mock('../transport/lease', () => ({ clientLease: () => 'active', onClientLeaseChange: () => () => {} }));
vi.mock('../transport/networkFetch', () => ({ networkFetch: mocks.fetch }));
vi.mock('../transport/deviceSession', () => ({
  pairedSessionHeaders: async () => ({}),
  fetchPairedComputer: (_backend: unknown, fetch: typeof mocks.fetch, url: string, options: unknown) => fetch(url, options),
}));
vi.mock('../transport/homeEndpoint', () => ({ backendUrl: (path: string) => path, backendCredentials: () => 'omit' }));

import { startBundleSync, stopBundleSync } from './bundleSync';
import { HOME_BACKEND } from '../transport/backendKey';
const CURRENT = 'a'.repeat(64), NEXT = 'b'.repeat(64);
function hello(bundleId = NEXT, bundleVersion = '1.1.0') {
  mocks.hello!(HOME_BACKEND, { bundleId, bundleVersion, minShellBuild: 9, backendName: 'Mac' } as TransportHello);
}
beforeEach(async () => {
  vi.clearAllMocks();
  mocks.state = { current: CURRENT, next: '', pendingHealth: '', lastKnownGood: CURRENT,
    rolledBack: [], versionCode: 9, orderedUpdates: true, packagedVersion: '1.0.0', currentVersion: '1.0.0', nextVersion: '' };
  mocks.fetch.mockImplementation(async (url: string) => url.endsWith('manifest.json')
    ? Response.json({ id: NEXT, version: '1.1.0', minShellBuild: 9, files: [] }) : new Response('archive'));
  mocks.stage.mockImplementation(async ({ id }: { id: string }) => { mocks.state.next = id; mocks.state.nextVersion = '1.1.0'; });
  mocks.discard.mockImplementation(async ({ id }: { id: string }) => {
    if (mocks.state.next === id) { mocks.state.next = ''; mocks.state.nextVersion = ''; }
  });
  await startBundleSync();
});
afterEach(() => stopBundleSync());

describe('bundle selection across asynchronous boundaries', () => {
  it('keeps newer installed code without downloading or announcing an older host', async () => {
    hello(NEXT, '0.9.0');
    await Promise.resolve();
    expect(mocks.fetch).not.toHaveBeenCalled();
    expect(mocks.ready).not.toHaveBeenCalled();
  });

  it('does not stage a download after the host returns to the running bundle', async () => {
    const archive = deferred<Response>();
    mocks.fetch.mockImplementation(async (url: string) => url.endsWith('manifest.json')
      ? Response.json({ id: NEXT, version: '1.1.0' }) : archive.promise);
    hello();
    await vi.waitFor(() => expect(mocks.fetch).toHaveBeenCalledTimes(2));
    hello(CURRENT, '1.0.0');
    archive.resolve(new Response('old archive'));
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(mocks.stage).not.toHaveBeenCalled();
    expect(mocks.ready).not.toHaveBeenCalled();
  });

  it.each(['host change', 'stop'])('discards a native stage completed after %s', async (change) => {
    const staged = deferred<void>();
    mocks.stage.mockImplementation(async ({ id }: { id: string }) => { await staged.promise; mocks.state.next = id; });
    hello();
    await vi.waitFor(() => expect(mocks.stage).toHaveBeenCalledTimes(1));
    if (change === 'stop') stopBundleSync();
    else hello(CURRENT, '1.0.0');
    staged.resolve();
    await vi.waitFor(() => expect(mocks.discard).toHaveBeenCalledExactlyOnceWith({ id: NEXT }));
    expect(mocks.state.next).toBe('');
    expect(mocks.ready).not.toHaveBeenCalled();
  });

  it('removes a queued update and its notice when the selected host changes', async () => {
    hello();
    await vi.waitFor(() => expect(mocks.ready).toHaveBeenCalled());
    expect(mocks.state.next).toBe(NEXT);
    mocks.clear.mockClear();
    hello(CURRENT, '1.0.0');
    await vi.waitFor(() => expect(mocks.clear).toHaveBeenCalled());
    expect(mocks.discard).toHaveBeenCalledExactlyOnceWith({ id: NEXT });
    expect(mocks.state.next).toBe('');
  });

  it('requires the manifest version to match the selected offer', async () => {
    mocks.fetch.mockResolvedValue(Response.json({ id: NEXT, version: '0.9.0' }));
    hello();
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(mocks.stage).not.toHaveBeenCalled();
    expect(mocks.ready).not.toHaveBeenCalled();
    expect(mocks.fetch).toHaveBeenCalledTimes(1);
  });
});
