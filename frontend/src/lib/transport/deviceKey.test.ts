// The signing key and the proofs it mints. Needs a real IndexedDB and a
// real WebCrypto, so it brings the first and relies on Node's for the
// second — the assertions below are worthless against a fake signer,
// since what they are checking is that a Go verifier will accept this.
//
// `fake-indexeddb/auto` is imported for its side effect and must come
// before the module under test, which reads `indexedDB` at call time.
import 'fake-indexeddb/auto';
import { beforeEach, describe, expect, it } from 'vitest';
import {
  canHoldDeviceKey,
  clearDeviceKey,
  deviceKeyPair,
  enrollDeviceKey,
  mintDeviceProof,
} from './deviceKey';

const VERIFY_ALGORITHM: EcdsaParams = { name: 'ECDSA', hash: 'SHA-256' };

/** Decode one base64url segment of a compact JWS to text. */
function decodeSegment(segment: string): string {
  const padded = segment.replaceAll('-', '+').replaceAll('_', '/');
  return atob(padded.padEnd(padded.length + ((4 - (padded.length % 4)) % 4), '='));
}

// Allocated rather than `Uint8Array.from`, whose result is typed over
// ArrayBufferLike and so is not a BufferSource WebCrypto will accept.
function decodeSegmentBytes(segment: string): Uint8Array<ArrayBuffer> {
  const binary = decodeSegment(segment);
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index += 1) bytes[index] = binary.charCodeAt(index);
  return bytes;
}

interface ProofHeader {
  typ: string;
  alg: string;
  jwk: Record<string, string>;
}

interface ProofPayload {
  htm: string;
  htp: string;
  jti: string;
  iatMs: number;
}

function readProof(proof: string): { header: ProofHeader; payload: ProofPayload } {
  const [header, payload] = proof.split('.');
  return {
    header: JSON.parse(decodeSegment(header)) as ProofHeader,
    payload: JSON.parse(decodeSegment(payload)) as ProofPayload,
  };
}

beforeEach(async () => {
  await clearDeviceKey();
});

describe('canHoldDeviceKey', () => {
  it('answers true where both a secure context and storage exist', () => {
    expect(canHoldDeviceKey()).toBe(true);
  });
});

describe('enrollDeviceKey', () => {
  it('generates a private key this page can never read back out', async () => {
    const pair = await enrollDeviceKey();
    expect(pair).not.toBeNull();
    expect(pair?.privateKey.extractable).toBe(false);
    await expect(crypto.subtle.exportKey('jwk', pair!.privateKey)).rejects.toThrow();
    // The public half stays extractable whatever is asked for, which is
    // what lets the proof header carry it.
    expect(pair?.publicKey.extractable).toBe(true);
  });

  it('keeps the same key across calls rather than minting a second', async () => {
    const first = await enrollDeviceKey();
    const second = await enrollDeviceKey();
    const firstJwk = await crypto.subtle.exportKey('jwk', first!.publicKey);
    const secondJwk = await crypto.subtle.exportKey('jwk', second!.publicKey);
    expect(secondJwk.x).toBe(firstJwk.x);
    expect(secondJwk.y).toBe(firstJwk.y);
  });

  it('survives the page: a key stored once is read back able to sign', async () => {
    await enrollDeviceKey();
    const held = await deviceKeyPair();
    expect(held).not.toBeNull();
    const signature = await crypto.subtle.sign(
      { name: 'ECDSA', hash: 'SHA-256' },
      held!.privateKey,
      new TextEncoder().encode('after the round trip'),
    );
    // Raw r‖s, which is what a JWS ES256 signature is. An ASN.1-wrapped
    // signature would be longer and the Go verifier would refuse it.
    expect(new Uint8Array(signature).length).toBe(64);
  });

  it('single-flights: concurrent enrolments observe ONE generation', async () => {
    const [a, b, c] = await Promise.all([enrollDeviceKey(), enrollDeviceKey(), enrollDeviceKey()]);
    const [ja, jb, jc] = await Promise.all([
      crypto.subtle.exportKey('jwk', a!.publicKey),
      crypto.subtle.exportKey('jwk', b!.publicKey),
      crypto.subtle.exportKey('jwk', c!.publicKey),
    ]);
    expect(jb.x).toBe(ja.x);
    expect(jc.x).toBe(ja.x);
  });
});

