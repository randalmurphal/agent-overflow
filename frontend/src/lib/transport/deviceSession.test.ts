// This file runs under happy-dom, which provides no IndexedDB, so every
// case here takes the KEYLESS path — which is not an accident of the
// environment but the exact shape of spec §15 constraint 6: a plain-HTTP
// LAN browser has no secure context, cannot hold a signing key, and
// enrolls with a bare identifier instead. Keeping these assertions
// unchanged through phase 5 is the regression test that the class still
// works. The signing device is deviceSessionKeyed.test.ts, which brings
// its own IndexedDB.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  PairingRefusedError,
  clearPairedSession,
  deviceKeyThumbprint,
  endpointMatchesOrigin,
  hasPairedSession,
  mintDialTicket,
  parsePairingFragment,
  probeActivation,
  redeemPairing,
  signInWithPasskey,
  type PairingPayload,
} from './deviceSession';

const PAYLOAD: PairingPayload = {
  v: 1,
  backendId: 'backend-1',
  backendName: 'Home',
  endpoint: 'http://192.168.1.20:8123',
  token: 'link-token-abc',
};

function encodePayload(payload: unknown): string {
  return btoa(JSON.stringify(payload)).replaceAll('+', '-').replaceAll('/', '_').replace(/=+$/, '');
}

function grantResponse(overrides: Record<string, unknown> = {}): Response {
  return new Response(
    JSON.stringify({
      sessionId: 'sess-1',
      credential: 'cred-1',
      expiresAtMs: Date.now() + 15 * 60_000,
      refreshSecret: 'refresh-1',
      refreshExpiresAtMs: Date.now() + 12 * 3600_000,
      ...overrides,
    }),
    { status: 200, headers: { 'Content-Type': 'application/json' } },
  );
}

function refusal(reason: string): Response {
  return new Response(JSON.stringify({ reason }), { status: 401 });
}

beforeEach(() => {
  localStorage.clear();
});

describe('parsePairingFragment', () => {
  it('returns null for a hash that is not a pairing fragment', () => {
    expect(parsePairingFragment('')).toBeNull();
    expect(parsePairingFragment('#other')).toBeNull();
  });

  it('decodes what the backend encodes', () => {
    const parsed = parsePairingFragment(`#pair=${encodePayload(PAYLOAD)}`);
    expect(parsed).toEqual(PAYLOAD);
  });

  // A later payload version changes field meanings; partially reading it
  // is exactly what the version field exists to prevent.
  it('refuses an unknown version rather than guessing', () => {
    expect(() => parsePairingFragment(`#pair=${encodePayload({ ...PAYLOAD, v: 2 })}`)).toThrow(
      /newer version/,
    );
  });

  it('refuses damage rather than redeeming garbage', () => {
    expect(() => parsePairingFragment('#pair=%%%%')).toThrow(/damaged/);
    expect(() => parsePairingFragment('#pair=')).toThrow(/damaged/);
    expect(() => parsePairingFragment(`#pair=${encodePayload({ v: 1 })}`)).toThrow(/damaged/);
  });
});

describe('endpointMatchesOrigin', () => {
  it('matches the origin the page loaded from', () => {
    expect(endpointMatchesOrigin(PAYLOAD, 'http://192.168.1.20:8123')).toBe(true);
    expect(endpointMatchesOrigin(PAYLOAD, 'http://192.168.1.21:8123')).toBe(false);
    expect(endpointMatchesOrigin({ ...PAYLOAD, endpoint: 'not a url' }, 'http://x')).toBe(false);
  });
});

describe('deviceKeyThumbprint', () => {
  it('mints once and answers the same value forever after', () => {
    const first = deviceKeyThumbprint();
    expect(first).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(deviceKeyThumbprint()).toBe(first);
  });
});

describe('redeemPairing', () => {
  it('presents the link token with the device identifier and stores the grant', async () => {
    const fetcher = vi.fn(async () => grantResponse({ verificationNumber: '123456' }));
    const outcome = await redeemPairing(PAYLOAD, 'My phone', fetcher as unknown as typeof fetch);

    expect(outcome.verificationNumber).toBe('123456');
    expect(outcome.sessionId).toBe('sess-1');
    expect(hasPairedSession()).toBe(true);

    const [path, init] = fetcher.mock.calls[0] as unknown as [string, RequestInit];
    expect(path).toBe('/auth/pair');
    const body = JSON.parse(init.body as string) as Record<string, string>;
    expect(body.token).toBe(PAYLOAD.token);
    expect(body.keyThumbprint).toBe(deviceKeyThumbprint());
    expect(body.label).toBe('My phone');
  });

  it('throws the reason on a refusal and stores nothing', async () => {
    const fetcher = vi.fn(async () => refusal('unknown_credential'));
    await expect(
      redeemPairing(PAYLOAD, 'My phone', fetcher as unknown as typeof fetch),
    ).rejects.toThrow(PairingRefusedError);
    expect(hasPairedSession()).toBe(false);
  });
});

