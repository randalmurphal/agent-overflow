import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { ComputerRouteContext } from './computerRoutes';

const mocks = vi.hoisted(() => ({ supported: vi.fn(), probe: vi.fn(), pinned: vi.fn() }));
vi.mock('../native/computerRouteProbe', () => ({ canVerifyComputerRoutes: mocks.supported, verifyComputerRoute: mocks.probe }));
vi.mock('../native/networkHttp', () => ({ pinnedFetch: mocks.pinned }));
const primary = { endpoint: 'https://original.test', certFingerprint: `sha256:${'a'.repeat(64)}` };
const alternate = { endpoint: 'https://192.168.1.4:8443', certFingerprint: `sha256:${'b'.repeat(64)}` };
const publicRoute = { endpoint: 'https://gpu.tailnet.test' };
const context = (): ComputerRouteContext => ({ backend: 'gpu', backendId: 'gpu', sessionId: 'pair-1', primary, current: () => true });
const request = `${primary.endpoint}/auth/token/recover`;
beforeEach(() => {
  vi.resetModules(); vi.resetAllMocks(); localStorage.clear();
  mocks.supported.mockResolvedValue(true);
  mocks.probe.mockResolvedValue(undefined);
  mocks.pinned.mockImplementation(async () => new Response('{}'));
});
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); vi.useRealTimers(); });

it('surfaces a lost POST reply once, then switches the next request without changing its body or identity', async () => {
  const routes = await import('./computerRoutes');
  const ctx = context();
  await routes.learnComputerRoutes(ctx, [alternate]);
  const original = vi.fn<typeof fetch>().mockRejectedValue(new TypeError('reply lost'));
  const body = new Blob(['the original file']);
  const init = { method: 'POST', body, headers: { 'X-AO-Session': 'credential' } };
  await expect(routes.fetchComputerRoute(ctx, original, request, init)).rejects.toThrow('reply lost');
  expect(mocks.probe).not.toHaveBeenCalled();
  expect(mocks.pinned).not.toHaveBeenCalled();
  const response = await routes.fetchComputerRoute(ctx, original, request, init);
  expect(original).toHaveBeenCalledTimes(1);
  expect(mocks.pinned).toHaveBeenCalledWith(`${alternate.endpoint}/auth/token/recover`, init, alternate.certFingerprint);
  expect(mocks.pinned.mock.calls[0][1].body).toBe(body);
  expect(routes.computerResponseURL(response, request)).toBe(`${alternate.endpoint}/auth/token/recover`);
  expect(routes.computerSocketRoute(ctx, 'wss://original.test/ws?t=one-use')).toEqual({ url: 'wss://192.168.1.4:8443/ws?t=one-use', pin: alternate.certFingerprint });
});

it('coalesces selection, bypasses a stalled route, and cancels losers', async () => {
  const routes = await import('./computerRoutes');
  const ctx = context();
  await routes.learnComputerRoutes(ctx, [alternate, publicRoute]);
  routes.failComputerRoute(ctx, primary.endpoint);
  mocks.probe.mockImplementation((route, _id, signal: AbortSignal) => route.endpoint === alternate.endpoint ? Promise.resolve() : new Promise((_resolve, reject) => signal.addEventListener('abort', () => reject(signal.reason), { once: true })));
  const original = vi.fn<typeof fetch>();
  await Promise.all(Array.from({ length: 12 }, () => routes.fetchComputerRoute(ctx, original, request)));
  expect(mocks.probe).toHaveBeenCalledTimes(3);
  expect(mocks.probe.mock.calls.every((call) => call[2].aborted)).toBe(true);
  expect(mocks.pinned).toHaveBeenCalledTimes(12);
  expect(original).not.toHaveBeenCalled();
});

it('does not let one cancelled request cancel the shared selection', async () => {
  const routes = await import('./computerRoutes');
  const ctx = context();
  await routes.learnComputerRoutes(ctx, [alternate]);
  routes.failComputerRoute(ctx, primary.endpoint);
  let release!: () => void;
  mocks.probe.mockImplementation((route) => route.endpoint === alternate.endpoint ? new Promise<void>((resolve) => { release = resolve; }) : Promise.reject(new Error('offline')));
  const controller = new AbortController();
  const first = routes.fetchComputerRoute(ctx, vi.fn(), request, { signal: controller.signal });
  const rejected = expect(first).rejects.toThrow('cancelled');
  const second = routes.fetchComputerRoute(ctx, vi.fn(), request);
  await vi.waitFor(() => expect(release).toBeTypeOf('function'));
  controller.abort(new Error('cancelled'));
  await rejected;
  release();
  await second;
  expect(mocks.pinned).toHaveBeenCalledTimes(1);
});

