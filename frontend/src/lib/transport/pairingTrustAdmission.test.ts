import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
let preparePairingTrust: typeof import('../native/networkTrust').preparePairingTrust;
let pairingEndpoint: typeof import('../native/networkTrust').pairingEndpoint;
let certificatePin: typeof import('../native/networkTrust').certificatePin;
let clearPairedSession: typeof import('./deviceSession').clearPairedSession;
let pairedSessionId: typeof import('./deviceSession').pairedSessionId;
let redeemPairing: typeof import('./deviceSession').redeemPairing;
let storedBackendEndpoint: typeof import('./homeEndpoint').storedBackendEndpoint;
const pinnedFetch = vi.hoisted(() => vi.fn());
vi.mock('../native/networkHttp', () => ({ pinnedFetch }));
const OLD = `sha256:${'a'.repeat(64)}`;
const CANDIDATE = `sha256:${'b'.repeat(64)}`;
const NEWER = `sha256:${'c'.repeat(64)}`;
const payload = { v: 1, backendId: 'computer', endpoint: 'https://computer.test', token: 'invitation', certFingerprint: CANDIDATE };
const grant = (id = 'accepted') => new Response(JSON.stringify({ sessionId: id, credential: 'credential' }));

beforeEach(async () => {
  localStorage.clear(); pinnedFetch.mockReset(); vi.resetModules();
  vi.stubGlobal('Capacitor', { isNativePlatform: () => true });
  ({ preparePairingTrust, pairingEndpoint, certificatePin } = await import('../native/networkTrust'));
  ({ clearPairedSession, pairedSessionId, redeemPairing } = await import('./deviceSession'));
  ({ storedBackendEndpoint } = await import('./homeEndpoint'));
});
afterEach(() => vi.unstubAllGlobals());

describe('pairing trust admission', () => {
  it('only validates when a QR code is scanned, preserving existing trust', async () => {
    pairingEndpoint({ ...payload, certFingerprint: OLD });
    const { adoptPairingEndpoint } = await import('../native/boot');
    expect(adoptPairingEndpoint(payload)).toBe('');
    expect(certificatePin(payload.endpoint)).toBe(OLD);
    expect(storedBackendEndpoint('computer')).toBe('');
  });

  it.each(['refused', 'removed', 'sponsor disconnected', 'replaced'] as const)('leaves prior trust intact when candidate is %s', async (reason) => {
    pairingEndpoint({ ...payload, certFingerprint: OLD });
    const trust = preparePairingTrust(payload);
    expect(certificatePin(payload.endpoint)).toBe(OLD);
    let finish!: (response: Response) => void;
    let current = true;
    vi.mocked(pinnedFetch).mockImplementationOnce(() => new Promise((resolve) => { finish = resolve; }));
    const pending = redeemPairing(payload, 'Phone', fetch, 'computer', { endpoint: trust.endpoint, trust, current: () => current });
    const rejected = expect(pending).rejects.toThrow();
    await vi.waitFor(() => expect(pinnedFetch).toHaveBeenCalledOnce());
    expect(vi.mocked(pinnedFetch).mock.calls[0][2]).toBe(CANDIDATE);
    expect(certificatePin(payload.endpoint)).toBe(OLD);
    if (reason === 'removed') clearPairedSession('computer');
    if (reason === 'sponsor disconnected') current = false;
    if (reason === 'replaced') {
      const replacement = { ...payload, certFingerprint: NEWER };
      const nextTrust = preparePairingTrust(replacement);
      vi.mocked(pinnedFetch).mockResolvedValueOnce(grant('newer'));
      await redeemPairing(replacement, 'Phone', fetch, 'computer', { endpoint: nextTrust.endpoint, trust: nextTrust, current: () => true });
    }
    finish(reason === 'refused' ? new Response('{}', { status: 403 }) : grant());
    await rejected;
    expect(certificatePin(payload.endpoint)).toBe(reason === 'replaced' ? NEWER : OLD);
    expect(pairedSessionId('computer')).toBe(reason === 'replaced' ? 'newer' : null);
    expect(storedBackendEndpoint('computer')).toBe(reason === 'replaced' ? payload.endpoint : '');
  });

  it('uses candidate WebPKI without consulting old private trust and commits only success', async () => {
    pairingEndpoint({ ...payload, certFingerprint: OLD });
    const candidate = { ...payload, certFingerprint: '' };
    const trust = preparePairingTrust(candidate);
    const fetcher = vi.fn(async () => {
      expect(certificatePin(payload.endpoint)).toBe(OLD);
      return grant();
    });
    vi.stubGlobal('fetch', fetcher);
    await redeemPairing(candidate, 'Phone', fetch, 'computer', { endpoint: trust.endpoint, trust, current: () => true });
    expect(fetcher).toHaveBeenCalledOnce();
    expect(pinnedFetch).not.toHaveBeenCalled();
    expect(certificatePin(payload.endpoint)).toBeNull();
    expect(pairedSessionId('computer')).toBe('accepted');
  });
});