describe('deviceKeyPair', () => {
  // The whole reason reading and generating are separate functions. A
  // device enrolled `key` whose storage was cleared must NOT quietly
  // acquire a different key: its session is bound to the thumbprint of
  // the old one, so a new key is not a recovery, and the honest answer
  // is that this page holds nothing.
  it('answers null before enrolment instead of generating one', async () => {
    expect(await deviceKeyPair()).toBeNull();
    expect(await mintDeviceProof('POST', '/auth/ticket')).toBeNull();
  });

  it('answers null again once the stored key is dropped', async () => {
    await enrollDeviceKey();
    expect(await deviceKeyPair()).not.toBeNull();
    await clearDeviceKey();
    expect(await deviceKeyPair()).toBeNull();
  });
});

describe('mintDeviceProof', () => {
  it('mints a compact JWS the Go verifier accepts, field for field', async () => {
    const pair = await enrollDeviceKey();
    const proof = await mintDeviceProof('POST', '/auth/token');
    expect(proof).not.toBeNull();

    const segments = proof!.split('.');
    expect(segments).toHaveLength(3);
    const { header, payload } = readProof(proof!);

    // Pinned by internal/identity/deviceproof.go. A drift in either
    // spelling is refused there as a malformed proof.
    expect(header.typ).toBe('ao-device-proof+jws');
    expect(header.alg).toBe('ES256');
    // Exactly the four members RFC 7638 hashes, and no others: exportKey
    // also answers `ext` and `key_ops`, and including either would make
    // this page's thumbprint disagree with the backend's.
    expect(Object.keys(header.jwk).sort()).toEqual(['crv', 'kty', 'x', 'y']);
    expect(header.jwk.crv).toBe('P-256');
    expect(header.jwk.kty).toBe('EC');

    const exported = await crypto.subtle.exportKey('jwk', pair!.publicKey);
    expect(header.jwk.x).toBe(exported.x);
    expect(header.jwk.y).toBe(exported.y);

    expect(payload.htm).toBe('POST');
    // The PATH alone, never a full URL: one backend answers under
    // loopback, a LAN address, a WSL relay and a proxy authority, and
    // the client cannot predict which one its request is seen under.
    expect(payload.htp).toBe('/auth/token');
    expect(payload.jti).toMatch(/^[A-Za-z0-9_-]{22}$/);
    expect(payload.iatMs).toBeGreaterThan(Date.now() - 5_000);
    expect(payload.iatMs).toBeLessThanOrEqual(Date.now());

    // The signature covers header.payload, and verifies under the key
    // whose JWK the header carries.
    const signed = await crypto.subtle.verify(
      VERIFY_ALGORITHM,
      pair!.publicKey,
      decodeSegmentBytes(segments[2]),
      new TextEncoder().encode(`${segments[0]}.${segments[1]}`),
    );
    expect(signed).toBe(true);
  });

  it('binds each proof to the request it was minted for', async () => {
    await enrollDeviceKey();
    const token = readProof((await mintDeviceProof('POST', '/auth/token'))!);
    const ticket = readProof((await mintDeviceProof('GET', '/bootstrap.json'))!);
    expect(token.payload.htp).toBe('/auth/token');
    expect(ticket.payload.htp).toBe('/bootstrap.json');
    expect(ticket.payload.htm).toBe('GET');
  });

  it('never repeats a proof id, because the backend spends each once', async () => {
    await enrollDeviceKey();
    const ids = new Set<string>();
    for (let i = 0; i < 32; i += 1) {
      ids.add(readProof((await mintDeviceProof('POST', '/auth/ticket'))!).payload.jti);
    }
    expect(ids.size).toBe(32);
  });
});
