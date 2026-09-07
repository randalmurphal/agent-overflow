import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { installComputerRouteUpdates } from './computerRouteUpdates';
import { refreshComputerRoutes } from '../transport/bootstrap';
import { detachBackend } from '../transport/backends';
import { stageBackend, resetStagedBackends } from '../../test/helpers/backends';
import { emitWailsEvent } from '../../test/mocks/wailsio-runtime';
import type { TransportHello } from '../transport/wsClient';

vi.mock('../transport/bootstrap', async (original) => ({
  ...await original<typeof import('../transport/bootstrap')>(), refreshComputerRoutes: vi.fn(async () => {}),
}));
const A = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const B = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
const hello: TransportHello = { backendId: A, backendName: 'Mac', capabilities: ['computer-routes.v1'], protocolVersion: 1,
  serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0 };
const refresh = vi.mocked(refreshComputerRoutes);
let stop = () => {};
const drain = async () => { for (let i = 0; i < 10; i++) await Promise.resolve(); };
function stage(id = A) { return stageBackend({ id, backendId: id, hello: { ...hello, backendId: id } }); }
beforeEach(() => { resetStagedBackends(); localStorage.clear(); resetBindingMocks(); refresh.mockReset().mockResolvedValue();
  setBindingMock('GetComputerRoutes', () => { throw new Error('route RPC unavailable'); }); });
afterEach(() => { stop(); resetStagedBackends(); vi.useRealTimers(); });

it('coalesces startup, then refreshes only the changed computer without reconnecting', async () => {
  const a = stage(); const b = stage(B);
  stop = installComputerRouteUpdates(); await drain();
  expect(refresh).toHaveBeenCalledTimes(2);
  refresh.mockClear();
  for (let i = 0; i < 10; i++) emitWailsEvent('computer-routes:changed', {}, A);
  await drain();
  expect(refresh).toHaveBeenCalledOnce();
  expect(refresh.mock.calls[0][0]?.id).toBe(A);
  expect(a.reconnect).not.toHaveBeenCalled(); expect(b.reconnect).not.toHaveBeenCalled();
  emitWailsEvent('transport:gap', { channel: 'threads:changed' }, B); await drain();
  expect(refresh).toHaveBeenCalledOnce();
  emitWailsEvent('transport:gap', { channel: 'computer-routes:changed' }, B); await drain();
  expect(refresh).toHaveBeenCalledTimes(2);
  expect(refresh.mock.calls[1][0]?.id).toBe(B);
});

it('supersedes an in-flight snapshot and reads one fresh snapshot after a burst', async () => {
  stage();
  let finish!: () => void;
  refresh.mockImplementationOnce(() => new Promise<void>((resolve) => { finish = resolve; }));
  stop = installComputerRouteUpdates(); await drain();
  const current = refresh.mock.calls[0][2]; expect(current()).toBe(true);
  for (let i = 0; i < 10; i++) emitWailsEvent('computer-routes:changed', {}, A);
  await drain(); expect(current()).toBe(false); expect(refresh).toHaveBeenCalledOnce();
  finish(); await drain();
  expect(refresh).toHaveBeenCalledTimes(2); expect(refresh.mock.calls[1][2]()).toBe(true);
});

it.each(['disconnect', 'remove', 'replace', 'stop'] as const)('rejects a late snapshot after %s', async (reason) => {
  const backend = stage(); let finish!: () => void;
  refresh.mockImplementationOnce(() => new Promise<void>((resolve) => { finish = resolve; }));
  stop = installComputerRouteUpdates(); await drain();
  const [, , current, signal] = refresh.mock.calls[0];
  if (reason === 'disconnect') backend.setStatus('reconnecting');
  else if (reason === 'stop') stop();
  else { detachBackend(A); if (reason === 'replace') stage(); }
  expect(current()).toBe(false); expect(signal.aborted).toBe(true);
  finish(); await drain();
  expect(refresh).toHaveBeenCalledTimes(reason === 'replace' ? 2 : 1);
  if (reason === 'disconnect') { backend.setStatus('connected'); await drain(); expect(refresh).toHaveBeenCalledTimes(2); }
});