async function pairFirst(): Promise<void> {
  const fetcher = vi.fn(async () => grantResponse());
  await redeemPairing(PAYLOAD, 'My phone', fetcher as unknown as typeof fetch);
}

describe('mintDialTicket', () => {
  it('answers null with nothing stored, without touching the network', async () => {
    const fetcher = vi.fn();
    expect(await mintDialTicket(fetcher as unknown as typeof fetch)).toBeNull();
    expect(fetcher).not.toHaveBeenCalled();
  });

  it('mints a ticket with the stored credential', async () => {
    await pairFirst();
    const fetcher = vi.fn(async () => new Response(JSON.stringify({ ticket: 'tik-1' }), { status: 200 }));
    expect(await mintDialTicket(fetcher as unknown as typeof fetch)).toBe('tik-1');

    const [path, init] = fetcher.mock.calls[0] as unknown as [string, RequestInit];
    expect(path).toBe('/auth/ticket');
    const headers = init.headers as Record<string, string>;
    expect(headers['X-AO-Session']).toBe('cred-1');
    expect(headers['X-AO-Device-Key']).toBe(deviceKeyThumbprint());
  });

  it('renews first when the access credential is near expiry', async () => {
    const fetcher = vi.fn(async () => grantResponse({ expiresAtMs: Date.now() + 1000 }));
    await redeemPairing(PAYLOAD, 'My phone', fetcher as unknown as typeof fetch);

    const calls: string[] = [];
    const dialFetcher = vi.fn(async (path: string) => {
      calls.push(path);
      if (path === '/auth/token') return grantResponse({ credential: 'cred-2', refreshSecret: 'refresh-2' });
      return new Response(JSON.stringify({ ticket: 'tik-2' }), { status: 200 });
    });
    expect(await mintDialTicket(dialFetcher as unknown as typeof fetch)).toBe('tik-2');
    expect(calls).toEqual(['/auth/token', '/auth/ticket']);
  });

  // The 404 on the ticket route covers "not admitted yet" and "not
  // admitted any more"; the renewal's typed refusal is what separates
  // them. A revoked session's renewal refuses terminally → cleared.
  it('clears the store when the backend refuses the renewal terminally', async () => {
    await pairFirst();
    const fetcher = vi.fn(async (path: string) => {
      if (path === '/auth/ticket') return new Response('not found', { status: 404 });
      return refusal('revoked_session');
    });
    expect(await mintDialTicket(fetcher as unknown as typeof fetch)).toBeNull();
    expect(hasPairedSession()).toBe(false);
  });

  // A pairing awaiting the owner's confirmation is real and inert; it
  // must survive a dial attempt so it is still there when they confirm.
  it('keeps the store while the pairing awaits confirmation', async () => {
    await pairFirst();
    const fetcher = vi.fn(async (path: string) => {
      if (path === '/auth/ticket') return new Response('not found', { status: 404 });
      return refusal('pending_confirmation');
    });
    expect(await mintDialTicket(fetcher as unknown as typeof fetch)).toBeNull();
    expect(hasPairedSession()).toBe(true);
  });

  it('keeps the store across a rate-limit answer', async () => {
    await pairFirst();
    const fetcher = vi.fn(async (path: string) => {
      if (path === '/auth/ticket') return new Response('not found', { status: 404 });
      return new Response('slow down', { status: 429 });
    });
    expect(await mintDialTicket(fetcher as unknown as typeof fetch)).toBeNull();
    expect(hasPairedSession()).toBe(true);
  });

  it('single-flights concurrent mints', async () => {
    await pairFirst();
    let resolveTicket: (r: Response) => void = () => {};
    const fetcher = vi.fn(
      () => new Promise<Response>((resolve) => { resolveTicket = resolve; }),
    );
    const a = mintDialTicket(fetcher as unknown as typeof fetch);
    const b = mintDialTicket(fetcher as unknown as typeof fetch);
    // Waited for rather than assumed: assembling the headers is async (a
    // signing device mints a proof there), so the fetcher is called a
    // microtask after the mint starts and resolving before that would
    // resolve nothing. vi.waitFor FAILS at its deadline rather than
    // returning, so a mint that never reaches the network is a failure
    // naming this line instead of a timeout naming the whole case.
    await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(1));
    resolveTicket(new Response(JSON.stringify({ ticket: 'tik-3' }), { status: 200 }));
    expect(await a).toBe('tik-3');
    expect(await b).toBe('tik-3');
    expect(fetcher).toHaveBeenCalledTimes(1);
  });
});

