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
  acceptPairingEndpoint,
  clearPairedSession,
  deviceKeyThumbprint,
  endpointMatchesOrigin,
  hasPairedSession,
  mintDialTicket,
  parsePairingFragment,
  probeActivation,
  redeemPairing,
  signInWithPasskey,
  unpairHome,
  type PairingPayload,
} from './deviceSession';
import {
  __resetHomeEndpointForTest,
  homeEndpoint,
  setHomeEndpoint,
  storeBackendEndpoint,
  storedBackendEndpoint,
} from './homeEndpoint';
import { __resetDetachStepsForTest, onBeforeBackendDetach } from './detachSteps';
import { renewalLeaseKey, withRenewalLease } from './renewalLease';

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

// Every exchange in this module was same-origin because the pairing link
// landed the browser on the backend it was pairing with. A shell page is
// served from its own fixed origin, which can never be a backend's, so
// each exchange is carried onto the endpoint and presents no cookie —
// while the (method, PATH) the device proof binds is byte-identical, which
// is what makes this one exchange rather than two.
describe('the auth exchanges under a shell origin', () => {
  const ENDPOINT = 'https://desk.tail-scale.ts.net:7777';

  beforeEach(() => {
    localStorage.clear();
    __resetHomeEndpointForTest();
    setHomeEndpoint(ENDPOINT);
  });

  afterEach(() => {
    __resetHomeEndpointForTest();
    localStorage.clear();
  });

  it('redeems against the endpoint, with no cookie', async () => {
    const fetcher = vi.fn().mockResolvedValue(grantResponse({ verificationNumber: '123456' }));

    await redeemPairing(PAYLOAD, 'Phone', fetcher as unknown as typeof fetch);

    const [url, init] = fetcher.mock.calls[0] as [string, RequestInit];
    expect(url).toBe(`${ENDPOINT}/auth/pair`);
    expect(init.credentials).toBe('omit');
  });

  it('mints the dial ticket against the endpoint too', async () => {
    const fetcher = vi.fn()
      .mockResolvedValueOnce(grantResponse())
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ ticket: 'tkt-1' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }),
      );
    await redeemPairing(PAYLOAD, 'Phone', fetcher as unknown as typeof fetch);

    await expect(mintDialTicket(fetcher as unknown as typeof fetch)).resolves.toBe('tkt-1');

    const [url, init] = fetcher.mock.calls[1] as [string, RequestInit];
    expect(url).toBe(`${ENDPOINT}/auth/ticket`);
    expect(init.credentials).toBe('omit');
  });

  it('adopts the endpoint a scanned payload names instead of comparing origins', () => {
    // A browser compares, because the page it is on IS the backend. A
    // shell adopts, because scanning the code is how it learns where the
    // backend is at all — and it remembers, beside the session slot.
    expect(acceptPairingEndpoint(PAYLOAD, 'https://shell.agent-overflow.invalid')).toBe(true);
    expect(homeEndpoint()).toBe('http://192.168.1.20:8123');
    expect(storedBackendEndpoint('')).toBe('http://192.168.1.20:8123');
  });

  it('refuses a payload whose endpoint names nowhere to present a credential', () => {
    expect(acceptPairingEndpoint({ ...PAYLOAD, endpoint: 'not-a-url' }, 'https://shell.test'))
      .toBe(false);
  });

  it('unpairing home forgets the session and the endpoint, and nothing else', async () => {
    // The shell's only way back to "scan a code": a fixed origin cannot
    // be navigated to a new link, so it stops knowing where home is.
    const fetcher = vi.fn().mockResolvedValue(grantResponse());
    await redeemPairing(PAYLOAD, 'Phone', fetcher as unknown as typeof fetch);
    expect(acceptPairingEndpoint(PAYLOAD, 'https://shell.agent-overflow.invalid')).toBe(true);
    storeBackendEndpoint('b-1', 'https://laptop.test:7777');
    expect(hasPairedSession()).toBe(true);

    unpairHome();

    expect(hasPairedSession()).toBe(false);
    expect(storedBackendEndpoint('')).toBe('');
    expect(storedBackendEndpoint('b-1')).toBe('https://laptop.test:7777');
  });

  it('unpairing home runs the detach steps while the credential is still held', async () => {
    // The other removal door, and the same rule: a phone withdrawing its
    // push registration is a call that needs the session it is about to
    // drop (./detachSteps.ts).
    __resetDetachStepsForTest();
    const fetcher = vi.fn().mockResolvedValue(grantResponse());
    await redeemPairing(PAYLOAD, 'Phone', fetcher as unknown as typeof fetch);
    const seen: { backend: string; held: boolean }[] = [];
    onBeforeBackendDetach((backend) => seen.push({ backend, held: hasPairedSession() }));

    unpairHome();

    expect(seen).toEqual([{ backend: '', held: true }]);
    __resetDetachStepsForTest();
  });
});

