import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { deferred } from '../../test/helpers/providerAccounts';
import { certificatePin, pairingEndpoint } from './networkTrust';
import type { SocketEvent } from './networkPlugin';
let pinnedFetch: typeof import('./networkHttp').pinnedFetch;
let networkFetch: typeof import('../transport/networkFetch').networkFetch;

const bridge = vi.hoisted(() => ({
  httpStart: vi.fn(async (): Promise<void> => undefined),
  httpHeaders: vi.fn(async (): Promise<{ status: number; headers: Record<string, string> }> => ({ status: 200, headers: { 'content-type': 'text/plain' } })),
  httpWrite: vi.fn(async () => undefined),
  httpRead: vi.fn(async () => ({ data: '' })),
  httpClose: vi.fn(async () => undefined),
  socketOpen: vi.fn(async (_options: { id: string; url: string; pin: string }): Promise<void> => undefined),
  socketAck: vi.fn(async () => undefined),
  socketSend: vi.fn(async () => undefined),
  socketClose: vi.fn(async () => undefined),
  addListener: vi.fn(async (_type: string, _listener: (event: SocketEvent) => void) => ({ remove: async () => undefined })),
}));
vi.mock('./networkPlugin', () => ({ networkPlugin: async () => bridge }));

const PIN = `sha256:${'a'.repeat(64)}`;
const payload = { v: 1, backendId: 'gpu', endpoint: 'http://192.168.1.50:7777', token: 'secret', certFingerprint: PIN };

beforeEach(async () => {
  vi.clearAllMocks();
  vi.resetModules();
  localStorage.clear();
  vi.stubGlobal('Capacitor', { isNativePlatform: () => true });
  // Bind the adapters to this case's mocked plugin after resetting the module
  // graph; the shared test setup can load the real transport before mocks run.
  ({ pinnedFetch } = await import('./networkHttp'));
  ({ networkFetch } = await import('../transport/networkFetch'));
});
afterEach(() => vi.unstubAllGlobals());

describe('private LAN trust', () => {
  it('promotes pairing to TLS, persists exact trust and leaves other computers alone', () => {
    expect(pairingEndpoint(payload)).toBe('https://192.168.1.50:7777');
    expect(certificatePin('wss://192.168.1.50:7777/ws?ticket=one')).toBe(PIN);
    expect(certificatePin('https://192.168.1.51:7777/bootstrap.json')).toBeNull();
    expect(certificatePin('https://192.168.1.50:7778/bootstrap.json')).toBeNull();
    expect(JSON.parse(localStorage.getItem('agent-overflow:certificatePins')!)).toEqual({ 'https://192.168.1.50:7777': PIN });
  });

  it.each(['sha256:wrong', '', 'https://attacker.test'])('never trusts an invalid pin: %s', (pin) => {
    if (!pin) expect(pairingEndpoint({ ...payload, certFingerprint: pin })).toBe(payload.endpoint);
    else expect(() => pairingEndpoint({ ...payload, certFingerprint: pin })).toThrow(/fingerprint/);
    expect(certificatePin('https://192.168.1.50:7777/bootstrap.json')).toBeNull();
  });

  it('repairs one origin in damaged saved trust while other origins still fail closed', () => {
    localStorage.setItem('agent-overflow:certificatePins', '{broken');
    expect(() => certificatePin('https://gpu.test/bootstrap.json')).toThrow(/Pair/);
    pairingEndpoint(payload);
    expect(certificatePin('https://192.168.1.50:7777/bootstrap.json')).toBe(PIN);
    expect(() => certificatePin('https://gpu.test/bootstrap.json')).toThrow(/Pair/);
    pairingEndpoint({ ...payload, endpoint: 'https://gpu.test', certFingerprint: '' });
    expect(certificatePin('https://gpu.test/bootstrap.json')).toBeNull();
  });

  it('keeps browser fetch and WebPKI untouched', async () => {
    pairingEndpoint(payload);
    vi.stubGlobal('Capacitor', { isNativePlatform: () => false });
    const fetch = vi.fn(async () => new Response('ordinary'));
    vi.stubGlobal('fetch', fetch);
    expect(await (await networkFetch('https://192.168.1.50:7777/bootstrap.json')).text()).toBe('ordinary');
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(bridge.httpStart).not.toHaveBeenCalled();
  });
});