describe('probeActivation', () => {
  it('answers false while pending and true once admitted', async () => {
    await pairFirst();
    let admitted = false;
    const fetcher = vi.fn(async () =>
      admitted
        ? new Response(JSON.stringify({ ticket: 'tik-4' }), { status: 200 })
        : new Response('not found', { status: 404 }),
    );
    expect(await probeActivation(fetcher as unknown as typeof fetch)).toBe(false);
    expect(hasPairedSession()).toBe(true);
    admitted = true;
    expect(await probeActivation(fetcher as unknown as typeof fetch)).toBe(true);
  });
});

describe('clearPairedSession', () => {
  it('drops the session and keeps the device identity', async () => {
    await pairFirst();
    const key = deviceKeyThumbprint();
    clearPairedSession();
    expect(hasPairedSession()).toBe(false);
    expect(deviceKeyThumbprint()).toBe(key);
  });
});

describe('signInWithPasskey', () => {
  // The keyless path, like everything else in this file: happy-dom has no
  // IndexedDB, so the device enrolls with a bare identifier. What the
  // sign-in is asserted to do is store the SAME thing pairing stores —
  // one writer, one shape, or a passkey-signed browser and a paired one
  // would present differently on the next dial.
  const credential = {
    id: 'cred-id',
    rawId: new Uint8Array([1, 2]).buffer,
    type: 'public-key',
    authenticatorAttachment: 'platform',
    getClientExtensionResults: () => ({}),
    response: {
      clientDataJSON: new Uint8Array([3]).buffer,
      authenticatorData: new Uint8Array([4]).buffer,
      signature: new Uint8Array([5]).buffer,
      userHandle: new Uint8Array([6]).buffer,
    },
  };

  function installAuthenticator(answer: unknown = credential): void {
    Object.defineProperty(window, 'PublicKeyCredential', {
      configurable: true,
      value: function PublicKeyCredential() {},
    });
    Object.defineProperty(navigator, 'credentials', {
      configurable: true,
      value: {
        create: () => Promise.resolve(answer),
        get: () =>
          answer instanceof Error ? Promise.reject(answer) : Promise.resolve(answer),
      },
    });
  }

  afterEach(() => {
    Reflect.deleteProperty(window, 'PublicKeyCredential');
    Reflect.deleteProperty(navigator, 'credentials');
  });

  it('stores the session the finish granted, with no confirmation step', async () => {
    installAuthenticator();
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ ceremonyId: 'cer-1', options: { challenge: 'AQID' } }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }),
      )
      .mockResolvedValueOnce(grantResponse({ scopes: ['threads:read'] }));

    await signInWithPasskey('Phone', fetcher as unknown as typeof fetch);

    expect(fetcher.mock.calls[0]![0]).toBe('/auth/passkey/begin');
    const finish = fetcher.mock.calls[1]!;
    expect(finish[0]).toBe('/auth/passkey/finish');
    const body = JSON.parse((finish[1] as RequestInit).body as string) as Record<string, unknown>;
    expect(body.ceremonyId).toBe('cer-1');
    expect(body.label).toBe('Phone');
    // A device row is what a revocation reaches, so the sign-in still
    // enrolls one. Keyless here, so it presents the bare identifier.
    expect(body.keyThumbprint).toBe(deviceKeyThumbprint());
    // Pairing waits for the owner to confirm a number; a passkey
    // assertion IS the confirmation, so the session is live on arrival.
    expect(hasPairedSession()).toBe(true);
  });

  it('spends the ceremony before it enrolls anything', async () => {
    installAuthenticator(new DOMException('dismissed', 'NotAllowedError'));
    const fetcher = vi.fn().mockResolvedValueOnce(
      new Response(JSON.stringify({ ceremonyId: 'cer-1', options: { challenge: 'AQID' } }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );

    await expect(signInWithPasskey('Phone', fetcher as unknown as typeof fetch)).rejects.toThrow();
    // No finish, and nothing stored: a dismissed prompt leaves this
    // browser exactly as it was.
    expect(fetcher).toHaveBeenCalledTimes(1);
    expect(hasPairedSession()).toBe(false);
  });

  it('carries a refused begin through as the typed refusal it is', async () => {
    installAuthenticator();
    const fetcher = vi.fn().mockResolvedValueOnce(refusal('passkey_unavailable'));

    await expect(
      signInWithPasskey('Phone', fetcher as unknown as typeof fetch),
    ).rejects.toMatchObject({ reason: 'passkey_unavailable' });
    expect(hasPairedSession()).toBe(false);
  });
});