// Rotation is single-use on the backend, and presenting a spent refresh
// secret is REUSE EVIDENCE that ends the whole session family
// (internal/identity). The in-realm single-flight Map cannot see a second
// TAB, which is a second realm over the same localStorage, so two tabs
// waking together on a near-expiry credential each renewed and logged the
// person out of both. The cross-context half is ./renewalLease.ts.
//
// Two realms are the fixture: `vi.resetModules()` plus a dynamic import
// gives a genuinely separate module instance, which is what a second tab
// is, over the localStorage happy-dom already shares between them.
describe('renewal across browsing contexts', () => {
  type Tab = typeof import('./deviceSession');

  async function openTab(): Promise<Tab> {
    vi.resetModules();
    return await import('./deviceSession');
  }

  /** A near-expiry stored session, so any dial renews before it tickets. */
  async function pairNearExpiry(tab: Tab): Promise<void> {
    const fetcher = vi.fn(async () => grantResponse({ expiresAtMs: Date.now() + 1_000 }));
    await tab.redeemPairing(PAYLOAD, 'My phone', fetcher as unknown as typeof fetch);
  }

  it('rotates once for two tabs dialing at the same moment', async () => {
    const tabA = await openTab();
    const tabB = await openTab();
    await pairNearExpiry(tabA);

    let rotations = 0;
    const fetcher = (async (path: string) => {
      if (path === '/auth/token') {
        rotations++;
        return grantResponse({
          credential: `cred-${rotations + 1}`,
          refreshSecret: `refresh-${rotations + 1}`,
          expiresAtMs: Date.now() + 15 * 60_000,
        });
      }
      return new Response(JSON.stringify({ ticket: 'tik' }), { status: 200 });
    }) as unknown as typeof fetch;

    const [a, b] = await Promise.all([tabA.mintDialTicket(fetcher), tabB.mintDialTicket(fetcher)]);

    // A second POST here is the defect: it presents the secret the first
    // one just spent, and the backend reads that as reuse.
    expect(rotations).toBe(1);
    // And both tabs are dialing, not just the winner. The loser's answer
    // is the winner's rotation, read back out of the shared store.
    expect([a, b]).toEqual(['tik', 'tik']);
    expect(hasPairedSession()).toBe(true);
  });

  it('renews through a lease left behind by a tab that was killed', async () => {
    const tab = await openTab();
    await pairNearExpiry(tab);
    // No context can release a lease it died holding, so time is the only
    // release there is. Without the expiry this renewal never happens.
    localStorage.setItem(
      renewalLeaseKey('agent-overflow:deviceSession'),
      JSON.stringify({ id: 'a-tab-that-is-gone', at: Date.now() - 60_000 }),
    );

    let rotations = 0;
    const fetcher = (async (path: string) => {
      if (path === '/auth/token') {
        rotations++;
        return grantResponse({ credential: 'cred-2', refreshSecret: 'refresh-2' });
      }
      return new Response(JSON.stringify({ ticket: 'tik-2' }), { status: 200 });
    }) as unknown as typeof fetch;

    expect(await tab.mintDialTicket(fetcher)).toBe('tik-2');
    expect(rotations).toBe(1);
  });

  it('tells the work it has lost the lease the moment another context claims it', async () => {
    // The tie two contexts can both win: each writes, each reads its own
    // value back. The verdict is therefore re-read at the step that
    // cannot be taken back, which is what this pins.
    const key = renewalLeaseKey('agent-overflow:deviceSession');
    const seen: boolean[] = [];
    await withRenewalLease(key, async (hold) => {
      seen.push(hold.held());
      localStorage.setItem(key, JSON.stringify({ id: 'another-tab', at: Date.now() }));
      seen.push(hold.held());
    });

    expect(seen).toEqual([true, false]);
    // And the release left the other context's lease alone: removing it
    // would put two contexts inside at once.
    expect(localStorage.getItem(key)).toContain('another-tab');
  });
});

it.each([401, 408, 429, 500, 503])('keeps the pairing when renewal gets an unstructured HTTP %s response', async (status) => {
  await pairFirst();
  const unavailable = vi.fn(async (path: string) => path === '/auth/ticket'
    ? new Response('not found', { status: 404 })
    : new Response('<html>Upstream unavailable</html>', { status }));
  expect(await mintDialTicket(unavailable as unknown as typeof fetch)).toBeNull();
  expect(hasPairedSession()).toBe(true);
  const recovered = vi.fn(async () => new Response(JSON.stringify({ ticket: 'reconnected' }), { status: 200 }));
  expect(await mintDialTicket(recovered as unknown as typeof fetch)).toBe('reconnected');
});
