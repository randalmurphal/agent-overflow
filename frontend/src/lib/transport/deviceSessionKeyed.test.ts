// The paired-session flow as a device that CAN sign performs it. Its
// sibling deviceSession.test.ts covers the same flow for a device that
// cannot (spec §15 constraint 6), and the two must both keep passing:
// this phase added a stronger presentation without removing the weaker
// one, because the weaker one is the only one a plain-HTTP LAN browser
// can make.
//
// Split into its own file because the difference between the two is
// environmental — whether IndexedDB exists — and the import below is
// what supplies it. happy-dom provides none, so a single file could not
// stage both.
import 'fake-indexeddb/auto';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { clearDeviceKey, enrollDeviceKey } from './deviceKey';
import {
  clearPairedSession,
  hasPairedSession,
  mintDialTicket,
  pairedSessionHeaders,
  redeemPairing,
  type PairingPayload,
} from './deviceSession';

const PAYLOAD: PairingPayload = {
  v: 1,
  backendId: 'backend-1',
  backendName: 'Home',
  endpoint: 'http://192.168.1.20:8123',
  token: 'link-token-abc',
};

const DEVICE_KEY_HEADER = 'X-AO-Device-Key';

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

function decodeSegment(segment: string): string {
  const padded = segment.replaceAll('-', '+').replaceAll('_', '/');
  return atob(padded.padEnd(padded.length + ((4 - (padded.length % 4)) % 4), '='));
}

interface ProofPayload {
  htm: string;
  htp: string;
  jti: string;
  iatMs: number;
}

function proofPayload(proof: string): ProofPayload {
  return JSON.parse(decodeSegment(proof.split('.')[1])) as ProofPayload;
}

/** A proof, as opposed to the bare identifier a keyless device sends. */
function isSignedProof(value: string | undefined): boolean {
  return typeof value === 'string' && value.split('.').length === 3;
}

function headersOf(call: unknown): Record<string, string> {
  return (call as [string, RequestInit])[1].headers as Record<string, string>;
}

async function pairFirst(): Promise<void> {
  const fetcher = vi.fn(async () => grantResponse());
  await redeemPairing(PAYLOAD, 'My phone', fetcher as unknown as typeof fetch);
}

beforeEach(async () => {
  localStorage.clear();
  await clearDeviceKey();
});

describe('redeemPairing on a device that can sign', () => {
  it('proves the key in the redemption itself and claims no bare identifier', async () => {
    const fetcher = vi.fn(async () => grantResponse({ verificationNumber: '123456' }));
    await redeemPairing(PAYLOAD, 'My phone', fetcher as unknown as typeof fetch);

    const [path, init] = fetcher.mock.calls[0] as unknown as [string, RequestInit];
    expect(path).toBe('/auth/pair');
    const headers = init.headers as Record<string, string>;
    const proof = headers[DEVICE_KEY_HEADER];
    expect(isSignedProof(proof)).toBe(true);
    // Bound to the redemption request, so the same proof presented on
    // any other route proves nothing there.
    expect(proofPayload(proof)).toMatchObject({ htm: 'POST', htp: '/auth/pair' });

    // The body's identifier is the KEYLESS device's claim. A signed
    // redemption names its key inside the proof, and sending a second,
    // weaker claim about the same fact would invite the two to disagree.
    const body = JSON.parse(init.body as string) as Record<string, string>;
    expect(body.keyThumbprint).toBe('');
    expect(hasPairedSession()).toBe(true);
  });

  it('enrols one key, so re-pairing the same browser is the same device', async () => {
    await pairFirst();
    const firstPair = await enrollDeviceKey();
    const firstJwk = await crypto.subtle.exportKey('jwk', firstPair!.publicKey);

    clearPairedSession();
    await pairFirst();
    const secondPair = await enrollDeviceKey();
    const secondJwk = await crypto.subtle.exportKey('jwk', secondPair!.publicKey);
    expect(secondJwk.x).toBe(firstJwk.x);
  });
});

