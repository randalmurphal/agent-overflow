// The browser half of the passkey ceremonies, with a fake authenticator
// standing in for the platform one — the same choice internal/identity's
// Go tests make (passkeysoft_test.go), and for the same reason: a captured
// credential pins what one browser did once and cannot be asked what
// happens when a member is absent or a person dismisses the prompt.
//
// happy-dom provides no WebAuthn at all, which makes it the right place to
// assert the ABSENT case honestly and means every present case installs
// its own globals.
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  PasskeyAbandonedError,
  answerChallenge,
  decodeBase64url,
  encodeBase64url,
  passkeysSupported,
  passkeysUsable,
  setPasskeysAvailableFromBootstrap,
} from './passkey';

function bytes(...values: number[]): Uint8Array {
  return new Uint8Array(values);
}

function b64(value: Uint8Array): string {
  return encodeBase64url(value.buffer as ArrayBuffer);
}

/**
 * Install a `navigator.credentials` that records what it was asked and
 * answers with the credential given. Returns the recorder.
 */
function installAuthenticator(answer: unknown): { create: unknown[]; get: unknown[] } {
  const seen = { create: [] as unknown[], get: [] as unknown[] };
  const respond = (kind: 'create' | 'get') => (options: unknown) => {
    seen[kind].push(options);
    if (answer instanceof Error) return Promise.reject(answer);
    return Promise.resolve(answer);
  };
  Object.defineProperty(window, 'PublicKeyCredential', {
    configurable: true,
    value: function PublicKeyCredential() {},
  });
  Object.defineProperty(navigator, 'credentials', {
    configurable: true,
    value: { create: respond('create'), get: respond('get') },
  });
  return seen;
}

function uninstallAuthenticator(): void {
  Reflect.deleteProperty(window, 'PublicKeyCredential');
  Reflect.deleteProperty(navigator, 'credentials');
}

/** A registration credential in the shape the DOM hands one back. */
function attestationCredential(overrides: Record<string, unknown> = {}) {
  return {
    id: 'cred-id',
    rawId: bytes(1, 2, 3).buffer,
    type: 'public-key',
    authenticatorAttachment: 'platform',
    getClientExtensionResults: () => ({}),
    response: {
      clientDataJSON: bytes(10, 11).buffer,
      attestationObject: bytes(20, 21).buffer,
      getTransports: () => ['internal', 'hybrid'],
    },
    ...overrides,
  };
}

/** An assertion credential, the shape both discoverable ceremonies get. */
function assertionCredential(overrides: Record<string, unknown> = {}) {
  return {
    id: 'cred-id',
    rawId: bytes(1, 2, 3).buffer,
    type: 'public-key',
    authenticatorAttachment: null,
    getClientExtensionResults: () => ({}),
    response: {
      clientDataJSON: bytes(10, 11).buffer,
      authenticatorData: bytes(30, 31).buffer,
      signature: bytes(40, 41).buffer,
      userHandle: bytes(50, 51).buffer,
    },
    ...overrides,
  };
}

afterEach(() => {
  uninstallAuthenticator();
  setPasskeysAvailableFromBootstrap(false);
  vi.restoreAllMocks();
});

describe('base64url', () => {
  it('round-trips every byte value, which is the whole contract', () => {
    const all = new Uint8Array(256);
    for (let i = 0; i < 256; i++) all[i] = i;
    expect(decodeBase64url(encodeBase64url(all.buffer as ArrayBuffer))).toEqual(all);
  });

  it('reads the unpadded spelling the wire uses, at every remainder', () => {
    // One, two and three bytes cover all three padding cases; a decoder
    // that only handled the padded form would fail on two of them.
    expect(decodeBase64url(b64(bytes(1)))).toEqual(bytes(1));
    expect(decodeBase64url(b64(bytes(1, 2)))).toEqual(bytes(1, 2));
    expect(decodeBase64url(b64(bytes(1, 2, 3)))).toEqual(bytes(1, 2, 3));
  });

  it('emits the url alphabet, never + or /', () => {
    // 0xFB 0xFF encodes to `+/` in the standard alphabet.
    expect(b64(bytes(0xfb, 0xff))).not.toMatch(/[+/=]/);
  });
});

describe('availability', () => {
  it('answers false on a page with no WebAuthn, whatever the backend said', () => {
    setPasskeysAvailableFromBootstrap(true);
    expect(passkeysSupported()).toBe(false);
    expect(passkeysUsable()).toBe(false);
  });

  it('needs BOTH halves: the page can, and the backend offers', () => {
    installAuthenticator(assertionCredential());
    expect(passkeysSupported()).toBe(true);
    // Manifest not seen yet, or seen and negative.
    expect(passkeysUsable()).toBe(false);
    setPasskeysAvailableFromBootstrap(true);
    expect(passkeysUsable()).toBe(true);
  });
});

