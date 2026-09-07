import { afterEach, beforeEach, expect, it, vi } from 'vitest';

const mocks = vi.hoisted(() => ({ capabilities: vi.fn(), fetch: vi.fn() }));
vi.mock('./networkPlugin', () => ({ networkPlugin: async () => ({ getCapabilities: mocks.capabilities }) }));
vi.mock('./networkHttp', () => ({ pinnedFetch: mocks.fetch }));
beforeEach(() => {
  vi.resetModules(); vi.clearAllMocks(); vi.stubGlobal('Capacitor', { isNativePlatform: () => true });
});
afterEach(() => vi.unstubAllGlobals());

it('negotiates native route support and leaves an older shell usable', async () => {
  mocks.capabilities.mockRejectedValue(new Error('not implemented'));
  expect(await (await import('./computerRouteProbe')).canVerifyComputerRoutes()).toBe(false);
  vi.resetModules();
  mocks.capabilities.mockResolvedValue({ computerRoutes: true });
  expect(await (await import('./computerRouteProbe')).canVerifyComputerRoutes()).toBe(true);
});

it.each(['', `sha256:${'a'.repeat(64)}`])('probes identity with explicit TLS trust and no credentials (%s)', async (pin) => {
  mocks.fetch.mockResolvedValue(new Response(JSON.stringify({ backendId: 'gpu' })));
  const { verifyComputerRoute } = await import('./computerRouteProbe');
  const signal = new AbortController().signal;
  await verifyComputerRoute({ endpoint: 'https://gpu', certFingerprint: pin }, 'gpu', signal);
  expect(mocks.fetch).toHaveBeenCalledWith('https://gpu/healthz', { signal, credentials: 'omit', redirect: 'error' }, pin);
});

it('refuses a different computer and bounds health response bytes', async () => {
  const { verifyComputerRoute } = await import('./computerRouteProbe');
  const signal = new AbortController().signal;
  mocks.fetch.mockResolvedValueOnce(new Response(JSON.stringify({ backendId: 'another' })));
  await expect(verifyComputerRoute({ endpoint: 'https://gpu' }, 'gpu', signal)).rejects.toThrow('different computer');
  let cancelled = false;
  mocks.fetch.mockResolvedValueOnce(new Response(new ReadableStream({
    pull(controller) { controller.enqueue(new Uint8Array(65537)); },
    cancel() { cancelled = true; },
  })));
  await expect(verifyComputerRoute({ endpoint: 'https://gpu' }, 'gpu', signal)).rejects.toThrow('too large');
  expect(cancelled).toBe(true);
});

it('bounds native health slots and waiting checks, and releases them after cancellation', async () => {
  const { verifyComputerRoute } = await import('./computerRouteProbe');
  const controller = new AbortController();
  mocks.fetch.mockImplementation((_url, init) => new Promise((_resolve, reject) => init.signal.addEventListener('abort', () => reject(init.signal.reason), { once: true })));
  const pending = Array.from({ length: 40 }, () => verifyComputerRoute({ endpoint: 'https://gpu' }, 'gpu', controller.signal).catch(() => undefined));
  await vi.waitFor(() => expect(mocks.fetch).toHaveBeenCalledTimes(8));
  await expect(verifyComputerRoute({ endpoint: 'https://gpu' }, 'gpu', controller.signal)).rejects.toThrow('busy');
  controller.abort();
  await Promise.all(pending);
  mocks.fetch.mockResolvedValue(new Response(JSON.stringify({ backendId: 'gpu' })));
  await verifyComputerRoute({ endpoint: 'https://gpu' }, 'gpu', new AbortController().signal);
  expect(mocks.fetch).toHaveBeenCalledTimes(9);
});

it('retries a transient capability failure without restarting the frontend', async () => {
  mocks.capabilities.mockRejectedValueOnce(new Error('bridge temporarily unavailable'))
    .mockResolvedValue({ computerRoutes: true });
  const { canVerifyComputerRoutes } = await import('./computerRouteProbe');
  expect(await canVerifyComputerRoutes()).toBe(false);
  expect(await canVerifyComputerRoutes()).toBe(true);
  expect(await canVerifyComputerRoutes()).toBe(true);
  expect(mocks.capabilities).toHaveBeenCalledTimes(2);
});

it('caches explicit older-APK refusal rather than repeatedly invoking a missing method', async () => {
  mocks.capabilities.mockRejectedValue({ code: 'UNIMPLEMENTED' });
  const { canVerifyComputerRoutes } = await import('./computerRouteProbe');
  expect(await canVerifyComputerRoutes()).toBe(false);
  expect(await canVerifyComputerRoutes()).toBe(false);
  expect(mocks.capabilities).toHaveBeenCalledOnce();
});