it('refuses all failed identity checks before any credential reaches a candidate', async () => {
  const routes = await import('./computerRoutes');
  const ctx = context();
  await routes.learnComputerRoutes(ctx, [alternate]);
  routes.failComputerRoute(ctx, primary.endpoint);
  mocks.probe.mockRejectedValue(new Error('wrong computer or certificate'));
  const original = vi.fn<typeof fetch>();
  await expect(routes.fetchComputerRoute(ctx, original, request)).rejects.toThrow('No verified address');
  expect(original).not.toHaveBeenCalled();
  expect(mocks.pinned).not.toHaveBeenCalled();
});

it('uses WebPKI for a public route without inheriting the original private pin', async () => {
  const routes = await import('./computerRoutes');
  const ctx = context();
  await routes.learnComputerRoutes(ctx, [publicRoute]);
  routes.failComputerRoute(ctx, primary.endpoint);
  const publicFetch = vi.fn().mockResolvedValue(new Response('{}'));
  vi.stubGlobal('fetch', publicFetch);
  await routes.fetchComputerRoute(ctx, vi.fn(), request);
  expect(publicFetch).toHaveBeenCalledWith(`${publicRoute.endpoint}/auth/token/recover`, undefined);
  expect(mocks.pinned).not.toHaveBeenCalled();
  expect(routes.computerSocketRoute(ctx, 'wss://original.test/ws')?.pin).toBeUndefined();
});

it('re-verifies the saved last route after reload and discards hints belonging to a previous pairing', async () => {
  let routes = await import('./computerRoutes');
  const ctx = context();
  await routes.learnComputerRoutes(ctx, [alternate]);
  routes.failComputerRoute(ctx, primary.endpoint);
  await routes.fetchComputerRoute(ctx, vi.fn(), request);
  vi.resetModules(); mocks.probe.mockClear();
  mocks.probe.mockImplementation((route) => route.endpoint === primary.endpoint ? Promise.reject(new Error('offline')) : Promise.resolve());
  routes = await import('./computerRoutes');
  await routes.fetchComputerRoute(ctx, vi.fn(), request);
  expect(mocks.probe).toHaveBeenCalled();
  mocks.probe.mockClear();
  const newPair = { ...ctx, sessionId: 'pair-2' };
  const original = vi.fn<typeof fetch>().mockResolvedValue(new Response('{}'));
  await routes.fetchComputerRoute(newPair, original, request);
  expect(original).toHaveBeenCalledTimes(1);
  expect(mocks.probe).not.toHaveBeenCalled();
});

it('fences removal during selection and leaves another computer usable', async () => {
  const routes = await import('./computerRoutes');
  const ctx = context();
  await routes.learnComputerRoutes(ctx, [alternate]);
  routes.failComputerRoute(ctx, primary.endpoint);
  let release!: () => void;
  mocks.probe.mockImplementation((route) => route.endpoint === alternate.endpoint ? new Promise<void>((resolve) => { release = resolve; }) : Promise.reject(new Error('offline')));
  const pending = routes.fetchComputerRoute(ctx, vi.fn(), request);
  const rejected = expect(pending).rejects.toThrow('pairing changed');
  await vi.waitFor(() => expect(release).toBeTypeOf('function'));
  routes.forgetComputerRoutes(ctx.backend);
  release();
  await rejected;
  expect(mocks.pinned).not.toHaveBeenCalled();
  const original = vi.fn<typeof fetch>().mockResolvedValue(new Response('{}'));
  await routes.fetchComputerRoute({ ...ctx, backend: 'other', backendId: 'other' }, original, request);
  expect(original).toHaveBeenCalledTimes(1);
});

it('checks request credentials after selection without binding future requests to their old generation', async () => {
  const routes = await import('./computerRoutes');
  const ctx = context();
  const original = vi.fn<typeof fetch>().mockResolvedValue(new Response('{}'));
  await expect(routes.fetchComputerRoute({ ...ctx, beforeRequest: () => { throw new Error('session changed'); } }, original, request)).rejects.toThrow('session changed');
  await routes.fetchComputerRoute(ctx, original, request);
  expect(original).toHaveBeenCalledTimes(1);
});

