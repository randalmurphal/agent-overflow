// The paired device's signing key, and the proofs it mints
// (docs/specs/remote-access.md §4, phase 5).
//
// The backend half is `internal/identity/deviceproof.go`; that file owns
// the wire shape and the argument for every field. This one owns the key:
// where it lives, why it lives there, and what happens when it is gone.
//
// Non-extractable, in IndexedDB. Those two go together and neither works
// alone:
//
//   - `generateKey(..., extractable: false, ...)` means no code in this
//     page can ever read the private key back out — `exportKey` throws.
//     Script in the page can still CALL sign() while the tab is open, and
//     the boundaries doc says so plainly rather than claiming otherwise:
//     non-extractability bounds reuse AFTER the page closes, nothing more.
//     That is still the difference between a copied string working forever
//     and working never.
//   - localStorage cannot hold a CryptoKey at all — it stores strings, so
//     a key put there would have to be extractable to get there, which
//     defeats the first point entirely. IndexedDB stores structured
//     clones, and a non-extractable CryptoKey survives one and comes back
//     able to sign. Confirmed against a real Chromium in the phase-5
//     spike, not assumed.
//
// When there is no key. Two different situations, and they must not be
// confused:
//
//   - NO SECURE CONTEXT. A LAN page served over plain http has no
//     `crypto.subtle` at all, so there is nothing to generate. Spec §15
//     constraint 6 states this is a permanent property of that class, not
//     a gap to close: such a device enrolls with a bare random identifier
//     and the backend records it `bearer`. Everything about it keeps
//     working exactly as it did before phase 5.
//   - A KEY THAT WENT MISSING. A device enrolled `key` whose IndexedDB was
//     cleared while its localStorage session survived. It cannot sign, so
//     the backend refuses everything it sends with `proof_downgraded`.
//     deviceSession.ts detects this and clears the stored session rather
//     than retrying forever — the honest recovery is to pair again.

/**
 * Where the keypair lives. Versioned in the database NAME rather than
 * through `onupgradeneeded` migrations: there is exactly one record and
 * it is disposable — a device with no usable key pairs again, which is a
 * shorter path than any migration would be.
 */
const KEY_DB_NAME = 'agent-overflow-device-key';
const KEY_STORE_NAME = 'keys';
const KEY_RECORD_ID = 'device';

/** Matches internal/identity/deviceproof.go's pinned header values. */
const PROOF_TYP = 'ao-device-proof+jws';
const PROOF_ALG = 'ES256';

/** WebCrypto's spelling of the one curve the backend accepts. */
const KEY_ALGORITHM: EcKeyGenParams = { name: 'ECDSA', namedCurve: 'P-256' };
const SIGN_ALGORITHM: EcdsaParams = { name: 'ECDSA', hash: 'SHA-256' };

/** The stored record: both halves of one pair. */
interface StoredDeviceKey {
  privateKey: CryptoKey;
  publicKey: CryptoKey;
}

/**
 * Whether this page can hold a signing key at all.
 *
 * `crypto.subtle` is absent outside a secure context, which is the whole
 * of §15 constraint 6 as a runtime test. IndexedDB is checked alongside it
 * because a key that cannot be persisted would be regenerated per page
 * load, and a device whose identity changes every visit is not a device.
 */
export function canHoldDeviceKey(): boolean {
  return (
    typeof crypto !== 'undefined' &&
    typeof crypto.subtle?.generateKey === 'function' &&
    typeof indexedDB !== 'undefined'
  );
}

function openKeyDatabase(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open(KEY_DB_NAME, 1);
    request.onupgradeneeded = () => {
      const db = request.result;
      if (!db.objectStoreNames.contains(KEY_STORE_NAME)) db.createObjectStore(KEY_STORE_NAME);
    };
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error);
    request.onblocked = () => reject(new Error('device key database is blocked'));
  });
}

function readStoredKey(db: IDBDatabase): Promise<StoredDeviceKey | null> {
  return new Promise((resolve, reject) => {
    const tx = db.transaction(KEY_STORE_NAME, 'readonly');
    const request = tx.objectStore(KEY_STORE_NAME).get(KEY_RECORD_ID);
    request.onsuccess = () => resolve((request.result as StoredDeviceKey | undefined) ?? null);
    request.onerror = () => reject(request.error);
  });
}