describe('presenting a key-bound session', () => {
  it('signs the manifest fetch for the route it is presented on', async () => {
    await pairFirst();
    const headers = await pairedSessionHeaders('GET', '/bootstrap.json');
    expect(headers['X-AO-Session']).toBe('cred-1');
    expect(isSignedProof(headers[DEVICE_KEY_HEADER])).toBe(true);
    expect(proofPayload(headers[DEVICE_KEY_HEADER])).toMatchObject({
      htm: 'GET',
      htp: '/bootstrap.json',
    });
  });

  it('signs the ticket mint, and never twice with the same proof', async () => {
    await pairFirst();
    const fetcher = vi.fn(
      async () => new Response(JSON.stringify({ ticket: 'tik-1' }), { status: 200 }),
    );
    expect(await mintDialTicket(fetcher as unknown as typeof fetch)).toBe('tik-1');
    expect(await mintDialTicket(fetcher as unknown as typeof fetch)).toBe('tik-1');

    const first = headersOf(fetcher.mock.calls[0])[DEVICE_KEY_HEADER];
    const second = headersOf(fetcher.mock.calls[1])[DEVICE_KEY_HEADER];
    expect(isSignedProof(first)).toBe(true);
    expect(proofPayload(first)).toMatchObject({ htm: 'POST', htp: '/auth/ticket' });
    // A proof is spent on first use. Caching one would be refused as a
    // replay, so the two mints must not have sent the same bytes.
    expect(second).not.toBe(first);
    expect(proofPayload(second).jti).not.toBe(proofPayload(first).jti);
  });

  it('mints a FRESH proof for the retry after a renewal', async () => {
    await pairFirst();
    let minted = 0;
    const fetcher = vi.fn(async (path: string) => {
      if (path === '/auth/token') return grantResponse({ credential: 'cred-2' });
      minted += 1;
      return minted === 1
        ? new Response('not found', { status: 404 })
        : new Response(JSON.stringify({ ticket: 'tik-2' }), { status: 200 });
    });
    expect(await mintDialTicket(fetcher as unknown as typeof fetch)).toBe('tik-2');

    const ticketProofs = fetcher.mock.calls
      .filter((call) => call[0] === '/auth/ticket')
      .map((call) => headersOf(call)[DEVICE_KEY_HEADER]);
    expect(ticketProofs).toHaveLength(2);
    expect(ticketProofs[1]).not.toBe(ticketProofs[0]);

    // The renewal itself is signed too, and bound to its own route.
    const renewal = fetcher.mock.calls.find((call) => call[0] === '/auth/token');
    expect(proofPayload(headersOf(renewal)[DEVICE_KEY_HEADER])).toMatchObject({
      htm: 'POST',
      htp: '/auth/token',
    });
  });
});

// The recovery story for the one failure this design introduces: a
// device enrolled `key` whose IndexedDB was cleared while its
// localStorage session survived. It cannot sign, and the backend refuses
// the bare identifier from a key-bound device — so presenting one would
// spend a round trip to be told what the page already knows.
describe('a key-bound session whose key is gone', () => {
  it('clears the session rather than presenting a weaker claim', async () => {
    await pairFirst();
    await clearDeviceKey();

    const headers = await pairedSessionHeaders('GET', '/bootstrap.json');
    expect(headers).toEqual({});
    expect(hasPairedSession()).toBe(false);
  });

  it('never reaches the network to find that out', async () => {
    await pairFirst();
    await clearDeviceKey();

    const fetcher = vi.fn();
    expect(await mintDialTicket(fetcher as unknown as typeof fetch)).toBeNull();
    expect(fetcher).not.toHaveBeenCalled();
  });
});

// A session stored before phase 5 records no proofKind, and the v77
// migration defaulted its device row to `bearer`. The two agree without
// either being told, which is the whole of the migration: such a device
// keeps presenting the identifier it always did, on a browser that is
// now perfectly capable of signing.
describe('a session stored before the signed proof existed', () => {
  it('keeps presenting the bare identifier its device row still expects', async () => {
    localStorage.setItem(
      'agent-overflow:deviceSession',
      JSON.stringify({
        sessionId: 'sess-old',
        credential: 'cred-old',
        expiresAtMs: Date.now() + 15 * 60_000,
        refreshSecret: 'refresh-old',
      }),
    );

    const headers = await pairedSessionHeaders('GET', '/bootstrap.json');
    expect(headers['X-AO-Session']).toBe('cred-old');
    expect(isSignedProof(headers[DEVICE_KEY_HEADER])).toBe(false);
    expect(headers[DEVICE_KEY_HEADER]).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(hasPairedSession()).toBe(true);
  });
});