it('does not enable route selection in an older APK or for an unrelated request origin', async () => {
  const routes = await import('./computerRoutes');
  const ctx = context();
  mocks.supported.mockResolvedValue(false);
  await routes.learnComputerRoutes(ctx, [alternate]);
  const original = vi.fn<typeof fetch>().mockResolvedValue(new Response('{}'));
  await routes.fetchComputerRoute(ctx, original, request);
  expect(mocks.probe).not.toHaveBeenCalled();
  expect(localStorage.length).toBe(0);
  mocks.supported.mockResolvedValue(true);
  await expect(routes.fetchComputerRoute(ctx, original, 'https://unrelated.test/auth/token')).rejects.toThrow('does not belong');
  expect(original).toHaveBeenCalledTimes(1);
});

it('keeps reconnecting to an older host without advertisements after its socket fails', async () => {
  const routes = await import('./computerRoutes');
  const ctx = context();
  const original = vi.fn<typeof fetch>().mockImplementation(async () => new Response('{}'));
  await routes.fetchComputerRoute(ctx, original, request);
  routes.failComputerRoute(ctx, primary.endpoint);
  await routes.fetchComputerRoute(ctx, original, request);
  expect(routes.computerSocketRoute(ctx, 'wss://original.test/ws')?.url).toBe('wss://original.test/ws');
  expect(() => routes.computerSocketRoute(ctx, 'wss://user:secret@original.test/ws')).toThrow('Invalid computer socket');
  expect(() => routes.computerSocketRoute(ctx, 'wss://original.test/ws#fragment')).toThrow('Invalid computer socket');
  expect(mocks.probe).not.toHaveBeenCalled();
});

it('keeps authenticated candidates usable when saving optional address hints fails', async () => {
  const routes = await import('./computerRoutes');
  const ctx = context();
  localStorage.setItem('draft', 'unfinished message');
  vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw new Error('Quota exceeded'); });
  await routes.learnComputerRoutes(ctx, [alternate]);
  routes.failComputerRoute(ctx, primary.endpoint);
  await routes.fetchComputerRoute(ctx, vi.fn(), request);
  expect(mocks.pinned).toHaveBeenCalledTimes(1);
  expect(localStorage.getItem('draft')).toBe('unfinished message');
});

it('repairs a new address without sending a credential or changing the paired origin', async () => {
  const routes = await import('./computerRoutes');
  const ctx = context();
  const endpoint = 'https://192.168.1.55:9443';
  expect(await routes.repairComputerAddress(ctx, endpoint)).toBe(endpoint);
  expect(mocks.pinned).not.toHaveBeenCalled();
  expect(mocks.probe).toHaveBeenCalledWith({ endpoint, certFingerprint: primary.certFingerprint }, ctx.backendId, expect.any(AbortSignal));
  const held = JSON.parse(localStorage.getItem('agent-overflow:computerRoutes:gpu')!);
  expect(held).toMatchObject({ sessionId: ctx.sessionId, primary, lastEndpoint: endpoint });
  await routes.fetchComputerRoute(ctx, vi.fn(), request);
  expect(mocks.pinned).toHaveBeenCalledWith(`${endpoint}/auth/token/recover`, undefined, primary.certFingerprint);
});

it.each(['removed', 'trust-replaced'])('rejects a delayed address repair after the computer is %s', async (change) => {
  const routes = await import('./computerRoutes');
  const ctx = context();
  let release!: () => void;
  mocks.probe.mockImplementation(() => new Promise<void>((resolve) => { release = resolve; }));
  const pending = routes.repairComputerAddress(ctx, alternate.endpoint);
  const rejected = expect(pending).rejects.toThrow(change === 'removed' ? 'pairing changed' : 'trust changed');
  await vi.waitFor(() => expect(release).toBeTypeOf('function'));
  if (change === 'removed') routes.forgetComputerRoutes(ctx.backend);
  else await routes.learnComputerRoutes(ctx, [{ ...primary, certFingerprint: alternate.certFingerprint }]);
  release();
  await rejected;
  expect(mocks.pinned).not.toHaveBeenCalled();
  expect(localStorage.getItem('agent-overflow:computerRoutes:gpu') ?? '').not.toContain(alternate.endpoint);
});