function writeStoredKey(db: IDBDatabase, key: StoredDeviceKey): Promise<void> {
  return new Promise((resolve, reject) => {
    const tx = db.transaction(KEY_STORE_NAME, 'readwrite');
    tx.objectStore(KEY_STORE_NAME).put(key, KEY_RECORD_ID);
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error);
    tx.onabort = () => reject(tx.error);
  });
}

/**
 * A stored keypair that is actually usable for signing.
 *
 * The usage check is not paranoia about our own writer: a record written
 * by an older build, or one whose structured clone came back degraded,
 * would otherwise reach `sign()` and throw there — inside a credential
 * request, where the failure reads as a refused session rather than as a
 * missing key.
 */
function usableKey(held: StoredDeviceKey | null): StoredDeviceKey | null {
  if (!held?.privateKey || !held.publicKey) return null;
  if (!held.privateKey.usages?.includes('sign')) return null;
  return held;
}

// Single-flight, shared by both accessors. Two callers asking at once must
// observe ONE generation: a second concurrent generate would overwrite the
// first, and a device whose key changed between two requests of one page
// load would be refused on whichever request lost.
let keyInFlight: Promise<StoredDeviceKey | null> | null = null;

/**
 * The stored keypair, or null when this page holds none.
 *
 * Read-only on purpose. Generating on a miss would be the wrong answer for
 * the case that actually happens: a device already enrolled `key` whose
 * IndexedDB was cleared while its localStorage session survived. A fresh
 * key there is not a recovery — the session is bound to the OLD key's
 * thumbprint, so every request would be refused anyway, one round trip
 * later and under a reason (`key_mismatch`) that describes a different
 * problem. Null instead, so the caller can clear the session and ask the
 * person to pair, which is the only thing that actually works.
 *
 * Generation belongs to enrolment alone, which is the one moment a device
 * is allowed to decide which key names it: enrollDeviceKey.
 */
export function deviceKeyPair(): Promise<StoredDeviceKey | null> {
  return withKeyDatabase(async (db) => usableKey(await readStoredKey(db)));
}

/**
 * The keypair this device enrolls with, generating and persisting one if
 * it holds none. Only pairing calls this.
 *
 * Null when this page cannot hold a key (no secure context) or when
 * storage refused — both of which mean the caller enrolls `bearer`, never
 * that pairing fails. IndexedDB is unavailable in some private-browsing
 * modes, and a page that could not pair at all there would be worse than
 * one that pairs `bearer`.
 *
 * An EXISTING key is returned rather than replaced: re-pairing the same
 * browser is the same device, and the backend adopts its row by
 * thumbprint. Minting a second key would strand the first row.
 */
export function enrollDeviceKey(): Promise<StoredDeviceKey | null> {
  return withKeyDatabase(async (db) => {
    const held = usableKey(await readStoredKey(db));
    if (held) return held;
    // `extractable: false` applies to the PRIVATE half only — WebCrypto
    // always leaves an ECDSA public key extractable, which is what lets
    // the JWK be exported for the proof header. Confirmed in the spike.
    const generated = (await crypto.subtle.generateKey(KEY_ALGORITHM, false, [
      'sign',
      'verify',
    ])) as CryptoKeyPair;
    const pair: StoredDeviceKey = {
      privateKey: generated.privateKey,
      publicKey: generated.publicKey,
    };
    // Persisted BEFORE it is returned: a key that signed a redemption and
    // then failed to store would enroll a device this page can never
    // present again.
    await writeStoredKey(db, pair);
    return pair;
  });
}

/**
 * Run one database operation under the shared single-flight, answering
 * null for every "this page cannot" — no secure context, storage refused,
 * a transaction that failed. Callers treat null as "no key", never as an
 * error to report.
 */