describe('answerChallenge', () => {
  it('decodes exactly the members the dialect spells base64url', async () => {
    const seen = installAuthenticator(attestationCredential());
    await answerChallenge(
      {
        ceremonyId: 'cer-1',
        options: {
          challenge: b64(bytes(7, 8, 9)),
          rp: { id: 'ao.localhost', name: 'Agent Overflow' },
          user: { id: b64(bytes(1, 2)), name: 'owner', displayName: 'Owner' },
          excludeCredentials: [{ type: 'public-key', id: b64(bytes(3, 4)) }],
          // A string that is base64url-SHAPED and must be left alone: a
          // walker that decoded by shape rather than by name would turn
          // this into bytes the authenticator cannot read.
          attestation: 'none',
        },
      },
      'create',
    );
    const options = (seen.create[0] as { publicKey: Record<string, unknown> }).publicKey;
    expect(new Uint8Array(options.challenge as ArrayBuffer)).toEqual(bytes(7, 8, 9));
    expect(new Uint8Array((options.user as { id: ArrayBuffer }).id)).toEqual(bytes(1, 2));
    const excluded = options.excludeCredentials as { id: ArrayBuffer }[];
    expect(new Uint8Array(excluded[0]!.id)).toEqual(bytes(3, 4));
    expect(options.attestation).toBe('none');
    expect(options.rp).toEqual({ id: 'ao.localhost', name: 'Agent Overflow' });
  });

  it('decodes allowCredentials on the sign-in side too', async () => {
    const seen = installAuthenticator(assertionCredential());
    await answerChallenge(
      {
        ceremonyId: 'cer-2',
        options: {
          challenge: b64(bytes(1)),
          allowCredentials: [{ type: 'public-key', id: b64(bytes(5, 6)) }],
        },
      },
      'get',
    );
    const options = (seen.get[0] as { publicKey: Record<string, unknown> }).publicKey;
    const allowed = options.allowCredentials as { id: ArrayBuffer }[];
    expect(new Uint8Array(allowed[0]!.id)).toEqual(bytes(5, 6));
  });

  it('encodes a registration in the shape the backend parses', async () => {
    installAuthenticator(attestationCredential());
    const encoded = JSON.parse(
      await answerChallenge({ ceremonyId: 'c', options: { challenge: b64(bytes(1)) } }, 'create'),
    );
    expect(encoded).toMatchObject({
      id: 'cred-id',
      rawId: b64(bytes(1, 2, 3)),
      type: 'public-key',
      authenticatorAttachment: 'platform',
      response: {
        clientDataJSON: b64(bytes(10, 11)),
        attestationObject: b64(bytes(20, 21)),
        transports: ['internal', 'hybrid'],
      },
    });
    // No assertion members leak onto a registration.
    expect(encoded.response.signature).toBeUndefined();
    expect(encoded.response.userHandle).toBeUndefined();
  });

  it('carries the user handle, which is what names the account on a sign-in', async () => {
    installAuthenticator(assertionCredential());
    const encoded = JSON.parse(
      await answerChallenge({ ceremonyId: 'c', options: { challenge: b64(bytes(1)) } }, 'get'),
    );
    expect(encoded.response).toEqual({
      clientDataJSON: b64(bytes(10, 11)),
      authenticatorData: b64(bytes(30, 31)),
      signature: b64(bytes(40, 41)),
      userHandle: b64(bytes(50, 51)),
    });
    expect(encoded.response.attestationObject).toBeUndefined();
  });

  it('omits transports rather than sending null when the engine has none', async () => {
    installAuthenticator(
      attestationCredential({
        response: {
          clientDataJSON: bytes(10, 11).buffer,
          attestationObject: bytes(20, 21).buffer,
        },
      }),
    );
    const encoded = JSON.parse(
      await answerChallenge({ ceremonyId: 'c', options: { challenge: b64(bytes(1)) } }, 'create'),
    );
    expect('transports' in encoded.response).toBe(false);
  });

  it('reads a dismissed prompt as abandonment, never as a failure', async () => {
    installAuthenticator(new DOMException('denied', 'NotAllowedError'));
    await expect(
      answerChallenge({ ceremonyId: 'c', options: { challenge: b64(bytes(1)) } }, 'get'),
    ).rejects.toBeInstanceOf(PasskeyAbandonedError);
  });

  it('lets a real error through as itself', async () => {
    installAuthenticator(new DOMException('bad options', 'NotSupportedError'));
    await expect(
      answerChallenge({ ceremonyId: 'c', options: { challenge: b64(bytes(1)) } }, 'get'),
    ).rejects.not.toBeInstanceOf(PasskeyAbandonedError);
  });

  it('refuses on a page that cannot run one at all', async () => {
    await expect(
      answerChallenge({ ceremonyId: 'c', options: { challenge: b64(bytes(1)) } }, 'get'),
    ).rejects.toThrow(/cannot use passkeys/);
  });
});
