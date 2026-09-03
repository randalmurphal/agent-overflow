// The app's one random-identifier mint.
//
// It exists because `crypto.randomUUID` is a SECURE-CONTEXT API and this
// app has a shipped, supported, insecure context: a plain-HTTP LAN page
// (docs/specs/remote-access.md §15 constraint 6, which states outright
// that there is deliberately no LAN-HTTP proof path and that such a page
// keeps working). On that origin `crypto.randomUUID` is not merely
// absent — reading it answers undefined and CALLING it throws a
// TypeError, so an unguarded call site is not a degraded feature, it is
// an uncaught exception wherever it sits.
//
// It sat in `wsClient.generateId`, which mints the id of every RPC. A
// paired LAN browser therefore threw on its first call and rendered a
// BLANK PAGE — pairing succeeded, the credential was real, and the app
// never mounted (found by the harness, 2026-08-31). The comment there
// asserted that "every environment we run in ships randomUUID", which is
// exactly the belief a secure-context API invites and exactly the one
// that was false.
//
// `crypto.getRandomValues` is NOT secure-context-gated, so the fallback
// is a real CSPRNG rather than a downgrade: the same reason
// `transport/deviceSession.ts` mints its device identifier from it. Only
// the last resort — no WebCrypto at all, which in practice means an old
// happy-dom in a unit test — reaches `Math.random`.

const HEX = '0123456789abcdef';

/** 16 CSPRNG bytes, or Math.random-grade bytes where no CSPRNG exists. */
function randomBytes16(): Uint8Array {
  const bytes = new Uint8Array(16);
  if (typeof crypto !== 'undefined' && typeof crypto.getRandomValues === 'function') {
    crypto.getRandomValues(bytes);
    return bytes;
  }
  for (let i = 0; i < bytes.length; i++) bytes[i] = Math.floor(Math.random() * 256);
  return bytes;
}

/**
 * A random RFC 4122 version-4 identifier, in every context this app runs
 * in — embedded webview, `--connect` client, HTTPS browser, and the
 * plain-HTTP LAN browser that has no `crypto.randomUUID`.
 *
 * The shape is a UUID string in all of them, because callers key maps,
 * slice it for DOM ids, and hand it to a backend that bounds client
 * identifiers to `[A-Za-z0-9-]{8,64}` — a fallback of a different shape
 * would be a second format nothing validates against.
 */
export function randomId(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  const bytes = randomBytes16();
  // Version 4, variant 10xx: the two fields that make the string a
  // well-formed UUID rather than 32 arbitrary hex characters.
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  let out = '';
  for (let i = 0; i < 16; i++) {
    if (i === 4 || i === 6 || i === 8 || i === 10) out += '-';
    out += HEX[bytes[i] >> 4] + HEX[bytes[i] & 0x0f];
  }
  return out;
}