function withKeyDatabase(
  operation: (db: IDBDatabase) => Promise<StoredDeviceKey | null>,
): Promise<StoredDeviceKey | null> {
  if (keyInFlight) return keyInFlight;
  keyInFlight = (async () => {
    if (!canHoldDeviceKey()) return null;
    let db: IDBDatabase;
    try {
      db = await openKeyDatabase();
    } catch {
      return null;
    }
    try {
      return await operation(db);
    } catch {
      return null;
    } finally {
      db.close();
    }
  })().finally(() => {
    keyInFlight = null;
  });
  return keyInFlight;
}

/** Drop the stored keypair. Only for tests and an explicit re-enrolment. */
export async function clearDeviceKey(): Promise<void> {
  if (typeof indexedDB === 'undefined') return;
  await new Promise<void>((resolve) => {
    const request = indexedDB.deleteDatabase(KEY_DB_NAME);
    request.onsuccess = () => resolve();
    request.onerror = () => resolve();
    request.onblocked = () => resolve();
  });
}

function base64url(bytes: ArrayBuffer | Uint8Array): string {
  const view = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes);
  let binary = '';
  for (const byte of view) binary += String.fromCharCode(byte);
  return btoa(binary).replaceAll('+', '-').replaceAll('/', '_').replace(/=+$/, '');
}

function base64urlText(text: string): string {
  return base64url(new TextEncoder().encode(text));
}

/**
 * The four JWK members RFC 7638 hashes for an EC key, and no others.
 *
 * `exportKey('jwk')` also returns `ext` and `key_ops`. Including either
 * would make this page's thumbprint disagree with the backend's, and the
 * device would be refused with `key_mismatch` for a key that is correct —
 * so the four are picked out explicitly rather than spreading the export.
 */
function requiredJwkMembers(jwk: JsonWebKey): { crv: string; kty: string; x: string; y: string } {
  return { crv: jwk.crv ?? '', kty: jwk.kty ?? '', x: jwk.x ?? '', y: jwk.y ?? '' };
}

/**
 * Mint one proof for one request.
 *
 * Null when this device holds no signing key, which is the caller's cue to
 * present its bearer identifier instead. A proof is single-use and bound
 * to `method` and `path`, so callers mint one per request and never cache
 * one — a cached proof is refused as `proof_replayed`, and one presented
 * on another route as `proof_not_bound`.
 *
 * `path` is the request path alone, matching what the backend reads off
 * the request. Never a full URL: one backend is reached under several
 * authorities and the client cannot predict which one its request will be
 * seen under (internal/identity/deviceproof.go).
 */
export async function mintDeviceProof(method: string, path: string): Promise<string | null> {
  const pair = await deviceKeyPair();
  if (!pair) return null;
  try {
    const jwk = await crypto.subtle.exportKey('jwk', pair.publicKey);
    const header = base64urlText(
      JSON.stringify({ typ: PROOF_TYP, alg: PROOF_ALG, jwk: requiredJwkMembers(jwk) }),
    );
    const payload = base64urlText(
      JSON.stringify({ htm: method, htp: path, jti: newProofID(), iatMs: Date.now() }),
    );
    const signingInput = `${header}.${payload}`;
    // WebCrypto answers r‖s — 64 raw bytes for P-256 — which is exactly
    // what a JWS ES256 signature is. No ASN.1 unwrapping to do, and the Go
    // side would silently fail to verify if there were.
    const signature = await crypto.subtle.sign(
      SIGN_ALGORITHM,
      pair.privateKey,
      new TextEncoder().encode(signingInput),
    );
    return `${signingInput}.${base64url(signature)}`;
  } catch {
    // Signing failed against a key we hold: a corrupted record, or storage
    // revoked mid-session. Null sends the caller down the bearer path,
    // where the backend refuses it as the downgrade it is and the session
    // is cleared — which is the correct outcome, and a better one than an
    // exception escaping into a credential request.
    return null;
  }
}

/**
 * A fresh proof identifier: 128 bits of CSPRNG output, base64url.
 *
 * Its only job is to be unique per proof, since the backend spends it
 * once. Random rather than a counter because a counter would have to
 * survive reloads to stay unique, and the backend's guard is a set rather
 * than a high-water mark.
 */
function newProofID(): string {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  return base64url(bytes);
}
