import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { deferred } from '../../test/helpers/providerAccounts';
import type { BundleState } from './plugins';
import type { TransportHello } from '../transport/wsClient';
import type { BackendKey } from '../transport/backendKey';

const mocks = vi.hoisted(() => ({
  hello: null as ((backend: BackendKey, hello: TransportHello | null) => void) | null,
  state: {} as BundleState,
  fetch: vi.fn(), stage: vi.fn(), discard: vi.fn(), ready: vi.fn(), clear: vi.fn(),
  downloading: vi.fn(), stopped: vi.fn(), stateFails: false,
}));
vi.mock('./platform', () => ({ isNativeShell: () => true }));
vi.mock('./plugins', () => ({ bundlePlugin: async () => ({
  state: async () => {
    if (mocks.stateFails) throw new Error('the native store did not answer');
    return { ...mocks.state };
  },
  ready: async () => {},
  stage: mocks.stage, discardPending: mocks.discard,
}) }));
vi.mock('../stores/transportStatus.svelte', () => ({ onBackendHelloChange: (listener: typeof mocks.hello) => {
  mocks.hello = listener;
  return () => { mocks.hello = null; };
} }));
vi.mock('../stores/bundleNotice.svelte', () => ({
  noteBundleReady: mocks.ready, clearBundleNotice: mocks.clear, noteBundleTooOld: vi.fn(),
  noteBundleDownloading: mocks.downloading, noteBundleDownloadStopped: mocks.stopped,
}));
vi.mock('../transport/lease', () => ({ clientLease: () => 'active', onClientLeaseChange: () => () => {} }));
vi.mock('../transport/networkFetch', () => ({ networkFetch: mocks.fetch }));
vi.mock('../transport/deviceSession', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../transport/deviceSession')>()),
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
  mocks.stateFails = false;
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
  it.each(['manifest', 'archive'])('releases a refused bundle %s response without staging it', async (part) => {
    const response = new Response('unavailable', { status: 503 });
    const cancel = vi.spyOn(response.body!, 'cancel');
    mocks.fetch.mockImplementation(async (url: string) => part === 'archive' && url.endsWith('manifest.json')
      ? Response.json({ id: NEXT, version: '1.1.0', minShellBuild: 9, files: [] }) : response);
    hello();
    await vi.waitFor(() => expect(cancel).toHaveBeenCalledOnce());
    expect(mocks.stage).not.toHaveBeenCalled();
    expect(mocks.ready).not.toHaveBeenCalled();
  });

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
    await vi.waitFor(() => expect(mocks.ready).toHaveBeenCalledWith('1.1.0'));
    expect(mocks.state.next).toBe(NEXT);
    mocks.clear.mockClear();
    hello(CURRENT, '1.0.0');
    await vi.waitFor(() => expect(mocks.clear).toHaveBeenCalled());
    expect(mocks.discard).toHaveBeenCalledExactlyOnceWith({ id: NEXT });
    expect(mocks.state.next).toBe('');
  });

  // What a refusal on the phone tells the person to do comes from these
  // (stores/versionMismatch.ts): each must hold only while it is true.
  it('says a release is downloading until it is staged, and not after a failed fetch', async () => {
    let drop!: (err: Error) => void;
    const archive = new Promise<Response>((_resolve, reject) => { drop = reject; });
    mocks.fetch.mockImplementation(async (url: string) => url.endsWith('manifest.json')
      ? Response.json({ id: NEXT, version: '1.1.0', minShellBuild: 9, files: [] }) : archive);
    hello();
    await vi.waitFor(() => expect(mocks.downloading).toHaveBeenCalledExactlyOnceWith('1.1.0'));
    expect(mocks.stopped).not.toHaveBeenCalled();
    drop(new Error('the connection dropped'));
    await vi.waitFor(() => expect(mocks.stopped).toHaveBeenCalledOnce());
    expect(mocks.ready).not.toHaveBeenCalled();
  });

  it('stops promising a staged release once it is being discarded, even if the store does not answer after', async () => {
    hello();
    await vi.waitFor(() => expect(mocks.ready).toHaveBeenCalledWith('1.1.0'));
    mocks.clear.mockClear();
    mocks.stateFails = true;
    hello(CURRENT, '1.0.0');
    await vi.waitFor(() => expect(mocks.discard).toHaveBeenCalledExactlyOnceWith({ id: NEXT }));
    expect(mocks.clear).toHaveBeenCalled();
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
