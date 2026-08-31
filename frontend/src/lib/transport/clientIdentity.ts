// Who this screen is, as the backend sees it. Leaf module: imports nothing,
// because both the transport client (which puts the identity on the WebSocket
// upgrade URL) and the UI-state store (which scopes its preference bucket by
// it) need it, and the transport cannot import a store.
//
// Two ids, mirroring transport.ClientIdentity on the Go side:
//
//   - The DEVICE id is durable, cached in localStorage, and shared by every
//     tab of this browser profile. It scopes per-device preferences and is
//     what a future "edited on <device>" affordance would name.
//   - The CONNECTION id is minted per page load and lives only in memory. Two
//     tabs have two of them, which is what makes it the correct key for "is
//     this frame the echo of my own change?" — the device id would make one
//     tab suppress the other's edits and sit on stale content.
//
// Both are opaque; the backend bounds them to 8..64 chars of [A-Za-z0-9-] and
// treats anything else as no identity at all.

const CLIENT_ID_CACHE_KEY = 'agent-overflow:uistate:clientId';

// Mirrors validClientID in app_uistate.go and validClientIdentityID in
// internal/transport/clientidentity.go.
const CLIENT_ID_PATTERN = /^[A-Za-z0-9-]{8,64}$/;

export function isValidClientId(value: unknown): value is string {
  return typeof value === 'string' && CLIENT_ID_PATTERN.test(value);
}

function readLocal(key: string): string | null {
  if (typeof localStorage === 'undefined') return null;
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

function writeLocal(key: string, value: string): void {
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.setItem(key, value);
  } catch {
    // Best-effort cache; the RPC layer is the durable copy.
  }
}

function mintId(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  // Non-crypto fallback for environments without crypto.randomUUID (older
  // happy-dom builds in tests). These ids name a preference bucket and a
  // connection, not a credential, so Math.random-grade uniqueness is fine.
  let out = '';
  for (let i = 0; i < 32; i++) out += Math.floor(Math.random() * 16).toString(16);
  return out;
}

/**
 * Resolve the durable device id: the `cid` query parameter wins (that is how a
 * launcher pins a screen to a known bucket), then the localStorage cache, then
 * a freshly minted one which is cached for next time.
 */
export function resolveDeviceId(): string {
  if (typeof window !== 'undefined') {
    const cid = new URLSearchParams(window.location.search).get('cid');
    if (isValidClientId(cid)) {
      writeLocal(CLIENT_ID_CACHE_KEY, cid);
      return cid;
    }
  }
  const cached = readLocal(CLIENT_ID_CACHE_KEY);
  if (isValidClientId(cached)) return cached;
  const minted = mintId();
  writeLocal(CLIENT_ID_CACHE_KEY, minted);
  return minted;
}

let deviceId = resolveDeviceId();

/**
 * Minted once per page load and never persisted. Survives reconnects on
 * purpose: a dropped socket is the same composer on the same screen, so its
 * own writes must keep being recognized as its own.
 */
const connectionId = mintId();

export function getDeviceId(): string {
  return deviceId;
}

export function getConnectionId(): string {
  return connectionId;
}

/** Test helper — re-run device-id resolution against current localStorage. */
export function reresolveDeviceIdForTest(): string {
  deviceId = resolveDeviceId();
  return deviceId;
}

/** Test helper — wipe the cached device id, then re-resolve (minting a new one). */
export function clearCachedDeviceIdForTest(): void {
  if (typeof localStorage !== 'undefined') {
    try {
      localStorage.removeItem(CLIENT_ID_CACHE_KEY);
    } catch {
      // ignore
    }
  }
  deviceId = resolveDeviceId();
}
