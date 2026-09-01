// The insecure-context regression suite for the app's identifier mint.
//
// The bug this pins was not a wrong id, it was a blank page: on a
// plain-HTTP LAN origin `crypto.randomUUID` does not exist, `wsClient`
// minted every RPC id through it, and the first call of the boot fan-out
// threw. So the cases below stage that context by DELETING the property
// rather than by mocking the module — the shape of the environment is the
// thing under test.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { randomId } from './randomId';
import { isValidClientId } from '../transport/clientIdentity';

const UUID_V4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

afterEach(() => {
  vi.unstubAllGlobals();
});

/** The plain-HTTP LAN page: WebCrypto present, secure-context APIs absent. */
function stubInsecureContext(): void {
  vi.stubGlobal('crypto', {
    getRandomValues: <T extends ArrayBufferView>(view: T): T => {
      const bytes = new Uint8Array(view.buffer, view.byteOffset, view.byteLength);
      for (let i = 0; i < bytes.length; i++) bytes[i] = (i * 37 + 11) & 0xff;
      return view;
    },
  });
}

describe('randomId', () => {
  it('answers a v4 uuid where crypto.randomUUID exists', () => {
    expect(randomId()).toMatch(UUID_V4);
  });

  it('answers a v4 uuid where crypto.randomUUID does NOT exist', () => {
    stubInsecureContext();
    expect(randomId()).toMatch(UUID_V4);
  });

  it('answers a v4 uuid with no WebCrypto at all', () => {
    vi.stubGlobal('crypto', undefined);
    expect(randomId()).toMatch(UUID_V4);
  });

  it('draws the insecure-context bytes from the CSPRNG, not Math.random', () => {
    stubInsecureContext();
    const spy = vi.spyOn(Math, 'random');
    randomId();
    expect(spy, 'crypto.getRandomValues is not secure-context gated').not.toHaveBeenCalled();
    spy.mockRestore();
  });

  it('stays inside the identifier bounds the backend enforces', () => {
    // Both mints feed `transport/clientIdentity`, whose ids the backend
    // bounds to [A-Za-z0-9-]{8,64} and otherwise treats as no identity at
    // all — so a fallback of a different shape would be silently ignored
    // rather than loudly wrong.
    expect(isValidClientId(randomId())).toBe(true);
    stubInsecureContext();
    expect(isValidClientId(randomId())).toBe(true);
  });

  it('does not repeat itself', () => {
    stubInsecureContext();
    // The stub is deterministic, so this asserts the mint is not caching a
    // single value; randomness itself is the CSPRNG's contract, not ours.
    const first = randomId();
    vi.stubGlobal('crypto', {
      getRandomValues: <T extends ArrayBufferView>(view: T): T => {
        const bytes = new Uint8Array(view.buffer, view.byteOffset, view.byteLength);
        for (let i = 0; i < bytes.length; i++) bytes[i] = (i * 91 + 3) & 0xff;
        return view;
      },
    });
    expect(randomId()).not.toBe(first);
  });
});
