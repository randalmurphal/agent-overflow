import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ComputerRoute } from './computerRoute';

const mocks = vi.hoisted(() => ({ probe: vi.fn(), supported: vi.fn(), pinned: vi.fn() }));
vi.mock('../native/computerRouteProbe', () => ({ canVerifyComputerRoutes: mocks.supported, verifyComputerRoute: mocks.probe }));
vi.mock('../native/networkHttp', () => ({ pinnedFetch: mocks.pinned }));
const LAN = { endpoint: 'https://192.168.1.20:60522', certFingerprint: `sha256:${'a'.repeat(64)}` };
const TAILNET = { endpoint: 'https://computer.tailnet.test' };
const payload = { v: 1, backendId: 'computer', endpoint: LAN.endpoint, certFingerprint: LAN.certFingerprint, token: 'one-use-invitation', purpose: 'own-introduction' };
const link = `https://computer.test/#pair=${btoa(JSON.stringify(payload)).replaceAll('+', '-').replaceAll('/', '_').replace(/=+$/, '')}`;
const grant = () => new Response(JSON.stringify({ sessionId: 'admitted', credential: 'credential' }));
let attach: typeof import('./backendAttach').attachIntroducedBackend;
let certificatePin: typeof import('../native/networkTrust').certificatePin;
let pairedSessionId: typeof import('./deviceSession').pairedSessionId;

beforeEach(async () => {
  vi.resetModules(); vi.resetAllMocks(); localStorage.clear();
  vi.stubGlobal('Capacitor', { isNativePlatform: () => true });
  mocks.supported.mockResolvedValue(true);
  ({ attachIntroducedBackend: attach } = await import('./backendAttach'));
  ({ certificatePin } = await import('../native/networkTrust'));
  ({ pairedSessionId } = await import('./deviceSession'));
});
afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers(); });

describe('cold automatic introduction routes', () => {
  it.each([LAN, TAILNET])('enrolls through only reachable $endpoint, before holding any session', async (reachable) => {
    mocks.probe.mockImplementation((route: ComputerRoute, id: string, signal: AbortSignal) => {
      expect(id).toBe('computer');
      expect(pairedSessionId('computer')).toBeNull();
      expect(mocks.pinned).not.toHaveBeenCalled();
      if (route.endpoint === reachable.endpoint) return Promise.resolve();
      return new Promise((_resolve, reject) => signal.addEventListener('abort', () => reject(signal.reason), { once: true }));
    });
    mocks.pinned.mockResolvedValue(grant());
    const fetcher = vi.fn(async () => grant());
    vi.stubGlobal('fetch', fetcher);
    const paired = await attach(link, () => true, [LAN, TAILNET]);
    expect(paired.id).toBe('computer');
    expect(pairedSessionId('computer')).toBe('admitted');
    expect(mocks.probe).toHaveBeenCalledTimes(2);
    expect(mocks.probe.mock.calls.every((call) => call[2].aborted)).toBe(true);
    if (reachable === LAN) {
      expect(mocks.pinned).toHaveBeenCalledExactlyOnceWith(`${LAN.endpoint}/auth/pair`, expect.objectContaining({ method: 'POST' }), LAN.certFingerprint);
      expect(fetcher).not.toHaveBeenCalled();
      expect(certificatePin(LAN.endpoint)).toBe(LAN.certFingerprint);
    } else {
      expect(fetcher).toHaveBeenCalledExactlyOnceWith(`${TAILNET.endpoint}/auth/pair`, expect.objectContaining({ method: 'POST' }));
      expect(mocks.pinned).not.toHaveBeenCalled();
      expect(certificatePin(LAN.endpoint)).toBeNull();
    }
  });

  it('discards route selection after the sponsor is removed', async () => {
    let current = true;
    let finish!: () => void;
    mocks.probe.mockImplementation(() => new Promise<void>((resolve) => { finish = resolve; }));
    const request = attach(link, () => current, [LAN]);
    const rejected = expect(request).rejects.toMatchObject({ name: 'AbortError' });
    await vi.waitFor(() => expect(mocks.probe).toHaveBeenCalledOnce());
    current = false; finish();
    await rejected;
    expect(mocks.pinned).not.toHaveBeenCalled();
    expect(certificatePin(LAN.endpoint)).toBeNull();
    expect(pairedSessionId('computer')).toBeNull();
  });

  it('bounds an entirely stalled route selection and cancels every probe', async () => {
    vi.useFakeTimers();
    mocks.probe.mockImplementation((_route, _id, signal: AbortSignal) => new Promise((_resolve, reject) => {
      signal.addEventListener('abort', () => reject(signal.reason), { once: true });
    }));
    const rejected = expect(attach(link, () => true, [LAN, TAILNET])).rejects.toThrow('No verified address');
    await vi.advanceTimersByTimeAsync(20_000);
    await rejected;
    expect(mocks.probe).toHaveBeenCalledTimes(2);
    expect(mocks.probe.mock.calls.every((call) => call[2].aborted)).toBe(true);
    expect(mocks.pinned).not.toHaveBeenCalled();
  });

  it('never spends the invitation when every route fails identity or certificate verification', async () => {
    mocks.probe.mockRejectedValue(new Error('Wrong identity or pin'));
    await expect(attach(link, () => true, [LAN, TAILNET])).rejects.toThrow('No verified address');
    expect(mocks.pinned).not.toHaveBeenCalled();
    expect(pairedSessionId('computer')).toBeNull();
  });

  it('does not replay a spent invitation on a second route after losing the POST response', async () => {
    mocks.probe.mockResolvedValue(undefined);
    mocks.pinned.mockRejectedValue(new Error('Response lost'));
    const fetcher = vi.fn(); vi.stubGlobal('fetch', fetcher);
    await expect(attach(link, () => true, [LAN, TAILNET])).rejects.toThrow('Response lost');
    expect(mocks.pinned).toHaveBeenCalledOnce();
    expect(fetcher).not.toHaveBeenCalled();
    expect(certificatePin(LAN.endpoint)).toBeNull();
  });
});