describe('bounded native HTTP', () => {
  it('preserves renewal negotiation and successor JSON through the native adapter', async () => {
    bridge.httpHeaders.mockResolvedValueOnce({ status: 200, headers: { 'content-type': 'application/json', ['X-AO-Refresh-Recovery']: '1' } });
    bridge.httpRead.mockResolvedValueOnce({ data: btoa('{"refreshSecret":"next"}') });
    const body = JSON.stringify({ refreshSecret: 'old', nextRefreshSecret: 'next' });
    const response = await pinnedFetch('https://gpu.test/auth/token/recover', { method: 'POST', body }, PIN);
    expect(response.headers.get('X-AO-Refresh-Recovery')).toBe('1');
    expect(await response.json()).toEqual({ refreshSecret: 'next' });
    const written = bridge.httpWrite.mock.calls as unknown as [{ data: string }][];
    expect(written.map(([part]) => atob(part.data)).join('')).toBe(body);
    expect(bridge.httpClose).toHaveBeenCalledTimes(1);
  });

  it('streams a file in bounded chunks with a single native request', async () => {
    const bytes = new Uint8Array(1024 * 1024 + 19).fill(42);
    const file = new File([bytes], 'data.bin');
    const response = await pinnedFetch('https://gpu.test/attachments/upload?ticket=one', { method: 'PUT', body: file }, PIN);
    await response.text();
    const calls = bridge.httpWrite.mock.calls as unknown as [{ data: string; end: boolean }][];
    expect(calls.length).toBe(17);
    expect(calls.reduce((sum, [part]) => sum + atob(part.data).length, 0)).toBe(file.size);
    expect(calls.every(([part]) => atob(part.data).length <= 64 * 1024)).toBe(true);
    expect(calls.filter(([part]) => part.end)).toHaveLength(1);
    expect(calls.at(-1)![0].end).toBe(true);
    expect(bridge.httpStart).toHaveBeenCalledExactlyOnceWith(expect.objectContaining({ length: file.size, method: 'PUT', pin: PIN }));
    expect(bridge.httpClose).toHaveBeenCalledTimes(1);
  });

  it('does not consume the entire download until the reader asks', async () => {
    bridge.httpRead.mockResolvedValueOnce({ data: btoa('first') }).mockResolvedValueOnce({ data: btoa('second') });
    const response = await pinnedFetch('https://gpu.test/attachments/a/b', undefined, PIN);
    await Promise.resolve();
    expect(bridge.httpRead.mock.calls.length).toBeLessThanOrEqual(1);
    expect(await response.text()).toBe('firstsecond');
    expect(bridge.httpClose).toHaveBeenCalledTimes(1);
  });

  it('reclaims an aborted request even if abort arrived during native start', async () => {
    const start = deferred<void>();
    bridge.httpStart.mockReturnValueOnce(start.promise);
    const controller = new AbortController();
    const pending = pinnedFetch('https://gpu.test/bootstrap.json', { signal: controller.signal }, PIN);
    await vi.waitFor(() => expect(bridge.httpStart).toHaveBeenCalledTimes(1));
    controller.abort();
    start.resolve();
    await expect(pending).rejects.toThrow(/abort/i);
    expect(bridge.httpClose).toHaveBeenCalledTimes(1);
  });

  it('refuses redirects rather than replaying an authenticated request', async () => {
    bridge.httpHeaders.mockResolvedValueOnce({ status: 307, headers: { 'content-type': 'text/plain' } });
    await expect(pinnedFetch('https://gpu.test/auth/token', { method: 'POST', body: '{}' }, PIN)).rejects.toThrow(/redirected/);
    expect(bridge.httpStart).toHaveBeenCalledTimes(1);
    expect(bridge.httpClose).toHaveBeenCalledTimes(1);
  });
});

describe('native sockets', () => {
  it('listens before opening, preserves frames and ignores events after close', async () => {
    const { PinnedSocket } = await import('./networkSocket');
    const ws = new PinnedSocket('wss://gpu.test/ws?ticket=one', PIN);
    const messages: string[] = [];
    ws.addEventListener('message', (event) => messages.push(event.data));
    await vi.waitFor(() => expect(bridge.socketOpen).toHaveBeenCalledTimes(1));
    const event = bridge.addListener.mock.calls[0][1];
    const { id } = bridge.socketOpen.mock.calls[0][0];
    event({ id, type: 'open', data: '', code: 0 });
    ws.send('request');
    event({ id, type: 'message', data: 'reply', code: 0 });
    ws.close();
    event({ id, type: 'message', data: 'stale', code: 0 });
    expect(messages).toEqual(['reply']);
    expect(bridge.socketSend).toHaveBeenCalledWith({ id, data: 'request' });
    expect(ws.readyState).toBe(3);
  });

  it('closes a native socket that finishes opening after cancellation', async () => {
    const opened = deferred<void>();
    bridge.socketOpen.mockReturnValueOnce(opened.promise);
    const { PinnedSocket } = await import('./networkSocket');
    const ws = new PinnedSocket('wss://gpu.test/ws?ticket=two', PIN);
    await vi.waitFor(() => expect(bridge.socketOpen).toHaveBeenCalledTimes(1));
    ws.close();
    opened.resolve();
    await vi.waitFor(() => expect(bridge.socketClose).toHaveBeenCalledTimes(2));
    expect(ws.readyState).toBe(3);
  });
});