it('retries a failed refresh on a healthy socket and cancels pending retries on disconnect', async () => {
  vi.useFakeTimers(); const backend = stage(); refresh.mockRejectedValue(new Error('temporary failure'));
  stop = installComputerRouteUpdates(); await vi.advanceTimersByTimeAsync(0);
  expect(refresh).toHaveBeenCalledOnce();
  await vi.advanceTimersByTimeAsync(1000); expect(refresh).toHaveBeenCalledTimes(2);
  backend.setStatus('reconnecting'); await vi.advanceTimersByTimeAsync(60_000);
  expect(refresh).toHaveBeenCalledTimes(2);
  refresh.mockResolvedValue(); backend.setStatus('connected'); await vi.advanceTimersByTimeAsync(0);
  expect(refresh).toHaveBeenCalledTimes(3); expect(backend.reconnect).not.toHaveBeenCalled();
});

it('does not start route refreshes against an older host', async () => {
  const backend = stage(); backend.setHello({ ...hello, capabilities: [] });
  stop = installComputerRouteUpdates(); emitWailsEvent('computer-routes:changed', {}, A); await drain();
  expect(refresh).not.toHaveBeenCalled();
});

it('recovers an attached desktop port through the surviving socket and existing verified repair', async () => {
  stage(); refresh.mockRejectedValueOnce(new Error('old HTTP listener closed'));
  const snapshot = setBindingMock('GetComputerRoutes', async () => [{ endpoint: 'https://192.168.1.55:4444' }]);
  const repair = setBindingMock('RepairBackendAddress', async () => 'https://192.168.1.55:4444');
  stop = installComputerRouteUpdates();
  await vi.waitFor(() => expect(refresh).toHaveBeenCalledTimes(2));
  expect(snapshot).toHaveBeenCalledOnce();
  expect(repair).toHaveBeenCalledExactlyOnceWith(A, 'https://192.168.1.55:4444');
});

it('does not apply a route RPC that finishes after the connection was removed', async () => {
  stage(); refresh.mockRejectedValueOnce(new Error('old HTTP listener closed'));
  let finish!: (routes: { endpoint: string }[]) => void;
  const snapshot = setBindingMock('GetComputerRoutes', () => new Promise<{ endpoint: string }[]>((resolve) => { finish = resolve; }));
  const repair = setBindingMock('RepairBackendAddress', async () => 'unused');
  stop = installComputerRouteUpdates(); await vi.waitFor(() => expect(snapshot).toHaveBeenCalledOnce());
  detachBackend(A); finish([{ endpoint: 'https://192.168.1.55:4444' }]); await drain();
  expect(repair).not.toHaveBeenCalled(); expect(refresh).toHaveBeenCalledOnce();
});

it('retries after the HTTP deadline instead of treating its abort as a successful refresh', async () => {
  vi.useFakeTimers(); stage();
  refresh.mockImplementationOnce((_descriptor, _id, _current, signal) => new Promise<void>((_resolve, reject) => {
    signal.addEventListener('abort', () => reject(signal.reason), { once: true });
  }));
  stop = installComputerRouteUpdates(); await vi.advanceTimersByTimeAsync(20_000);
  expect(refresh).toHaveBeenCalledOnce();
  await vi.advanceTimersByTimeAsync(1_000); expect(refresh).toHaveBeenCalledTimes(2);
});

it('rejects a pending snapshot when the pairing changes under the same client', async () => {
  stage(); let finish!: () => void;
  refresh.mockImplementationOnce(() => new Promise<void>((resolve) => { finish = resolve; }));
  stop = installComputerRouteUpdates(); await drain();
  const current = refresh.mock.calls[0][2]; expect(current()).toBe(true);
  localStorage.setItem(`agent-overflow:deviceSession:${A}`, JSON.stringify({ backendId: A, sessionId: 'new-pairing', credential: 'new-credential' }));
  expect(current()).toBe(false); finish(); await drain();
  expect(refresh).toHaveBeenCalledOnce();
});

it('can use the live socket after HTTP times out, without canceling route recovery with that HTTP request', async () => {
  vi.useFakeTimers(); stage();
  refresh.mockImplementationOnce((_descriptor, _id, _current, signal) => new Promise<void>((_resolve, reject) => {
    signal.addEventListener('abort', () => reject(signal.reason), { once: true });
  }));
  const snapshot = setBindingMock('GetComputerRoutes', async () => [{ endpoint: 'https://192.168.1.55:4444' }]);
  const repair = setBindingMock('RepairBackendAddress', async () => 'https://192.168.1.55:4444');
  stop = installComputerRouteUpdates(); await vi.advanceTimersByTimeAsync(20_000);
  expect(snapshot).toHaveBeenCalledOnce(); expect(repair).toHaveBeenCalledOnce(); expect(refresh).toHaveBeenCalledTimes(2);
  await vi.advanceTimersByTimeAsync(30_000); expect(refresh).toHaveBeenCalledTimes(2);
});
